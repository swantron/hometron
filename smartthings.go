package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Samsung fridge (and anything else SmartThings reports as a refrigerator) through the
// SmartThings cloud API. Samsung appliances have no local API.
//
// Config: <state>/smartthings.json, one of:
//
//	{"token": "<personal access token>"}                  // quick test: SmartThings PATs last 24 hours
//	{"clientId": "...", "clientSecret": "...", "redirectUri": "https://…/smartthings/callback"}
//
// With an OAuth app, open /smartthings/connect once in the dashboard's browser to sign in;
// hometron then keeps the tokens fresh in <state>/smartthings-token.json. SmartThings
// refresh tokens are single-use, so each new one is saved before it's relied on.
type stConfig struct {
	Token        string    `json:"token,omitempty"`
	Auto         bool      `json:"auto,omitempty"`      // renew the 24-hour token through a signed-in browser profile
	RenewedAt    time.Time `json:"renewedAt,omitempty"` // when hometron last made the token
	ClientID     string    `json:"clientId,omitempty"`
	ClientSecret string    `json:"clientSecret,omitempty"`
	RedirectURI  string    `json:"redirectUri,omitempty"`
}

type stTokens struct {
	Access  string    `json:"access_token"`
	Refresh string    `json:"refresh_token"`
	Expiry  time.Time `json:"expiry"`
}

const stAPI = "https://api.smartthings.com/v1/"

type SmartThings struct {
	cfg       stConfig
	cfgPath   string
	tokenPath string

	// auto-renewal of the personal token (see stbrowser.go)
	browser   *stBrowser
	needRenew bool
	signingIn bool
	renewErr  error
	lastTry   time.Time
	aliveAt   time.Time // last keep-alive visit
	api       string    // stAPI, or a test server
	http      *http.Client
	events    *Events

	authMu sync.Mutex // one refresh at a time: a refresh token works once
	tok    stTokens
	state  string // OAuth state for the connect flow in progress

	mu      sync.Mutex
	fridges map[string]*fridge
	err     string
}

type fridge struct {
	ID, Name string
	Parts    map[string]*fridgePart // component -> state ("cooler", "freezer", "main", ...)
	FilterPc *int                   // water filter used, percent
	FilterOK string
	Ice      string // "Cubed ice making · Ice Bites ready"
	KWhMonth *float64
	KWhLast  *float64
	Notes    []string // defrosting, Power Freeze on, ...
	Seen     time.Time
}

type fridgePart struct {
	Open      *bool
	OpenSince time.Time
	TempF     *float64
	SetF      *float64
	Mode      string // convertible drawer: "Deli", "Wine", ...
	warmSince time.Time
	alerted   bool // door-open alert sent for this opening
	warmSent  bool
}

