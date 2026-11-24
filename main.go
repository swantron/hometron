// hometron: one dashboard for Nest cameras, doorbell and thermostat, talking
// straight to Google's Smart Device Management API. No Google Home app required.
package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

//go:embed web
var webFS embed.FS

func main() {
	homeDir, _ := os.UserHomeDir()
	listen := flag.String("listen", "127.0.0.1:8080", "comma-separated addresses to serve on: the Tailscale IP for phones, plus 127.0.0.1:PORT if `tailscale serve` adds HTTPS in front")
	state := flag.String("state", filepath.Join(homeDir, "nest"), "directory with credentials.json, token.json and events/")
	topic := flag.String("topic", "home-nest-events", "Pub/Sub topic Device Access publishes to")
	sub := flag.String("sub", "hometron", "Pub/Sub subscription to pull from")
	noEvents := flag.Bool("no-events", false, "skip Pub/Sub (devices and live view only)")
	flag.Parse()

	g, err := loadGoogle(*state)
	if err != nil {
		log.Fatal(err)
	}
	sdm := &SDM{g: g}
	events, err := newEvents(sdm, *state)
	if err != nil {
		log.Fatal(err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if devs, err := sdm.Devices(ctx, 0); err != nil {
		log.Fatalf("listing devices: %v", err)
	} else {
		log.Printf("%d devices", len(devs))
	}

	lan := newNetwork(*state)
	home := &Home{Printer: newPrinter(*state, lan), Internet: newInternet(*state, events), Net: newNetWatch(*state, lan, events)}
	lan.OnScan = home.Net.OnScan // set before the first scan
	go lan.Run(ctx, 10*time.Minute)
	go home.Printer.Run(ctx)
	go home.Internet.Run(ctx)
	go home.Net.Run(ctx)

	if home.Weather, err = loadWeather(*state); err != nil {
		log.Fatalf("weather: %v", err)
	}
	var outdoor func() *float64
	if home.Weather != nil {
		go home.Weather.Run(ctx)
		outdoor = home.Weather.OutdoorF
	}
	home.Net.outdoor = outdoor
	nestP := nest{sdm}
	home.History = newHistory(*state, nestP.Devices, outdoor)
	go home.History.Run(ctx, sdm.Invalidate)

	if !*noEvents {
		ps := newPubSub(g, *topic, *sub)
		if err := ps.Ensure(ctx); err != nil {
			log.Fatalf("pubsub setup: %v", err)
		}
		log.Printf("events: pulling %s (Device Access must publish to %s)", ps.sub, ps.topic)
		go ps.Run(ctx, func(b []byte) {
			if isTraitUpdate(b) {
				home.History.Poke() // thermostat changed: record it
			}
			events.Handle(ctx, b)
		})
	}
	providers := []Provider{nestP}
	tw, err := loadTailwind(*state, events)
	if err != nil {
		log.Fatalf("tailwind: %v", err)
	}
	if tw != nil {
		log.Printf("tailwind: polling %s", tw.cfg.Host)
		go tw.Run(ctx)
		providers = append(providers, tw)
	}
	laundry, err := loadLaundry(*state, events)
	if err != nil {
		log.Fatalf("laundry: %v", err)
	}
	if laundry != nil {
		log.Printf("laundry: polling LG ThinQ")
		go laundry.Run(ctx)
		providers = append(providers, laundry)
	}

	if home.SmartThings, err = loadSmartThings(*state, events); err != nil {
		log.Fatalf("smartthings: %v", err)
	}
	if home.SmartThings != nil {
		go home.SmartThings.Run(ctx)
		providers = append(providers, home.SmartThings)
	}
	if home.Fleet, err = loadFleet(*state, home.Internet, events); err != nil {
		log.Fatalf("fleet: %v", err)
	}
	if home.Fleet != nil {
		go home.Fleet.Run(ctx)
	}
	if home.Calendar, err = loadCalendar(*state, events); err != nil {
		log.Fatalf("calendar: %v", err)
	}
	if home.Calendar != nil {
		go home.Calendar.Run(ctx)
	}
	if home.Caster, err = loadCaster(*state, lan); err != nil {
		log.Fatalf("cast: %v", err)
	}
	if home.Caster != nil {
		go func() {
			if err := home.Caster.Serve(ctx); err != nil && ctx.Err() == nil {
				log.Printf("cast: can't serve the chime: %v", err) // dashboard keeps working
			}
		}()
	}

	push, err := loadPush(*state)
	if err != nil {
		log.Fatalf("push: %v", err)
	}
	events.OnAlert = func(ev Event) {
		if home.Caster != nil {
			go home.Caster.OnEvent(ev)
		}
		push.Notify(ev)
	}

	go func() {
		for t := time.NewTicker(time.Hour); ; <-t.C {
			events.Prune(14 * 24 * time.Hour)
		}
	}()

	if cap := newCapture(*state, *listen); cap != nil {
		go cap.Run(ctx)
	}

	mediaDir := filepath.Join(*state, "events")
	srv := &http.Server{Handler: routes(providers, events, lan, newSnapshots(mediaDir), push, home, mediaDir)}
	go func() {
		<-ctx.Done()
		shut, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shut)
	}()
	errs := make(chan error, 1)
	for _, addr := range strings.Split(*listen, ",") {
		ln, err := net.Listen("tcp", strings.TrimSpace(addr))
		if err != nil {
			log.Fatal(err) // launchd restarts us; e.g. the Tailscale IP isn't up yet at boot
		}
		log.Printf("serving on http://%s", ln.Addr())
		go func() { errs <- srv.Serve(ln) }()
	}
	if err := <-errs; err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

func routes(providers []Provider, events *Events, lan *Network, snaps *Snapshots, push *Push, home *Home, mediaDir string) http.Handler {
	mux := http.NewServeMux()
	home.routes(mux)

	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		b, _ := webFS.ReadFile("web/index.html")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.Write(b)
	})

	static := func(path, ctype string, body func() []byte, cache string) {
		mux.HandleFunc("GET "+path, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", ctype)
			w.Header().Set("Cache-Control", cache)
			w.Write(body())
		})
	}
	embedded := func(name string) func() []byte {
		return func() []byte { b, _ := webFS.ReadFile("web/" + name); return b }
	}
	static("/sw.js", "text/javascript; charset=utf-8", embedded("sw.js"), "no-cache") // browsers re-check it anyway
	static("/manifest.webmanifest", "application/manifest+json", embedded("manifest.webmanifest"), "no-cache")
	for path, size := range map[string]int{"/icon-192.png": 192, "/icon-512.png": 512, "/apple-touch-icon.png": 180} {
		static(path, "image/png", func() []byte { return iconPNG(size) }, "public, max-age=604800")
	}

	mux.HandleFunc("GET /api/push/key", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]string{"publicKey": push.PublicKey()})
	})
	mux.HandleFunc("POST /api/push/subscribe", func(w http.ResponseWriter, r *http.Request) {
		var s PushSub
		if !sameOrigin(r) || json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&s) != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		s.Label = shortUA(r.UserAgent())
		if err := push.Subscribe(s); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, map[string]any{"ok": true, "kinds": s.Kinds})
	})
	mux.HandleFunc("POST /api/push/unsubscribe", func(w http.ResponseWriter, r *http.Request) {
		var s struct{ Endpoint string }
		if !sameOrigin(r) || json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&s) != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		push.Unsubscribe(s.Endpoint)
		writeJSON(w, map[string]bool{"ok": true})
	})
	mux.HandleFunc("POST /api/push/test", func(w http.ResponseWriter, r *http.Request) {
		var s struct{ Endpoint string }
		if !sameOrigin(r) || json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&s) != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		sub, ok := push.Get(s.Endpoint)
		if !ok {
			http.Error(w, "not subscribed", http.StatusNotFound)
			return
		}
		err := push.Send(r.Context(), sub, PushMessage{Title: "hometron", Body: "Alerts are working on this device.", Tag: "test", URL: "/", Time: time.Now().UnixMilli()})
		if err != nil {
			httpErr(w, err)
			return
		}
		writeJSON(w, map[string]bool{"ok": true})
	})

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok\n")) })

	mux.HandleFunc("GET /api/devices", func(w http.ResponseWriter, r *http.Request) {
		out := []deviceView{}
		for i, p := range providers {
			devs, err := p.Devices(r.Context())
			if err != nil {
				if i == 0 { // Nest is the core; without it there's nothing to show
					httpErr(w, err)
					return
				}
				log.Printf("devices: %v", err)
				continue
			}
			out = append(out, devs...)
		}
		writeJSON(w, out)
	})

	mux.HandleFunc("POST /api/devices/{id}/command", func(w http.ResponseWriter, r *http.Request) {
		if !sameOrigin(r) {
			http.Error(w, "cross-origin request refused", http.StatusForbidden)
			return
		}
		var req struct {
			Command string         `json:"command"`
			Params  map[string]any `json:"params"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		if req.Params == nil {
			req.Params = map[string]any{}
		}
		id := r.PathValue("id")
		var owner Provider
		// Most specific first: Nest's Owns would otherwise hit the API for foreign ids.
		for i := len(providers) - 1; i >= 0; i-- {
			if providers[i].Owns(r.Context(), id) {
				owner = providers[i]
				break
			}
		}
		if owner == nil {
			http.NotFound(w, r)
			return
		}
		res, err := owner.Command(r.Context(), id, req.Command, req.Params)
		if errors.Is(err, errNotAllowed) {
			http.Error(w, err.Error(), http.StatusForbidden)
			return
		}
		if err != nil {
			httpErr(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if len(res) == 0 {
			res = json.RawMessage("{}")
		}
		w.Write(res)
	})

	mux.HandleFunc("GET /api/snapshots", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, snaps.All())
	})

	// The browser uploads a frame grabbed from a live stream. A cross-site page can't send
	// image/jpeg without a CORS preflight, which we never answer, so the type check plus
	// Origin check keeps this same-origin like the command route.
	mux.HandleFunc("POST /api/devices/{id}/snapshot", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Content-Type") != "image/jpeg" || !originOK(r) {
			http.Error(w, "cross-origin request refused", http.StatusForbidden)
			return
		}
		id := r.PathValue("id")
		if !providers[0].Owns(r.Context(), id) {
			http.NotFound(w, r)
			return
		}
		b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxSnapshot))
		if err != nil || !isJPEG(b) {
			http.Error(w, "expected a JPEG under 3 MB", http.StatusBadRequest)
			return
		}
		now := time.Now()
		// ?event= also makes this frame the picture for a person event that came without one.
		if evID := r.URL.Query().Get("event"); evID != "" {
			if _, err := events.Attach(evID, id, b, now); err != nil {
				http.Error(w, "that event can't take a picture (wrong camera, too old, or already has one)", http.StatusConflict)
				return
			}
		}
		snap, err := snaps.Save(id, b, now)
		if err != nil {
			httpErr(w, err)
			return
		}
		writeJSON(w, snap)
	})

	mux.HandleFunc("GET /api/network", func(w http.ResponseWriter, r *http.Request) {
		s := lan.Last()
		if s.At.IsZero() {
			s = lan.Scan(r.Context())
		}
		writeJSON(w, s)
	})

	mux.HandleFunc("POST /api/network/scan", func(w http.ResponseWriter, r *http.Request) {
		if !sameOrigin(r) {
			http.Error(w, "cross-origin request refused", http.StatusForbidden)
			return
		}
		writeJSON(w, lan.Scan(r.Context()))
	})

	mux.HandleFunc("GET /api/events", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, events.Recent())
	})

	mux.HandleFunc("GET /api/events/stream", func(w http.ResponseWriter, r *http.Request) {
		fl, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		ch, done := events.Subscribe()
		defer done()
		ping := time.NewTicker(25 * time.Second)
		defer ping.Stop()
		fmt.Fprint(w, ": hello\n\n")
		fl.Flush()
		for {
			select {
			case <-r.Context().Done():
				return
			case ev := <-ch:
				b, _ := json.Marshal(ev)
				fmt.Fprintf(w, "data: %s\n\n", b)
				fl.Flush()
			case <-ping.C:
				fmt.Fprint(w, ": ping\n\n")
				fl.Flush()
			}
		}
	})

	mux.HandleFunc("GET /media/{name}", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if name != filepath.Base(name) || strings.HasPrefix(name, ".") || strings.HasSuffix(name, ".json") || strings.HasSuffix(name, ".jsonl") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Cache-Control", "private, max-age=86400")
		http.ServeFile(w, r, filepath.Join(mediaDir, name))
	})

	return logRequests(mux)
}

// deviceView is the trimmed device shape the UI needs.
type deviceView struct {
	ID    string `json:"id"`
	Kind  string `json:"kind"`
	Label string `json:"label"`
	Room  string `json:"room"`
	Live  bool   `json:"live"`

	// garage only
	Door     string `json:"door,omitempty"`     // open | closed | unknown
	Problem  string `json:"problem,omitempty"`  // why it can't be operated right now
	NoSensor bool   `json:"noSensor,omitempty"` // door position unknowable; show a camera instead
	Camera   string `json:"camera,omitempty"`   // label of the camera that watches it

	// laundry
	Run       string `json:"run,omitempty"`       // "Washing", "Done", "Off"
	RemainMin *int   `json:"remainMin,omitempty"` // minutes left in the cycle
	Active    bool   `json:"active,omitempty"`    // a load is in progress

	// fridge
	Parts    []FridgePart `json:"parts,omitempty"`
	FilterPc *int         `json:"filterPc,omitempty"` // water filter used, percent
	Ice      string       `json:"ice,omitempty"`      // ice makers, summarized
	KWhMonth *float64     `json:"kwhMonth,omitempty"` // energy used this month
	KWhLast  *float64     `json:"kwhLast,omitempty"`
	Notes    []string     `json:"notes,omitempty"`    // defrosting, Power Freeze on, ...
	SignInBy *time.Time   `json:"signInBy,omitempty"` // when the current token runs out (sign in before then)

	// thermostat (and garage)
	Online   bool     `json:"online,omitempty"`
	TempF    *float64 `json:"tempF,omitempty"`
	Humidity *float64 `json:"humidity,omitempty"`
	Mode     string   `json:"mode,omitempty"`
	Modes    []string `json:"modes,omitempty"`
	Hvac     string   `json:"hvac,omitempty"`
	Eco      string   `json:"eco,omitempty"`
	HeatF    *float64 `json:"heatF,omitempty"`
	CoolF    *float64 `json:"coolF,omitempty"`
}

func viewOf(d Device) deviceView {
	v := deviceView{ID: d.ID(), Kind: d.Kind(), Label: d.Label(), Live: d.Has("CameraLiveStream")}
	if len(d.ParentRelations) > 0 {
		v.Room = d.ParentRelations[0].DisplayName
	}
	if v.Kind != "thermostat" {
		return v
	}
	var (
		conn struct{ Status string }
		temp struct {
			AmbientTemperatureCelsius *float64 `json:"ambientTemperatureCelsius"`
		}
		hum struct {
			AmbientHumidityPercent *float64 `json:"ambientHumidityPercent"`
		}
		mode struct {
			Mode           string   `json:"mode"`
			AvailableModes []string `json:"availableModes"`
		}
		hvac struct{ Status string }
		eco  struct{ Mode string }
		sp   struct {
			HeatCelsius *float64 `json:"heatCelsius"`
			CoolCelsius *float64 `json:"coolCelsius"`
		}
	)
	t := func(name string, v any) { _ = json.Unmarshal(d.Traits["sdm.devices.traits."+name], v) }
	t("Connectivity", &conn)
	t("Temperature", &temp)
	t("Humidity", &hum)
	t("ThermostatMode", &mode)
	t("ThermostatHvac", &hvac)
	t("ThermostatEco", &eco)
	t("ThermostatTemperatureSetpoint", &sp)
	v.Online = conn.Status == "ONLINE"
	v.TempF, v.Humidity = toF(temp.AmbientTemperatureCelsius), hum.AmbientHumidityPercent
	v.Mode, v.Modes, v.Hvac, v.Eco = mode.Mode, mode.AvailableModes, hvac.Status, eco.Mode
	v.HeatF, v.CoolF = toF(sp.HeatCelsius), toF(sp.CoolCelsius)
	return v
}

func toF(c *float64) *float64 {
	if c == nil {
		return nil
	}
	f := math.Round((*c*9/5+32)*10) / 10
	return &f
}

// sameOrigin blocks other websites from driving the thermostat through the user's browser.
func sameOrigin(r *http.Request) bool {
	return r.Header.Get("Content-Type") == "application/json" && originOK(r)
}

func originOK(r *http.Request) bool {
	o := r.Header.Get("Origin")
	host := r.Host
	// Behind `tailscale serve` (which connects from loopback) the browser's origin is the
	// https://<machine>.ts.net name; trust the proxy's forwarded host only from loopback.
	if fh := r.Header.Get("X-Forwarded-Host"); fh != "" && isLoopback(r.RemoteAddr) {
		host = fh
	}
	if o == "" {
		return true // non-browser client (curl)
	}
	u, err := url.Parse(o)
	return err == nil && u.Host == host
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func httpErr(w http.ResponseWriter, err error) {
	log.Printf("error: %v", err)
	status := http.StatusBadGateway
	if ae, ok := err.(*APIError); ok && ae.Status/100 == 4 {
		status = ae.Status
	}
	http.Error(w, err.Error(), status)
}

func logRequests(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			log.Printf("%s %s", r.Method, r.URL.Path)
		}
		h.ServeHTTP(w, r)
	})
}

func isLoopback(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	ip := net.ParseIP(host)
	return err == nil && ip != nil && ip.IsLoopback()
}

// shortUA labels a push subscription so the log says "Android Chrome", not a 200-char UA.
func shortUA(ua string) string {
	platform, browser := "", ""
	switch {
	case strings.Contains(ua, "Android"):
		platform = "Android"
	case strings.Contains(ua, "iPhone"), strings.Contains(ua, "iPad"):
		platform = "iOS"
	case strings.Contains(ua, "Mac OS X"):
		platform = "Mac"
	case strings.Contains(ua, "Windows"):
		platform = "Windows"
	}
	switch {
	case strings.Contains(ua, "Firefox/"):
		browser = "Firefox"
	case strings.Contains(ua, "Edg/"):
		browser = "Edge"
	case strings.Contains(ua, "Chrome/"):
		browser = "Chrome"
	case strings.Contains(ua, "Safari/"):
		browser = "Safari"
	}
	return strings.TrimSpace(platform + " " + browser)
}
