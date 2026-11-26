package main

import (
	"strings"
	"testing"
	"time"
)

func TestFleetHostOf(t *testing.T) {
	drop := Resource{Provider: "DigitalOcean", Kind: "droplet", Name: "pds", IP: "203.0.113.5"}
	app := Resource{Provider: "DigitalOcean", Kind: "app", Name: "site", Domains: []string{"example.com"}}
	run := Resource{Provider: "Google Cloud", Project: "shop", Kind: "Cloud Run", Name: "shop", URL: "https://shop-x.a.run.app"}
	res := []Resource{drop, app, run}
	byIP := map[string]Resource{drop.IP: drop}
	byDomain := map[string]Resource{"example.com": app}
	cases := []struct {
		domain, cname string
		ips           []string
		host, target  string
	}{
		{"example.com", "", []string{"172.66.0.96"}, "DigitalOcean app", "site"},
		{"pds.dev", "", []string{"203.0.113.5"}, "DigitalOcean droplet", "pds"},
		{"shop.com", "", []string{"216.239.32.21"}, "Google (Cloud Run / App Engine)", "shop (shop)"},
		{"car.com", "car-db.web.app.", []string{"199.36.158.100"}, "Firebase Hosting", ""},
		{"blog.com", "me.github.io.", []string{"185.199.108.153"}, "GitHub Pages", "me.github.io"},
		{"x.com", "", []string{"76.76.21.21"}, "Vercel", ""},
		{"mail.com", "", nil, "", ""},
	}
	for _, c := range cases {
		h, tg := hostOf(c.domain, c.ips, c.cname, byIP, byDomain, res)
		if h != c.host || tg != c.target {
			t.Errorf("%s: got %q %q, want %q %q", c.domain, h, tg, c.host, c.target)
		}
	}
}

func TestFleetSuggestions(t *testing.T) {
	f := &Fleet{cfg: fleetConfig{QuotaProject: "quota"}, domains: map[string]*DomainInfo{
		"held.com": {Name: "held.com", Expires: "2027-01-01"},
		"mail.com": {Name: "mail.com", Mail: true},
	}}
	usd := func(v float64) *float64 { return &v }
	inv := FleetInventory{
		Projects: []GCPProject{
			{ID: "busy", Billing: "Main", Resources: 3, APIs: 30},
			{ID: "empty", Billing: "Main", Resources: 0, APIs: 25},
			{ID: "gemini-key", Resources: 0, APIs: 21, Notable: []string{"generativelanguage"}},
			{ID: "quota", Resources: 0, APIs: 25}, // the project hometron bills its lookups to
		},
		Resources: []Resource{
			{Provider: "DigitalOcean", Kind: "app", Name: "site", Detail: "web 2× professional-xs", USD: usd(24)},
			{Provider: "DigitalOcean", Kind: "volume", Name: "old", State: "unattached", Detail: "10 GB"},
			{Provider: "Google Cloud", Project: "busy", Kind: "VM", Name: "a", Detail: "e2-micro · us-west1-a", State: "running"},
			{Provider: "Google Cloud", Project: "busy", Kind: "VM", Name: "b", Detail: "e2-micro · us-central1-a", State: "running"},
			{Provider: "Google Cloud", Project: "busy", Kind: "Cloud Run", Name: "idle", Usage: "0 requests/30d"},
			{Provider: "Google Cloud", Project: "busy", Kind: "image repo", Name: "imgs", Detail: "us-central1 · docker · 2.2 GB", Bytes: 2.2e9},
			{Provider: "Google Cloud", Project: "busy", Kind: "image repo", Name: "tidy", Detail: "us-central1 · docker · 2.3 GB · cleanup policy", Bytes: 2.3e9},
			{Provider: "Google Cloud", Project: "busy", Kind: "bucket", Name: "us.artifacts.busy.appspot.com", Detail: "us · 485 MB", Bytes: 485e6, Infra: true},
			{Provider: "Google Cloud", Project: "busy", Kind: "bucket", Name: "busy.appspot.com", Detail: "us", Infra: true},
			{Provider: "Google Cloud", Project: "gemini-key", Kind: "API key", Name: "old key", Detail: "generativelanguage", Usage: "0 requests/30d"},
			{Provider: "Google Cloud", Project: "gemini-key", Kind: "API key", Name: "live key", Detail: "generativelanguage", Usage: "12 requests/30d"},
		},
	}
	var got []string
	for _, s := range f.suggest(inv) {
		got = append(got, s.Level+": "+s.Text)
	}
	all := strings.Join(got, "\n")
	for _, want := range []string{
		"kill: Google Cloud project empty is empty",
		"check: Google Cloud project gemini-key runs nothing, but has generativelanguage turned on",
		"trim: site app costs $24/mo",
		"kill: volume old (10 GB) isn't attached",
		"kill: Cloud Run idle (busy) had no requests",
		"trim: Image repo imgs (busy) holds 2.2 GB with no cleanup policy",
		"kill: Bucket us.artifacts.busy.appspot.com (busy) is old Container Registry storage (485 MB)",
		`check: API key "old key" (gemini-key) wasn't used in 30 days`,
		`check: 2 e2-micro VMs on billing account "Main"`,
		"check: held.com has no website and no email",
	} {
		if !strings.Contains(all, want) {
			t.Errorf("missing %q in:\n%s", want, all)
		}
	}
	if strings.Contains(all, "project busy") || strings.Contains(all, "project quota") || strings.Contains(all, "mail.com") ||
		strings.Contains(all, "tidy") || strings.Contains(all, "live key") || strings.Contains(all, "Bucket busy.appspot.com") {
		t.Errorf("flagged something in use:\n%s", all)
	}
}

