package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// NetWatch is the "Network health" view: every minute it pings each step on the way out
// of the house (Google Wifi → the ISP's router → the ISP → the internet) so a problem
// can be pinned to one of them; it also watches the Google Wifi units, devices dropping
// off or joining the LAN, Tailscale, and the server's own Wi-Fi link.
type NetWatch struct {
	path   string // <state>/netwatch.jsonl: one line per minute
	lan    *Network
	events *Events

	ping     func(ctx context.Context, host string) Hop
	resolve  func(ctx context.Context) (int, error) // DNS lookup time, ms
	tsStatus func(ctx context.Context) ([]byte, error)
	wifiInfo func(ctx context.Context) ([]byte, error)
	http     *http.Client

	mu       sync.Mutex
	samples  []NetSample // last 24 h, oldest first
	ispHop   string      // first public router on the way out, from traceroute
	ispAt    time.Time
	routers  map[string]*RouterInfo // ip -> Google Wifi unit
	routerAt time.Time
	sweepAt  time.Time         // last full search for units
	unitName map[string]string // ip -> Home-app name
	tail     []TailNode
	tailAt   time.Time
	tsWarned map[string]int // node -> smallest "expires in N days" already announced
	tsErr    string
	link     *LinkInfo
	devices  map[string]*watched // mac -> dropout tracking
	scans    int

	outdoor    func() *float64   // outside temperature, if weather is set up
	meshPath   string            // <state>/mesh.json: Wi-Fi point drop log
	meshLog    []MeshDrop        // newest first
	meshFrom   time.Time         // when point watching began; availability counts from here
	savedUnits map[string]string // units as last written to mesh.json
	meshDown   map[string]*meshState
}

type meshState struct {
	misses    int
	since     time.Time
	announced bool
	uptime    int64 // before it went missing
}

// Hop is one ping result. Ms is the average round trip; Loss is percent.
type Hop struct {
	Name   string  `json:"name,omitempty"` // Wi-Fi points only
	Host   string  `json:"host,omitempty"`
	Ms     float64 `json:"ms"`
	Jitter float64 `json:"jitter,omitempty"`
	Loss   int     `json:"loss"`
}

func (h Hop) ok() bool { return h.Loss < 100 }

type NetSample struct {
	T        time.Time `json:"t"`
	Wifi     *Hop      `json:"wifi,omitempty"`
	Router   *Hop      `json:"router,omitempty"`
	ISP      *Hop      `json:"isp,omitempty"`
	Internet *Hop      `json:"internet,omitempty"`
	DNSms    int       `json:"dns"`              // -1 when lookups fail
	Points   []Hop     `json:"points,omitempty"` // each mesh Wi-Fi point (pinging one crosses the mesh link)
}

// MeshDrop is one stretch of a Wi-Fi point being unreachable.
type MeshDrop struct {
	Unit      string    `json:"unit"`
	IP        string    `json:"ip"`
	Start     time.Time `json:"start"`
	End       time.Time `json:"end,omitempty"`
	Restarted bool      `json:"restarted,omitempty"` // its uptime reset while it was gone
	OutF      *float64  `json:"outF,omitempty"`      // outside temperature when it dropped
}

type RouterInfo struct {
	Name      string    `json:"name"`
	IP        string    `json:"ip"`
	Root      bool      `json:"root,omitempty"`   // the unit plugged into the ISP's router
	Online    *bool     `json:"online,omitempty"` // root only: its WAN is up
	UptimeSec int64     `json:"uptimeSec"`
	Firmware  string    `json:"firmware,omitempty"`
	Updating  bool      `json:"updating,omitempty"`
	LED       string    `json:"led,omitempty"` // the unit's own light state, e.g. CONNECTED
	Seen      time.Time `json:"seen"`

	// view only
	Ping    *Hop       `json:"ping,omitempty"`
	Avail24 *float64   `json:"avail24,omitempty"` // percent of the last day's minutes it answered
	Avail7  *float64   `json:"avail7,omitempty"`
	Drops7  int        `json:"drops7,omitempty"`
	Drops   []MeshDrop `json:"drops,omitempty"` // newest first

	upstream string // root only: the router it plugs into
}

type TailNode struct {
	Name      string     `json:"name"`
	OS        string     `json:"os"`
	Self      bool       `json:"self,omitempty"`
	Online    bool       `json:"online"`
	LastSeen  *time.Time `json:"lastSeen,omitempty"`
	KeyExpiry *time.Time `json:"keyExpiry,omitempty"` // nil: expiry disabled
}

