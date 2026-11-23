package main

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Event is one thing that happened at a device, as shown in the UI feed.
type Event struct {
	ID       string    `json:"id"`
	Time     time.Time `json:"time"`
	DeviceID string    `json:"deviceId"`
	Device   string    `json:"device"`
	Kind     string    `json:"kind"`            // chime, person, motion, sound, clip, opened, closed
	Media    string    `json:"media,omitempty"` // file name under events/, served at /media/
	Count    int       `json:"count,omitempty"` // >1 when repeats were folded into this row
}

// rawEvent is the Pub/Sub payload Google sends for SDM events.
type rawEvent struct {
	EventID        string    `json:"eventId"`
	Timestamp      time.Time `json:"timestamp"`
	EventThreadID  string    `json:"eventThreadId"`
	ResourceUpdate struct {
		Name   string                     `json:"name"`
		Events map[string]json.RawMessage `json:"events"`
	} `json:"resourceUpdate"`
}

var eventKinds = map[string]string{
	"sdm.devices.events.DoorbellChime.Chime":           "chime",
	"sdm.devices.events.CameraPerson.Person":           "person",
	"sdm.devices.events.CameraMotion.Motion":           "motion",
	"sdm.devices.events.CameraSound.Sound":             "sound",
	"sdm.devices.events.CameraClipPreview.ClipPreview": "clip",
}

// kindRank picks the headline when one payload carries several events (a ring beats a person beats motion).
var kindRank = map[string]int{"chime": 0, "person": 1, "motion": 2, "sound": 3, "clip": 4}

// parsed is what we extract from one Pub/Sub payload.
type parsed struct {
	deviceID   string
	when       time.Time
	thread     string
	kind       string // headline kind, "" if only a clip arrived
	imageEvent string // eventId usable with GenerateImage
	previewURL string // clip preview URL, if any
}

func parseEvent(data []byte) (parsed, bool) {
	var r rawEvent
	if err := json.Unmarshal(data, &r); err != nil || len(r.ResourceUpdate.Events) == 0 {
		return parsed{}, false // trait updates (thermostat temp etc.) carry no events
	}
	p := parsed{
		deviceID: r.ResourceUpdate.Name[strings.LastIndex(r.ResourceUpdate.Name, "/")+1:],
		when:     r.Timestamp,
		thread:   r.EventThreadID,
	}
	names := make([]string, 0, len(r.ResourceUpdate.Events))
	for n := range r.ResourceUpdate.Events {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		k, ok := eventKinds[n]
		if !ok {
			continue
		}
		var body struct {
			EventID    string `json:"eventId"`
			PreviewURL string `json:"previewUrl"`
		}
		_ = json.Unmarshal(r.ResourceUpdate.Events[n], &body)
		if k == "clip" {
			p.previewURL = body.PreviewURL
			continue
		}
		if p.kind == "" || kindRank[k] < kindRank[p.kind] {
			p.kind = k
			if body.EventID != "" {
				p.imageEvent = body.EventID
			}
		}
	}
	if p.kind == "" && p.previewURL == "" {
		return parsed{}, false
	}
	return p, true
}

// Events keeps a recent feed in memory, appends it to disk, and fans out to SSE listeners.
type Events struct {
	sdm *SDM
	dir string // <state>/events

	mu      sync.Mutex
	recent  []Event
	threads map[string]int // eventThreadId -> index in recent, to merge updates to one session
	subs    map[chan Event]struct{}

	// OnAlert, if set, runs for each new row and for a row whose headline got more
	// important (motion that turned out to be a person). Push alerts hang off this.
	OnAlert func(Event)
}

const keepRecent = 200

func newEvents(sdm *SDM, stateDir string) (*Events, error) {
	dir := filepath.Join(stateDir, "events")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	e := &Events{sdm: sdm, dir: dir, threads: map[string]int{}, subs: map[chan Event]struct{}{}}
	e.loadLog()
	return e, nil
}

func (e *Events) loadLog() {
	b, err := os.ReadFile(filepath.Join(e.dir, "events.jsonl"))
	if err != nil {
		return
	}
	// The log is append-only, so an event updated later (clip arrived, repeats folded)
	// appears more than once. Keep its first position and its last contents.
	at := map[string]int{}
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var ev Event
		if json.Unmarshal([]byte(l), &ev) != nil {
			continue
		}
		if i, ok := at[ev.ID]; ok {
			e.recent[i] = ev
			continue
		}
		at[ev.ID] = len(e.recent)
		e.recent = append(e.recent, ev)
	}
	if len(e.recent) > keepRecent {
		e.recent = e.recent[len(e.recent)-keepRecent:]
	}
}

// Recent returns the feed, newest first.
func (e *Events) Recent() []Event {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]Event, len(e.recent))
	for i, ev := range e.recent {
		out[len(e.recent)-1-i] = ev
	}
	return out
}

func (e *Events) Subscribe() (chan Event, func()) {
	ch := make(chan Event, 16)
	e.mu.Lock()
	e.subs[ch] = struct{}{}
	e.mu.Unlock()
	return ch, func() {
		e.mu.Lock()
		delete(e.subs, ch)
		e.mu.Unlock()
	}
}