func TestFleetDiff(t *testing.T) {
	dir := t.TempDir()
	ev, err := newEvents(nil, dir)
	if err != nil {
		t.Fatal(err)
	}
	f := &Fleet{events: ev}
	do := func(name string) Resource { return Resource{Provider: "DigitalOcean", Kind: "droplet", Name: name} }
	old := FleetInventory{At: time.Now(), Resources: []Resource{do("keep"), do("gone")}}
	cur := FleetInventory{At: time.Now(), Resources: []Resource{do("keep"), do("new"),
		{Provider: "Google Cloud", Project: "p", Kind: "VM", Name: "first-time"}}} // a provider just connected
	f.diff(old, cur)
	time.Sleep(50 * time.Millisecond)
	var got []string
	for _, e := range ev.Recent() {
		got = append(got, e.Device)
	}
	all := strings.Join(got, "\n")
	if !strings.Contains(all, "New droplet: new") || !strings.Contains(all, "Gone: droplet gone") {
		t.Errorf("events = %v", got)
	}
	if strings.Contains(all, "first-time") || strings.Contains(all, "keep") {
		t.Errorf("reported a newly connected provider or an unchanged droplet: %v", got)
	}
}

func TestWorkspaceSuggestions(t *testing.T) {
	old := time.Now().AddDate(0, -4, 0).Format("2006-01-02")
	recent := time.Now().AddDate(0, 0, -2).Format("2006-01-02")
	w := &WorkspaceView{PerUser: 8.4, Users: []WSUser{
		{Email: "me@x.com", Admin: true, TwoStep: true, LastLogin: recent},
		{Email: "kid@x.com", TwoStep: false, LastLogin: recent},
		{Email: "boss@x.com", Admin: true, TwoStep: false, LastLogin: recent},
		{Email: "old@x.com", TwoStep: true, LastLogin: old},
		{Email: "new@x.com", TwoStep: true},
		{Email: "gone@x.com", Suspended: true},
	}}
	var got []string
	for _, s := range workspaceSuggestions(w) {
		got = append(got, s.Level+": "+s.Text)
	}
	all := strings.Join(got, "\n")
	for _, want := range []string{
		"check: Workspace user kid@x.com has no 2-step verification",
		"fix: Workspace user boss@x.com has no 2-step verification (and is an admin)",
		"check: Workspace user old@x.com hasn't signed in since " + old,
		"check: Workspace user new@x.com has never signed in",
		"kill: Workspace user gone@x.com is suspended but still a paid seat",
	} {
		if !strings.Contains(all, want) {
			t.Errorf("missing %q in:\n%s", want, all)
		}
	}
	if strings.Contains(all, "me@x.com") || strings.Contains(all, "gone@x.com has no 2-step") {
		t.Errorf("flagged a healthy or suspended account:\n%s", all)
	}
}