type LinkInfo struct {
	Wired   bool      `json:"wired"`
	Band    string    `json:"band,omitempty"`    // "2.4 GHz" / "5 GHz"
	Channel string    `json:"channel,omitempty"` // "11 (2GHz, 20MHz)"
	Signal  int       `json:"signal,omitempty"`  // dBm
	Noise   int       `json:"noise,omitempty"`
	RateMb  int       `json:"rateMbps,omitempty"`
	At      time.Time `json:"at"`
}

type watched struct {
	name   string
	down   bool
	downAt time.Time
}

func newNetWatch(stateDir string, lan *Network, events *Events) *NetWatch {
	w := &NetWatch{
		path: filepath.Join(stateDir, "netwatch.jsonl"), lan: lan, events: events,
		ping: pingHost, resolve: dnsTime, tsStatus: tailscaleStatus, wifiInfo: airportInfo,
		http:    &http.Client{Timeout: 4 * time.Second},
		routers: map[string]*RouterInfo{}, devices: map[string]*watched{}, tsWarned: map[string]int{},
		meshPath: filepath.Join(stateDir, "mesh.json"), meshDown: map[string]*meshState{},
	}
	w.samples = w.read(time.Now().Add(-24 * time.Hour))
	var saved meshFile
	if b, err := os.ReadFile(w.meshPath); err == nil {
		json.Unmarshal(b, &saved)
	}
	w.meshLog, w.meshFrom = saved.Drops, saved.Since
	w.savedUnits = saved.Units
	for ip, name := range saved.Units {
		w.routers[ip] = &RouterInfo{IP: ip, Name: name}
	}
	if w.meshFrom.IsZero() {
		w.meshFrom = time.Now()
		w.saveMesh()
	}
	return w
}

func (w *NetWatch) Run(ctx context.Context) {
	for {
		w.tick(ctx, time.Now())
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Minute):
		}
	}
}

// tick runs one round: the hop pings every minute, the slower checks less often.
func (w *NetWatch) tick(ctx context.Context, now time.Time) {
	gw := defaultGateway()
	w.mu.Lock()
	needISP := w.ispHop == "" || now.Sub(w.ispAt) > time.Hour
	needRouters := now.Sub(w.routerAt) > 5*time.Minute || w.sweepAt.IsZero() // retry until the first sweep finds the units
	needSlow := now.Sub(w.tailAt) > 10*time.Minute
	w.mu.Unlock()

	if needRouters {
		w.checkRouters(ctx, gw, now)
	}
	if needISP {
		if hop := firstPublicHop(ctx); hop != "" {
			w.mu.Lock()
			w.ispHop, w.ispAt = hop, now
			w.mu.Unlock()
		}
	}
	if needSlow {
		w.checkTailscale(ctx, now)
		w.checkLink(ctx, now)
		w.mu.Lock()
		w.tailAt = now
		w.mu.Unlock()
	}

	w.mu.Lock()
	upstream, isp := "", w.ispHop
	var points []RouterInfo
	for _, r := range w.routers {
		if r.Root && r.upstream != "" {
			upstream = r.upstream
		}
		if !r.Root {
			points = append(points, *r)
		}
	}
	w.mu.Unlock()
	sort.Slice(points, func(i, j int) bool { return points[i].Name < points[j].Name })

	s := NetSample{T: now.Truncate(time.Second)}
	var wg sync.WaitGroup
	do := func(dst **Hop, host string) {
		if host == "" {
			return
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			h := w.ping(ctx, host)
			*dst = &h
		}()
	}
	do(&s.Wifi, gw)
	do(&s.Router, upstream)
	do(&s.ISP, isp)
	do(&s.Internet, "1.1.1.1")
	s.Points = make([]Hop, len(points))
	for i, p := range points {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h := w.ping(ctx, p.IP)
			h.Name = p.Name
			s.Points[i] = h
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		if ms, err := w.resolve(ctx); err == nil {
			s.DNSms = ms
		} else {
			s.DNSms = -1
		}
	}()
	wg.Wait()
	if ctx.Err() != nil {
		return // shutting down: the probes were cut short, not lost
	}
	if s.Internet != nil && !s.Internet.ok() { // 1.1.1.1 alone isn't the internet
		if h := w.ping(ctx, "8.8.8.8"); h.ok() {
			s.Internet = &h
		}
	}
	w.add(s)
	w.trackPoints(ctx, s, now)
}

