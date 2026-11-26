package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Fleet: what's running out on the internet. A daily-ish inventory of DigitalOcean and
// Google Cloud (read-only), where each domain points, and a check of every site each
// minute from here. The point is to notice what's broken and what's no longer used.
//
// Config is <state>/fleet.json:
//
//	{"domains": [{"name": "example.com", "expires": "2027-11-08", "registrar": "Squarespace"}],
//	 "checks": ["https://status.example.com/health"],      // extra URLs to watch
//	 "fixed": [{"name": "Claude Pro", "usd": 20}],          // subscriptions with no API
//	 "gcpQuotaProject": "my-project"}                       // a project with the Resource Manager + Billing APIs on
//
// Credentials: <state>/digitalocean.json {"token": "..."} (a read-only token) and
// <state>/fleet-token.json (a Google refresh token with cloud-platform.read-only).

type fleetConfig struct {
	Domains []struct {
		Name      string `json:"name"`
		Expires   string `json:"expires"`
		Registrar string `json:"registrar"`
	} `json:"domains"`
	Checks       []string         `json:"checks"`
	Fixed        []FixedCost      `json:"fixed"`
	QuotaProject string           `json:"gcpQuotaProject"`
	Workspace    *workspaceConfig `json:"workspace"`
}

type FixedCost struct {
	Name string  `json:"name"`
	USD  float64 `json:"usd"`
	Note string  `json:"note,omitempty"`
}

type Fleet struct {
	cfg      fleetConfig
	doToken  string
	gcp      *Google // nil without fleet-token.json
	internet *Internet
	events   *Events
	http     *http.Client
	logPath  string
	invPath  string // last inventory, so a restart shows it straight away

	mu      sync.Mutex
	inv     FleetInventory
	sites   map[string]*siteState // by check name
	history []fleetSample         // last 7 days, oldest first
	domains map[string]*DomainInfo
	refresh chan struct{}
}

// ---------- inventory ----------

type Resource struct {
	Provider string   `json:"provider"` // DigitalOcean | Google Cloud
	Project  string   `json:"project,omitempty"`
	Kind     string   `json:"kind"`
	Name     string   `json:"name"`
	Detail   string   `json:"detail,omitempty"`
	State    string   `json:"state,omitempty"`
	URL      string   `json:"url,omitempty"`
	USD      *float64 `json:"usd,omitempty"` // monthly, where the provider says
	Usage    string   `json:"usage,omitempty"`
	Created  string   `json:"created,omitempty"`
	IP       string   `json:"ip,omitempty"`
	Domains  []string `json:"domains,omitempty"`
	Infra    bool     `json:"infra,omitempty"` // created by the platform (staging buckets etc.), not by you
}

type GCPProject struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Created   string   `json:"created"`
	Billing   string   `json:"billing,omitempty"` // billing account name
	Billed    bool     `json:"billed"`
	BillRead  bool     `json:"billingKnown"`      // the billing lookup worked
	Resources int      `json:"resources"`         // things you made (not platform leftovers)
	Notable   []string `json:"notable,omitempty"` // APIs on beyond the defaults (e.g. a Gemini or Docs key in use)
	APIs      int      `json:"apis"`
}

type Suggestion struct {
	Level string `json:"level"` // kill | trim | check
	Text  string `json:"text"`
	Why   string `json:"why,omitempty"`
}

type FleetInventory struct {
	At          time.Time      `json:"at"`
	Resources   []Resource     `json:"resources"`
	Projects    []GCPProject   `json:"projects,omitempty"`
	DOMonth     *float64       `json:"doMonthToDate,omitempty"`
	DOInvoices  []Invoice      `json:"doInvoices,omitempty"`
	Errors      []string       `json:"errors,omitempty"`
	Suggestions []Suggestion   `json:"suggestions,omitempty"`
	Workspace   *WorkspaceView `json:"workspace,omitempty"`
}

type Invoice struct {
	Period string  `json:"period"`
	USD    float64 `json:"usd"`
}

func loadFleet(stateDir string, internet *Internet, events *Events) (*Fleet, error) {
	var cfg fleetConfig
	if err := readJSON(filepath.Join(stateDir, "fleet.json"), &cfg); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	f := &Fleet{cfg: cfg, internet: internet, events: events, http: &http.Client{Timeout: 30 * time.Second},
		logPath: filepath.Join(stateDir, "fleet-checks.jsonl"), invPath: filepath.Join(stateDir, "fleet-inventory.json"), sites: map[string]*siteState{},
		domains: map[string]*DomainInfo{}, refresh: make(chan struct{}, 1)}
	var do struct{ Token string }
	if readJSON(filepath.Join(stateDir, "digitalocean.json"), &do) == nil {
		f.doToken = do.Token
	}
	if g, err := loadGoogleToken(stateDir, "fleet-token.json"); err == nil {
		f.gcp = g
	} else if !errors.Is(err, os.ErrNotExist) {
		log.Printf("fleet: google: %v", err)
	}
	f.loadHistory(time.Now())
	var saved fleetSaved
	if readJSON(f.invPath, &saved) == nil {
		f.inv = saved.Inventory
		for k, v := range saved.Domains {
			f.domains[k] = v
		}
	}
	return f, nil
}

func (f *Fleet) Run(ctx context.Context) {
	go f.runChecks(ctx)
	for {
		f.Inventory(ctx)
		select {
		case <-ctx.Done():
			return
		case <-time.After(6 * time.Hour):
		case <-f.refresh:
		}
	}
}

// Refresh asks for a new inventory now.
func (f *Fleet) Refresh() {
	select {
	case f.refresh <- struct{}{}:
	default:
	}
}

