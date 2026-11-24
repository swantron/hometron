package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func newTestEvents(t *testing.T) *Events {
	t.Helper()
	e, err := newEvents(nil, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestFoldRepeats(t *testing.T) {
	e := newTestEvents(t)
	t0 := time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)
	e.Post(Event{Time: t0, DeviceID: "cam", Kind: "motion"})
	e.Post(Event{Time: t0.Add(time.Minute), DeviceID: "cam", Kind: "motion"})
	e.Post(Event{Time: t0.Add(2 * time.Minute), DeviceID: "cam", Kind: "motion"})
	e.Post(Event{Time: t0.Add(2 * time.Minute), DeviceID: "other", Kind: "motion"})
	e.Post(Event{Time: t0.Add(3 * time.Minute), DeviceID: "cam", Kind: "person"})  // never folds
	e.Post(Event{Time: t0.Add(4 * time.Minute), DeviceID: "cam", Kind: "motion"})  // newest row for cam is person
	e.Post(Event{Time: t0.Add(20 * time.Minute), DeviceID: "cam", Kind: "motion"}) // outside window

	got := e.Recent()
	if len(got) != 5 {
		t.Fatalf("want 5 rows, got %d: %+v", len(got), got)
	}
	first := got[len(got)-1]
	if first.Count != 3 || !first.Time.Equal(t0.Add(2*time.Minute)) {
		t.Errorf("folded row = %+v, want count 3 at +2m", first)
	}
}

func TestLoadLogDedupes(t *testing.T) {
	dir := t.TempDir()
	e, _ := newEvents(nil, dir)
	t0 := time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)
	e.Post(Event{Time: t0, DeviceID: "cam", Kind: "motion"})
	e.Post(Event{Time: t0.Add(time.Minute), DeviceID: "cam", Kind: "motion"}) // rewrites row 1
	e.Post(Event{Time: t0.Add(time.Hour), DeviceID: "cam", Kind: "person"})

	b, _ := os.ReadFile(filepath.Join(dir, "events", "events.jsonl"))
	if n := strings.Count(string(b), "\n"); n != 3 {
		t.Fatalf("log has %d lines, want 3", n)
	}
	again, _ := newEvents(nil, dir)
	got := again.Recent()
	if len(got) != 2 || got[1].Count != 2 || got[0].Kind != "person" {
		t.Errorf("reloaded feed = %+v", got)
	}
}

// fakeTailwind mimics the controller's /json endpoint.
type fakeTailwind struct {
	mu     sync.Mutex
	status string
	ops    []string
}