// trackPoints turns per-minute pings of each Wi-Fi point into drops: two missed minutes
// in a row is "dropped off the mesh"; when it answers again we check its uptime to tell
// a restart from a lost link, and log both with the outside temperature.
func (w *NetWatch) trackPoints(ctx context.Context, s NetSample, now time.Time) {
	var posts []Event
	for _, p := range s.Points {
		w.mu.Lock()
		st := w.meshDown[p.Host]
		if st == nil {
			st = &meshState{}
			w.meshDown[p.Host] = st
		}
		if r := w.routers[p.Host]; r != nil && st.misses == 0 {
			st.uptime = r.UptimeSec
		}
		w.mu.Unlock()

		if !p.ok() {
			w.mu.Lock()
			if st.misses == 0 {
				st.since = s.T
			}
			st.misses++
			if st.misses == 2 && !st.announced {
				st.announced = true
				posts = append(posts, Event{Time: now, DeviceID: "mesh:" + p.Host, Device: p.Name + " dropped off the mesh", Kind: "network"})
			}
			w.mu.Unlock()
			continue
		}
		if st.misses == 0 {
			continue
		}
		w.mu.Lock()
		was := *st
		*st = meshState{}
		w.mu.Unlock()
		if !was.announced {
			continue // a single missed minute: not worth a log entry
		}
		// Back: did it restart? Ask it for its uptime now.
		restarted := false
		if stt, err := w.routerStatus(ctx, p.Host); err == nil {
			gone := now.Sub(was.since).Seconds()
			restarted = (was.uptime > 0 && stt.System.Uptime < was.uptime) || float64(stt.System.Uptime) < gone+120
			w.mu.Lock()
			if r := w.routers[p.Host]; r != nil {
				r.UptimeSec, r.LED = stt.System.Uptime, stt.System.LedAnimation
			}
			w.mu.Unlock()
		}
		d := MeshDrop{Unit: p.Name, IP: p.Host, Start: was.since, End: s.T, Restarted: restarted}
		if w.outdoor != nil {
			d.OutF = w.outdoor()
		}
		w.mu.Lock()
		w.meshLog = append([]MeshDrop{d}, w.meshLog...)
		if len(w.meshLog) > 300 {
			w.meshLog = w.meshLog[:300]
		}
		w.mu.Unlock()
		w.saveMesh()
		msg := p.Name + " is back on the mesh (gone " + shortDur(d.End.Sub(d.Start)) + ")"
		if restarted {
			msg = p.Name + " restarted and is back (gone " + shortDur(d.End.Sub(d.Start)) + ")"
		}
		posts = append(posts, Event{Time: now, DeviceID: "mesh:" + p.Host, Device: msg, Kind: "network"})
	}
	for _, e := range posts {
		if w.events != nil {
			w.events.Post(e)
		}
	}
}

func (w *NetWatch) add(s NetSample) {
	w.mu.Lock()
	w.samples = append(w.samples, s)
	cut := s.T.Add(-24 * time.Hour)
	i := 0
	for i < len(w.samples) && w.samples[i].T.Before(cut) {
		i++
	}
	w.samples = w.samples[i:]
	w.mu.Unlock()

	f, err := os.OpenFile(w.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	b, _ := json.Marshal(s)
	f.Write(append(b, '\n'))
}

func (w *NetWatch) read(since time.Time) []NetSample {
	f, err := os.Open(w.path)
	if err != nil {
		return nil
	}
	defer f.Close()
	if st, err := f.Stat(); err == nil && st.Size() > 1<<20 {
		f.Seek(st.Size()-1<<20, io.SeekStart) // ~4 days of lines; a day is all we chart
	}
	var out []NetSample
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var s NetSample
		if json.Unmarshal(sc.Bytes(), &s) == nil && !s.T.Before(since) {
			out = append(out, s)
		}
	}
	return out
}

