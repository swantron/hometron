package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestParsePing(t *testing.T) {
	ok := `--- 1.1.1.1 ping statistics ---
5 packets transmitted, 4 packets received, 20.0% packet loss
round-trip min/avg/max/stddev = 21.112/27.594/40.381/7.522 ms`
	if h := parsePing(ok); h.Loss != 20 || h.Ms != 27.6 || h.Jitter != 7.5 {
		t.Errorf("ok = %+v", h)
	}
	dead := "5 packets transmitted, 0 packets received, 100.0% packet loss\n"
	if h := parsePing(dead); h.ok() || h.Ms != 0 {
		t.Errorf("dead = %+v", h)
	}
	if h := parsePing("ping: cannot resolve x: Unknown host"); h.ok() {
		t.Errorf("garbage counted as reachable: %+v", h)
	}
}

func TestDiagnose(t *testing.T) {
	good, bad := &Hop{Ms: 5}, &Hop{Loss: 100}
	cases := []struct {
		s    NetSample
		ok   bool
		want string
	}{
		{NetSample{Wifi: good, Router: good, ISP: good, Internet: good, DNSms: 12}, true, "All good"},
		{NetSample{Wifi: bad, Router: bad, Internet: bad}, false, "Google Wifi from the server"},
		{NetSample{Wifi: good, Router: bad, ISP: bad, Internet: bad}, false, "router it plugs into"},
		{NetSample{Wifi: good, Router: good, ISP: bad, Internet: bad}, false, "ISP isn't answering"},
		{NetSample{Wifi: good, Router: good, ISP: good, Internet: bad}, false, "internet beyond"},
		// ISP routers often ignore pings; with the internet fine that's not a problem.
		{NetSample{Wifi: good, Router: good, ISP: bad, Internet: good}, true, "All good"},
		{NetSample{Wifi: good, Internet: good, DNSms: -1}, false, "DNS"},
		{NetSample{Wifi: good, Internet: &Hop{Ms: 30, Loss: 40}}, false, "40%"},
	}
	for i, c := range cases {
		ok, words := Diagnose(c.s)
		if ok != c.ok || !strings.Contains(words, c.want) {
			t.Errorf("%d: %v %q", i, ok, words)
		}
	}
}

func TestUptimeReport(t *testing.T) {
	now := mustTime(t, "2026-11-30 12:00")
	s := InternetState{Up: true, Watching: now.AddDate(0, 0, -10), Outages: []Outage{
		{Start: now.Add(-2 * time.Hour), End: now.Add(-2*time.Hour + 30*time.Minute)},
		{Start: now.AddDate(0, 0, -9), End: now.AddDate(0, 0, -9).Add(90 * time.Minute)},
	}}
	r := uptimeReport(s, now)
	if r.Outages30 != 2 || r.DownMin30 != 120 {
		t.Errorf("30d = %+v", r)
	}
	// 7 days only includes the 30-minute one.
	if want := 100 * (1 - 30.0/(7*24*60)); r.Up7 < want-0.001 || r.Up7 > want+0.001 {
		t.Errorf("up7 = %v, want %v", r.Up7, want)
	}
	// 30 days only counts the 10 days we've been watching.
	if want := 100 * (1 - 120.0/(10*24*60)); r.Up30 < want-0.001 || r.Up30 > want+0.001 {
		t.Errorf("up30 = %v, want %v", r.Up30, want)
	}
}

func TestOnScanDropoutsAndNewcomers(t *testing.T) {
	e := newTestEvents(t)
	w := &NetWatch{events: e, routers: map[string]*RouterInfo{}, devices: map[string]*watched{}}
	t0 := mustTime(t, "2026-11-23 10:00")
	old := t0.Add(-72 * time.Hour)
	printer := NetHost{IP: "10.0.0.150", MAC: "aa:00:00:00:00:01", Vendor: "HP Inc.", FirstSeen: old, LastSeen: t0}
	switchOff := NetHost{IP: "10.0.0.136", MAC: "aa:00:00:00:00:02", Vendor: "Nintendo Co.,Ltd", FirstSeen: old, LastSeen: t0}
	phone := NetHost{IP: "10.0.0.90", MAC: "da:00:00:00:00:03", Private: true, FirstSeen: t0, LastSeen: t0}

	w.OnScan(NetScan{At: t0, Hosts: []NetHost{printer, switchOff, phone}}) // first scan: never announces
	// 30 minutes later the printer and the Switch are gone; a new real device appears.
	newcomer := NetHost{IP: "10.0.0.77", MAC: "aa:00:00:00:00:04", Vendor: "Shelly", FirstSeen: t0.Add(30 * time.Minute), LastSeen: t0.Add(30 * time.Minute)}
	w.OnScan(NetScan{At: t0.Add(30 * time.Minute), Hosts: []NetHost{newcomer}, Gone: []NetHost{printer, switchOff}})
	w.OnScan(NetScan{At: t0.Add(40 * time.Minute), Gone: []NetHost{printer, switchOff}}) // still gone: no repeat
	printer.LastSeen = t0.Add(3 * time.Hour)
	w.OnScan(NetScan{At: t0.Add(3 * time.Hour), Hosts: []NetHost{printer}})

	var got []string
	for _, ev := range e.Recent() {
		got = append(got, ev.Device)
	}
	want := []string{"Printer is back on the network (gone 3h 0m)", "Printer dropped off the network", "New device joined: Shelly (10.0.0.77)"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("feed = %q", got)
	}
}

