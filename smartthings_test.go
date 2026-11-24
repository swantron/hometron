package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeST is a SmartThings API with one Samsung fridge whose doors and temps tests can set,
// and single-use refresh tokens like the real one.
type fakeST struct {
	mu          sync.Mutex
	coolerOpen  bool
	freezerF    float64
	validAccess map[string]bool
	validRefr   map[string]bool
	n           int
	refreshes   int
}

func (f *fakeST) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.URL.Path == "/oauth/token" {
		id, secret, _ := r.BasicAuth()
		r.ParseForm()
		if id != "cid" || secret != "sec" {
			w.WriteHeader(401)
			return
		}
		switch r.Form.Get("grant_type") {
		case "refresh_token":
			if !f.validRefr[r.Form.Get("refresh_token")] {
				w.WriteHeader(400)
				return
			}
			delete(f.validRefr, r.Form.Get("refresh_token")) // single use
			f.refreshes++
		case "authorization_code":
			if r.Form.Get("code") != "good-code" || r.Form.Get("redirect_uri") != "https://x/smartthings/callback" {
				w.WriteHeader(400)
				return
			}
		}
		f.n++
		a, rf := fmt.Sprintf("acc%d", f.n), fmt.Sprintf("ref%d", f.n)
		f.validAccess[a], f.validRefr[rf] = true, true
		json.NewEncoder(w).Encode(map[string]any{"access_token": a, "refresh_token": rf, "expires_in": 86399})
		return
	}
	if !f.validAccess[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")] {
		w.WriteHeader(401)
		return
	}
	switch r.URL.Path {
	case "/devices":
		w.Write([]byte(`{"items":[
		 {"deviceId":"tv1","label":"Living room TV","components":[{"id":"main","capabilities":[{"id":"switch"}]}]},
		 {"deviceId":"fr1","label":"Refrigerator","ocf":{"ocfDeviceType":"oic.d.refrigerator"},
		  "components":[{"id":"main","capabilities":[{"id":"refrigeration"}]}]}]}`))
	case "/devices/fr1/status":
		door := "closed"
		if f.coolerOpen {
			door = "open"
		}
		fmt.Fprintf(w, `{"components":{
		 "main":{"contactSensor":{"contact":{"value":%q}},"samsungce.energyManagement":{"thisMonthEnergyConsumption":{"value":37828},"lastMonthEnergyConsumption":{"value":51200}},"refrigeration":{"defrost":{"value":"on"}},"custom.waterFilter":{"waterFilterUsage":{"value":68,"unit":"%%"},"waterFilterStatus":{"value":"normal"}}},
		 "cooler":{"contactSensor":{"contact":{"value":%q}},"temperatureMeasurement":{"temperature":{"value":37,"unit":"F"}},"thermostatCoolingSetpoint":{"coolingSetpoint":{"value":37,"unit":"F"}}},
		 "freezer":{"contactSensor":{"contact":{"value":"closed"}},"temperatureMeasurement":{"temperature":{"value":%g,"unit":"F"}}},
		 "cvroom":{"contactSensor":{"contact":{"value":"closed"}},"custom.fridgeMode":{"fridgeMode":{"value":"CV_FDR_DELI"}}},
		 "icemaker":{"switch":{"switch":{"value":"on"}},"samsungce.fridgeIcemakerInfo":{"name":{"value":"CUBED_ICE"}},"samsungce.fridgeIcemakerOperatingState":{"operatingState":{"value":"running"}}},
		 "icemaker-02":{"switch":{"switch":{"value":"on"}},"samsungce.fridgeIcemakerInfo":{"name":{"value":"ICE_BITES"}},"samsungce.fridgeIcemakerOperatingState":{"operatingState":{"value":"ready"}}}}}`, door, door, f.freezerF)
	default:
		w.WriteHeader(404)
	}
}

