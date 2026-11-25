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
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// LG washer and dryer via LG's official ThinQ Connect API (the same one Home Assistant's
// built-in LG integration uses). It's a cloud API: the machines have no local one.
//
// Config: <state>/lg.json
//
//	{"token": "<personal access token from https://connect-pat.lgthinq.com>", "country": "US"}
//
// hometron adds a client id on first run.
type lgConfig struct {
	Token    string `json:"token"`
	Country  string `json:"country"`
	ClientID string `json:"clientId,omitempty"`
}

// lgAPIKey is the public key LG ships in its own open-source SDK (thinqconnect/const.py).
const lgAPIKey = "v6GFvkweNo7DK7yD3ylIZ9w52aKBU0eJ7wLXkSR3"

type Laundry struct {
	cfg    lgConfig
	base   string
	http   *http.Client
	events *Events

	mu       sync.Mutex
	machines map[string]*machine // LG device id -> latest
	err      string

	remind   *LaundryReminderConfig
	say      func(ctx context.Context, text string) error
	wait     *LaundryWait
	waitPath string
}

type machine struct {
	ID, Name, Type string // Type: washer | dryer
	State          string // LG's currentState: RUNNING, RINSING, SPINNING, DRYING, END, POWER_OFF, ...
	RemainMin      int
	Seen           time.Time
	active         bool
}

// lgRegions maps countries to API hosts. LG has three: Americas, Europe/Africa, Asia/Pacific.
func lgRegion(country string) string {
	switch country {
	case "US", "CA", "MX", "BR", "AR", "CL", "CO", "PE", "PA", "PR", "GT", "CR", "DO", "EC", "HN", "JM", "NI", "SV", "TT", "UY", "VE", "BO", "PY":
		return "aic"
	case "KR", "JP", "CN", "TW", "HK", "SG", "MY", "TH", "VN", "PH", "ID", "IN", "AU", "NZ":
		return "kic"
	}
	return "eic"
}

func loadLaundry(stateDir string, events *Events) (*Laundry, error) {
	path := filepath.Join(stateDir, "lg.json")
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var cfg lgConfig
	if err := json.Unmarshal(b, &cfg); err != nil {
		return nil, fmt.Errorf("lg.json: %w", err)
	}
	if cfg.Token == "" {
		return nil, errors.New("lg.json: token missing")
	}
	if cfg.Country == "" {
		cfg.Country = "US"
	}
	if cfg.ClientID == "" {
		// LG wants a stable id per client install; the SDK uses "thinq-open-" + random.
		r := make([]byte, 12)
		rand.Read(r)
		cfg.ClientID = "thinq-open-hometron-" + base64.RawURLEncoding.EncodeToString(r)
		if out, err := json.MarshalIndent(cfg, "", "  "); err == nil {
			os.WriteFile(path, out, 0o600)
		}
	}
	return &Laundry{
		cfg: cfg, base: "https://api-" + lgRegion(cfg.Country) + ".lgthinq.com/",
		http: &http.Client{Timeout: 20 * time.Second}, events: events,
		machines: map[string]*machine{},
	}, nil
}

func (l *Laundry) call(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, "GET", l.base+path, nil)
	if err != nil {
		return err
	}
	id := make([]byte, 16)
	rand.Read(id)
	req.Header.Set("Authorization", "Bearer "+l.cfg.Token)
	req.Header.Set("x-country", l.cfg.Country)
	req.Header.Set("x-message-id", base64.RawURLEncoding.EncodeToString(id))
	req.Header.Set("x-client-id", l.cfg.ClientID)
	req.Header.Set("x-api-key", lgAPIKey)
	req.Header.Set("x-service-phase", "OP")
	res, err := l.http.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	var env struct {
		Response json.RawMessage `json:"response"`
		Error    *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(b, &env); err != nil {
		return fmt.Errorf("LG %s: HTTP %d", path, res.StatusCode)
	}
	if res.StatusCode/100 != 2 || env.Error != nil {
		if env.Error != nil {
			return fmt.Errorf("LG %s: %s (%s)", path, env.Error.Message, env.Error.Code)
		}
		return fmt.Errorf("LG %s: HTTP %d", path, res.StatusCode)
	}
	return json.Unmarshal(env.Response, out)
}