// Inventory lists everything, works out where domains point, and diffs against last time.
func (f *Fleet) Inventory(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	inv := FleetInventory{At: time.Now()}
	var mu sync.Mutex
	var wg sync.WaitGroup
	// A provider that fails outright (expired sign-in, API down) keeps its last good
	// resources below, so a bad morning doesn't empty the page or look like everything was deleted.
	var doFailed, gcpFailed, wsFailed bool
	wg.Add(2)
	go func() {
		defer wg.Done()
		if f.doToken == "" {
			return
		}
		res, month, invs, err := f.digitalOcean(ctx)
		mu.Lock()
		defer mu.Unlock()
		inv.Resources = append(inv.Resources, res...)
		inv.DOMonth, inv.DOInvoices = month, invs
		if err != nil {
			inv.Errors = append(inv.Errors, "DigitalOcean: "+err.Error())
			doFailed = len(res) == 0
		}
	}()
	go func() {
		defer wg.Done()
		if f.gcp == nil {
			return
		}
		ws, wsErr := f.workspace(ctx)
		res, projects, errs := f.googleCloud(ctx)
		mu.Lock()
		defer mu.Unlock()
		inv.Resources = append(inv.Resources, res...)
		inv.Projects = projects
		inv.Workspace = ws
		switch {
		case wsErr == errScope:
			inv.Errors = append(inv.Errors, "Google Workspace: the Fleet sign-in doesn't include the Admin read-only scopes yet")
		case wsErr != nil:
			inv.Errors = append(inv.Errors, "Google Workspace: "+shortErr(wsErr))
		}
		inv.Errors = append(inv.Errors, errs...)
		gcpFailed = len(projects) == 0 && len(errs) > 0
		wsFailed = ws == nil && wsErr != nil && wsErr != errScope
	}()
	wg.Wait()
	f.mu.Lock()
	last := f.inv
	f.mu.Unlock()
	if !last.At.IsZero() {
		stale := " (showing what it found " + last.At.Format("Jan 2 15:04") + ")"
		keep := func(provider string) {
			for _, r := range last.Resources {
				if r.Provider == provider {
					inv.Resources = append(inv.Resources, r)
				}
			}
			for i, e := range inv.Errors {
				if strings.HasPrefix(e, provider) && !strings.HasSuffix(e, ")") {
					inv.Errors[i] = e + stale
				}
			}
		}
		if doFailed {
			keep("DigitalOcean")
			inv.DOMonth, inv.DOInvoices = last.DOMonth, last.DOInvoices
		}
		if gcpFailed {
			keep("Google Cloud")
			inv.Projects = last.Projects
		}
		if wsFailed && last.Workspace != nil {
			inv.Workspace = last.Workspace
		}
	}
	sort.SliceStable(inv.Resources, func(i, j int) bool {
		a, b := inv.Resources[i], inv.Resources[j]
		if a.Provider != b.Provider {
			return a.Provider > b.Provider // Google Cloud, then DigitalOcean
		}
		if a.Project != b.Project {
			return a.Project < b.Project
		}
		return a.Kind+a.Name < b.Kind+b.Name
	})
	f.lookupDomains(ctx, inv.Resources)

	f.mu.Lock()
	old := f.inv
	f.mu.Unlock()
	inv.Suggestions = f.suggest(inv)
	inv.Suggestions = append(inv.Suggestions, workspaceSuggestions(inv.Workspace)...)
	if old.At.IsZero() || (len(inv.Errors) == 0 && len(old.Errors) == 0) {
		f.diff(old, inv)
	}
	f.mu.Lock()
	f.inv = inv
	saved, _ := json.Marshal(fleetSaved{Inventory: inv, Domains: f.domains})
	f.mu.Unlock()
	os.WriteFile(f.invPath, saved, 0o600)
	log.Printf("fleet: %d resources, %d projects, %d suggestions, %d errors", len(inv.Resources), len(inv.Projects), len(inv.Suggestions), len(inv.Errors))
	for _, e := range inv.Errors {
		log.Printf("fleet: %s", e)
	}
}

// diff posts what appeared or vanished since the last inventory (not on the first one).
func (f *Fleet) diff(old, cur FleetInventory) {
	if old.At.IsZero() || f.events == nil {
		return
	}
	key := func(r Resource) string { return r.Provider + "|" + r.Project + "|" + r.Kind + "|" + r.Name }
	// Only compare providers both inventories could read: a newly connected one isn't
	// "all new", and one that failed this time isn't "all gone".
	in := func(inv FleetInventory) map[string]bool {
		m := map[string]bool{}
		for _, r := range inv.Resources {
			m[r.Provider] = true
		}
		return m
	}
	before, after := in(old), in(cur)
	was := map[string]Resource{}
	for _, r := range old.Resources {
		was[key(r)] = r
	}
	now := time.Now()
	for _, r := range cur.Resources {
		if _, ok := was[key(r)]; !ok && !r.Infra && before[r.Provider] {
			f.events.Post(Event{Time: now, DeviceID: "fleet:" + key(r), Device: "New " + r.Kind + ": " + r.Name + where(r), Kind: "fleet"})
		}
		delete(was, key(r))
	}
	for _, r := range was {
		if !r.Infra && after[r.Provider] {
			f.events.Post(Event{Time: now, DeviceID: "fleet:" + key(r), Device: "Gone: " + r.Kind + " " + r.Name + where(r), Kind: "fleet"})
		}
	}
}

func where(r Resource) string {
	if r.Project != "" {
		return " (" + r.Provider + " · " + r.Project + ")"
	}
	return " (" + r.Provider + ")"
}

// ---------- DigitalOcean ----------

func (f *Fleet) doGet(ctx context.Context, path string, out any) error {
	req, _ := http.NewRequestWithContext(ctx, "GET", "https://api.digitalocean.com/v2/"+path, nil)
	req.Header.Set("Authorization", "Bearer "+f.doToken)
	resp, err := f.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("%s: HTTP %d", strings.SplitN(path, "?", 2)[0], resp.StatusCode)
	}
	return json.Unmarshal(b, out)
}

