package main

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Capture keeps the dashboard open in a headless Chrome on the server, in capture mode
// (?capture), so camera pictures don't depend on someone having a tab open: Google sends
// no images for most cameras' events, and live view only runs in a browser (WebRTC). The
// page grabs a frame when a person shows up or the doorbell rings, and keeps every
// camera's still fresh. It's a separate profile from the SmartThings one and holds no
// sign-ins: it only talks to hometron over loopback.
type Capture struct {
	b      *stBrowser
	url    string
	opened time.Time
}

func newCapture(stateDir, listen string) *Capture {
	if _, err := os.Stat(chromeBin); err != nil {
		return nil
	}
	if _, err := os.Stat(filepath.Join(stateDir, "capture-off")); err == nil {
		return nil // touch <state>/capture-off to disable
	}
	for _, a := range strings.Split(listen, ",") {
		a = strings.TrimSpace(a)
		if strings.HasPrefix(a, "127.0.0.1:") || strings.HasPrefix(a, "localhost:") {
			return &Capture{
				b: &stBrowser{bin: chromeBin, profile: filepath.Join(stateDir, "capture-browser"), headless: true,
					extra: []string{"--autoplay-policy=no-user-gesture-required", "--mute-audio",
						// Nobody ever looks at this tab, and Chrome treats an unseen tab as
						// hidden: it throttles timers and can pause video. Keep it running.
						"--disable-background-timer-throttling", "--disable-renderer-backgrounding",
						"--disable-backgrounding-occluded-windows"}},
				url: "http://" + a + "/?capture",
			}
		}
	}
	return nil // needs a loopback address to load the page from
}

func (c *Capture) Run(ctx context.Context) {
	time.Sleep(5 * time.Second) // let the server start listening
	for {
		if err := c.ensure(ctx); err != nil {
			log.Printf("capture: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Minute):
		}
	}
}

// ensure starts the browser if it isn't running and keeps exactly one capture tab,
// reopened every 6 hours so a long-running page can't slowly leak.
func (c *Capture) ensure(ctx context.Context) error {
	c.b.mu.Lock()
	defer c.b.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	cd, err := c.b.connect(ctx, true)
	if err != nil {
		return err
	}
	defer cd.disconnect()
	var r struct {
		Targets []struct {
			TargetID, Type, URL string
		} `json:"targetInfos"`
	}
	if err := cd.call(ctx, "", "Target.getTargets", map[string]any{}, &r); err != nil {
		return err
	}
	stale := time.Since(c.opened) > 6*time.Hour
	found := false
	var old []string // about:blank from startup, duplicates, or the stale one
	for _, t := range r.Targets {
		if t.Type != "page" {
			continue
		}
		if t.URL == c.url && !found && !stale {
			found = true
			cd.call(ctx, "", "Target.activateTarget", map[string]any{"targetId": t.TargetID}, nil) // the front tab is the visible one
			continue
		}
		old = append(old, t.TargetID)
	}
	// Close them after opening the new one: Chrome with no tabs left may quit.
	defer func() {
		for _, id := range old {
			cd.closeTab(id)
		}
	}()
	if found {
		return nil
	}
	var t struct {
		TargetID string `json:"targetId"`
	}
	if err := cd.call(ctx, "", "Target.createTarget", map[string]any{"url": c.url}, &t); err != nil {
		return err
	}
	cd.call(ctx, "", "Target.activateTarget", map[string]any{"targetId": t.TargetID}, nil)
	if c.opened.IsZero() {
		log.Printf("capture: dashboard open in headless Chrome for camera pictures")
	}
	c.opened = time.Now()
	return nil
}
