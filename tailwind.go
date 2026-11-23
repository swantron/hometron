package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Tailwind talks to a Tailwind iQ3 garage controller over its local HTTP API
// (no cloud). Config lives in <state>/tailwind.json:
//
//	{"host": "192.168.x.y", "token": "123456", "names": {"door1": "Garage"},
//	 "sensor": false, "camera": "Garage camera"}
//
// The token is the "Local Control Key" from web.gotailwind.com. The API is poll-only,
// so a goroutine reads status every few seconds and posts open/close to the feed.
//
// "sensor": false is for installs without a door sensor: the Tailwind's open/closed is
// then a guess, so it isn't shown or logged, and the UI offers one "press" button next to
// a still from "camera" (a camera label) instead.
type Tailwind struct {
	cfg    tailwindConfig
	http   *http.Client
	events *Events

	mu     sync.Mutex
	doors  map[string]twDoor // door1, door2, ...
	seen   bool              // at least one good poll
	err    error
	polled time.Time
}

type tailwindConfig struct {
	Host   string            `json:"host"`
	Token  string            `json:"token"`
	Names  map[string]string `json:"names"`
	Sensor *bool             `json:"sensor"` // default true
	Camera string            `json:"camera"` // camera label to show on the garage card
}

func (c tailwindConfig) hasSensor() bool { return c.Sensor == nil || *c.Sensor }

type twDoor struct {
	Index    int    `json:"index"`
	Status   string `json:"status"` // "open" | "close"
	Lockup   int    `json:"lockup"`
	Disabled int    `json:"disabled"`
}

const twPrefix = "tailwind-"

// loadTailwind returns nil (integration off) when tailwind.json doesn't exist.
func loadTailwind(stateDir string, events *Events) (*Tailwind, error) {
	var c tailwindConfig
	if err := readJSON(filepath.Join(stateDir, "tailwind.json"), &c); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	if c.Host == "" || c.Token == "" {
		return nil, fmt.Errorf("tailwind.json needs host and token")
	}
	return &Tailwind{cfg: c, http: &http.Client{Timeout: 5 * time.Second}, events: events}, nil
}

func (t *Tailwind) call(ctx context.Context, data map[string]any) (map[string]json.RawMessage, error) {
	body, _ := json.Marshal(map[string]any{"version": "0.1", "data": data})
	req, err := http.NewRequestWithContext(ctx, "POST", "http://"+t.cfg.Host+"/json", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("TOKEN", t.cfg.Token)
	resp, err := t.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out map[string]json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("tailwind: bad response (%s): %w", resp.Status, err)
	}
	var result, info, msg string
	_ = json.Unmarshal(out["result"], &result)
	_ = json.Unmarshal(out["info"], &info)
	_ = json.Unmarshal(out["message"], &msg)
	switch {
	case result == "OK":
		return out, nil
	// Older firmware says "token fail"; 10.4x says "Fail" with info "token check fail".
	case result == "token fail" || strings.Contains(info, "token"):
		return nil, errors.New("tailwind: local control key rejected")
	default:
		return nil, fmt.Errorf("tailwind: %s %s%s", result, info, msg)
	}
}

func (t *Tailwind) poll(ctx context.Context) {
	out, err := t.call(ctx, map[string]any{"type": "get", "name": "dev_st"})
	var doors map[string]twDoor
	if err == nil {
		err = json.Unmarshal(out["data"], &doors)
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.polled = time.Now()
	if err != nil {
		if t.err == nil || t.err.Error() != err.Error() {
			log.Printf("tailwind: %v", err)
		}
		t.err = err
		return
	}
	if t.err != nil {
		log.Printf("tailwind: back online")
	}
	t.err = nil
	if t.seen && t.cfg.hasSensor() { // without a sensor, state changes are fiction
		for id, d := range doors {
			if old, ok := t.doors[id]; ok && old.Status != d.Status {
				kind := map[string]string{"open": "opened", "close": "closed"}[d.Status]
				if kind != "" {
					t.events.Post(Event{Time: time.Now(), DeviceID: twPrefix + id, Device: t.name(id), Kind: kind})
				}
			}
		}
	}
	t.doors, t.seen = doors, true
}

// Run polls until ctx is done. 5s is quick enough to notice a door and gentle on the controller.
func (t *Tailwind) Run(ctx context.Context) {
	for tick := time.NewTicker(5 * time.Second); ctx.Err() == nil; <-tick.C {
		pc, cancel := context.WithTimeout(ctx, 5*time.Second)
		t.poll(pc)
		cancel()
	}
}

func (t *Tailwind) name(id string) string {
	if n := t.cfg.Names[id]; n != "" {
		return n
	}
	if len(t.doors) <= 1 {
		return "Garage door"
	}
	return "Garage " + strings.TrimPrefix(id, "door")
}

// ---- Provider ----

func (t *Tailwind) Owns(_ context.Context, id string) bool { return strings.HasPrefix(id, twPrefix) }

func (t *Tailwind) Devices(context.Context) ([]deviceView, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.seen {
		// Never reached the controller: show one placeholder so the UI can say why.
		return []deviceView{{ID: twPrefix + "door1", Kind: "garage", Label: "Garage door", Door: "unknown",
			Problem: errText(t.err), Camera: t.cfg.Camera, NoSensor: !t.cfg.hasSensor()}}, nil
	}
	ids := make([]string, 0, len(t.doors))
	for id := range t.doors {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]deviceView, 0, len(ids))
	for _, id := range ids {
		d := t.doors[id]
		v := deviceView{ID: twPrefix + id, Kind: "garage", Label: t.name(id), Online: t.err == nil,
			Door: map[string]string{"open": "open", "close": "closed"}[d.Status], Problem: errText(t.err),
			Camera: t.cfg.Camera, NoSensor: !t.cfg.hasSensor()}
		switch {
		case d.Disabled != 0:
			v.Problem = "disabled in the Tailwind app"
		case d.Lockup != 0:
			v.Problem = "the Tailwind reports this door locked out; check the Tailwind app"
		}
		if v.Door == "" || v.NoSensor {
			v.Door = "unknown"
		}
		out = append(out, v)
	}
	return out, nil
}

// garage.press is for sensorless doors: send whichever command the Tailwind thinks moves
// the door, which pulses the opener's button either way.
var garageCommands = map[string]string{"garage.open": "open", "garage.close": "close", "garage.press": ""}

func (t *Tailwind) Command(ctx context.Context, id, command string, _ map[string]any) (json.RawMessage, error) {
	cmd, ok := garageCommands[command]
	if !ok {
		return nil, errNotAllowed
	}
	t.mu.Lock()
	d, ok := t.doors[strings.TrimPrefix(id, twPrefix)]
	t.mu.Unlock()
	switch {
	case !ok:
		return nil, fmt.Errorf("unknown door %s", id)
	case d.Disabled != 0 || d.Lockup != 0:
		return nil, errors.New("the Tailwind has this door disabled or locked out; it won't take remote commands until that clears")
	}
	if cmd == "" {
		cmd = "open"
		if d.Status == "open" {
			cmd = "close"
		}
	}
	if _, err := t.call(ctx, map[string]any{"type": "set", "name": "door_op",
		"value": map[string]any{"door_idx": d.Index, "cmd": cmd}}); err != nil {
		return nil, err
	}
	log.Printf("tailwind: %s %s", cmd, id)
	go func() { // pick up the new state sooner than the next tick
		time.Sleep(2 * time.Second)
		pc, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		t.poll(pc)
	}()
	return json.RawMessage(`{}`), nil
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