// Diagnose says, in words, what's wrong now (or that nothing is), from the latest sample.
// The first failing step is the culprit: if the Google Wifi answers but the router past
// it doesn't, it's that router, whatever happens further out.
func Diagnose(s NetSample) (ok bool, words string) {
	switch {
	case s.Wifi != nil && !s.Wifi.ok():
		return false, "Can't reach the Google Wifi from the server (could be the server's own Wi-Fi)"
	case s.Router != nil && !s.Router.ok() && (s.Internet == nil || !s.Internet.ok()):
		return false, "Google Wifi is fine, but the router it plugs into isn't answering"
	case s.Internet != nil && !s.Internet.ok() && s.ISP != nil && !s.ISP.ok():
		return false, "Your routers are fine; the ISP isn't answering"
	case s.Internet != nil && !s.Internet.ok():
		return false, "Routers and the ISP answer, but the internet beyond doesn't"
	case downPoint(s) != "":
		return false, downPoint(s) + " isn't reachable: devices near it may be offline"
	case s.DNSms < 0:
		return false, "Internet works but name lookups (DNS) fail, so websites won't load"
	case s.Internet != nil && s.Internet.Loss >= 20:
		return false, fmt.Sprintf("Internet is up but dropping %d%% of packets", s.Internet.Loss)
	case s.Internet != nil && s.Internet.Ms > 250:
		return false, fmt.Sprintf("Internet is up but slow to respond (%.0f ms)", s.Internet.Ms)
	}
	return true, "All good"
}

type meshFile struct {
	Since time.Time         `json:"since"`
	Drops []MeshDrop        `json:"drops"`
	Units map[string]string `json:"units,omitempty"` // ip -> name: the points to watch from the first minute after a restart
}

func (w *NetWatch) saveMesh() {
	w.mu.Lock()
	w.savedUnits = w.unitList()
	b, _ := json.MarshalIndent(meshFile{Since: w.meshFrom, Drops: w.meshLog, Units: w.unitList()}, "", " ")
	w.mu.Unlock()
	if w.meshPath != "" {
		os.WriteFile(w.meshPath, b, 0o600)
	}
}

func downPoint(s NetSample) string {
	for _, p := range s.Points {
		if !p.ok() {
			return p.Name
		}
	}
	return ""
}

// ---------- Google Wifi units ----------

// checkRouters reads each Google/Nest Wifi unit's local status page (no login needed):
// uptime, firmware, and for the main unit whether its internet side is up. A drop in
// uptime means the unit restarted, which goes in the feed.
func (w *NetWatch) checkRouters(ctx context.Context, gw string, now time.Time) {
	w.mu.Lock()
	full := now.Sub(w.sweepAt) > time.Hour
	candidates := map[string]bool{}
	for ip := range w.routers {
		candidates[ip] = true
	}
	w.mu.Unlock()
	if gw != "" {
		candidates[gw] = true
	}
	hosts := w.lan.Last().Hosts
	if full && len(hosts) > 0 { // hourly (once the first LAN scan is in): find units, refresh names
		for _, h := range hosts {
			if strings.HasPrefix(h.Vendor, "Google") || strings.HasPrefix(h.Vendor, "Nest") {
				candidates[h.IP] = true
			}
		}
		names := map[string]string{}
		for n, a := range discoverCast(ctx, hosts) {
			names[a] = n // points answer Cast's name query with their Home-app name
		}
		w.mu.Lock()
		w.unitName, w.sweepAt = names, now
		w.mu.Unlock()
	}
	for ip := range candidates {
		st, err := w.routerStatus(ctx, ip)
		if err != nil {
			continue
		}
		w.mu.Lock()
		names := w.unitName
		w.mu.Unlock()
		w.mu.Lock()
		r := w.routers[ip]
		if r == nil {
			r = &RouterInfo{IP: ip}
			w.routers[ip] = r
		}
		restarted := r.UptimeSec > 0 && st.System.Uptime < r.UptimeSec
		r.Root = st.System.GroupRole == "root" || ip == gw
		r.UptimeSec, r.Firmware, r.Seen = st.System.Uptime, st.Software.SoftwareVersion, now
		r.Updating = st.Software.UpdateStatus != "" && st.Software.UpdateStatus != "idle"
		r.LED = st.System.LedAnimation
		r.upstream = ""
		if r.Root {
			on := st.WAN.Online
			r.Online = &on
			if g := net.ParseIP(st.WAN.GatewayIPAddress); g != nil && g.IsPrivate() {
				r.upstream = g.String()
			}
		}
		switch {
		case names[ip] != "":
			r.Name = titleCase(names[ip])
		case r.Root:
			r.Name = "Main Wifi"
		case r.Name != "" && !strings.HasPrefix(r.Name, "Wifi point "):
			// keep the name from the last sweep (or mesh.json) until the next one
		default:
			r.Name = "Wifi point " + ip
		}
		name := r.Name
		w.mu.Unlock()
		w.mu.Lock()
		missing := w.meshDown[ip] != nil && w.meshDown[ip].misses > 0
		w.mu.Unlock()
		if restarted && !missing && w.events != nil { // a point coming back after a drop is reported by trackPoints
			w.events.Post(Event{Time: now, DeviceID: "net:" + ip, Device: name + " restarted", Kind: "network"})
		}
	}
	w.mu.Lock()
	w.routerAt = now
	changed := fmt.Sprint(w.unitList()) != fmt.Sprint(w.savedUnits)
	w.mu.Unlock()
	if changed {
		w.saveMesh()
	}
}