func (f *Fleet) digitalOcean(ctx context.Context) ([]Resource, *float64, []Invoice, error) {
	const P = "DigitalOcean"
	var out []Resource
	var errs []string
	fail := func(err error) {
		if err != nil {
			errs = append(errs, err.Error())
		}
	}
	usd := func(v float64) *float64 { return &v }

	var bal struct {
		MTD string `json:"month_to_date_usage"`
	}
	var month *float64
	if err := f.doGet(ctx, "customers/my/balance", &bal); err == nil {
		if v, err := strconv.ParseFloat(bal.MTD, 64); err == nil {
			month = &v
		}
	} else {
		fail(err)
	}
	var invs struct {
		Invoices []struct {
			Period string `json:"invoice_period"`
			Amount string `json:"amount"`
		} `json:"invoices"`
	}
	var invoices []Invoice
	if err := f.doGet(ctx, "customers/my/invoices?per_page=6", &invs); err == nil {
		for _, i := range invs.Invoices {
			v, _ := strconv.ParseFloat(i.Amount, 64)
			invoices = append(invoices, Invoice{Period: i.Period, USD: v})
		}
	} else {
		fail(err)
	}

	// App Platform: price each component from the instance size list.
	var tiers struct {
		Sizes []struct {
			Slug  string `json:"slug"`
			Month string `json:"usd_per_month"`
		} `json:"instance_sizes"`
	}
	price := map[string]float64{}
	if f.doGet(ctx, "apps/tiers/instance_sizes", &tiers) == nil {
		for _, s := range tiers.Sizes {
			price[s.Slug], _ = strconv.ParseFloat(s.Month, 64)
		}
	}
	var apps struct {
		Apps []struct {
			Spec struct {
				Name     string `json:"name"`
				Services []struct {
					Name  string `json:"name"`
					Size  string `json:"instance_size_slug"`
					Count int    `json:"instance_count"`
				} `json:"services"`
				Workers []struct {
					Name  string `json:"name"`
					Size  string `json:"instance_size_slug"`
					Count int    `json:"instance_count"`
				} `json:"workers"`
				StaticSites []struct {
					Name string `json:"name"`
				} `json:"static_sites"`
				Domains []struct {
					Domain string `json:"domain"`
				} `json:"domains"`
			} `json:"spec"`
			LiveURL          string `json:"live_url"`
			DefaultIngress   string `json:"default_ingress"`
			CreatedAt        string `json:"created_at"`
			LastDeployActive string `json:"last_deployment_active_at"`
			Active           *struct {
				Phase string `json:"phase"`
			} `json:"active_deployment"`
			InProgress *struct {
				Phase string `json:"phase"`
			} `json:"in_progress_deployment"`
		} `json:"apps"`
	}
	if err := f.doGet(ctx, "apps?per_page=100", &apps); err == nil {
		for _, a := range apps.Apps {
			r := Resource{Provider: P, Kind: "app", Name: a.Spec.Name, URL: a.LiveURL, Created: day(a.CreatedAt)}
			var parts []string
			total := 0.0
			for _, s := range append(a.Spec.Services, a.Spec.Workers...) {
				n := max(s.Count, 1)
				parts = append(parts, fmt.Sprintf("%s %d× %s", s.Name, n, s.Size))
				total += price[s.Size] * float64(n)
			}
			for _, s := range a.Spec.StaticSites {
				parts = append(parts, s.Name+" (static)")
			}
			r.Detail = strings.Join(parts, ", ")
			if total > 0 {
				r.USD = usd(total)
			}
			if a.Active != nil {
				r.State = strings.ToLower(a.Active.Phase)
			}
			if a.InProgress != nil {
				r.State = "deploying"
			}
			if a.LastDeployActive != "" {
				r.Usage = "deployed " + day(a.LastDeployActive)
			}
			for _, d := range a.Spec.Domains {
				r.Domains = append(r.Domains, d.Domain)
			}
			out = append(out, r)
		}
	} else {
		fail(err)
	}

	var drops struct {
		Droplets []struct {
			Name    string `json:"name"`
			Status  string `json:"status"`
			Created string `json:"created_at"`
			Size    struct {
				Slug  string  `json:"slug"`
				Month float64 `json:"price_monthly"`
			} `json:"size"`
			Region struct {
				Slug string `json:"slug"`
			} `json:"region"`
			Networks struct {
				V4 []struct {
					IP   string `json:"ip_address"`
					Type string `json:"type"`
				} `json:"v4"`
			} `json:"networks"`
			Backups []int `json:"backup_ids"`
		} `json:"droplets"`
	}
	if err := f.doGet(ctx, "droplets?per_page=200", &drops); err == nil {
		for _, d := range drops.Droplets {
			r := Resource{Provider: P, Kind: "droplet", Name: d.Name, State: d.Status, Detail: d.Size.Slug + " · " + d.Region.Slug,
				USD: usd(d.Size.Month), Created: day(d.Created)}
			for _, n := range d.Networks.V4 {
				if n.Type == "public" {
					r.IP = n.IP
				}
			}
			out = append(out, r)
		}
	} else {
		fail(err)
	}

	var vols struct {
		Volumes []struct {
			Name     string `json:"name"`
			Size     int    `json:"size_gigabytes"`
			Droplets []int  `json:"droplet_ids"`
			Created  string `json:"created_at"`
		} `json:"volumes"`
	}
	if f.doGet(ctx, "volumes?per_page=200", &vols) == nil {
		for _, v := range vols.Volumes {
			st := "attached"
			if len(v.Droplets) == 0 {
				st = "unattached"
			}
			out = append(out, Resource{Provider: P, Kind: "volume", Name: v.Name, Detail: fmt.Sprintf("%d GB", v.Size), State: st,
				USD: usd(float64(v.Size) * 0.10), Created: day(v.Created)})
		}
	}
	var snaps struct {
		Snapshots []struct {
			Name    string  `json:"name"`
			Size    float64 `json:"size_gigabytes"`
			Created string  `json:"created_at"`
			Type    string  `json:"resource_type"`
		} `json:"snapshots"`
	}
	if f.doGet(ctx, "snapshots?per_page=200", &snaps) == nil {
		for _, s := range snaps.Snapshots {
			out = append(out, Resource{Provider: P, Kind: "snapshot", Name: s.Name, Detail: fmt.Sprintf("%s · %.1f GB", s.Type, s.Size),
				USD: usd(s.Size * 0.06), Created: day(s.Created)})
		}
	}
	var dbs struct {
		Databases []struct {
			Name    string `json:"name"`
			Engine  string `json:"engine"`
			Size    string `json:"size"`
			Status  string `json:"status"`
			Created string `json:"created_at"`
		} `json:"databases"`
	}
	if f.doGet(ctx, "databases", &dbs) == nil {
		for _, d := range dbs.Databases {
			out = append(out, Resource{Provider: P, Kind: "database", Name: d.Name, Detail: d.Engine + " · " + d.Size, State: d.Status, Created: day(d.Created)})
		}
	}
	var lbs struct {
		LBs []struct {
			Name    string `json:"name"`
			Status  string `json:"status"`
			IP      string `json:"ip"`
			Created string `json:"created_at"`
		} `json:"load_balancers"`
	}
	if f.doGet(ctx, "load_balancers", &lbs) == nil {
		for _, l := range lbs.LBs {
			out = append(out, Resource{Provider: P, Kind: "load balancer", Name: l.Name, State: l.Status, IP: l.IP, Created: day(l.Created)})
		}
	}
	var rips struct {
		IPs []struct {
			IP      string           `json:"ip"`
			Droplet *json.RawMessage `json:"droplet"`
		} `json:"reserved_ips"`
	}
	if f.doGet(ctx, "reserved_ips", &rips) == nil {
		for _, ip := range rips.IPs {
			st := "assigned"
			if ip.Droplet == nil || string(*ip.Droplet) == "null" {
				st = "unassigned"
			}
			out = append(out, Resource{Provider: P, Kind: "reserved IP", Name: ip.IP, IP: ip.IP, State: st})
		}
	}
	var doms struct {
		Domains []struct {
			Name string `json:"name"`
		} `json:"domains"`
	}
	if f.doGet(ctx, "domains", &doms) == nil {
		for _, d := range doms.Domains {
			out = append(out, Resource{Provider: P, Kind: "DNS zone", Name: d.Name, Infra: true})
		}
	}
	var err error
	if len(errs) > 0 {
		err = errors.New(strings.Join(errs, "; "))
	}
	return out, month, invoices, err
}

func day(ts string) string {
	if len(ts) >= 10 {
		return ts[:10]
	}
	return ts
}

// ---------- Google Cloud ----------

// gget calls a Google API, charging quota to project (the project being looked at, so
// no API needs turning on anywhere else). Disabled APIs come back as errAPIOff.
var errAPIOff = errors.New("api not enabled")

// errScope: the API only accepts full cloud-platform access, which hometron doesn't ask for.
var errScope = errors.New("needs full cloud access")

func (f *Fleet) gget(ctx context.Context, project, rawURL string, out any) error {
	err := f.gcp.DoAs(ctx, "GET", rawURL, project, nil, out)
	var ae *APIError
	if errors.As(err, &ae) && (ae.Status == 403 || ae.Status == 404) && (strings.Contains(ae.Body, "SERVICE_DISABLED") || strings.Contains(ae.Body, "has not been used") || strings.Contains(ae.Body, "is disabled")) {
		return errAPIOff
	}
	if errors.As(err, &ae) && ae.Status == 403 && strings.Contains(ae.Body, "insufficient authentication scopes") {
		return errScope
	}
	return err
}

