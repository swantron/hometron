package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// ---------- printer (IPP) ----------

// Printer reads ink/toner levels and state with IPP Get-Printer-Attributes, which every
// network printer from the last 15 years answers on port 631. No driver, no HP app.
type Printer struct {
	host string // fixed host from <state>/printer.json {"host": "..."}, or "" to find one
	lan  *Network
	http *http.Client

	mu   sync.Mutex
	last PrinterView
}

type PrinterView struct {
	Name    string    `json:"name,omitempty"`
	State   string    `json:"state"` // idle | printing | stopped | offline
	Reasons []string  `json:"reasons,omitempty"`
	Message string    `json:"message,omitempty"`
	Markers []Marker  `json:"markers,omitempty"`
	Host    string    `json:"-"`
	At      time.Time `json:"at"`
}

type Marker struct {
	Name  string `json:"name"`
	Level int    `json:"level"` // percent; -1 unknown
	Color string `json:"color,omitempty"`
}

func newPrinter(stateDir string, lan *Network) *Printer {
	p := &Printer{lan: lan, http: &http.Client{Timeout: 8 * time.Second}}
	if b, err := os.ReadFile(filepath.Join(stateDir, "printer.json")); err == nil {
		var c struct{ Host string }
		if json.Unmarshal(b, &c) == nil {
			p.host = c.Host
		}
	}
	return p
}

var printerVendors = []string{"HP", "Hewlett", "Brother", "Canon", "Epson", "Seiko Epson", "Xerox", "Lexmark", "Kyocera", "Ricoh"}