// unitList is the known Wi-Fi units by ip. Holds w.mu.
func (w *NetWatch) unitList() map[string]string {
	m := map[string]string{}
	for ip, r := range w.routers {
		m[ip] = r.Name
	}
	return m
}

type gwifiStatus struct {
	Software struct {
		SoftwareVersion string `json:"softwareVersion"`
		UpdateStatus    string `json:"updateStatus"`
	} `json:"software"`
	System struct {
		GroupRole    string `json:"groupRole"`
		Uptime       int64  `json:"uptime"`
		LedAnimation string `json:"ledAnimation"`
	} `json:"system"`
	WAN struct {
		Online           bool   `json:"online"`
		GatewayIPAddress string `json:"gatewayIpAddress"`
	} `json:"wan"`
}

func (w *NetWatch) routerStatus(ctx context.Context, ip string) (gwifiStatus, error) {
	var st gwifiStatus
	req, _ := http.NewRequestWithContext(ctx, "GET", "http://"+ip+"/api/v1/status", nil)
	res, err := w.http.Do(req)
	if err != nil {
		return st, err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return st, fmt.Errorf("HTTP %d", res.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 64<<10)).Decode(&st); err != nil {
		return st, err
	}
	if st.System.Uptime == 0 && st.Software.SoftwareVersion == "" {
		return st, fmt.Errorf("not a Google Wifi")
	}
	return st, nil
}

func titleCase(s string) string {
	parts := strings.Fields(s)
	for i, p := range parts {
		parts[i] = strings.ToUpper(p[:1]) + p[1:]
	}
	return strings.Join(parts, " ")
}

// ---------- devices dropping off / joining ----------

// watchVendors are the always-on things worth an alert when they disappear. Phones,
// consoles and TVs come and go all day, so they're left out.
var watchVendors = []string{"Nest Labs", "Google", "Espressif", "The Chamberlain", "HP", "Hewlett"}

// OnScan diffs each LAN scan: a watched device missing for 20+ minutes is "offline",
// its return is "back", and a device never seen before (with a real MAC) is "new".
func (w *NetWatch) OnScan(s NetScan) {
	if s.Error != "" {
		return
	}
	w.mu.Lock()
	w.scans++
	first := w.scans == 1
	names := map[string]string{}
	for _, r := range w.routers {
		names[r.IP] = r.Name
	}
	var posts []Event
	for _, h := range s.Hosts {
		d := w.devices[h.MAC]
		if d != nil && d.down {
			posts = append(posts, Event{Time: s.At, DeviceID: "net:" + h.MAC, Device: d.name + " is back on the network (gone " + shortDur(s.At.Sub(d.downAt)) + ")", Kind: "network"})
			d.down = false
		}
		// Brand new to this network. On the very first scan everything looks new if the
		// history file was empty, so skip that one.
		if !first && !h.Private && !h.Self && h.FirstSeen.Equal(h.LastSeen) {
			v := h.Vendor
			if v == "" {
				v = "Unknown maker"
			}
			posts = append(posts, Event{Time: s.At, DeviceID: "net:" + h.MAC, Device: "New device joined: " + v + " (" + h.IP + ")", Kind: "network"})
		}
	}
	for _, g := range s.Gone {
		if _, unit := w.routers[g.IP]; unit {
			continue // Wi-Fi units are watched every minute by trackPoints
		}
		if !watchable(g) || s.At.Sub(g.LastSeen) < 20*time.Minute || s.At.Sub(g.LastSeen) > 24*time.Hour {
			continue
		}
		d := w.devices[g.MAC]
		if d == nil {
			d = &watched{}
			w.devices[g.MAC] = d
		}
		if d.down {
			continue
		}
		d.name = deviceName(g, names)
		d.down, d.downAt = true, g.LastSeen
		if !first { // don't announce what dropped off while hometron was stopped
			posts = append(posts, Event{Time: s.At, DeviceID: "net:" + g.MAC, Device: d.name + " dropped off the network", Kind: "network"})
		}
	}
	w.mu.Unlock()
	for _, e := range posts {
		if w.events != nil {
			w.events.Post(e)
		}
	}
}