func (f *Fleet) googleCloud(ctx context.Context) ([]Resource, []GCPProject, []string) {
	qp := f.cfg.QuotaProject
	var errs []string
	var pl struct {
		Projects []struct {
			ID      string `json:"projectId"`
			Name    string `json:"name"`
			Created string `json:"createTime"`
		} `json:"projects"`
	}
	if err := f.gget(ctx, qp, "https://cloudresourcemanager.googleapis.com/v1/projects?filter=lifecycleState%3AACTIVE&pageSize=200", &pl); err != nil {
		return nil, nil, []string{"Google Cloud projects: " + shortErr(err)}
	}
	accounts := map[string]string{}
	var ba struct {
		Accounts []struct {
			Name, DisplayName string
		} `json:"billingAccounts"`
	}
	if f.gget(ctx, qp, "https://cloudbilling.googleapis.com/v1/billingAccounts", &ba) == nil {
		for _, a := range ba.Accounts {
			accounts[a.Name] = a.DisplayName
		}
	}

	var mu sync.Mutex
	var wg sync.WaitGroup
	var all []Resource
	projects := make([]GCPProject, len(pl.Projects))
	for i, p := range pl.Projects {
		wg.Add(1)
		go func() {
			defer wg.Done()
			gp := GCPProject{ID: p.ID, Name: p.Name, Created: day(p.Created)}
			var bi struct {
				Account string `json:"billingAccountName"`
				Enabled bool   `json:"billingEnabled"`
			}
			if f.gget(ctx, qp, "https://cloudbilling.googleapis.com/v1/projects/"+p.ID+"/billingInfo", &bi) == nil {
				gp.Billed = bi.Enabled
				gp.BillRead = true
				gp.Billing = accounts[bi.Account]
				if gp.Billing == "" {
					gp.Billing = strings.TrimPrefix(bi.Account, "billingAccounts/")
				}
			}
			res, apis, perrs := f.gcpProject(ctx, p.ID)
			gp.APIs = len(apis)
			for _, a := range apis {
				if !defaultAPI(a) {
					gp.Notable = append(gp.Notable, a)
				}
			}
			sort.Strings(gp.Notable)
			for _, r := range res {
				if !r.Infra {
					gp.Resources++
				}
			}
			mu.Lock()
			projects[i] = gp
			all = append(all, res...)
			for _, e := range perrs {
				errs = append(errs, p.ID+": "+e)
			}
			mu.Unlock()
		}()
	}
	wg.Wait()
	sort.Slice(projects, func(i, j int) bool { return projects[i].ID < projects[j].ID })
	return all, projects, errs
}

// gcpProject lists one project's resources, asking only the APIs it has turned on.
func (f *Fleet) gcpProject(ctx context.Context, p string) ([]Resource, []string, []string) {
	const P = "Google Cloud"
	var su struct {
		Services []struct {
			Config struct {
				Name string `json:"name"`
			} `json:"config"`
		} `json:"services"`
	}
	on := map[string]bool{}
	if err := f.gget(ctx, p, "https://serviceusage.googleapis.com/v1/projects/"+p+"/services?filter=state:ENABLED&pageSize=200", &su); err != nil {
		if err == errAPIOff {
			return nil, nil, nil // not even Service Usage is on: nothing can be running here
		}
		return nil, nil, []string{"enabled APIs: " + shortErr(err)}
	}
	for _, s := range su.Services {
		on[strings.TrimSuffix(s.Config.Name, ".googleapis.com")] = true
	}
	var out []Resource
	var errs []string
	add := func(r Resource) { r.Provider, r.Project = P, p; out = append(out, r) }
	note := func(what string, err error) {
		if err != nil && err != errAPIOff && err != errScope {
			errs = append(errs, what+": "+shortErr(err))
		}
	}

	if on["run"] {
		var rs struct {
			Services []struct {
				Name     string `json:"name"`
				URI      string `json:"uri"`
				Created  string `json:"createTime"`
				Updated  string `json:"updateTime"`
				Template struct {
					Scaling struct {
						Min int `json:"minInstanceCount"`
					} `json:"scaling"`
				} `json:"template"`
				Terminal struct {
					State string `json:"state"`
				} `json:"terminalCondition"`
			} `json:"services"`
		}
		err := f.gget(ctx, p, "https://run.googleapis.com/v2/projects/"+p+"/locations/-/services", &rs)
		note("Cloud Run", err)
		reqs := f.runRequests(ctx, p)
		for _, s := range rs.Services {
			parts := strings.Split(s.Name, "/") // projects/P/locations/L/services/N
			name, region := parts[len(parts)-1], parts[3]
			d := region
			if s.Template.Scaling.Min > 0 {
				d += fmt.Sprintf(" · min %d instance(s) always on", s.Template.Scaling.Min)
			}
			r := Resource{Kind: "Cloud Run", Name: name, Detail: d, URL: s.URI, Created: day(s.Created), State: strings.ToLower(strings.TrimPrefix(s.Terminal.State, "CONDITION_"))}
			if n, ok := reqs[name]; ok {
				r.Usage = fmt.Sprintf("%s requests/30d (%s OK)", compact(n[0]), compact(n[1]))
			} else {
				r.Usage = "0 requests/30d"
			}
			add(r)
		}
		if err == errScope { // can't list services: show the ones monitoring saw serving traffic
			for name, n := range reqs {
				add(Resource{Kind: "Cloud Run", Name: name, Detail: "from monitoring (listing needs full access)",
					Usage: fmt.Sprintf("%s requests/30d (%s OK)", compact(n[0]), compact(n[1]))})
			}
		}
		var js struct {
			Jobs []struct {
				Name string `json:"name"`
			} `json:"jobs"`
		}
		if f.gget(ctx, p, "https://run.googleapis.com/v2/projects/"+p+"/locations/-/jobs", &js) == nil {
			for _, j := range js.Jobs {
				add(Resource{Kind: "Cloud Run job", Name: j.Name[strings.LastIndex(j.Name, "/")+1:]})
			}
		}
	}
	if on["compute"] {
		var vms struct {
			Items map[string]struct {
				Instances []struct {
					Name    string `json:"name"`
					Status  string `json:"status"`
					Machine string `json:"machineType"`
					Created string `json:"creationTimestamp"`
					NICs    []struct {
						Access []struct {
							NatIP string `json:"natIP"`
						} `json:"accessConfigs"`
					} `json:"networkInterfaces"`
				} `json:"instances"`
			} `json:"items"`
		}
		err := f.gget(ctx, p, "https://compute.googleapis.com/compute/v1/projects/"+p+"/aggregated/instances?returnPartialSuccess=true", &vms)
		note("VMs", err)
		cpu := f.vmCPU(ctx, p)
		for zone, z := range vms.Items {
			for _, v := range z.Instances {
				r := Resource{Kind: "VM", Name: v.Name, State: strings.ToLower(v.Status), Created: day(v.Created),
					Detail: v.Machine[strings.LastIndex(v.Machine, "/")+1:] + " · " + strings.TrimPrefix(zone, "zones/")}
				for _, n := range v.NICs {
					for _, a := range n.Access {
						if a.NatIP != "" {
							r.IP = a.NatIP
						}
					}
				}
				if c, ok := cpu[v.Name]; ok {
					r.Usage = fmt.Sprintf("%.0f%% CPU avg/7d", c*100)
				}
				add(r)
			}
		}
		var disks struct {
			Items map[string]struct {
				Disks []struct {
					Name  string   `json:"name"`
					Size  string   `json:"sizeGb"`
					Type  string   `json:"type"`
					Users []string `json:"users"`
				} `json:"disks"`
			} `json:"items"`
		}
		if f.gget(ctx, p, "https://compute.googleapis.com/compute/v1/projects/"+p+"/aggregated/disks?returnPartialSuccess=true", &disks) == nil {
			for _, z := range disks.Items {
				for _, d := range z.Disks {
					st := "attached"
					if len(d.Users) == 0 {
						st = "unattached"
					}
					add(Resource{Kind: "disk", Name: d.Name, Detail: d.Size + " GB " + d.Type[strings.LastIndex(d.Type, "/")+1:], State: st, Infra: st == "attached"})
				}
			}
		}
		var addrs struct {
			Items map[string]struct {
				Addresses []struct {
					Name    string `json:"name"`
					Address string `json:"address"`
					Status  string `json:"status"`
					Type    string `json:"addressType"`
				} `json:"addresses"`
			} `json:"items"`
		}
		if f.gget(ctx, p, "https://compute.googleapis.com/compute/v1/projects/"+p+"/aggregated/addresses?returnPartialSuccess=true", &addrs) == nil {
			for _, z := range addrs.Items {
				for _, a := range z.Addresses {
					if a.Type == "INTERNAL" {
						continue
					}
					add(Resource{Kind: "static IP", Name: a.Name, IP: a.Address, State: strings.ToLower(strings.ReplaceAll(a.Status, "_", " "))})
				}
			}
		}
	}
	if on["cloudfunctions"] {
		var fs struct {
			Functions []struct {
				Name    string `json:"name"`
				State   string `json:"state"`
				Env     string `json:"environment"`
				Updated string `json:"updateTime"`
			} `json:"functions"`
		}
		err := f.gget(ctx, p, "https://cloudfunctions.googleapis.com/v2/projects/"+p+"/locations/-/functions", &fs)
		note("Functions", err)
		for _, fn := range fs.Functions {
			// 2nd-gen functions run as a Cloud Run service of the same name, listed above.
			add(Resource{Kind: "function", Name: fn.Name[strings.LastIndex(fn.Name, "/")+1:], State: strings.ToLower(fn.State),
				Detail: strings.ToLower(strings.ReplaceAll(fn.Env, "_", " ")), Usage: "updated " + day(fn.Updated)})
		}
	}
	if on["artifactregistry"] {
		var locs struct {
			Locations []struct {
				ID string `json:"locationId"`
			} `json:"locations"`
		}
		if f.gget(ctx, p, "https://artifactregistry.googleapis.com/v1/projects/"+p+"/locations", &locs) == nil {
			for _, l := range locs.Locations {
				if !strings.HasPrefix(l.ID, "us") { // where these projects keep images
					continue
				}
				var rs struct {
					Repos []struct {
						Name string `json:"name"`
						Size string `json:"sizeBytes"`
						Fmt  string `json:"format"`
					} `json:"repositories"`
				}
				if f.gget(ctx, p, "https://artifactregistry.googleapis.com/v1/projects/"+p+"/locations/"+l.ID+"/repositories", &rs) != nil {
					continue
				}
				for _, r := range rs.Repos {
					n, _ := strconv.ParseFloat(r.Size, 64)
					name := r.Name[strings.LastIndex(r.Name, "/")+1:]
					add(Resource{Kind: "image repo", Name: name, Detail: fmt.Sprintf("%s · %s · %s", l.ID, strings.ToLower(r.Fmt), bytesStr(n)),
						Infra: n < 1e6 || name == "gcf-artifacts" || name == "gae-standard"})
				}
			}
		}
	}
	if on["firebasehosting"] {
		var hs struct {
			Sites []struct {
				Name string `json:"name"`
				URL  string `json:"defaultUrl"`
				Type string `json:"type"`
			} `json:"sites"`
		}
		err := f.gget(ctx, p, "https://firebasehosting.googleapis.com/v1beta1/projects/"+p+"/sites", &hs)
		note("Firebase Hosting", err)
		for _, s := range hs.Sites {
			add(Resource{Kind: "Firebase site", Name: s.Name[strings.LastIndex(s.Name, "/")+1:], URL: s.URL})
		}
	}
	if on["storage"] || on["storage-api"] || on["storage-component"] {
		var bs struct {
			Items []struct {
				Name     string `json:"name"`
				Location string `json:"location"`
				Created  string `json:"timeCreated"`
			} `json:"items"`
		}
		err := f.gget(ctx, p, "https://storage.googleapis.com/storage/v1/b?project="+p, &bs)
		note("buckets", err)
		sizes := f.bucketBytes(ctx, p)
		for _, b := range bs.Items {
			r := Resource{Kind: "bucket", Name: b.Name, Detail: strings.ToLower(b.Location), Created: day(b.Created), Infra: platformBucket(b.Name)}
			if n, ok := sizes[b.Name]; ok {
				r.Detail += " · " + bytesStr(n)
			}
			add(r)
		}
	}
	var apis []string
	for a := range on {
		apis = append(apis, a)
	}
	return out, apis, errs
}