func (p *Printer) Run(ctx context.Context) {
	for {
		wait := 5 * time.Minute
		if !p.poll(ctx) && p.View() == nil {
			wait = 30 * time.Second // at startup the network scan may not have finished yet
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// poll asks the printer for its state; it reports whether one answered.
func (p *Printer) poll(ctx context.Context) bool {
	hosts := []string{p.host}
	if p.host == "" {
		hosts = nil
		p.mu.Lock()
		if p.last.Host != "" {
			hosts = append(hosts, p.last.Host)
		}
		p.mu.Unlock()
		for _, h := range p.lan.Last().Hosts {
			for _, v := range printerVendors {
				if strings.HasPrefix(h.Vendor, v) {
					hosts = append(hosts, h.IP)
					break
				}
			}
		}
	}
	for _, h := range hosts {
		v, err := p.query(ctx, h)
		if err == nil {
			v.Host, v.At = h, time.Now()
			p.mu.Lock()
			p.last = v
			p.mu.Unlock()
			return true
		}
	}
	p.mu.Lock()
	if !p.last.At.IsZero() {
		p.last.State = "offline" // keep the last known ink levels
	}
	p.mu.Unlock()
	return false
}

func (p *Printer) View() *PrinterView {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.last.At.IsZero() {
		return nil
	}
	v := p.last
	return &v
}

func (p *Printer) query(ctx context.Context, host string) (PrinterView, error) {
	uri := "ipp://" + host + ":631/ipp/print"
	req, _ := http.NewRequestWithContext(ctx, "POST", "http://"+net.JoinHostPort(host, "631")+"/ipp/print", bytes.NewReader(ippGetAttrs(uri)))
	req.Header.Set("Content-Type", "application/ipp")
	res, err := p.http.Do(req)
	if err != nil {
		return PrinterView{}, err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return PrinterView{}, fmt.Errorf("IPP HTTP %d", res.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return PrinterView{}, err
	}
	attrs, err := parseIPP(b)
	if err != nil {
		return PrinterView{}, err
	}
	return printerFrom(attrs), nil
}

func ippGetAttrs(uri string) []byte {
	var b bytes.Buffer
	b.Write([]byte{2, 0})       // IPP 2.0
	b.Write([]byte{0x00, 0x0B}) // Get-Printer-Attributes
	b.Write([]byte{0, 0, 0, 1}) // request id
	b.WriteByte(0x01)           // operation-attributes
	attr := func(tag byte, name, value string) {
		b.WriteByte(tag)
		binary.Write(&b, binary.BigEndian, uint16(len(name)))
		b.WriteString(name)
		binary.Write(&b, binary.BigEndian, uint16(len(value)))
		b.WriteString(value)
	}
	attr(0x47, "attributes-charset", "utf-8")
	attr(0x48, "attributes-natural-language", "en")
	attr(0x45, "printer-uri", uri)
	for i, k := range []string{"printer-state", "printer-state-reasons", "printer-state-message", "printer-make-and-model",
		"marker-names", "marker-levels", "marker-colors", "marker-types"} {
		name := "requested-attributes"
		if i > 0 {
			name = "" // additional value of the same attribute
		}
		attr(0x44, name, k)
	}
	b.WriteByte(0x03) // end
	return b.Bytes()
}

// ippValue is a decoded attribute value: an int for integer/enum, else a string.
type ippValue any

// parseIPP decodes a response into name -> values (all groups merged).
func parseIPP(b []byte) (map[string][]ippValue, error) {
	if len(b) < 8 {
		return nil, errors.New("IPP: short response")
	}
	if status := binary.BigEndian.Uint16(b[2:4]); status > 0x00FF {
		return nil, fmt.Errorf("IPP status 0x%04x", status)
	}
	out := map[string][]ippValue{}
	last := ""
	for p := 8; p < len(b); {
		tag := b[p]
		p++
		if tag == 0x03 {
			break
		}
		if tag < 0x10 { // group delimiter
			continue
		}
		if p+2 > len(b) {
			return nil, errors.New("IPP: truncated")
		}
		nl := int(binary.BigEndian.Uint16(b[p:]))
		p += 2
		if p+nl+2 > len(b) {
			return nil, errors.New("IPP: truncated")
		}
		name := string(b[p : p+nl])
		p += nl
		vl := int(binary.BigEndian.Uint16(b[p:]))
		p += 2
		if p+vl > len(b) {
			return nil, errors.New("IPP: truncated")
		}
		val := b[p : p+vl]
		p += vl
		if name == "" {
			name = last
		}
		last = name
		var v ippValue
		switch {
		case (tag == 0x21 || tag == 0x23) && vl == 4: // integer, enum
			v = int(int32(binary.BigEndian.Uint32(val)))
		case tag >= 0x40 && tag <= 0x4F, tag >= 0x35 && tag <= 0x36: // text-ish
			v = string(val)
		default:
			continue
		}
		out[name] = append(out[name], v)
	}
	return out, nil
}

func printerFrom(a map[string][]ippValue) PrinterView {
	str := func(k string) string {
		if len(a[k]) > 0 {
			s, _ := a[k][0].(string)
			return s
		}
		return ""
	}
	v := PrinterView{Name: str("printer-make-and-model"), Message: str("printer-state-message")}
	state := 0
	if len(a["printer-state"]) > 0 {
		state, _ = a["printer-state"][0].(int)
	}
	v.State = map[int]string{3: "idle", 4: "printing", 5: "stopped"}[state]
	for _, r := range a["printer-state-reasons"] {
		if s, _ := r.(string); s != "" && s != "none" {
			v.Reasons = append(v.Reasons, s)
		}
	}
	for i, n := range a["marker-names"] {
		m := Marker{Level: -1}
		m.Name, _ = n.(string)
		if i < len(a["marker-levels"]) {
			if l, ok := a["marker-levels"][i].(int); ok && l >= 0 {
				m.Level = l
			}
		}
		if i < len(a["marker-colors"]) {
			m.Color, _ = a["marker-colors"][i].(string)
			// Multi-color cartridges list each color ("#00FFFF#FF00FF#FFFF00"); show the first.
			if k := strings.IndexByte(m.Color[min(1, len(m.Color)):], '#'); strings.HasPrefix(m.Color, "#") && k >= 0 {
				m.Color = m.Color[:k+1]
			}
		}
		v.Markers = append(v.Markers, m)
	}
	return v
}

// ---------- internet ----------

// Internet checks the connection every minute and measures download speed every 6 hours.
// Google Wifi won't share its own stats, so the M2 measures from inside the house.
type Internet struct {
	path   string // <state>/internet.json: outage history and last speed
	events *Events
	dial   func(ctx context.Context, addr string) error
	speed  func(ctx context.Context) (float64, error)

	mu     sync.Mutex
	state  InternetState
	downAt time.Time // first failed check of a possible outage
}

type InternetState struct {
	Up        bool      `json:"up"`
	Since     time.Time `json:"since"`
	LatencyMs int       `json:"latencyMs,omitempty"`
	Speeds    []Speed   `json:"speeds,omitempty"`  // browser speed tests, newest first
	Outages   []Outage  `json:"outages,omitempty"` // newest first
	Checked   time.Time `json:"checked"`
	Watching  time.Time `json:"watching"` // monitoring began; uptime only counts from here

	Report *UptimeReport `json:"report,omitempty"` // computed for the view
}

type Speed struct {
	At     time.Time `json:"at"`
	Down   float64   `json:"down"`         // Mbps
	Up     float64   `json:"up,omitempty"` // Mbps
	PingMs float64   `json:"pingMs,omitempty"`
	By     string    `json:"by,omitempty"` // which device ran it ("Mac Chrome")
	Where  string    `json:"where,omitempty"`
}

type UptimeReport struct {
	Up7       float64   `json:"up7"`
	Up30      float64   `json:"up30"`
	Outages30 int       `json:"outages30"`
	DownMin30 int       `json:"downMin30"`
	Since     time.Time `json:"since"`
}

type Outage struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
}

func newInternet(stateDir string, events *Events) *Internet {
	in := &Internet{path: filepath.Join(stateDir, "internet.json"), events: events}
	in.dial = func(ctx context.Context, addr string) error {
		c, err := (&net.Dialer{Timeout: 4 * time.Second}).DialContext(ctx, "tcp", addr)
		if err == nil {
			c.Close()
		}
		return err
	}
	if b, err := os.ReadFile(in.path); err == nil {
		json.Unmarshal(b, &in.state)
	}
	if in.state.Watching.IsZero() {
		in.state.Watching = time.Now()
	}
	return in
}

func (in *Internet) Run(ctx context.Context) {
	in.check(ctx, time.Now())
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Minute):
		}
		in.check(ctx, time.Now())
	}
}