func watchable(h NetHost) bool {
	if h.Private || h.FirstSeen.IsZero() || h.LastSeen.Sub(h.FirstSeen) < 24*time.Hour {
		return false // new arrivals have to stick around a day before they count
	}
	for _, v := range watchVendors {
		if strings.HasPrefix(h.Vendor, v) {
			return true
		}
	}
	return h.Tailwind
}

func deviceName(h NetHost, routers map[string]string) string {
	switch {
	case routers[h.IP] != "":
		return routers[h.IP]
	case h.Tailwind || strings.HasPrefix(h.Vendor, "Espressif"):
		return "Tailwind garage opener"
	case strings.HasPrefix(h.Vendor, "HP"), strings.HasPrefix(h.Vendor, "Hewlett"):
		return "Printer"
	case strings.HasPrefix(h.Vendor, "The Chamberlain"):
		return "MyQ garage hub"
	case h.Name != "":
		return h.Name
	case strings.HasPrefix(h.Vendor, "Nest Labs"):
		return "Nest device (" + h.IP + ")"
	}
	return h.Vendor + " (" + h.IP + ")"
}

// ---------- Tailscale ----------

func (w *NetWatch) checkTailscale(ctx context.Context, now time.Time) {
	b, err := w.tsStatus(ctx)
	if err != nil {
		w.mu.Lock()
		first := w.tail == nil && w.tsErr == ""
		w.tsErr = err.Error()
		w.mu.Unlock()
		if first {
			log.Printf("tailscale: %v", err)
		}
		return
	}
	nodes, err := parseTailscale(b)
	if err != nil {
		log.Printf("tailscale: %v", err)
		return
	}
	var posts []Event
	w.mu.Lock()
	w.tail = nodes
	for _, n := range nodes {
		if n.KeyExpiry == nil {
			continue
		}
		days := int(n.KeyExpiry.Sub(now).Hours() / 24)
		for _, step := range []int{14, 7, 3, 1} {
			if days <= step && (w.tsWarned[n.Name] == 0 || step < w.tsWarned[n.Name]) {
				w.tsWarned[n.Name] = step
				posts = append(posts, Event{Time: now, DeviceID: "ts:" + n.Name,
					Device: fmt.Sprintf("Tailscale key for %s expires in %d days: disable key expiry in the admin console", n.Name, max(days, 0)), Kind: "network"})
				break
			}
		}
	}
	w.mu.Unlock()
	for _, e := range posts {
		if w.events != nil {
			w.events.Post(e)
		}
	}
}

func parseTailscale(b []byte) ([]TailNode, error) {
	type node struct {
		HostName  string     `json:"HostName"`
		DNSName   string     `json:"DNSName"`
		OS        string     `json:"OS"`
		Online    bool       `json:"Online"`
		LastSeen  time.Time  `json:"LastSeen"`
		KeyExpiry *time.Time `json:"KeyExpiry"`
	}
	var st struct {
		Self node            `json:"Self"`
		Peer map[string]node `json:"Peer"`
	}
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, err
	}
	conv := func(n node, self bool) TailNode {
		t := TailNode{Name: n.HostName, OS: n.OS, Self: self, Online: n.Online || self}
		if n.LastSeen.Year() > 2000 {
			ls := n.LastSeen
			t.LastSeen = &ls
		}
		if n.KeyExpiry != nil && n.KeyExpiry.Year() > 2000 {
			t.KeyExpiry = n.KeyExpiry
		}
		return t
	}
	out := []TailNode{conv(st.Self, true)}
	for _, p := range st.Peer {
		out = append(out, conv(p, false))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Self != out[j].Self {
			return out[i].Self
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out, nil
}

func tailscaleStatus(ctx context.Context) ([]byte, error) {
	for _, bin := range []string{"/Applications/Tailscale.app/Contents/MacOS/Tailscale", "/usr/local/bin/tailscale", "/opt/homebrew/bin/tailscale"} {
		if _, err := os.Stat(bin); err == nil {
			ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, bin, "status", "--json")
			// The Mac app's binary only acts as a CLI when it sees a terminal type; under
			// launchd (no TERM) it tries to open the GUI instead.
			cmd.Env = append(os.Environ(), "TERM=dumb")
			out, err := cmd.Output()
			if err == nil && !strings.HasPrefix(strings.TrimSpace(string(out)), "{") {
				err = fmt.Errorf("unexpected output: %.60s", out)
			}
			return out, err
		}
	}
	return nil, fmt.Errorf("tailscale CLI not found")
}

