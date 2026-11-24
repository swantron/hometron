package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"time"
)

// Home holds the "house at a glance" services. Any of them may be nil when not set up.
type Home struct {
	Caster   *Caster
	History  *History
	Weather  *Weather
	Calendar *Calendar
	Printer  *Printer
	Internet *Internet
	Net      *NetWatch

	SmartThings *SmartThings
	Fleet       *Fleet
}

type homeView struct {
	Weather    *WeatherView   `json:"weather,omitempty"`
	Thermostat *thermoHistory `json:"thermostat,omitempty"`
	Calendar   *calendarView  `json:"calendar,omitempty"`
	Printer    *PrinterView   `json:"printer,omitempty"`
	Internet   *InternetState `json:"internet,omitempty"`
	Net        *NetHealthView `json:"net,omitempty"`
	Chime      bool           `json:"chime,omitempty"` // speakers ring for the doorbell
	Setup      []string       `json:"setup,omitempty"` // what isn't configured yet
}

type thermoHistory struct {
	Samples       []Sample `json:"samples"`
	HeatToday     int      `json:"heatTodayMin"`
	CoolToday     int      `json:"coolTodayMin"`
	HeatYesterday int      `json:"heatYesterdayMin"`
	CoolYesterday int      `json:"coolYesterdayMin"`
}

type calendarView struct {
	Events []CalEvent `json:"events"`
	Errors []string   `json:"errors,omitempty"`
}

func (h *Home) view(hours int) homeView {
	var v homeView
	if h == nil {
		return v
	}
	if h.Weather != nil {
		w := h.Weather.View()
		v.Weather = &w
	} else {
		v.Setup = append(v.Setup, "weather")
	}
	if h.History != nil {
		now := time.Now()
		today := dateOnly(now)
		yesterday := today.AddDate(0, 0, -1)
		// Read from before yesterday so the first sample's state carries into the window.
		all := h.History.Read(yesterday.Add(-6 * time.Hour))
		th := &thermoHistory{Samples: []Sample{}}
		cut := now.Add(-time.Duration(hours) * time.Hour)
		for _, s := range all {
			if !s.T.Before(cut) {
				th.Samples = append(th.Samples, s)
			}
		}
		ht, ct := Runtime(all, nil, today, now)
		hy, cy := Runtime(all, nil, yesterday, today)
		th.HeatToday, th.CoolToday = int(ht.Minutes()), int(ct.Minutes())
		th.HeatYesterday, th.CoolYesterday = int(hy.Minutes()), int(cy.Minutes())
		v.Thermostat = th
	}
	if h.Calendar != nil {
		evs, errs := h.Calendar.Upcoming()
		v.Calendar = &calendarView{Events: evs, Errors: errs}
	} else {
		v.Setup = append(v.Setup, "calendar")
	}
	if h.Printer != nil {
		v.Printer = h.Printer.View()
	}
	if h.Internet != nil {
		s := h.Internet.View()
		v.Internet = &s
	}
	if h.Net != nil {
		n := h.Net.View(false)
		v.Net = &n
	}
	if h.Caster != nil {
		v.Chime = true
	} else {
		v.Setup = append(v.Setup, "chime")
	}
	return v
}