// platformBucket: buckets Google makes for App Engine, Cloud Build, Functions and Firebase.
func platformBucket(name string) bool {
	for _, s := range []string{".appspot.com", "_cloudbuild", "gcf-sources-", "gcf-v2-", "gcf-uploads", ".firebasestorage.app", "artifacts.", "-tfstate"} {
		if strings.Contains(name, s) {
			return true
		}
	}
	return false
}

// monitoring sums or averages one metric per resource label over a window.
func (f *Fleet) monitoring(ctx context.Context, p, metric, aligner, groupBy string, window time.Duration, extraGroup string) map[string][]float64 {
	q := url.Values{}
	q.Set("filter", `metric.type="`+metric+`"`)
	q.Set("interval.startTime", time.Now().Add(-window).UTC().Format(time.RFC3339))
	q.Set("interval.endTime", time.Now().UTC().Format(time.RFC3339))
	q.Set("aggregation.alignmentPeriod", strconv.Itoa(int(window.Seconds()))+"s")
	q.Set("aggregation.perSeriesAligner", aligner)
	if aligner == "ALIGN_SUM" {
		q.Set("aggregation.crossSeriesReducer", "REDUCE_SUM")
		q.Add("aggregation.groupByFields", groupBy)
		if extraGroup != "" {
			q.Add("aggregation.groupByFields", extraGroup)
		}
	}
	var r struct {
		Series []struct {
			Metric struct {
				Labels map[string]string `json:"labels"`
			} `json:"metric"`
			Resource struct {
				Labels map[string]string `json:"labels"`
			} `json:"resource"`
			Points []struct {
				Value struct {
					Int    string   `json:"int64Value"`
					Double *float64 `json:"doubleValue"`
				} `json:"value"`
			} `json:"points"`
		} `json:"timeSeries"`
	}
	out := map[string][]float64{}
	if f.gget(ctx, p, "https://monitoring.googleapis.com/v3/projects/"+p+"/timeSeries?"+q.Encode(), &r) != nil {
		return out
	}
	for _, s := range r.Series {
		key := s.Resource.Labels[strings.TrimPrefix(groupBy, "resource.label.")]
		if key == "" {
			key = s.Metric.Labels[strings.TrimPrefix(groupBy, "metric.label.")]
		}
		total := 0.0
		for _, pt := range s.Points {
			if pt.Value.Double != nil {
				total += *pt.Value.Double
			} else {
				v, _ := strconv.ParseFloat(pt.Value.Int, 64)
				total += v
			}
		}
		if aligner == "ALIGN_MEAN" && len(s.Points) > 0 {
			total /= float64(len(s.Points))
		}
		ok := 0.0
		if s.Metric.Labels["response_code_class"] == "2xx" || extraGroup == "" {
			ok = total
		}
		cur := out[key]
		if cur == nil {
			cur = []float64{0, 0}
		}
		out[key] = []float64{cur[0] + total, cur[1] + ok}
	}
	return out
}

// runRequests: [all, 2xx] requests per Cloud Run service over 30 days.
func (f *Fleet) runRequests(ctx context.Context, p string) map[string][]float64 {
	return f.monitoring(ctx, p, "run.googleapis.com/request_count", "ALIGN_SUM", "resource.label.service_name", 30*24*time.Hour, "metric.label.response_code_class")
}

// vmCPU: average CPU utilization (0..1) per VM over 7 days.
func (f *Fleet) vmCPU(ctx context.Context, p string) map[string]float64 {
	out := map[string]float64{}
	for k, v := range f.monitoring(ctx, p, "compute.googleapis.com/instance/cpu/utilization", "ALIGN_MEAN", "metric.label.instance_name", 7*24*time.Hour, "") {
		out[k] = v[0]
	}
	return out
}