// Run polls LG every minute.
func (l *Laundry) Run(ctx context.Context) {
	for {
		l.poll(ctx)
		l.checkReminder(ctx, time.Now())
		// Every minute, running or not: a machine someone just started should show up quickly.
		wait := time.Minute
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

func (l *Laundry) poll(ctx context.Context) (busy bool) {
	var list []struct {
		DeviceID   string `json:"deviceId"`
		DeviceInfo struct {
			DeviceType string `json:"deviceType"`
			Alias      string `json:"alias"`
		} `json:"deviceInfo"`
	}
	if err := l.call(ctx, "devices", &list); err != nil {
		l.setErr(err)
		return false
	}
	l.setErr(nil)
	for _, d := range list {
		typ := ""
		switch d.DeviceInfo.DeviceType {
		case "DEVICE_WASHER", "DEVICE_WASHTOWER_WASHER", "DEVICE_WASHCOMBO_MAIN":
			typ = "washer"
		case "DEVICE_DRYER", "DEVICE_WASHTOWER_DRYER":
			typ = "dryer"
		default:
			continue
		}
		var raw json.RawMessage
		if err := l.call(ctx, "devices/"+d.DeviceID+"/state", &raw); err != nil {
			log.Printf("laundry %s: %v", d.DeviceInfo.Alias, err)
			continue
		}
		state, remain := parseLaundryState(raw)
		name := d.DeviceInfo.Alias
		if name == "" {
			name = strings.ToUpper(typ[:1]) + typ[1:]
		}
		if l.update(d.DeviceID, name, typ, state, remain, time.Now()) {
			busy = true
		}
	}
	return busy
}

// parseLaundryState reads runState and timer. Washers report a list (one entry per drum,
// "MAIN"/"MINI"); dryers a single object. We take the main drum.
func parseLaundryState(raw json.RawMessage) (state string, remainMin int) {
	type st struct {
		Location *struct {
			LocationName string `json:"locationName"`
		} `json:"location"`
		RunState struct {
			CurrentState string `json:"currentState"`
		} `json:"runState"`
		Timer struct {
			RemainHour   int `json:"remainHour"`
			RemainMinute int `json:"remainMinute"`
		} `json:"timer"`
	}
	var one st
	var many []st
	if json.Unmarshal(raw, &many) == nil && len(many) > 0 {
		one = many[0]
		for _, s := range many {
			if s.Location != nil && s.Location.LocationName == "MAIN" {
				one = s
			}
		}
	} else if json.Unmarshal(raw, &one) != nil {
		return "", 0
	}
	return one.RunState.CurrentState, one.Timer.RemainHour*60 + one.Timer.RemainMinute
}

// laundryIdle: states where the machine isn't working on a load.
var laundryIdle = map[string]bool{"": true, "POWER_OFF": true, "INITIAL": true, "END": true, "SLEEP": true, "ERROR": true, "RESERVED": true, "PAUSE": true, "WRINKLE_CARE": true}

// update records a reading and posts a "done" event when a running machine finishes.
// It returns whether the machine is running now.
func (l *Laundry) update(id, name, typ, state string, remain int, now time.Time) bool {
	l.mu.Lock()
	m := l.machines[id]
	if m == nil {
		m = &machine{ID: id}
		l.machines[id] = m
	}
	wasActive := m.active
	m.Name, m.Type, m.State, m.RemainMin, m.Seen = name, typ, state, remain, now
	// PAUSE keeps a load "in progress" so resuming doesn't look like a new one finishing.
	m.active = !laundryIdle[state] || (wasActive && state == "PAUSE")
	finished := wasActive && !m.active && (state == "END" || state == "POWER_OFF" || state == "WRINKLE_CARE")
	l.onChange(m, !wasActive && m.active, finished, now)
	l.mu.Unlock()

	if finished && l.events != nil {
		l.events.Post(Event{Time: now, DeviceID: "lg:" + id, Device: name, Kind: "done"})
	}
	return m.active
}

func (l *Laundry) setErr(err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err == nil {
		l.err = ""
		return
	}
	if l.err != err.Error() {
		log.Printf("laundry: %v", err)
	}
	l.err = err.Error()
}

// Provider: the machines show as kind "laundry"; there are no commands (LG only allows
// remote start when someone pressed Remote Start on the machine, which isn't worth it).
func (l *Laundry) Devices(ctx context.Context) ([]deviceView, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.err != "" && len(l.machines) == 0 {
		return nil, errors.New(l.err)
	}
	out := []deviceView{}
	for _, m := range l.machines {
		v := deviceView{ID: "lg:" + m.ID, Kind: "laundry", Label: m.Name, Room: m.Type, Online: time.Since(m.Seen) < 10*time.Minute,
			Run: laundryWords(m.State, m.Type)}
		if m.active && m.RemainMin > 0 {
			r := m.RemainMin
			v.RemainMin = &r
		}
		v.Active = m.active
		if l.wait != nil && m.Type == "washer" && !m.active {
			since := l.wait.Since
			v.WaitSince = &since
		}
		if l.err != "" {
			v.Problem = "LG isn't answering: " + l.err
		}
		out = append(out, v)
	}
	return out, nil
}

func (l *Laundry) Owns(_ context.Context, id string) bool { return strings.HasPrefix(id, "lg:") }

func (l *Laundry) Command(context.Context, string, string, map[string]any) (json.RawMessage, error) {
	return nil, errNotAllowed
}

func laundryWords(s, typ string) string {
	switch s {
	case "", "POWER_OFF", "SLEEP":
		return "Off"
	case "INITIAL", "RESERVED":
		return "Ready"
	case "END":
		return "Done"
	case "PAUSE":
		return "Paused"
	case "ERROR":
		return "Error"
	case "DETECTING":
		return "Sensing load"
	case "RUNNING":
		if typ == "dryer" {
			return "Drying"
		}
		return "Washing"
	case "RINSING":
		return "Rinsing"
	case "SPINNING":
		return "Spinning"
	case "DRYING":
		return "Drying"
	case "COOLING", "COOL_DOWN":
		return "Cooling down"
	case "STEAM_SOFTENING", "REFRESHING", "STEAMING":
		return "Steaming"
	case "WRINKLE_CARE":
		return "Done · wrinkle care"
	}
	return strings.ToLower(strings.ReplaceAll(s, "_", " "))
}

// ---------- "the wash is still in the washer" ----------
//
// LG reports no door sensor, and LG washers switch themselves off a few minutes after a
// cycle, so "off" doesn't mean "emptied". The load counts as moved when the dryer starts,
// the washer starts again, or someone taps "Moved it" on the dashboard. Otherwise, after
// an hour, the speakers say so, except during quiet hours, when it waits for morning.

type LaundryReminderConfig struct {
	Speakers []string `json:"speakers,omitempty"` // Google Home names; default: the doorbell speakers
	AfterMin int      `json:"afterMin,omitempty"` // default 60
	Quiet    string   `json:"quiet,omitempty"`    // "21:00-08:00" (the default): no announcements then
}

// LaundryWait is a washer load nobody has moved yet.
type LaundryWait struct {
	Since    time.Time `json:"since"`
	Washer   string    `json:"washer"`
	Reminded bool      `json:"reminded"`
}

// SetReminder turns the reminder on. say announces the text; nil means feed/push only.
func (l *Laundry) SetReminder(cfg LaundryReminderConfig, stateDir string, say func(ctx context.Context, text string) error) {
	if cfg.AfterMin <= 0 {
		cfg.AfterMin = 60
	}
	if cfg.Quiet == "" {
		cfg.Quiet = "21:00-08:00"
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.remind, l.say, l.waitPath = &cfg, say, filepath.Join(stateDir, "laundry-wait.json")
	var w LaundryWait
	if readJSON(l.waitPath, &w) == nil && !w.Since.IsZero() {
		l.wait = &w
	}
}

// onChange runs under l.mu when a machine starts or a washer finishes.
func (l *Laundry) onChange(m *machine, started, finished bool, now time.Time) {
	if l.remind == nil {
		return
	}
	switch {
	case m.Type == "washer" && finished:
		l.wait = &LaundryWait{Since: now, Washer: m.Name}
	case l.wait != nil && started: // the dryer took the load, or the washer has a new one
		l.wait = nil
	default:
		return
	}
	l.saveWait()
}

func (l *Laundry) saveWait() {
	if l.waitPath == "" {
		return
	}
	if l.wait == nil {
		os.Remove(l.waitPath)
		return
	}
	b, _ := json.Marshal(l.wait)
	os.WriteFile(l.waitPath, b, 0o600)
}

// Emptied: someone moved the load ("Moved it" on the dashboard).
func (l *Laundry) Emptied() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.wait != nil {
		log.Printf("laundry: load marked as moved")
	}
	l.wait = nil
	l.saveWait()
}

// checkReminder announces a load that has sat too long, once, outside quiet hours.
func (l *Laundry) checkReminder(ctx context.Context, now time.Time) {
	l.mu.Lock()
	w, cfg, say := l.wait, l.remind, l.say
	if w == nil || cfg == nil || w.Reminded || now.Sub(w.Since) < time.Duration(cfg.AfterMin)*time.Minute {
		l.mu.Unlock()
		return
	}
	if now.Sub(w.Since) > 18*time.Hour { // long forgotten, or emptied without anyone telling us
		l.wait = nil
		l.saveWait()
		l.mu.Unlock()
		return
	}
	if quietHours(cfg.Quiet, now) {
		l.mu.Unlock()
		return
	}
	w.Reminded = true
	l.saveWait()
	l.mu.Unlock()

	text := laundryReminderText(w, now)
	if l.events != nil {
		l.events.Post(Event{Time: now, DeviceID: "lg:wait", Device: text, Kind: "reminder"})
	}
	if say != nil {
		ctx, cancel := context.WithTimeout(ctx, time.Minute)
		defer cancel()
		if err := say(ctx, text); err != nil {
			log.Printf("laundry: announcing: %v", err)
		}
	}
}

func laundryReminderText(w *LaundryWait, now time.Time) string {
	name := strings.ToLower(w.Washer)
	if name == "" {
		name = "washer"
	}
	if dateOnly(w.Since).Before(dateOnly(now)) {
		return "The " + name + " finished last night and the load is still in it. Time to move the laundry."
	}
	return fmt.Sprintf("The %s finished %s ago. Don't forget to move the laundry.", name, spokenDur(now.Sub(w.Since)))
}

func spokenDur(d time.Duration) string {
	m := int(d.Minutes() + 0.5)
	switch {
	case m < 90:
		return fmt.Sprintf("%d minutes", m)
	case m%60 < 15:
		return fmt.Sprintf("%d hours", m/60)
	}
	return fmt.Sprintf("about %d and a half hours", m/60)
}

// quietHours: "21:00-08:00" style windows, which may wrap past midnight.
func quietHours(spec string, now time.Time) bool {
	from, to, ok := strings.Cut(spec, "-")
	if !ok {
		return false
	}
	parse := func(s string) int {
		t, err := time.Parse("15:04", strings.TrimSpace(s))
		if err != nil {
			return -1
		}
		return t.Hour()*60 + t.Minute()
	}
	a, b, m := parse(from), parse(to), now.Hour()*60+now.Minute()
	if a < 0 || b < 0 {
		return false
	}
	if a <= b {
		return m >= a && m < b
	}
	return m >= a || m < b
}