func TestParseTailscale(t *testing.T) {
	exp := time.Date(2026, 12, 14, 0, 0, 0, 0, time.UTC)
	js := `{"Self":{"HostName":"swantron m2 air","OS":"macOS","Online":true,"LastSeen":"0001-01-01T00:00:00Z","KeyExpiry":"2026-12-14T00:00:00Z"},
	 "Peer":{"k1":{"HostName":"Pixel 9a","OS":"android","Online":false,"LastSeen":"2026-11-24T02:20:00Z","KeyExpiry":"2026-12-14T00:00:00Z"},
	         "k2":{"HostName":"swantron m4 air","OS":"macOS","Online":true}}}`
	nodes, err := parseTailscale([]byte(js))
	if err != nil || len(nodes) != 3 {
		t.Fatalf("%v %v", nodes, err)
	}
	if !nodes[0].Self || nodes[0].KeyExpiry == nil || !nodes[0].KeyExpiry.Equal(exp) || nodes[0].LastSeen != nil {
		t.Errorf("self = %+v", nodes[0])
	}
	if nodes[1].Name != "Pixel 9a" || nodes[1].Online || nodes[1].LastSeen == nil || nodes[2].KeyExpiry != nil {
		t.Errorf("peers = %+v", nodes[1:])
	}

	// Expiry warnings at 14, 7, 3, 1 days, each once.
	e := newTestEvents(t)
	w := &NetWatch{events: e, tsWarned: map[string]int{}, tsStatus: func(context.Context) ([]byte, error) { return []byte(js), nil }}
	for _, d := range []int{20, 13, 12, 6, 6, 2} {
		w.checkTailscale(context.Background(), exp.AddDate(0, 0, -d))
	}
	if n := len(e.Recent()); n != 6 { // two nodes × (14, 7, 3)
		t.Errorf("warnings = %d", n)
	}
}

func TestParseAirport(t *testing.T) {
	js := `{"SPAirPortDataType":[{"spairport_airport_interfaces":[{"_name":"en0",
	  "spairport_current_network_information":{"spairport_network_channel":"11 (2GHz, 20MHz)","spairport_signal_noise":"-71 dBm / -94 dBm","spairport_network_rate":28}}]}]}`
	l, ok := parseAirport([]byte(js), "en0")
	if !ok || l.Band != "2.4 GHz" || l.Signal != -71 || l.Noise != -94 || l.RateMb != 28 {
		t.Errorf("link = %+v %v", l, ok)
	}
	if _, ok := parseAirport([]byte(js), "en3"); ok {
		t.Error("ethernet interface treated as Wi-Fi")
	}
}

func TestParseTraceroute(t *testing.T) {
	out := " 1  192.168.86.1  7.563 ms\n 2  192.168.0.1  4.397 ms\n 3  100.64.0.1  9 ms\n 4  203.0.113.9  27.578 ms\n 5  *\n"
	if got := parseTraceroute(out); got != "203.0.113.9" {
		t.Errorf("got %q", got)
	}
}

func TestRouterRestart(t *testing.T) {
	uptime := int64(5000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"software": map[string]any{"softwareVersion": "14150.883.1264", "updateStatus": "idle"},
			"system":   map[string]any{"groupRole": "root", "uptime": uptime},
			"wan":      map[string]any{"online": true, "gatewayIpAddress": "192.168.0.1"},
		})
	}))
	defer srv.Close()
	e := newTestEvents(t)
	w := &NetWatch{events: e, lan: newNetwork(t.TempDir()), http: srv.Client(), routers: map[string]*RouterInfo{}, sweepAt: time.Now()}
	host := strings.TrimPrefix(srv.URL, "http://") // the fake "gateway" is host:port
	now := time.Now()
	w.checkRouters(context.Background(), host, now)
	uptime = 60 // it rebooted
	w.checkRouters(context.Background(), host, now.Add(5*time.Minute))
	v := w.View(false)
	if len(v.Routers) != 1 || !v.Routers[0].Root || v.Routers[0].Name != "Main Wifi" || w.routers[host].upstream != "192.168.0.1" {
		t.Fatalf("routers = %+v", v.Routers)
	}
	if r := e.Recent(); len(r) != 1 || r[0].Device != "Main Wifi restarted" {
		t.Errorf("feed = %+v", r)
	}
}