// check: up if either of two big anycast services answers on 443. Two failures in a row
// (two minutes) count as an outage, so one dropped packet doesn't.
func (in *Internet) check(ctx context.Context, now time.Time) {
	start := time.Now()
	up := false
	for _, addr := range []string{"1.1.1.1:443", "8.8.8.8:443"} {
		if in.dial(ctx, addr) == nil {
			up = true
			break
		}
	}
	lat := int(time.Since(start).Milliseconds())

	in.mu.Lock()
	was := in.state.Up
	firstRun := in.state.Checked.IsZero()
	in.state.Checked = now
	if up {
		in.state.LatencyMs = lat
	}
	var outage *Outage
	switch {
	case firstRun:
		in.state.Up, in.state.Since = up, now
	case up && !was:
		o := Outage{Start: in.state.Since, End: now}
		in.state.Outages = append([]Outage{o}, in.state.Outages...)
		if len(in.state.Outages) > 500 {
			in.state.Outages = in.state.Outages[:500]
		}
		in.state.Up, in.state.Since = true, now
		outage = &o
	case !up && was:
		// Record the down transition after a second failed check; until then remember
		// when the first failure was.
		if pending := in.downAt; pending.IsZero() {
			in.downAt = now
		} else {
			in.state.Up, in.state.Since = false, pending
			in.downAt = time.Time{}
		}
	case up:
		in.downAt = time.Time{}
	}
	in.save()
	in.mu.Unlock()

	if outage != nil && in.events != nil {
		in.events.Post(Event{Time: now, DeviceID: "internet", Device: "Internet was down " + shortDur(outage.End.Sub(outage.Start)), Kind: "internet"})
	}
}

// RecordSpeed stores a speed test run in someone's browser. The server is on weak Wi-Fi,
// so it can't measure the internet itself; a laptop or phone can.
func (in *Internet) RecordSpeed(sp Speed) {
	in.mu.Lock()
	defer in.mu.Unlock()
	in.state.Speeds = append([]Speed{sp}, in.state.Speeds...)
	if len(in.state.Speeds) > 50 {
		in.state.Speeds = in.state.Speeds[:50]
	}
	in.save()
}

func (in *Internet) View() InternetState {
	in.mu.Lock()
	defer in.mu.Unlock()
	s := in.state
	r := uptimeReport(s, time.Now())
	s.Report = &r
	s.Outages = append([]Outage(nil), s.Outages...)
	if len(s.Outages) > 20 {
		s.Outages = s.Outages[:20]
	}
	s.Speeds = append([]Speed(nil), s.Speeds...)
	if len(s.Speeds) > 10 {
		s.Speeds = s.Speeds[:10]
	}
	return s
}

func (in *Internet) save() {
	b, _ := json.MarshalIndent(in.state, "", " ")
	os.WriteFile(in.path, b, 0o600)
}

// uptimeReport works out the share of time the internet was up over the last 7 and 30
// days, counting only time since monitoring began.
func uptimeReport(s InternetState, now time.Time) UptimeReport {
	r := UptimeReport{Since: s.Watching}
	window := func(days int) (float64, int, time.Duration) {
		from := now.AddDate(0, 0, -days)
		if s.Watching.After(from) {
			from = s.Watching
		}
		total := now.Sub(from)
		var down time.Duration
		n := 0
		spans := append([]Outage(nil), s.Outages...)
		if !s.Up && !s.Since.IsZero() {
			spans = append(spans, Outage{Start: s.Since, End: now}) // still down
		}
		for _, o := range spans {
			a, b := o.Start, o.End
			if a.Before(from) {
				a = from
			}
			if b.After(a) {
				down += b.Sub(a)
				n++
			}
		}
		if total <= 0 {
			return 100, n, down
		}
		return 100 * (1 - down.Seconds()/total.Seconds()), n, down
	}
	r.Up7, _, _ = window(7)
	var down time.Duration
	r.Up30, r.Outages30, down = window(30)
	r.DownMin30 = int(down.Minutes())
	return r
}

func shortDur(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	return fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
}