func (f *fakeTailwind) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("TOKEN") != "123456" {
		// what firmware 10.45 actually sends
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"result": "Fail","info": "token check fail"}`)
		return
	}
	var req struct {
		Data struct {
			Type, Name string
			Value      struct {
				DoorIdx int    `json:"door_idx"`
				Cmd     string `json:"cmd"`
			}
		}
	}
	json.NewDecoder(r.Body).Decode(&req)
	f.mu.Lock()
	defer f.mu.Unlock()
	if req.Data.Name == "door_op" {
		f.ops = append(f.ops, req.Data.Value.Cmd)
		io.WriteString(w, `{"result":"OK"}`)
		return
	}
	io.WriteString(w, `{"result":"OK","door_num":1,"data":{"door1":{"index":0,"status":"`+f.status+`","lockup":0,"disabled":0}}}`)
}

func TestTailwind(t *testing.T) {
	fake := &fakeTailwind{status: "close"}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	ev := newTestEvents(t)
	host := strings.TrimPrefix(srv.URL, "http://")
	tw := &Tailwind{cfg: tailwindConfig{Host: host, Token: "123456"}, http: srv.Client(), events: ev}
	ctx := context.Background()

	tw.poll(ctx)
	devs, _ := tw.Devices(ctx)
	if len(devs) != 1 || devs[0].Door != "closed" || devs[0].ID != "tailwind-door1" || devs[0].Label != "Garage door" {
		t.Fatalf("devices = %+v", devs)
	}
	if _, err := tw.Command(ctx, "tailwind-door1", "garage.open", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Command(ctx, "tailwind-door1", "garage.explode", nil); err != errNotAllowed {
		t.Errorf("unknown command err = %v", err)
	}
	fake.mu.Lock()
	fake.status = "open"
	ops := append([]string(nil), fake.ops...)
	fake.mu.Unlock()
	if len(ops) != 1 || ops[0] != "open" {
		t.Errorf("ops = %v", ops)
	}
	tw.poll(ctx)
	if r := ev.Recent(); len(r) != 1 || r[0].Kind != "opened" || r[0].DeviceID != "tailwind-door1" {
		t.Errorf("feed = %+v", r)
	}

	bad := &Tailwind{cfg: tailwindConfig{Host: host, Token: "nope"}, http: srv.Client(), events: ev}
	bad.poll(ctx)
	if devs, _ := bad.Devices(ctx); devs[0].Door != "unknown" || !strings.Contains(devs[0].Problem, "key rejected") {
		t.Errorf("bad token devices = %+v", devs)
	}
}

func TestTailwindNoSensor(t *testing.T) {
	fake := &fakeTailwind{status: "open"}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	ev := newTestEvents(t)
	off := false
	tw := &Tailwind{cfg: tailwindConfig{Host: strings.TrimPrefix(srv.URL, "http://"), Token: "123456", Sensor: &off, Camera: "Garage camera"},
		http: srv.Client(), events: ev}
	ctx := context.Background()
	tw.poll(ctx)
	devs, _ := tw.Devices(ctx)
	if devs[0].Door != "unknown" || !devs[0].NoSensor || devs[0].Camera != "Garage camera" {
		t.Fatalf("devices = %+v", devs)
	}
	// press sends the opposite of what the Tailwind believes
	if _, err := tw.Command(ctx, "tailwind-door1", "garage.press", nil); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	ops := append([]string(nil), fake.ops...)
	fake.status = "close"
	fake.mu.Unlock()
	if len(ops) != 1 || ops[0] != "close" {
		t.Errorf("ops = %v", ops)
	}
	tw.poll(ctx)
	if r := ev.Recent(); len(r) != 0 {
		t.Errorf("sensorless state change should not hit the feed: %+v", r)
	}
}

func TestSnapshotRoute(t *testing.T) {
	dir := t.TempDir()
	snaps := newSnapshots(dir)
	pu, _ := loadPush(dir)
	h := routes([]Provider{fakeProvider{}}, newTestEvents(t), newNetwork(dir), snaps, pu, nil, dir)
	jpeg := append([]byte{0xFF, 0xD8, 0xFF, 0xE0}, make([]byte, 100)...)

	post := func(id, ctype, origin string, body []byte) int {
		req := httptest.NewRequest("POST", "/api/devices/"+id+"/snapshot", strings.NewReader(string(body)))
		req.Header.Set("Content-Type", ctype)
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	if c := post("cam1", "image/jpeg", "http://evil.test", jpeg); c != 403 {
		t.Errorf("cross-origin = %d", c)
	}
	if c := post("cam1", "text/plain", "", jpeg); c != 403 {
		t.Errorf("wrong type = %d", c)
	}
	if c := post("cam1", "image/jpeg", "", []byte("not a jpeg")); c != 400 {
		t.Errorf("garbage = %d", c)
	}
	if c := post("nope", "image/jpeg", "", jpeg); c != 404 {
		t.Errorf("unknown device = %d", c)
	}
	if c := post("cam1", "image/jpeg", "http://evil.test", jpeg); c != 403 {
		t.Errorf("cross-origin = %d", c)
	}
	if c := post("cam1", "image/jpeg", "", jpeg); c != 200 {
		t.Fatalf("good upload = %d", c)
	}
	s := newSnapshots(dir).All()["cam1"] // reload from disk
	if s.Media == "" {
		t.Fatal("snapshot index not persisted")
	}
	if b, _ := os.ReadFile(filepath.Join(dir, s.Media)); string(b) != string(jpeg) {
		t.Error("stored bytes differ")
	}
	// the index itself must not be downloadable
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/media/snapshots.json", nil))
	if rec.Code != 404 {
		t.Errorf("/media/snapshots.json = %d", rec.Code)
	}
}

type fakeProvider struct{}

func (fakeProvider) Devices(context.Context) ([]deviceView, error) { return nil, nil }
func (fakeProvider) Owns(_ context.Context, id string) bool        { return id == "cam1" }
func (fakeProvider) Command(context.Context, string, string, map[string]any) (json.RawMessage, error) {
	return nil, errNotAllowed
}

func TestAttachEventPicture(t *testing.T) {
	e := newTestEvents(t)
	now := time.Now()
	e.post(Event{Time: now.Add(-5 * time.Minute), DeviceID: "cam1", Device: "Kitchen", Kind: "person"}, "")
	e.post(Event{Time: now.Add(-10 * time.Second), DeviceID: "cam1", Device: "Kitchen", Kind: "person"}, "")
	recent := e.Recent()
	fresh, old := recent[0], recent[1]
	jpeg := append([]byte{0xFF, 0xD8, 0xFF, 0xE0}, make([]byte, 100)...)

	if _, err := e.Attach(fresh.ID, "cam2", jpeg, now); err == nil {
		t.Error("picture from another camera accepted")
	}
	if _, err := e.Attach(old.ID, "cam1", jpeg, now); err == nil {
		t.Error("picture for a 5-minute-old event accepted")
	}
	ch, stop := e.Subscribe()
	defer stop()
	ev, err := e.Attach(fresh.ID, "cam1", jpeg, now)
	if err != nil || ev.Media == "" {
		t.Fatalf("attach = %+v, %v", ev, err)
	}
	if got := <-ch; got.ID != fresh.ID || got.Media != ev.Media {
		t.Errorf("broadcast = %+v", got)
	}
	if _, err := e.Attach(fresh.ID, "cam1", jpeg, now); err == nil {
		t.Error("second picture replaced the first")
	}
	if b, _ := os.ReadFile(filepath.Join(e.dir, ev.Media)); string(b) != string(jpeg) {
		t.Error("stored bytes differ")
	}
	// The picture survives a restart.
	e2, _ := newEvents(nil, filepath.Dir(e.dir))
	if r := e2.Recent(); r[0].Media != ev.Media {
		t.Errorf("after reload = %+v", r[0])
	}
}