// bucketBytes: bytes stored per bucket (the metric is sampled daily).
func (f *Fleet) bucketBytes(ctx context.Context, p string) map[string]float64 {
	out := map[string]float64{}
	for k, v := range f.monitoring(ctx, p, "storage.googleapis.com/storage/total_bytes", "ALIGN_MEAN", "resource.label.bucket_name", 2*24*time.Hour, "") {
		out[k] = v[0]
	}
	return out
}

func compact(n float64) string {
	switch {
	case n >= 1e6:
		return fmt.Sprintf("%.1fM", n/1e6)
	case n >= 1e3:
		return fmt.Sprintf("%.0fk", n/1e3)
	}
	return fmt.Sprintf("%.0f", n)
}

func bytesStr(n float64) string {
	switch {
	case n >= 1e9:
		return fmt.Sprintf("%.1f GB", n/1e9)
	case n >= 1e6:
		return fmt.Sprintf("%.0f MB", n/1e6)
	case n >= 1e3:
		return fmt.Sprintf("%.0f KB", n/1e3)
	}
	return fmt.Sprintf("%.0f B", n)
}

func shortErr(err error) string {
	s := err.Error()
	var ae *APIError
	if errors.As(err, &ae) {
		var b struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal([]byte(ae.Body), &b) == nil && b.Error.Message != "" {
			s = fmt.Sprintf("HTTP %d: %s", ae.Status, b.Error.Message)
		}
	}
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}

// ---------- suggestions ----------

func (f *Fleet) suggest(inv FleetInventory) []Suggestion {
	var out []Suggestion
	add := func(level, text, why string) { out = append(out, Suggestion{Level: level, Text: text, Why: why}) }
	microsByBilling := map[string][]string{}
	billing := map[string]string{}
	for _, p := range inv.Projects {
		billing[p.ID] = p.Billing
		switch {
		case p.Resources > 0 || p.ID == f.cfg.QuotaProject:
		case len(p.Notable) > 0:
			add("check", "Google Cloud project "+p.ID+" runs nothing, but has "+strings.Join(p.Notable, ", ")+" turned on",
				"Probably an API key in use somewhere (Gemini, Docs, ...). Delete it only if that key isn't.")
		case p.APIs == 0:
			add("check", "Google Cloud project "+p.ID+" has nothing turned on", "Often an AI Studio project holding an API key. Delete it if the key is unused.")
		default:
			add("kill", "Google Cloud project "+p.ID+" is empty", "Only the APIs Google turns on for every project, and nothing running. Deleting a project can be undone for 30 days.")
		}
	}
	for _, r := range inv.Resources {
		switch {
		case r.Kind == "app" && strings.Contains(r.Detail, "professional") && r.USD != nil && *r.USD > 12:
			add("trim", fmt.Sprintf("%s app costs $%.0f/mo (%s)", r.Name, *r.USD, r.Detail),
				"Professional instances are sized for production traffic. A personal site fits on one basic instance (~$5/mo).")
		case r.Kind == "droplet" && r.State == "off":
			add("kill", "Droplet "+r.Name+" is powered off", "A powered-off droplet is still billed in full.")
		case (r.Kind == "volume" || r.Kind == "disk") && r.State == "unattached":
			add("kill", r.Kind+" "+r.Name+" ("+r.Detail+") isn't attached to anything", "Billed for storage while unused.")
		case (r.Kind == "reserved IP" && r.State == "unassigned") || (r.Kind == "static IP" && r.State == "reserved"):
			add("kill", r.Kind+" "+r.IP+" isn't in use", "Unused IPs are billed by the hour.")
		case r.Kind == "snapshot" && olderThan(r.Created, 90):
			add("check", "Snapshot "+r.Name+" is from "+r.Created, "Old snapshots are rarely restored.")
		case r.Kind == "Cloud Run" && strings.HasPrefix(r.Usage, "0 requests"):
			add("kill", "Cloud Run "+r.Name+" ("+r.Project+") had no requests in 30 days", "")
		case r.Kind == "image repo" && !r.Infra && strings.HasSuffix(r.Detail, "GB"):
			add("trim", "Image repo "+r.Name+" ("+r.Project+") holds "+strings.TrimSpace(r.Detail[strings.LastIndex(r.Detail, "·")+len("·"):]),
				"Old images pile up with every deploy. A cleanup policy that keeps the last few is a one-time setting.")
		case r.Kind == "VM" && strings.HasPrefix(r.Detail, "e2-micro") && r.State == "running":
			microsByBilling[billing[r.Project]] = append(microsByBilling[billing[r.Project]], r.Name)
		}
	}
	for acct, vms := range microsByBilling {
		if len(vms) > 1 {
			where := fmt.Sprintf("on billing account %q", acct)
			if acct == "" {
				where = "(check whether they share a billing account)"
			}
			add("check", fmt.Sprintf("%d e2-micro VMs %s: %s", len(vms), where, strings.Join(vms, ", ")),
				"Google's free tier covers one e2-micro per billing account (in us-west1, us-central1 or us-east1). The others cost roughly $7/mo each, plus about $3.65/mo per public IP.")
		}
	}
	f.mu.Lock()
	for _, d := range f.domains {
		if d.Host == "" && !d.Mail {
			add("check", d.Name+" has no website and no email", "Renews "+d.Expires+". Let it lapse if you're not holding it for later.")
		}
	}
	f.mu.Unlock()
	return out
}

func olderThan(date string, days int) bool {
	t, err := time.Parse("2006-01-02", date)
	return err == nil && time.Since(t) > time.Duration(days)*24*time.Hour
}

// ---------- domains ----------

type DomainInfo struct {
	Name      string   `json:"name"`
	IPs       []string `json:"ips,omitempty"`
	Host      string   `json:"host"`       // where the site lives
	Target    string   `json:"target"`     // the resource serving it, when known
	DNS       string   `json:"dns"`        // who answers for the domain
	Mail      bool     `json:"mail"`       // has MX records
	Expires   string   `json:"expires"`    // registration
	Registrar string   `json:"registrar"`  // from config
	Checked   string   `json:"rdapSource"` // "registry" or "config"
}

// lookupDomains resolves each domain and names whatever serves it.
func (f *Fleet) lookupDomains(ctx context.Context, res []Resource) {
	byIP := map[string]Resource{}
	byDomain := map[string]Resource{}
	for _, r := range res {
		if r.IP != "" {
			byIP[r.IP] = r
		}
		for _, d := range r.Domains {
			byDomain[d] = r
		}
	}
	var wg sync.WaitGroup
	for _, d := range f.cfg.Domains {
		wg.Add(1)
		go func() {
			defer wg.Done()
			info := &DomainInfo{Name: d.Name, Registrar: d.Registrar, Expires: d.Expires, Checked: "config"}
			ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
			defer cancel()
			var r net.Resolver
			info.IPs, _ = r.LookupHost(ctx, d.Name)
			cname, _ := r.LookupCNAME(ctx, "www."+d.Name)
			if mx, _ := r.LookupMX(ctx, d.Name); len(mx) > 0 {
				info.Mail = true
			}
			if ns, _ := r.LookupNS(ctx, d.Name); len(ns) > 0 {
				info.DNS = dnsHost(ns[0].Host)
			}
			info.Host, info.Target = hostOf(d.Name, info.IPs, cname, byIP, byDomain, res)
			if exp := f.rdapExpiry(ctx, d.Name); exp != "" {
				info.Expires, info.Checked = exp, "registry"
			}
			f.mu.Lock()
			f.domains[d.Name] = info
			f.mu.Unlock()
		}()
	}
	wg.Wait()
}