// Handle processes one Pub/Sub payload. Media is fetched right away because
// Google only keeps event images for about 30 seconds.
func (e *Events) Handle(ctx context.Context, data []byte) {
	p, ok := parseEvent(data)
	if !ok {
		return
	}
	dev, err := e.sdm.Device(ctx, p.deviceID)
	label := p.deviceID
	if err == nil {
		label = dev.Label()
	}

	media := ""
	stamp := p.when.UTC().Format("20060102-150405")
	if p.previewURL != "" {
		if b, err := e.sdm.FetchClipPreview(ctx, p.previewURL); err == nil {
			media = e.save(stamp+"-"+short(p.deviceID)+".mp4", b)
		} else {
			log.Printf("clip preview %s: %v", label, err)
		}
	}
	if media == "" && p.imageEvent != "" && err == nil && dev.EventImages() {
		if b, err := e.sdm.FetchEventImage(ctx, p.deviceID, p.imageEvent); err == nil {
			media = e.save(stamp+"-"+short(p.deviceID)+".jpg", b)
		} else {
			log.Printf("event image %s: %v", label, err)
		}
	}

	kind := p.kind
	if kind == "" {
		kind = "clip"
	}
	e.post(Event{Time: p.when, DeviceID: p.deviceID, Device: label, Kind: kind, Media: media}, p.thread)
}

// Post adds an event from any integration (garage door, ...) to the feed.
func (e *Events) Post(ev Event) { e.post(ev, "") }

// foldWindow: repeats of the same low-value event on one device within this window share a row.
const foldWindow = 3 * time.Minute

func (e *Events) post(in Event, thread string) {
	if in.ID == "" {
		in.ID = in.Time.UTC().Format("20060102-150405.000") + "-" + short(in.DeviceID)
	}
	e.mu.Lock()
	var ev Event
	alert := false
	if i, ok := e.threads[thread]; ok && thread != "" && i < len(e.recent) {
		// Same session (e.g. the clip arriving after the person event): update in place.
		ev = e.recent[i]
		if in.Kind != "clip" && kindRank[in.Kind] < kindRank[ev.Kind] {
			ev.Kind, alert = in.Kind, true
		}
		if in.Media != "" {
			ev.Media = in.Media
		}
		e.recent[i] = ev
	} else if i := e.foldable(in); i >= 0 {
		// Motion every 30s at the same camera is one row, not twenty.
		ev = e.recent[i]
		ev.Time, ev.Count = in.Time, max(ev.Count, 1)+1
		e.recent[i] = ev
		if thread != "" {
			e.threads[thread] = i
		}
	} else {
		ev, alert = in, true
		e.recent = append(e.recent, ev)
		if len(e.recent) > keepRecent {
			e.recent = e.recent[len(e.recent)-keepRecent:]
			e.threads = map[string]int{} // indexes shifted; only lose merging for old sessions
		}
		if thread != "" {
			e.threads[thread] = len(e.recent) - 1
		}
	}
	for ch := range e.subs {
		select {
		case ch <- ev:
		default: // slow listener; it will catch up on reload
		}
	}
	e.mu.Unlock()

	e.appendLog(ev)
	log.Printf("event: %s %s media=%q", ev.Device, ev.Kind, ev.Media)
	if alert && e.OnAlert != nil {
		go e.OnAlert(ev)
	}
}

// foldable returns the index of a recent row the event can be folded into, or -1.
// Only media-less motion/sound folds; people, rings and garage moves always get their own row.
func (e *Events) foldable(in Event) int {
	if in.Media != "" || (in.Kind != "motion" && in.Kind != "sound") {
		return -1
	}
	for i := len(e.recent) - 1; i >= 0 && i >= len(e.recent)-20; i-- {
		r := e.recent[i]
		if r.DeviceID != in.DeviceID {
			continue
		}
		if r.Kind == in.Kind && r.Media == "" && in.Time.Sub(r.Time) < foldWindow {
			return i
		}
		return -1 // the newest row for this device is something else
	}
	return -1
}

func (e *Events) save(name string, b []byte) string {
	if err := os.WriteFile(filepath.Join(e.dir, name), b, 0o600); err != nil {
		log.Printf("save %s: %v", name, err)
		return ""
	}
	return name
}

func (e *Events) appendLog(ev Event) {
	f, err := os.OpenFile(filepath.Join(e.dir, "events.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		log.Printf("event log: %v", err)
		return
	}
	defer f.Close()
	b, _ := json.Marshal(ev)
	f.Write(append(b, '\n'))
}

// Prune deletes media older than maxAge so the disk doesn't fill up.
func (e *Events) Prune(maxAge time.Duration) {
	entries, _ := os.ReadDir(e.dir)
	for _, de := range entries {
		n := de.Name()
		if strings.HasSuffix(n, ".jsonl") || strings.HasSuffix(n, ".json") || strings.HasPrefix(n, "snap-") {
			continue // the log, the snapshot index, and the one-per-camera stills stay
		}
		if info, err := de.Info(); err == nil && time.Since(info.ModTime()) > maxAge {
			os.Remove(filepath.Join(e.dir, de.Name()))
		}
	}
}

func short(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}
