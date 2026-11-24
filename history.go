package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Thermostat history: indoor temperature, setpoints and whether the furnace/AC is running,
// sampled whenever Google reports a change (Pub/Sub trait updates) and every 5 minutes
// regardless. Appended to <state>/thermostat.jsonl; outdoor temperature rides along when
// weather is configured.
type Sample struct {
	T     time.Time `json:"t"`
	TempF *float64  `json:"in,omitempty"`
	Hum   *float64  `json:"rh,omitempty"`
	HeatF *float64  `json:"heat,omitempty"`
	CoolF *float64  `json:"cool,omitempty"`
	Mode  string    `json:"mode,omitempty"`
	Hvac  string    `json:"hvac,omitempty"` // HEATING | COOLING | OFF
	OutF  *float64  `json:"out,omitempty"`
}

type History struct {
	path    string
	devices func(context.Context) ([]deviceView, error)
	outdoor func() *float64 // current outside temp, or nil

	poke chan struct{}
	mu   sync.Mutex
	last Sample
}

func newHistory(stateDir string, devices func(context.Context) ([]deviceView, error), outdoor func() *float64) *History {
	h := &History{path: filepath.Join(stateDir, "thermostat.jsonl"), devices: devices, outdoor: outdoor, poke: make(chan struct{}, 1)}
	if s := h.Read(time.Now().Add(-24 * time.Hour)); len(s) > 0 {
		h.last = s[len(s)-1]
	}
	return h
}

// Poke asks for a fresh sample soon (a trait update arrived).
func (h *History) Poke() {
	select {
	case h.poke <- struct{}{}:
	default:
	}
}

func (h *History) Run(ctx context.Context, invalidate func()) {
	t := time.NewTicker(5 * time.Minute)
	defer t.Stop()
	h.sample(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-h.poke:
			time.Sleep(2 * time.Second) // updates come in bursts; take one reading after
			invalidate()
		}
		h.sample(ctx)
	}
}

func (h *History) sample(ctx context.Context) {
	devs, err := h.devices(ctx)
	if err != nil {
		log.Printf("history: %v", err)
		return
	}
	for _, d := range devs {
		if d.Kind != "thermostat" {
			continue
		}
		s := Sample{T: time.Now().Truncate(time.Second), TempF: d.TempF, Hum: d.Humidity, HeatF: d.HeatF, CoolF: d.CoolF, Mode: d.Mode, Hvac: d.Hvac}
		if d.Eco == "MANUAL_ECO" {
			s.Mode = "ECO"
		}
		if h.outdoor != nil {
			s.OutF = h.outdoor()
		}
		h.record(s)
		return // one thermostat in this house
	}
}

// record appends s unless nothing changed and the last sample is recent.
func (h *History) record(s Sample) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if same(h.last, s) && s.T.Sub(h.last.T) < 15*time.Minute {
		return
	}
	f, err := os.OpenFile(h.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		log.Printf("history: %v", err)
		return
	}
	defer f.Close()
	b, _ := json.Marshal(s)
	f.Write(append(b, '\n'))
	h.last = s
}

func same(a, b Sample) bool {
	eq := func(x, y *float64) bool { return (x == nil) == (y == nil) && (x == nil || *x == *y) }
	return eq(a.TempF, b.TempF) && eq(a.Hum, b.Hum) && eq(a.HeatF, b.HeatF) && eq(a.CoolF, b.CoolF) &&
		a.Mode == b.Mode && a.Hvac == b.Hvac && eq(a.OutF, b.OutF)
}

// Read returns samples since t, oldest first. The file grows ~30 KB a day, so only the
// tail is read: a week of samples fits easily in the last 2 MB.
func (h *History) Read(since time.Time) []Sample {
	f, err := os.Open(h.path)
	if err != nil {
		return nil
	}
	defer f.Close()
	if st, err := f.Stat(); err == nil && st.Size() > 2<<20 {
		f.Seek(st.Size()-2<<20, io.SeekStart)
	}
	var out []Sample
	sc := bufio.NewScanner(f)
	first := true
	for sc.Scan() {
		var s Sample
		if json.Unmarshal(sc.Bytes(), &s) != nil {
			if !first {
				log.Printf("history: skipping a bad line")
			}
			first = false // a seek lands mid-line; that one's expected
			continue
		}
		first = false
		if !s.T.Before(since) {
			out = append(out, s)
		}
	}
	return out
}

// Runtime adds up how long the system was heating and cooling between from and to.
// A sample's status lasts until the next sample (or to, for the last one).
func Runtime(samples []Sample, before *Sample, from, to time.Time) (heat, cool time.Duration) {
	cur, at := "", from
	if before != nil {
		cur = before.Hvac
	}
	add := func(until time.Time) {
		if until.After(at) {
			switch cur {
			case "HEATING":
				heat += until.Sub(at)
			case "COOLING":
				cool += until.Sub(at)
			}
		}
	}
	for _, s := range samples {
		if s.T.Before(from) {
			cur = s.Hvac
			continue
		}
		if s.T.After(to) {
			break
		}
		add(s.T)
		cur, at = s.Hvac, s.T
	}
	add(to)
	return heat, cool
}

// isTraitUpdate reports whether a Pub/Sub payload carries trait changes (thermostat
// readings) rather than camera events.
func isTraitUpdate(data []byte) bool {
	var r struct {
		ResourceUpdate struct {
			Traits map[string]json.RawMessage `json:"traits"`
		} `json:"resourceUpdate"`
	}
	return json.Unmarshal(data, &r) == nil && len(r.ResourceUpdate.Traits) > 0
}