func newFakeST(t *testing.T, cfg stConfig) (*SmartThings, *fakeST, *Events) {
	f := &fakeST{freezerF: 0, validAccess: map[string]bool{}, validRefr: map[string]bool{}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	e := newTestEvents(t)
	st := &SmartThings{cfg: cfg, tokenPath: t.TempDir() + "/tok.json", api: srv.URL + "/", http: srv.Client(), events: e, fridges: map[string]*fridge{}}
	return st, f, e
}

func TestSmartThingsOAuthAndRotation(t *testing.T) {
	st, f, _ := newFakeST(t, stConfig{ClientID: "cid", ClientSecret: "sec", RedirectURI: "https://x/smartthings/callback"})
	if st.Connected() {
		t.Fatal("connected before sign-in")
	}
	if err := st.Finish(context.Background(), "good-code", "forged", "https://x/smartthings/callback"); err == nil {
		t.Fatal("accepted a callback without a matching state")
	}
	u, _ := st.ConnectURL("https://x/smartthings/callback")
	state := strings.Split(strings.Split(u, "state=")[1], "&")[0]
	if !strings.Contains(u, "scope=r%3Adevices%3A%2A") {
		t.Errorf("connect url = %s", u)
	}
	if err := st.Finish(context.Background(), "good-code", state, "https://x/smartthings/callback"); err != nil {
		t.Fatal(err)
	}
	st.poll(context.Background(), time.Now())
	if len(st.fridges) != 1 {
		t.Fatalf("fridges = %v (TV must be skipped)", st.fridges)
	}
	// The server revokes our access token early: one refresh, retry, carry on.
	f.mu.Lock()
	f.validAccess = map[string]bool{}
	f.mu.Unlock()
	st.poll(context.Background(), time.Now())
	if st.err != "" || f.refreshes != 1 {
		t.Fatalf("err=%q refreshes=%d", st.err, f.refreshes)
	}
	// The rotated refresh token was saved, so a restart can still refresh.
	var saved stTokens
	b, _ := os.ReadFile(st.tokenPath)
	json.Unmarshal(b, &saved)
	f.mu.Lock()
	ok := f.validRefr[saved.Refresh]
	f.mu.Unlock()
	if !ok {
		t.Errorf("saved refresh token %q isn't the current one", saved.Refresh)
	}
}

func TestFridgeAlerts(t *testing.T) {
	st, f, e := newFakeST(t, stConfig{Token: "pat"})
	f.validAccess["pat"] = true
	t0 := time.Now()
	poll := func(d time.Duration) { st.poll(context.Background(), t0.Add(d)) }
	poll(0)
	f.coolerOpen = true
	poll(30 * time.Second)
	poll(90 * time.Second) // open 1m: nothing yet
	poll(150 * time.Second)
	poll(180 * time.Second) // already alerted for this opening
	f.coolerOpen = false
	poll(240 * time.Second)
	// Freezer warms up: alert only after half an hour above 15°F.
	f.freezerF = 22
	poll(5 * time.Minute)
	poll(20 * time.Minute)
	poll(36 * time.Minute)
	poll(40 * time.Minute)

	var got []string
	for _, ev := range e.Recent() {
		got = append(got, ev.Device)
	}
	want := "Freezer is warm: 22°F for 31m|Fridge door closed (open 3m)|Fridge door has been open 2m"
	if strings.Join(got, "|") != want {
		t.Errorf("feed = %q", got)
	}
	devs, _ := st.Devices(context.Background())
	if len(devs) != 1 || len(devs[0].Parts) != 3 || devs[0].Parts[0].Name != "Fridge" || *devs[0].Parts[1].TempF != 22 ||
		devs[0].Parts[2].Mode != "Deli" || *devs[0].FilterPc != 68 || devs[0].Ice != "Cubed ice making · Ice Bites full" ||
		*devs[0].KWhMonth != 37.8 || *devs[0].KWhLast != 51.2 || len(devs[0].Notes) != 1 {
		b, _ := json.Marshal(devs)
		t.Errorf("view = %s", b)
	}
}

func TestSmartThingsPATExpired(t *testing.T) {
	st, _, _ := newFakeST(t, stConfig{Token: "old"})
	st.poll(context.Background(), time.Now())
	devs, _ := st.Devices(context.Background())
	if len(devs) != 1 || !strings.Contains(devs[0].Problem, "24 hours") {
		t.Errorf("view = %+v", devs)
	}
}