func TestTickUsesHops(t *testing.T) {
	w := &NetWatch{path: t.TempDir() + "/n.jsonl", lan: newNetwork(t.TempDir()), routers: map[string]*RouterInfo{}, devices: map[string]*watched{},
		tsWarned: map[string]int{}, http: &http.Client{Timeout: time.Millisecond},
		ispHop: "203.0.113.1", ispAt: time.Now(), tailAt: time.Now(), sweepAt: time.Now(), routerAt: time.Now()}
	w.routers["gw"] = &RouterInfo{IP: "gw", Root: true, upstream: "192.168.0.1"}
	pinged := map[string]bool{}
	var mu = make(chan struct{}, 1)
	w.ping = func(_ context.Context, host string) Hop {
		mu <- struct{}{}
		pinged[host] = true
		<-mu
		if host == "1.1.1.1" {
			return Hop{Loss: 100} // Cloudflare down, Google fine: internet is up
		}
		return Hop{Host: host, Ms: 10}
	}
	w.resolve = func(context.Context) (int, error) { return 15, nil }
	w.tick(context.Background(), time.Now())
	v := w.View(true)
	if !v.OK || v.Now.Internet.Host != "8.8.8.8" || !pinged["192.168.0.1"] || !pinged["203.0.113.1"] {
		t.Errorf("view = %+v pinged = %v", v.Now, pinged)
	}
	if got := w.read(time.Now().Add(-time.Hour)); len(got) != 1 {
		t.Errorf("persisted %d samples", len(got))
	}
}

func TestMeshPointDrops(t *testing.T) {
	e := newTestEvents(t)
	cold := 9.0
	uptime := int64(40000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"system": map[string]any{"groupRole": "leaf", "uptime": uptime, "ledAnimation": "CONNECTED"},
			"software": map[string]any{"softwareVersion": "1.56"}})
	}))
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")
	w := &NetWatch{events: e, http: srv.Client(), routers: map[string]*RouterInfo{host: {Name: "Garage Wifi", IP: host, UptimeSec: uptime}},
		meshDown: map[string]*meshState{}, outdoor: func() *float64 { return &cold }, meshFrom: time.Now().Add(-48 * time.Hour)}
	up := Hop{Name: "Garage Wifi", Host: host, Ms: 12}
	down := Hop{Name: "Garage Wifi", Host: host, Loss: 100}
	t0 := time.Now().Add(-30 * time.Minute)
	seq := []Hop{up, down, up, down, down, down, up} // a one-minute blip, then a 3-minute drop
	for i, h := range seq {
		if i == 6 {
			uptime = 90 // it restarted while gone
		}
		ts := t0.Add(time.Duration(i) * time.Minute)
		s := NetSample{T: ts, Points: []Hop{h}}
		w.samples = append(w.samples, s)
		w.trackPoints(context.Background(), s, ts)
	}
	var got []string
	for _, ev := range e.Recent() {
		got = append(got, ev.Device)
	}
	want := "Garage Wifi restarted and is back (gone 3m)|Garage Wifi dropped off the mesh"
	if strings.Join(got, "|") != want {
		t.Errorf("feed = %q", got)
	}
	if len(w.meshLog) != 1 || !w.meshLog[0].Restarted || w.meshLog[0].OutF == nil || *w.meshLog[0].OutF != 9 {
		t.Errorf("log = %+v", w.meshLog)
	}
	if ok, words := Diagnose(NetSample{Wifi: &Hop{Ms: 5}, Internet: &Hop{Ms: 20}, Points: []Hop{down}}); ok || !strings.Contains(words, "Garage Wifi isn't reachable") {
		t.Errorf("diagnose = %v %q", ok, words)
	}
	v := w.View(false)
	u := v.Routers[0]
	if u.Avail24 == nil || *u.Avail24 < 42.8 || *u.Avail24 > 42.9 || u.Drops7 != 1 || u.Avail7 == nil || *u.Avail7 > 99.99 {
		t.Errorf("unit view = %+v avail24=%v avail7=%v", u, deref(u.Avail24), deref(u.Avail7))
	}
}

func deref(f *float64) float64 {
	if f == nil {
		return -1
	}
	return *f
}