func dnsHost(ns string) string {
	ns = strings.ToLower(strings.TrimSuffix(ns, "."))
	switch {
	case strings.Contains(ns, "squarespacedns"):
		return "Squarespace"
	case strings.Contains(ns, "googledomains"):
		return "Squarespace (ex-Google Domains)"
	case strings.Contains(ns, "digitalocean"):
		return "DigitalOcean"
	case strings.Contains(ns, "nsone"):
		return "NS1 (Squarespace)"
	case strings.Contains(ns, "cloudflare"):
		return "Cloudflare"
	case strings.Contains(ns, "vercel"):
		return "Vercel"
	}
	return ns
}

// hostOf names the platform behind a domain from its addresses and www CNAME.
func hostOf(domain string, ips []string, cname string, byIP, byDomain map[string]Resource, res []Resource) (string, string) {
	if r, ok := byDomain[domain]; ok {
		return r.Provider + " " + r.Kind, r.Name
	}
	for _, ip := range ips {
		if r, ok := byIP[ip]; ok {
			return r.Provider + " " + r.Kind, r.Name
		}
	}
	stem := strings.SplitN(domain, ".", 2)[0]
	find := func(kinds ...string) string {
		for _, r := range res {
			for _, k := range kinds {
				if r.Kind == k && (r.Name == stem || strings.HasPrefix(r.Name, stem) || strings.Contains(r.URL, stem) || strings.Contains(cname, r.Name)) {
					return r.Name + " (" + r.Project + ")"
				}
			}
		}
		return ""
	}
	cname = strings.ToLower(cname)
	for _, ip := range ips {
		switch {
		case ip == "199.36.158.100" || strings.HasSuffix(strings.TrimSuffix(cname, "."), ".web.app"):
			return "Firebase Hosting", find("Firebase site")
		case strings.HasPrefix(ip, "216.239.3") && strings.HasSuffix(ip, ".21"):
			return "Google (Cloud Run / App Engine)", find("Cloud Run")
		case ip == "76.76.21.21" || strings.Contains(cname, "vercel"):
			return "Vercel", ""
		case strings.HasPrefix(ip, "185.199.10") && strings.HasSuffix(ip, ".153"):
			return "GitHub Pages", strings.TrimSuffix(cname, ".")
		}
	}
	if len(ips) > 0 {
		return "unknown (" + ips[0] + ")", ""
	}
	return "", ""
}

// rdapExpiry asks the registry (via rdap.org's redirector) when a domain expires.
func (f *Fleet) rdapExpiry(ctx context.Context, domain string) string {
	req, _ := http.NewRequestWithContext(ctx, "GET", "https://rdap.org/domain/"+url.PathEscape(domain), nil)
	req.Header.Set("Accept", "application/rdap+json")
	resp, err := f.http.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	var r struct {
		Events []struct {
			Action string `json:"eventAction"`
			Date   string `json:"eventDate"`
		} `json:"events"`
	}
	if resp.StatusCode != 200 || json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&r) != nil {
		return ""
	}
	for _, e := range r.Events {
		if e.Action == "expiration" {
			if t, err := time.Parse(time.RFC3339, e.Date); err == nil {
				return t.Local().Format("2006-01-02")
			}
			return day(e.Date)
		}
	}
	return ""
}

// ---------- site checks ----------

type siteState struct {
	Name     string    `json:"name"`
	URL      string    `json:"url"`
	Up       bool      `json:"up"`
	Status   int       `json:"status,omitempty"`
	Ms       int       `json:"ms"`
	Error    string    `json:"error,omitempty"`
	CertDays *int      `json:"certDays,omitempty"`
	Since    time.Time `json:"since"` // when Up last changed
	Checked  time.Time `json:"checked"`

	fails    int
	alerted  bool
	certWarn time.Time
}

type fleetSample struct {
	T time.Time      `json:"t"`
	R map[string]int `json:"r"` // name -> ms, or -1 for down
}

// targets: every domain with an address, plus the extra checks.
func (f *Fleet) targets() map[string]string {
	out := map[string]string{}
	for _, d := range f.cfg.Domains {
		f.mu.Lock()
		info := f.domains[d.Name]
		f.mu.Unlock()
		if info != nil && len(info.IPs) == 0 {
			continue // no website (mail only, or parked)
		}
		out[d.Name] = "https://" + d.Name + "/"
	}
	for _, u := range f.cfg.Checks {
		if p, err := url.Parse(u); err == nil {
			name := p.Host
			if p.Path != "" && p.Path != "/" {
				name += p.Path
			}
			out[name] = u
		}
	}
	return out
}

func (f *Fleet) runChecks(ctx context.Context) {
	time.Sleep(20 * time.Second) // let the first DNS lookups land
	for {
		f.checkAll(ctx, time.Now())
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Minute):
		}
	}
}

var siteClient = &http.Client{
	Timeout: 15 * time.Second,
	// The first answer is what we judge: a redirect means the site is serving.
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	Transport: &http.Transport{
		TLSClientConfig:   &tls.Config{},
		DisableKeepAlives: true, // measure a real connection each time
		Proxy:             nil,
	},
}

func (f *Fleet) checkAll(ctx context.Context, now time.Time) {
	if f.internet != nil {
		if s := f.internet.View(); !s.Checked.IsZero() && !s.Up {
			return // our own connection is down: says nothing about the sites
		}
	}
	targets := f.targets()
	type result struct {
		name, url, err string
		status, ms     int
		certDays       *int
	}
	results := make(chan result, len(targets))
	for name, u := range targets {
		go func() {
			r := result{name: name, url: u}
			start := time.Now()
			req, _ := http.NewRequestWithContext(ctx, "GET", u, nil)
			req.Header.Set("User-Agent", "hometron-fleet-check/1")
			resp, err := siteClient.Do(req)
			r.ms = int(time.Since(start).Milliseconds())
			if err != nil {
				r.err = err.Error()
				if i := strings.LastIndex(r.err, ": "); i > 0 {
					r.err = r.err[i+2:]
				}
			} else {
				io.Copy(io.Discard, io.LimitReader(resp.Body, 256<<10))
				resp.Body.Close()
				r.status = resp.StatusCode
				if resp.TLS != nil && len(resp.TLS.PeerCertificates) > 0 {
					d := int(time.Until(resp.TLS.PeerCertificates[0].NotAfter).Hours() / 24)
					r.certDays = &d
				}
			}
			results <- r
		}()
	}
	sample := fleetSample{T: now.Truncate(time.Second), R: map[string]int{}}
	for range targets {
		r := <-results
		if ctx.Err() != nil {
			return // shutting down
		}
		up := r.err == "" && r.status < 500
		f.mu.Lock()
		s := f.sites[r.name]
		if s == nil {
			s = &siteState{Name: r.name, Up: true, Since: now}
			f.sites[r.name] = s
		}
		s.URL, s.Status, s.Ms, s.Error, s.Checked = r.url, r.status, r.ms, r.err, now
		if r.certDays != nil {
			s.CertDays = r.certDays
		}
		if up {
			sample.R[r.name] = r.ms
			if s.alerted {
				f.post(now, r.name, r.name+" is back (down "+durWords(now.Sub(s.Since))+")")
				s.alerted = false
			}
			if !s.Up {
				s.Since = now
			}
			s.Up, s.fails = true, 0
		} else {
			sample.R[r.name] = -1
			s.fails++
			if s.Up {
				s.Up, s.Since = false, now
			}
			if s.fails == 3 && !s.alerted { // three minutes in a row
				why := r.err
				if why == "" {
					why = "HTTP " + strconv.Itoa(r.status)
				}
				f.post(now, r.name, r.name+" is down ("+why+")")
				s.alerted = true
			}
		}
		if s.CertDays != nil && *s.CertDays < 14 && now.Sub(s.certWarn) > 24*time.Hour {
			f.post(now, r.name, fmt.Sprintf("%s: TLS certificate expires in %d days", r.name, *s.CertDays))
			s.certWarn = now
		}
		f.mu.Unlock()
	}
	f.mu.Lock()
	f.history = append(f.history, sample)
	cut := 0
	for cut < len(f.history) && now.Sub(f.history[cut].T) > 7*24*time.Hour {
		cut++
	}
	f.history = f.history[cut:]
	f.mu.Unlock()
	if b, err := json.Marshal(sample); err == nil {
		if fh, err := os.OpenFile(f.logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
			fh.Write(append(b, '\n'))
			fh.Close()
		}
	}
	f.checkDomainExpiry(now)
}

