package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Snapshots keeps the latest still per camera. Google won't hand out event images for
// WebRTC-only cameras, so the browser grabs a frame from the live stream and uploads it.
type Snapshots struct {
	dir string // <state>/events (served at /media/)

	mu  sync.Mutex
	idx map[string]Snapshot // device id -> latest
}

type Snapshot struct {
	Media string    `json:"media"`
	Time  time.Time `json:"time"`
}

const maxSnapshot = 3 << 20

func newSnapshots(eventsDir string) *Snapshots {
	s := &Snapshots{dir: eventsDir, idx: map[string]Snapshot{}}
	if b, err := os.ReadFile(filepath.Join(eventsDir, "snapshots.json")); err == nil {
		_ = json.Unmarshal(b, &s.idx)
	}
	return s
}

func (s *Snapshots) All() map[string]Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]Snapshot, len(s.idx))
	for k, v := range s.idx {
		out[k] = v
	}
	return out
}

// isJPEG checks the magic bytes; we only ever store what a canvas produced.
func isJPEG(b []byte) bool {
	return len(b) > 4 && bytes.Equal(b[:3], []byte{0xFF, 0xD8, 0xFF})
}

// Save stores a new still for a device, replacing the previous one.
func (s *Snapshots) Save(deviceID string, jpeg []byte, at time.Time) (Snapshot, error) {
	sum := sha256.Sum256([]byte(deviceID))
	name := "snap-" + hex.EncodeToString(sum[:6]) + ".jpg"
	// A temp file of its own: a live view's own grab and a snapOnce grab can land in the same
	// second for the same camera, and a shared temp name made one of them fail.
	f, err := os.CreateTemp(s.dir, "."+name+"-*")
	if err != nil {
		return Snapshot{}, err
	}
	tmp := f.Name()
	_, err = f.Write(jpeg)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, filepath.Join(s.dir, name))
	}
	if err != nil {
		os.Remove(tmp)
		return Snapshot{}, err
	}
	snap := Snapshot{Media: name, Time: at}
	s.mu.Lock()
	s.idx[deviceID] = snap
	b, _ := json.MarshalIndent(s.idx, "", " ")
	s.mu.Unlock()
	return snap, os.WriteFile(filepath.Join(s.dir, "snapshots.json"), b, 0o600)
}