// ---------- the server's own link ----------

func (w *NetWatch) checkLink(ctx context.Context, now time.Time) {
	ifName, _, _, err := lanInterface()
	if err != nil {
		return
	}
	l := &LinkInfo{At: now, Wired: true}
	if b, err := w.wifiInfo(ctx); err == nil {
		if wl, ok := parseAirport(b, ifName); ok {
			wl.At = now
			l = wl
		}
	}
	w.mu.Lock()
	w.link = l
	w.mu.Unlock()
}

func airportInfo(ctx context.Context) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, "/usr/sbin/system_profiler", "SPAirPortDataType", "-json").Output()
}

var dbmRE = regexp.MustCompile(`(-?\d+) dBm / (-?\d+) dBm`)

// parseAirport pulls the current network's band, signal and rate for interface ifName
// out of system_profiler's JSON. ok is false if that interface isn't Wi-Fi.
func parseAirport(b []byte, ifName string) (*LinkInfo, bool) {
	var doc struct {
		Data []struct {
			Interfaces []struct {
				Name    string `json:"_name"`
				Current *struct {
					Channel    any    `json:"spairport_network_channel"`
					SignalNois string `json:"spairport_signal_noise"`
					Rate       any    `json:"spairport_network_rate"`
				} `json:"spairport_current_network_information"`
			} `json:"spairport_airport_interfaces"`
		} `json:"SPAirPortDataType"`
	}
	if json.Unmarshal(b, &doc) != nil {
		return nil, false
	}
	for _, d := range doc.Data {
		for _, ifc := range d.Interfaces {
			if ifc.Name != ifName || ifc.Current == nil {
				continue
			}
			l := &LinkInfo{Channel: fmt.Sprint(ifc.Current.Channel)}
			switch {
			case strings.Contains(l.Channel, "2GHz"):
				l.Band = "2.4 GHz"
			case strings.Contains(l.Channel, "5GHz"):
				l.Band = "5 GHz"
			case strings.Contains(l.Channel, "6GHz"):
				l.Band = "6 GHz"
			}
			if m := dbmRE.FindStringSubmatch(ifc.Current.SignalNois); m != nil {
				l.Signal, _ = strconv.Atoi(m[1])
				l.Noise, _ = strconv.Atoi(m[2])
			}
			switch r := ifc.Current.Rate.(type) {
			case float64:
				l.RateMb = int(r)
			case string:
				l.RateMb, _ = strconv.Atoi(strings.Fields(r + " 0")[0])
			}
			return l, true
		}
	}
	return nil, false
}

// ---------- probes ----------

var (
	lossRE = regexp.MustCompile(`([\d.]+)% packet loss`)
	rttRE  = regexp.MustCompile(`= ([\d.]+)/([\d.]+)/([\d.]+)/([\d.]+) ms`)
)

// pingHost sends 5 pings 0.2 s apart with the system ping (Go's standard library can't
// send ICMP without root).
func pingHost(ctx context.Context, host string) Hop {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	out, _ := exec.CommandContext(ctx, "/sbin/ping", "-c", "5", "-i", "0.2", "-t", "4", "-q", host).CombinedOutput()
	h := parsePing(string(out))
	h.Host = host
	return h
}

func parsePing(out string) Hop {
	h := Hop{Loss: 100}
	if m := lossRE.FindStringSubmatch(out); m != nil {
		f, _ := strconv.ParseFloat(m[1], 64)
		h.Loss = int(f + 0.5)
	}
	if m := rttRE.FindStringSubmatch(out); m != nil {
		h.Ms, _ = strconv.ParseFloat(m[2], 64)
		h.Jitter, _ = strconv.ParseFloat(m[4], 64)
		h.Ms, h.Jitter = round(h.Ms), round(h.Jitter)
	}
	return h
}