func loadSmartThings(stateDir string, events *Events) (*SmartThings, error) {
	b, err := os.ReadFile(filepath.Join(stateDir, "smartthings.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var cfg stConfig
	if err := json.Unmarshal(b, &cfg); err != nil {
		return nil, fmt.Errorf("smartthings.json: %w", err)
	}
	if cfg.Token == "" && (cfg.ClientID == "" || cfg.ClientSecret == "") {
		return nil, errors.New("smartthings.json: need a token, or clientId and clientSecret")
	}
	st := &SmartThings{cfg: cfg, cfgPath: filepath.Join(stateDir, "smartthings.json"), tokenPath: filepath.Join(stateDir, "smartthings-token.json"), api: stAPI,
		browser: &stBrowser{bin: chromeBin, profile: stProfileDir(stateDir), base: "https://account.smartthings.com"},
		http:    &http.Client{Timeout: 20 * time.Second}, events: events, fridges: map[string]*fridge{}}
	if b, err := os.ReadFile(st.tokenPath); err == nil {
		json.Unmarshal(b, &st.tok)
	}
	return st, nil
}

// Connected reports whether there's a way to call the API (vs. waiting for sign-in).
func (st *SmartThings) Connected() bool {
	st.authMu.Lock()
	defer st.authMu.Unlock()
	return st.cfg.Token != "" || st.tok.Refresh != ""
}

// maybeRenew keeps the browser session alive (every 10 minutes) and makes a new personal
// token when the current one is near its 24 hours or was rejected. Not while someone is
// signing in, and not more than every 10 minutes after a failure.
func (st *SmartThings) maybeRenew(ctx context.Context, now time.Time) {
	st.authMu.Lock()
	if !st.cfg.Auto || st.signingIn {
		st.authMu.Unlock()
		return
	}
	due := st.needRenew || st.cfg.RenewedAt.IsZero() || now.Sub(st.cfg.RenewedAt) > 20*time.Hour
	if st.renewErr != nil && now.Sub(st.lastTry) < 10*time.Minute {
		due = false
	}
	alive := !due && now.Sub(st.aliveAt) >= 10*time.Minute && !errors.Is(st.renewErr, errNeedsSignIn)
	if due {
		st.lastTry = now
	}
	if alive {
		st.aliveAt = now
	}
	st.authMu.Unlock()

	if alive {
		if err := st.browser.KeepAlive(ctx); err != nil {
			log.Printf("smartthings: keep-alive: %v", err)
			st.authMu.Lock()
			st.renewErr, st.lastTry = err, now
			st.authMu.Unlock()
		}
		return
	}
	if !due {
		return
	}
	tok, err := st.browser.Renew(ctx)
	st.authMu.Lock()
	defer st.authMu.Unlock()
	st.renewErr = err
	if err != nil {
		log.Printf("smartthings: renewing token: %v", err)
		return
	}
	st.cfg.Token, st.cfg.RenewedAt, st.needRenew, st.aliveAt = tok, now, false, now
	b, _ := json.MarshalIndent(st.cfg, "", "  ")
	tmp := st.cfgPath + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err == nil {
		os.Rename(tmp, st.cfgPath)
	}
	log.Printf("smartthings: new 24-hour token")
}

// SignIn puts the SmartThings tokens page in front in the session browser on the server's
// screen, then checks every 15 seconds (for up to half an hour) until the session works.
// No need to close anything afterwards: the browser stays running to keep the session.
func (st *SmartThings) SignIn() error {
	st.authMu.Lock()
	if st.signingIn {
		st.authMu.Unlock()
		return errors.New("already waiting for a sign-in on the server")
	}
	st.signingIn = true
	st.authMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	err := st.browser.ShowSignIn(ctx)
	cancel()
	if err != nil {
		st.authMu.Lock()
		st.signingIn = false
		st.authMu.Unlock()
		return err
	}
	go func() {
		deadline := time.Now().Add(30 * time.Minute)
		for time.Now().Before(deadline) {
			time.Sleep(15 * time.Second)
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			err := st.browser.KeepAlive(ctx)
			cancel()
			if err == nil {
				log.Printf("smartthings: signed in")
				st.authMu.Lock()
				st.signingIn, st.needRenew, st.renewErr = false, true, nil
				st.authMu.Unlock()
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
				st.maybeRenew(ctx, time.Now())
				st.poll(ctx, time.Now())
				cancel()
				return
			}
		}
		st.authMu.Lock()
		st.signingIn = false
		st.authMu.Unlock()
	}()
	return nil
}

// accessToken returns a usable bearer token, refreshing an OAuth one that's about to expire.
func (st *SmartThings) accessToken(ctx context.Context, force bool) (string, error) {
	st.authMu.Lock()
	defer st.authMu.Unlock()
	if st.cfg.Token != "" {
		return st.cfg.Token, nil
	}
	if st.tok.Refresh == "" {
		return "", errors.New("not connected: open /smartthings/connect in the dashboard to sign in to SmartThings")
	}
	if !force && st.tok.Access != "" && time.Until(st.tok.Expiry) > 5*time.Minute {
		return st.tok.Access, nil
	}
	t, err := st.tokenRequest(ctx, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {st.tok.Refresh}, "client_id": {st.cfg.ClientID}})
	if err != nil {
		return "", fmt.Errorf("refreshing SmartThings sign-in: %w", err)
	}
	st.tok = t
	return t.Access, nil
}

// tokenRequest calls the token endpoint and saves the result before returning it, since
// the refresh token it replaces no longer works. Caller holds authMu.
func (st *SmartThings) tokenRequest(ctx context.Context, form url.Values) (stTokens, error) {
	req, _ := http.NewRequestWithContext(ctx, "POST", st.api+"oauth/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.SetBasicAuth(st.cfg.ClientID, st.cfg.ClientSecret)
	res, err := st.http.Do(req)
	if err != nil {
		return stTokens{}, err
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(res.Body, 64<<10))
	if res.StatusCode != 200 {
		return stTokens{}, fmt.Errorf("token endpoint: HTTP %d", res.StatusCode) // body may echo secrets; leave it out
	}
	var r struct {
		Access    string `json:"access_token"`
		Refresh   string `json:"refresh_token"`
		ExpiresIn int    `json:"expires_in"`
	}
	if err := json.Unmarshal(b, &r); err != nil || r.Access == "" {
		return stTokens{}, errors.New("token endpoint: unexpected response")
	}
	t := stTokens{Access: r.Access, Refresh: r.Refresh, Expiry: time.Now().Add(time.Duration(max(r.ExpiresIn, 60)) * time.Second)}
	if t.Refresh == "" {
		t.Refresh = st.tok.Refresh
	}
	out, _ := json.MarshalIndent(t, "", " ")
	tmp := st.tokenPath + ".tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return stTokens{}, err
	}
	if err := os.Rename(tmp, st.tokenPath); err != nil {
		return stTokens{}, err
	}
	return t, nil
}

// ConnectURL starts the OAuth sign-in.
func (st *SmartThings) ConnectURL(redirect string) (string, error) {
	if st.cfg.ClientID == "" {
		return "", errors.New("smartthings.json has a personal token, not an OAuth app")
	}
	b := make([]byte, 16)
	rand.Read(b)
	st.authMu.Lock()
	st.state = base64.RawURLEncoding.EncodeToString(b)
	state := st.state
	st.authMu.Unlock()
	q := url.Values{"client_id": {st.cfg.ClientID}, "response_type": {"code"}, "redirect_uri": {redirect},
		"scope": {"r:devices:* r:locations:*"}, "state": {state}}
	return st.api + "oauth/authorize?" + q.Encode(), nil
}

// Finish exchanges the code from the callback for tokens.
func (st *SmartThings) Finish(ctx context.Context, code, state, redirect string) error {
	st.authMu.Lock()
	defer st.authMu.Unlock()
	if st.state == "" || state != st.state {
		return errors.New("sign-in state doesn't match; start again from /smartthings/connect")
	}
	st.state = ""
	t, err := st.tokenRequest(ctx, url.Values{"grant_type": {"authorization_code"}, "code": {code}, "client_id": {st.cfg.ClientID}, "redirect_uri": {redirect}})
	if err != nil {
		return err
	}
	st.tok = t
	return nil
}

func (st *SmartThings) get(ctx context.Context, path string, out any) error {
	st.authMu.Lock()
	pat := st.cfg.Token != "" // the token itself may be swapped by a renewal; read it via accessToken
	st.authMu.Unlock()
	for attempt := 0; attempt < 2; attempt++ {
		tok, err := st.accessToken(ctx, attempt > 0)
		if err != nil {
			return err
		}
		req, _ := http.NewRequestWithContext(ctx, "GET", st.api+path, nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Accept", "application/json")
		res, err := st.http.Do(req)
		if err != nil {
			return err
		}
		b, _ := io.ReadAll(io.LimitReader(res.Body, 4<<20))
		res.Body.Close()
		if res.StatusCode == 401 && attempt == 0 && !pat {
			continue // expired early: refresh once and retry
		}
		if res.StatusCode == 401 && pat {
			st.authMu.Lock()
			st.needRenew = st.cfg.Auto
			st.authMu.Unlock()
			return errors.New("SmartThings rejected the token (personal tokens last 24 hours)")
		}
		if res.StatusCode != 200 {
			return fmt.Errorf("SmartThings %s: HTTP %d", path, res.StatusCode)
		}
		return json.Unmarshal(b, out)
	}
	return errors.New("SmartThings: still unauthorized after refreshing")
}

// Run polls every 30 seconds so a door left open is noticed within a couple of minutes.
func (st *SmartThings) Run(ctx context.Context) {
	for {
		st.maybeRenew(ctx, time.Now())
		if st.Connected() {
			st.poll(ctx, time.Now())
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(30 * time.Second):
		}
	}
}

type stDevice struct {
	DeviceID   string `json:"deviceId"`
	Label      string `json:"label"`
	Name       string `json:"name"`
	Components []struct {
		ID           string `json:"id"`
		Capabilities []struct {
			ID string `json:"id"`
		} `json:"capabilities"`
	} `json:"components"`
	OCF *struct {
		DeviceType string `json:"ocfDeviceType"`
	} `json:"ocf"`
}

func (d stDevice) isFridge() bool {
	if d.OCF != nil && d.OCF.DeviceType == "oic.d.refrigerator" {
		return true
	}
	for _, c := range d.Components {
		for _, cp := range c.Capabilities {
			if cp.ID == "refrigeration" {
				return true
			}
		}
	}
	return false
}

func (st *SmartThings) poll(ctx context.Context, now time.Time) {
	var list struct {
		Items []stDevice `json:"items"`
	}
	if err := st.get(ctx, "devices", &list); err != nil {
		st.setErr(err)
		return
	}
	st.setErr(nil)
	for _, d := range list.Items {
		if !d.isFridge() {
			continue
		}
		var status stStatus
		if err := st.get(ctx, "devices/"+d.DeviceID+"/status", &status); err != nil {
			log.Printf("smartthings %s: %v", d.Label, err)
			continue
		}
		name := d.Label
		if name == "" {
			name = d.Name
		}
		st.update(d.DeviceID, name, status, now)
	}
}

// stStatus is component -> capability -> attribute -> {value, unit}.
type stStatus struct {
	Components map[string]map[string]map[string]struct {
		Value any    `json:"value"`
		Unit  string `json:"unit"`
	} `json:"components"`
}

func (s stStatus) attr(comp, capab, attr string) (any, string) {
	a, ok := s.Components[comp][capab][attr]
	if !ok {
		return nil, ""
	}
	return a.Value, a.Unit
}

func (s stStatus) tempF(comp, capab, attr string) *float64 {
	v, unit := s.attr(comp, capab, attr)
	f, ok := v.(float64)
	if !ok {
		return nil
	}
	if unit == "C" {
		f = f*9/5 + 32
	}
	f = math.Round(f)
	return &f
}

// Door left open this long gets an alert; a compartment this warm for warmFor does too.
const (
	fridgeDoorAlert = 2 * time.Minute
	warmFor         = 30 * time.Minute
)

func (st *SmartThings) update(id, name string, s stStatus, now time.Time) {
	var posts []Event
	st.mu.Lock()
	f := st.fridges[id]
	if f == nil {
		f = &fridge{ID: id, Parts: map[string]*fridgePart{}}
		st.fridges[id] = f
	}
	f.Name, f.Seen = name, now
	// "main" reports "any door open"; when the compartments report their own doors,
	// alerting on main too would say everything twice.
	perDoor := false
	for comp := range s.Components {
		if v, _ := s.attr(comp, "contactSensor", "contact"); v != nil && comp != "main" {
			perDoor = true
		}
	}
	for comp := range s.Components {
		p := f.Parts[comp]
		if p == nil {
			p = &fridgePart{}
		}
		if v, _ := s.attr(comp, "contactSensor", "contact"); v != nil && !(comp == "main" && perDoor) {
			open := v == "open"
			if open && (p.Open == nil || !*p.Open) {
				p.OpenSince, p.alerted = now, false
			}
			if !open && p.Open != nil && *p.Open && p.alerted {
				posts = append(posts, Event{Time: now, DeviceID: "st:" + id + ":" + comp, Device: partName(comp) + " door closed (open " + shortDur(now.Sub(p.OpenSince)) + ")", Kind: "fridge"})
			}
			p.Open = &open
			if open && !p.alerted && now.Sub(p.OpenSince) >= fridgeDoorAlert {
				p.alerted = true
				posts = append(posts, Event{Time: now, DeviceID: "st:" + id + ":" + comp, Device: partName(comp) + " door has been open " + shortDur(now.Sub(p.OpenSince)), Kind: "fridge"})
			}
		}
		if strings.HasPrefix(comp, "icemaker") {
			continue // summarized separately
		}
		if v, _ := s.attr(comp, "custom.fridgeMode", "fridgeMode"); v != nil && comp == "cvroom" {
			m, _ := v.(string)
			p.Mode = drawerMode(m)
		}
		p.TempF = s.tempF(comp, "temperatureMeasurement", "temperature")
		p.SetF = s.tempF(comp, "thermostatCoolingSetpoint", "coolingSetpoint")
		if limit, ok := warmLimit(comp); ok && p.TempF != nil {
			if *p.TempF > limit {
				if p.warmSince.IsZero() {
					p.warmSince = now
				}
				if !p.warmSent && now.Sub(p.warmSince) >= warmFor {
					p.warmSent = true
					posts = append(posts, Event{Time: now, DeviceID: "st:" + id + ":" + comp, Device: fmt.Sprintf("%s is warm: %.0f°F for %s", partName(comp), *p.TempF, shortDur(now.Sub(p.warmSince))), Kind: "fridge"})
				}
			} else {
				p.warmSince, p.warmSent = time.Time{}, false
			}
		}
		if p.Open != nil || p.TempF != nil {
			f.Parts[comp] = p
		}
	}
	if v, _ := s.attr("main", "custom.waterFilter", "waterFilterUsage"); v != nil {
		if n, ok := v.(float64); ok {
			pc := int(n)
			f.FilterPc = &pc
		}
	}
	if v, _ := s.attr("main", "custom.waterFilter", "waterFilterStatus"); v != nil {
		f.FilterOK, _ = v.(string)
	}
	f.Ice = iceSummary(s)
	kwh := func(attr string) *float64 {
		v, _ := s.attr("main", "samsungce.energyManagement", attr)
		if n, ok := v.(float64); ok {
			k := math.Round(n/100) / 10 // reported in Wh
			return &k
		}
		return nil
	}
	f.KWhMonth, f.KWhLast = kwh("thisMonthEnergyConsumption"), kwh("lastMonthEnergyConsumption")
	f.Notes = nil
	if v, _ := s.attr("main", "refrigeration", "defrost"); v == "on" {
		f.Notes = append(f.Notes, "defrosting")
	}
	if v, _ := s.attr("main", "samsungce.powerFreeze", "activated"); v == true {
		f.Notes = append(f.Notes, "Power Freeze on")
	}
	if v, _ := s.attr("main", "samsungce.powerCool", "activated"); v == true {
		f.Notes = append(f.Notes, "Power Cool on")
	}
	if v, _ := s.attr("main", "samsungce.sabbathMode", "status"); v == "on" {
		f.Notes = append(f.Notes, "Sabbath mode")
	}
	if v, _ := s.attr("main", "samsungce.selfCheck", "result"); v != nil && v != "passed" {
		f.Notes = append(f.Notes, fmt.Sprintf("self-check: %v", v))
	}
	st.mu.Unlock()
	for _, e := range posts {
		if st.events != nil {
			st.events.Post(e)
		}
	}
}

// iceSummary describes each ice maker: "Cubed ice making · Ice Bites ready".
func iceSummary(s stStatus) string {
	var comps []string
	for c := range s.Components {
		if strings.HasPrefix(c, "icemaker") {
			comps = append(comps, c)
		}
	}
	sort.Strings(comps)
	var out []string
	for _, c := range comps {
		name := "Ice"
		if v, _ := s.attr(c, "samsungce.fridgeIcemakerInfo", "name"); v != nil {
			switch v {
			case "CUBED_ICE":
				name = "Cubed ice"
			case "ICE_BITES":
				name = "Ice Bites"
			default:
				name = titleCase(strings.ToLower(strings.ReplaceAll(fmt.Sprint(v), "_", " ")))
			}
		}
		state := ""
		if v, _ := s.attr(c, "switch", "switch"); v == "off" {
			state = "off"
		} else if v, _ := s.attr(c, "samsungce.fridgeIcemakerOperatingState", "operatingState"); v != nil {
			state = map[string]string{"running": "making", "ready": "full"}[fmt.Sprint(v)]
			if state == "" {
				state = strings.ToLower(fmt.Sprint(v))
			}
		} else if v, _ := s.attr(c, "switch", "switch"); v == "on" {
			state = "on"
		}
		if state != "" {
			out = append(out, name+" "+state)
		}
	}
	return strings.Join(out, " · ")
}

func drawerMode(m string) string {
	m = strings.TrimPrefix(m, "CV_FDR_")
	switch m {
	case "WINE":
		return "Wine"
	case "DELI":
		return "Deli"
	case "BEVERAGE":
		return "Beverage"
	case "MEAT", "MEAT_FISH":
		return "Meat"
	case "":
		return ""
	}
	return titleCase(strings.ToLower(strings.ReplaceAll(m, "_", " ")))
}

// warmLimit: above this for half an hour is worth knowing about (FDA: fridge ≤ 40°F,
// freezer ≤ 0°F; a little slack for door openings and defrost cycles).
func warmLimit(comp string) (float64, bool) {
	switch comp {
	case "cooler":
		return 45, true
	case "freezer":
		return 15, true
	}
	return 0, false
}

func partName(comp string) string {
	switch comp {
	case "main":
		return "Fridge"
	case "cooler":
		return "Fridge"
	case "freezer":
		return "Freezer"
	case "cvroom":
		return "Convertible drawer"
	case "onedoor":
		return "Fridge"
	}
	return strings.ToUpper(comp[:1]) + comp[1:]
}

func (st *SmartThings) setErr(err error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if err == nil {
		st.err = ""
		return
	}
	if st.err != err.Error() {
		log.Printf("smartthings: %v", err)
	}
	st.err = err.Error()
}

// FridgePart is one compartment as the UI shows it.
type FridgePart struct {
	Name    string   `json:"name"`
	Open    *bool    `json:"open,omitempty"`
	OpenMin int      `json:"openMin,omitempty"`
	TempF   *float64 `json:"tempF,omitempty"`
	SetF    *float64 `json:"setF,omitempty"`
	Mode    string   `json:"mode,omitempty"`
}

func (st *SmartThings) Devices(ctx context.Context) ([]deviceView, error) {
	connected := st.Connected() // before st.mu: a refresh in progress holds authMu
	st.authMu.Lock()
	renewErr, signingIn, auto := st.renewErr, st.signingIn, st.cfg.Auto
	st.authMu.Unlock()
	st.mu.Lock()
	defer st.mu.Unlock()
	if signingIn {
		return []deviceView{{ID: "st:signin", Kind: "fridge", Label: "SmartThings", Run: "signing-in",
			Problem: "Sign in in the Chrome window on the server (Screen Sharing). Leave Chrome open afterwards: hometron notices the sign-in by itself."}}, nil
	}
	if auto && errors.Is(renewErr, errNeedsSignIn) && len(st.fridges) == 0 {
		return []deviceView{{ID: "st:signin", Kind: "fridge", Label: "SmartThings", Run: "signin", Problem: "SmartThings sign-in expired"}}, nil
	}
	if !connected {
		return []deviceView{{ID: "st:connect", Kind: "fridge", Label: "SmartThings", Problem: "not connected", Run: "connect"}}, nil
	}
	if st.err != "" && len(st.fridges) == 0 {
		return []deviceView{{ID: "st:error", Kind: "fridge", Label: "SmartThings", Problem: st.err}}, nil
	}
	out := []deviceView{}
	for _, f := range st.fridges {
		v := deviceView{ID: "st:" + f.ID, Kind: "fridge", Label: f.Name, Online: time.Since(f.Seen) < 5*time.Minute, FilterPc: f.FilterPc, Ice: f.Ice,
			KWhMonth: f.KWhMonth, KWhLast: f.KWhLast, Notes: f.Notes}
		if st.err != "" {
			v.Problem = st.err
		}
		if auto && errors.Is(renewErr, errNeedsSignIn) {
			v.Run = "needs-signin" // still working on the current token; sign in before it expires
		}
		if renewErr != nil && !errors.Is(renewErr, errNeedsSignIn) {
			v.Problem = strings.TrimSpace(v.Problem + " · token renewal: " + renewErr.Error())
		}
		names := make([]string, 0, len(f.Parts))
		for c := range f.Parts {
			names = append(names, c)
		}
		sort.Slice(names, func(i, j int) bool { return partOrder(names[i]) < partOrder(names[j]) })
		seen := map[string]bool{}
		for _, c := range names {
			p := f.Parts[c]
			n := partName(c)
			if c == "main" && (f.Parts["cooler"] != nil || f.Parts["freezer"] != nil) {
				continue // "main" mirrors the compartments when they're listed separately
			}
			if seen[n] {
				continue
			}
			seen[n] = true
			fp := FridgePart{Name: n, Open: p.Open, TempF: p.TempF, SetF: p.SetF, Mode: p.Mode}
			if p.Open != nil && *p.Open {
				fp.OpenMin = int(time.Since(p.OpenSince).Minutes())
			}
			v.Parts = append(v.Parts, fp)
		}
		out = append(out, v)
	}
	return out, nil
}

func partOrder(c string) int {
	switch c {
	case "cooler", "main", "onedoor":
		return 0
	case "freezer":
		return 1
	}
	return 2
}

func (st *SmartThings) Owns(_ context.Context, id string) bool { return strings.HasPrefix(id, "st:") }

func (st *SmartThings) Command(context.Context, string, string, map[string]any) (json.RawMessage, error) {
	return nil, errNotAllowed // read-only: nobody needs to change fridge settings from here
}