func (f *Fleet) post(now time.Time, name, text string) {
	if f.events != nil {
		go f.events.Post(Event{Time: now, DeviceID: "fleet:" + name, Device: text, Kind: "fleet"})
	}
}

// checkDomainExpiry warns once a day when a registration is within 30 days. Holds no lock.
var domainWarned = map[string]time.Time{}

func (f *Fleet) checkDomainExpiry(now time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, d := range f.domains {
		t, err := time.Parse("2006-01-02", d.Expires)
		if err != nil || t.Sub(now) > 30*24*time.Hour || now.Sub(domainWarned[d.Name]) < 24*time.Hour {
			continue
		}
		domainWarned[d.Name] = now
		f.post(now, d.Name, fmt.Sprintf("%s registration expires %s", d.Name, d.Expires))
	}
}

func durWords(d time.Duration) string {
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()+0.5))
	}
	return fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
}

// loadHistory reads the last week of checks, and trims the file when it has grown.
func (f *Fleet) loadHistory(now time.Time) {
	b, err := os.ReadFile(f.logPath)
	if err != nil {
		return
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	kept := lines[:0]
	for _, l := range lines {
		var s fleetSample
		if json.Unmarshal([]byte(l), &s) != nil || now.Sub(s.T) > 7*24*time.Hour {
			continue
		}
		f.history = append(f.history, s)
		kept = append(kept, l)
	}
	// Show the last results right away after a restart, rather than an empty table.
	if n := len(f.history); n > 0 {
		last := f.history[n-1]
		for name, ms := range last.R {
			f.sites[name] = &siteState{Name: name, Up: ms >= 0, Ms: max(ms, 0), Checked: last.T, Since: last.T}
		}
	}
	if len(kept) < len(lines) && len(b) > 4<<20 {
		os.WriteFile(f.logPath, []byte(strings.Join(kept, "\n")+"\n"), 0o600)
	}
}

// ---------- view ----------

type SiteView struct {
	siteState
	Up24   *float64    `json:"up24,omitempty"`
	Up7    *float64    `json:"up7,omitempty"`
	P50    int         `json:"p50,omitempty"` // median response, last 24h
	Strip  []int       `json:"strip"`         // last 24h in 48 half-hours: -1 none, else % up
	Domain *DomainInfo `json:"domain,omitempty"`
}

type FleetView struct {
	Inventory FleetInventory `json:"inventory"`
	Sites     []SiteView     `json:"sites"`
	Domains   []DomainInfo   `json:"domains"`
	Fixed     []FixedCost    `json:"fixed,omitempty"`
	Missing   []string       `json:"missing,omitempty"` // credentials not set up
}

func (f *Fleet) View() FleetView {
	f.mu.Lock()
	defer f.mu.Unlock()
	v := FleetView{Inventory: f.inv, Fixed: f.cfg.Fixed}
	if f.doToken == "" {
		v.Missing = append(v.Missing, "DigitalOcean (digitalocean.json)")
	}
	if f.gcp == nil {
		v.Missing = append(v.Missing, "Google Cloud (fleet-token.json)")
	}
	now := time.Now()
	for _, s := range f.sites {
		sv := SiteView{siteState: *s, Strip: make([]int, 48), Domain: f.domains[s.Name]}
		var n24, up24, n7, up7 int
		var lat []int
		slotN, slotUp := make([]int, 48), make([]int, 48)
		for _, h := range f.history {
			ms, ok := h.R[s.Name]
			if !ok {
				continue
			}
			age := now.Sub(h.T)
			n7++
			if ms >= 0 {
				up7++
			}
			if age <= 24*time.Hour {
				n24++
				slot := 47 - int(age/(30*time.Minute))
				if slot >= 0 && slot < 48 {
					slotN[slot]++
				}
				if ms >= 0 {
					up24++
					lat = append(lat, ms)
					if slot >= 0 && slot < 48 {
						slotUp[slot]++
					}
				}
			}
		}
		for i := range sv.Strip {
			sv.Strip[i] = -1
			if slotN[i] > 0 {
				sv.Strip[i] = slotUp[i] * 100 / slotN[i]
			}
		}
		if n24 > 0 {
			p := float64(up24) * 100 / float64(n24)
			sv.Up24 = &p
		}
		if n7 > 0 {
			p := float64(up7) * 100 / float64(n7)
			sv.Up7 = &p
		}
		if len(lat) > 0 {
			sort.Ints(lat)
			sv.P50 = lat[len(lat)/2]
		}
		v.Sites = append(v.Sites, sv)
	}
	sort.Slice(v.Sites, func(i, j int) bool {
		if v.Sites[i].Up != v.Sites[j].Up {
			return !v.Sites[i].Up // down first
		}
		return v.Sites[i].Name < v.Sites[j].Name
	})
	for _, d := range f.cfg.Domains {
		if info := f.domains[d.Name]; info != nil {
			v.Domains = append(v.Domains, *info)
		} else {
			v.Domains = append(v.Domains, DomainInfo{Name: d.Name, Expires: d.Expires, Registrar: d.Registrar, Checked: "config"})
		}
	}
	return v
}

type fleetSaved struct {
	Inventory FleetInventory         `json:"inventory"`
	Domains   map[string]*DomainInfo `json:"domains"`
}

// defaultAPI: services Google turns on for new projects (or that come along with
// them). Anything else on an otherwise empty project hints at a key in use.
func defaultAPI(a string) bool {
	for _, p := range []string{"bigquery", "storage", "cloudapis", "cloudtrace", "datastore", "logging", "monitoring", "servicemanagement",
		"serviceusage", "sql-component", "analyticshub", "dataform", "dataplex", "appenginereporting", "deploymentmanager", "oslogin",
		"runtimeconfig", "telemetry", "compute", "iamcredentials", "sts", "backupdr", "servicenetworking", "cloudresourcemanager",
		"pubsub", "containerregistry", "artifactregistry", "cloudbuild", "run", "iam", "secretmanager", "cloudbilling", "source", "testing"} {
		if a == p || strings.HasPrefix(a, p) && (p == "bigquery" || p == "storage") {
			return true
		}
	}
	return false
}