func round(f float64) float64 { return float64(int(f*10+0.5)) / 10 }

// dnsTime looks up a name through the house's own DNS (Google Wifi → router → ISP),
// which is what every phone and laptop uses.
func dnsTime(ctx context.Context) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	start := time.Now()
	// A fresh name each time so no cache along the way can answer for a dead upstream.
	name := fmt.Sprintf("hometron-%d.example.com", time.Now().UnixNano()%100000)
	_, err := net.DefaultResolver.LookupHost(ctx, name)
	if dnsErr, ok := err.(*net.DNSError); ok && dnsErr.IsNotFound {
		err = nil // NXDOMAIN came back from a working resolver: that's a success
	}
	return int(time.Since(start).Milliseconds()), err
}

// firstPublicHop finds the ISP's first router with a short traceroute.
func firstPublicHop(ctx context.Context) string {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/usr/sbin/traceroute", "-n", "-m", "5", "-w", "1", "-q", "1", "1.1.1.1").Output()
	if err != nil && len(out) == 0 {
		return ""
	}
	return parseTraceroute(string(out))
}

func parseTraceroute(out string) string {
	for _, l := range strings.Split(out, "\n") {
		f := strings.Fields(l)
		if len(f) < 2 {
			continue
		}
		ip := net.ParseIP(f[1])
		if ip == nil || ip.IsPrivate() || ip.IsLoopback() || ip.Equal(net.ParseIP("1.1.1.1")) {
			continue
		}
		if ip4 := ip.To4(); ip4 != nil && ip4[0] == 100 && ip4[1]&0xC0 == 64 {
			continue // carrier-grade NAT space: still inside the ISP's plumbing, but not "public"
		}
		return ip.String()
	}
	return ""
}

// ---------- view ----------

type NetHealthView struct {
	OK        bool         `json:"ok"`
	Words     string       `json:"words"`
	Now       *NetSample   `json:"now,omitempty"`
	Samples   []NetSample  `json:"samples,omitempty"`
	Routers   []RouterInfo `json:"routers,omitempty"`
	Tailscale []TailNode   `json:"tailscale,omitempty"`
	Link      *LinkInfo    `json:"link,omitempty"`
}

func (w *NetWatch) View(withSamples bool) NetHealthView {
	w.mu.Lock()
	defer w.mu.Unlock()
	v := NetHealthView{OK: true, Words: "Starting up…"}
	if n := len(w.samples); n > 0 {
		last := w.samples[n-1]
		v.Now = &last
		v.OK, v.Words = Diagnose(last)
	}
	if withSamples {
		v.Samples = append([]NetSample(nil), w.samples...)
	}
	now := time.Now()
	for _, r := range w.routers {
		u := *r
		if !u.Root {
			total, okN := 0, 0
			for _, s := range w.samples {
				for _, p := range s.Points {
					if p.Host == u.IP {
						total++
						if p.ok() {
							okN++
						}
						last := p
						u.Ping = &last
					}
				}
			}
			if total > 0 {
				a := 100 * float64(okN) / float64(total)
				u.Avail24 = &a
			}
			var down time.Duration
			for _, d := range w.meshLog {
				if d.IP != u.IP || d.Start.Before(now.AddDate(0, 0, -7)) {
					continue
				}
				u.Drops7++
				down += d.End.Sub(d.Start)
				if len(u.Drops) < 8 {
					u.Drops = append(u.Drops, d)
				}
			}
			if st := w.meshDown[u.IP]; st != nil && st.announced {
				down += now.Sub(st.since)
			}
			from := now.AddDate(0, 0, -7)
			if w.meshFrom.After(from) {
				from = w.meshFrom
			}
			if span := now.Sub(from); span > time.Hour {
				a7 := 100 * (1 - down.Seconds()/span.Seconds())
				u.Avail7 = &a7
			}
		}
		v.Routers = append(v.Routers, u)
	}
	sort.Slice(v.Routers, func(i, j int) bool { return v.Routers[i].Root && !v.Routers[j].Root })
	v.Tailscale = append([]TailNode(nil), w.tail...)
	if w.link != nil {
		l := *w.link
		v.Link = &l
	}
	return v
}