func (h *Home) routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/home", func(w http.ResponseWriter, r *http.Request) {
		hours, _ := strconv.Atoi(r.URL.Query().Get("hours"))
		if hours <= 0 || hours > 24*7 {
			hours = 24
		}
		writeJSON(w, h.view(hours))
	})

	// A speed test run in the browser (the server's own Wi-Fi is too weak to measure the
	// internet), reported here so the dashboard keeps a history.
	mux.HandleFunc("POST /api/internet/speed", func(w http.ResponseWriter, r *http.Request) {
		var sp Speed
		if !sameOrigin(r) || json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&sp) != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if h == nil || h.Internet == nil {
			http.NotFound(w, r)
			return
		}
		if sp.Down <= 0 || sp.Down > 100000 || sp.Up < 0 || sp.Up > 100000 || sp.PingMs < 0 || sp.PingMs > 10000 {
			http.Error(w, "speeds out of range", http.StatusBadRequest)
			return
		}
		if len(sp.Where) > 60 {
			sp.Where = sp.Where[:60]
		}
		sp.At, sp.By = time.Now(), shortUA(r.UserAgent())
		h.Internet.RecordSpeed(sp)
		writeJSON(w, sp)
	})

	// SmartThings sign-in (OAuth app setups only). The redirect URI must match one
	// registered with the app; it defaults to this server's own callback address.
	stRedirect := func(r *http.Request) string {
		if h.SmartThings.cfg.RedirectURI != "" {
			return h.SmartThings.cfg.RedirectURI
		}
		scheme, host := "http", r.Host
		if isLoopback(r.RemoteAddr) && r.Header.Get("X-Forwarded-Host") != "" {
			scheme, host = "https", r.Header.Get("X-Forwarded-Host")
		}
		return scheme + "://" + host + "/smartthings/callback"
	}
	mux.HandleFunc("GET /smartthings/connect", func(w http.ResponseWriter, r *http.Request) {
		if h == nil || h.SmartThings == nil {
			http.Error(w, "SmartThings isn't set up (smartthings.json)", http.StatusNotFound)
			return
		}
		u, err := h.SmartThings.ConnectURL(stRedirect(r))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		http.Redirect(w, r, u, http.StatusFound)
	})
	mux.HandleFunc("GET /smartthings/callback", func(w http.ResponseWriter, r *http.Request) {
		if h == nil || h.SmartThings == nil {
			http.NotFound(w, r)
			return
		}
		q := r.URL.Query()
		if e := q.Get("error"); e != "" {
			http.Error(w, "SmartThings sign-in was cancelled: "+e, http.StatusBadRequest)
			return
		}
		if err := h.SmartThings.Finish(r.Context(), q.Get("code"), q.Get("state"), stRedirect(r)); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		go h.SmartThings.poll(context.Background(), time.Now())
		http.Redirect(w, r, "/", http.StatusFound)
	})

	// Opens the Chrome sign-in window on the server's own screen (for Screen Sharing).
	mux.HandleFunc("POST /api/smartthings/signin", func(w http.ResponseWriter, r *http.Request) {
		if !sameOrigin(r) {
			http.Error(w, "cross-origin request refused", http.StatusForbidden)
			return
		}
		if h == nil || h.SmartThings == nil {
			http.NotFound(w, r)
			return
		}
		if err := h.SmartThings.SignIn(); err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		writeJSON(w, map[string]bool{"ok": true})
	})

	mux.HandleFunc("GET /api/fleet", func(w http.ResponseWriter, r *http.Request) {
		if h == nil || h.Fleet == nil {
			writeJSON(w, map[string]any{"setup": true})
			return
		}
		writeJSON(w, h.Fleet.View())
	})
	mux.HandleFunc("POST /api/fleet/refresh", func(w http.ResponseWriter, r *http.Request) {
		if !sameOrigin(r) || h == nil || h.Fleet == nil {
			http.Error(w, "no", http.StatusBadRequest)
			return
		}
		h.Fleet.Refresh()
		writeJSON(w, map[string]bool{"ok": true})
	})

	mux.HandleFunc("GET /api/network/health", func(w http.ResponseWriter, r *http.Request) {
		out := struct {
			Net      *NetHealthView `json:"net,omitempty"`
			Internet *InternetState `json:"internet,omitempty"`
		}{}
		if h != nil && h.Net != nil {
			v := h.Net.View(true)
			out.Net = &v
		}
		if h != nil && h.Internet != nil {
			s := h.Internet.View()
			out.Internet = &s
		}
		writeJSON(w, out)
	})

	// Plays the doorbell chime on the configured speakers, to check volume and placement.
	mux.HandleFunc("POST /api/chime/test", func(w http.ResponseWriter, r *http.Request) {
		if !sameOrigin(r) {
			http.Error(w, "cross-origin request refused", http.StatusForbidden)
			return
		}
		if h == nil || h.Caster == nil {
			http.Error(w, "no speakers set up (cast.json)", http.StatusNotFound)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
		defer cancel()
		if err := h.Caster.Ring(ctx); err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		writeJSON(w, map[string]bool{"ok": true})
	})

	mux.HandleFunc("GET /api/chime/speakers", func(w http.ResponseWriter, r *http.Request) {
		if h == nil || h.Caster == nil {
			writeJSON(w, map[string]string{})
			return
		}
		writeJSON(w, h.Caster.Speakers(r.Context()))
	})
}
