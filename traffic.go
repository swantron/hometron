package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Traffic: how much each public site and app gets used, from what the platforms already
// record (nothing new is added to the sites). Each site gets a 30-day daily series per source:
//
//	Firebase Hosting  bytes served per custom domain (crawlers included)
//	Cloud Run         successful requests per service (crawlers included)
//	Firebase Auth     people signed in: per day, and so far this month
//	Firestore         documents written (people saving things)
//	Vercel            visitors and pageviews from Web Analytics: <state>/vercel.json (see vercel.go)
//	Search Console    Google search clicks and impressions; needs webmasters.readonly
//
// The project's sign-ins and writes go on its app's site: fleet.json "apps" names it
// ({"my-project": "app.example.com"}), otherwise it's the busiest non-default Firebase site.

const trafficDays = 30

type Traffic struct {
	Site    string   `json:"site"`
	Project string   `json:"project,omitempty"`
	Series  []Series `json:"series,omitempty"`
	Note    string   `json:"note,omitempty"` // why there's no data, when there isn't
}

type Series struct {
	Name   string    `json:"name"`
	Source string    `json:"source"`
	Text   string    `json:"text"`            // the 30-day figure, ready to show
	Daily  []float64 `json:"daily"`           // trafficDays values, oldest first
	Total  float64   `json:"total"`           // sum of Daily
	Human  bool      `json:"human,omitempty"` // counts people (signed in, analytics, search), not requests
	Bytes  bool      `json:"bytes,omitempty"`
}

// metricDaily sums a metric into trafficDays 24-hour buckets ending now, per key (the
// groupBy label values joined with "|"; a single "" key without groupBy).
func (f *Fleet) metricDaily(ctx context.Context, p, metric string, groupBy ...string) map[string][]float64 {
	now := time.Now().UTC().Truncate(time.Minute)
	out := map[string][]float64{}
	for k, v := range f.metricBuckets(ctx, p, metric, now.Add(-trafficDays*24*time.Hour), now, 24*time.Hour, groupBy...) {
		out[k] = v
	}
	return out
}

// metricBuckets: a metric summed over [start, end) in buckets of period, oldest first.
func (f *Fleet) metricBuckets(ctx context.Context, p, metric string, start, end time.Time, period time.Duration, groupBy ...string) map[string][]float64 {
	n := int(math.Ceil(end.Sub(start).Seconds() / period.Seconds()))
	q := url.Values{}
	q.Set("filter", `metric.type="`+metric+`"`)
	q.Set("interval.startTime", start.UTC().Format(time.RFC3339))
	q.Set("interval.endTime", end.UTC().Format(time.RFC3339))
	q.Set("aggregation.alignmentPeriod", strconv.Itoa(int(period.Seconds()))+"s")
	q.Set("aggregation.perSeriesAligner", "ALIGN_SUM")
	q.Set("aggregation.crossSeriesReducer", "REDUCE_SUM")
	for _, g := range groupBy {
		q.Add("aggregation.groupByFields", g)
	}
	var r struct {
		Series []struct {
			Metric   struct{ Labels map[string]string } `json:"metric"`
			Resource struct{ Labels map[string]string } `json:"resource"`
			Points   []struct {
				Interval struct {
					End time.Time `json:"endTime"`
				} `json:"interval"`
				Value struct {
					Int    string   `json:"int64Value"`
					Double *float64 `json:"doubleValue"`
				} `json:"value"`
			} `json:"points"`
		} `json:"timeSeries"`
	}
	out := map[string][]float64{}
	u := "https://monitoring.googleapis.com/v3/projects/" + p + "/timeSeries?" + q.Encode()
	if f.gget(ctx, p, u, &r) != nil && (p == f.cfg.QuotaProject || f.gget(ctx, f.cfg.QuotaProject, u, &r) != nil) {
		return out
	}
	for _, s := range r.Series {
		var keys []string
		for _, g := range groupBy {
			l := strings.TrimPrefix(strings.TrimPrefix(g, "resource.label."), "metric.label.")
			v := s.Resource.Labels[l]
			if v == "" {
				v = s.Metric.Labels[l]
			}
			keys = append(keys, v)
		}
		key := strings.Join(keys, "|")
		vals := out[key]
		if vals == nil {
			vals = make([]float64, n)
		}
		for _, pt := range s.Points {
			// Buckets end at end, end-period, ...; place each by its end time.
			i := n - 1 - int(math.Round(end.Sub(pt.Interval.End).Seconds()/period.Seconds()))
			if i < 0 || i >= n {
				continue
			}
			if pt.Value.Double != nil {
				vals[i] += *pt.Value.Double
			} else {
				v, _ := strconv.ParseFloat(pt.Value.Int, 64)
				vals[i] += v
			}
		}
		out[key] = vals
	}
	return out
}

func plural(n float64, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func sum(v []float64) float64 {
	t := 0.0
	for _, x := range v {
		t += x
	}
	return t
}

func addTo(dst, src []float64) []float64 {
	if dst == nil {
		dst = make([]float64, len(src))
	}
	for i := range src {
		if i < len(dst) {
			dst[i] += src[i]
		}
	}
	return dst
}

// traffic gathers every source. It returns the rows plus notes on sources not connected.
func (f *Fleet) traffic(ctx context.Context, res []Resource) ([]Traffic, []string) {
	var mu sync.Mutex
	rows := map[string]*Traffic{}
	var notes []string
	put := func(site, project string, s Series) {
		s.Total = sum(s.Daily)
		r := rows[site]
		if r == nil {
			r = &Traffic{Site: site, Project: project}
			rows[site] = r
		}
		if r.Project == "" {
			r.Project = project
		}
		r.Series = append(r.Series, s)
	}

	// Google Cloud: projects with a Firebase site or a Cloud Run service.
	type proj struct{ sites, runs []Resource }
	projects := map[string]*proj{}
	for _, r := range res {
		if r.Provider != "Google Cloud" || (r.Kind != "Firebase site" && r.Kind != "Cloud Run") {
			continue
		}
		pr := projects[r.Project]
		if pr == nil {
			pr = &proj{}
			projects[r.Project] = pr
		}
		if r.Kind == "Firebase site" {
			pr.sites = append(pr.sites, r)
		} else {
			pr.runs = append(pr.runs, r)
		}
	}
	// Which domain a Cloud Run service answers on, from the domain lookups.
	f.mu.Lock()
	runDomain := map[string]string{}
	for name, d := range f.domains {
		if d != nil && d.Target != "" {
			runDomain[d.Target] = name
		}
	}
	f.mu.Unlock()

	var wg sync.WaitGroup
	var vercelOff map[string]bool
	if f.gcp != nil {
		for p, pr := range projects {
			wg.Add(1)
			go func() {
				defer wg.Done()
				now := time.Now().UTC().Truncate(time.Minute)
				var hosting, runs, dau, writes map[string][]float64
				var mau float64
				var inner sync.WaitGroup
				inner.Add(5)
				go func() {
					defer inner.Done()
					if len(pr.sites) > 0 {
						hosting = f.metricDaily(ctx, p, "firebasehosting.googleapis.com/network/sent_bytes_count", "resource.label.site_name", "resource.label.domain_name")
					}
				}()
				go func() {
					defer inner.Done()
					if len(pr.runs) > 0 {
						runs = f.metricDaily(ctx, p, "run.googleapis.com/request_count", "resource.label.service_name", "metric.label.response_code_class")
					}
				}()
				go func() {
					defer inner.Done()
					dau = f.metricDaily(ctx, p, "identitytoolkit.googleapis.com/usage/daily_new_signin_count")
				}()
				go func() {
					defer inner.Done()
					writes = f.metricDaily(ctx, p, "firestore.googleapis.com/document/write_count")
				}()
				go func() {
					defer inner.Done()
					// Monthly active users: each person counts once, the first time they sign in this month.
					local := time.Now()
					start := time.Date(local.Year(), local.Month(), 1, 0, 0, 0, 0, local.Location())
					if now.Sub(start) > time.Hour {
						for _, v := range f.metricBuckets(ctx, p, "identitytoolkit.googleapis.com/usage/monthly_new_signin_count", start, now, now.Sub(start).Truncate(time.Second)) {
							mau += sum(v)
						}
					}
				}()
				inner.Wait()

				// Each Firebase site's traffic goes to the domain it serves; the default
				// *.web.app / *.firebaseapp.com addresses and redirects fold into its main domain.
				primary := map[string]string{}
				own := map[string]bool{}
				for _, s := range pr.sites {
					primary[s.Name] = strings.TrimPrefix(s.URL, "https://")
					if len(s.Domains) > 0 {
						primary[s.Name] = s.Domains[0]
					}
					for _, d := range s.Domains {
						own[d] = true
					}
				}
				bySite := map[string][]float64{}
				siteBytes := map[string]float64{}
				for k, v := range hosting {
					site, domain, _ := strings.Cut(k, "|")
					to := primary[site]
					if own[domain] {
						to = domain
					}
					if to == "" {
						to = strings.TrimPrefix(domain, "www.")
					}
					bySite[to] = addTo(bySite[to], v)
					if site != p { // the project's default site is usually the landing page
						siteBytes[to] += sum(v)
					}
				}
				app := f.cfg.Apps[p]
				if app == "" {
					best := -1.0
					for d, b := range siteBytes {
						if b > best || b == best && d < app {
							app, best = d, b
						}
					}
				}
				if app == "" && len(pr.sites) > 0 {
					app = primary[pr.sites[0].Name]
				}

				mu.Lock()
				defer mu.Unlock()
				for d, v := range bySite {
					put(d, p, Series{Name: "served", Source: "Firebase Hosting", Daily: v, Bytes: true, Text: bytesStr(sum(v)) + " served"})
				}
				ok := map[string][]float64{}
				for k, v := range runs {
					svc, class, _ := strings.Cut(k, "|")
					if class == "2xx" {
						ok[svc] = addTo(ok[svc], v)
					} else if ok[svc] == nil {
						ok[svc] = make([]float64, len(v))
					}
				}
				for svc, v := range ok {
					site, name := runDomain[svc+" ("+p+")"], "requests"
					if site == "" {
						site, name = app, svc+" requests"
					}
					if site == "" {
						site, name = svc+" (Cloud Run)", "requests"
					}
					put(site, p, Series{Name: name, Source: "Cloud Run", Daily: v, Text: compact(sum(v)) + plural(sum(v), " request", " requests")})
				}
				site := app
				if site == "" {
					site = p
				}
				if v := dau[""]; sum(v) > 0 || mau > 0 {
					put(site, p, Series{Name: "signed in", Source: "Firebase Auth", Daily: v, Human: true,
						Text: fmt.Sprintf("%.0f %s so far in %s", mau, plural(mau, "person", "people"), time.Now().Format("January"))})
				}
				if v := writes[""]; sum(v) > 0 {
					put(site, p, Series{Name: "saves", Source: "Firestore", Daily: v, Human: true, Text: compact(sum(v)) + " documents saved"})
				}
			}()
		}
	}

	// Vercel Web Analytics.
	if f.vercel.Token != "" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, off, vnotes := f.vercelTraffic(ctx)
			mu.Lock()
			defer mu.Unlock()
			notes = append(notes, vnotes...)
			vercelOff = off
			for d, s := range got {
				put(d, "", s)
			}
		}()
	}

	// Search Console.
	if f.gcp != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := f.searchConsole(ctx)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == errScope:
				notes = append(notes, "Google search clicks need the Fleet sign-in redone with webmasters.readonly (see README)")
			case err == errAPIOff:
				notes = append(notes, "Google search clicks need the Search Console API turned on in "+f.cfg.QuotaProject)
			case err != nil:
				notes = append(notes, "Search Console: "+shortErr(err))
			}
			for d, s := range got {
				put(d, "", s)
			}
		}()
	}
	wg.Wait()

	// Every site with a website gets a row, saying why when there's nothing to show.
	f.mu.Lock()
	for _, d := range f.cfg.Domains {
		info := f.domains[d.Name]
		if info == nil || len(info.IPs) == 0 || rows[d.Name] != nil {
			continue
		}
		note := "no traffic source"
		switch {
		case info.Host == "GitHub Pages":
			note = "GitHub Pages keeps no visitor stats"
		case info.Host == "Vercel":
			note = "on Vercel: add vercel.json to read its Web Analytics"
			if vercelOff[d.Name] {
				note = "on Vercel, with Web Analytics turned off for its project"
			} else if f.vercel.Token != "" {
				note = "on Vercel, with no Web Analytics data"
			}
		case strings.Contains(info.Host, "static IP") || strings.Contains(info.Host, "VM"):
			note = "self-hosted: no request stats collected"
		}
		rows[d.Name] = &Traffic{Site: d.Name, Note: note}
	}
	f.mu.Unlock()

	out := make([]Traffic, 0, len(rows))
	for _, r := range rows {
		sort.SliceStable(r.Series, func(i, j int) bool { return seriesRank(r.Series[i]) < seriesRank(r.Series[j]) })
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool {
		if (len(out[i].Series) > 0) != (len(out[j].Series) > 0) {
			return len(out[i].Series) > 0
		}
		return out[i].Site < out[j].Site
	})
	sort.Strings(notes)
	return out, notes
}

// seriesRank: people first, then search, then raw traffic.
func seriesRank(s Series) int {
	switch s.Source {
	case "Vercel Web Analytics":
		return 0
	case "Firebase Auth":
		return 1
	case "Firestore":
		return 2
	case "Search Console":
		return 3
	case "Firebase Hosting":
		return 4
	}
	return 5
}

// byDate places dated values ("2006-01-02") into trafficDays slots ending today.
func byDate(n int, at func(int) (string, float64)) []float64 {
	out := make([]float64, trafficDays)
	y, m, d := time.Now().Date()
	today := time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
	for i := 0; i < n; i++ {
		day, v := at(i)
		t, err := time.Parse("2006-01-02", day)
		if err != nil {
			continue
		}
		if k := trafficDays - 1 - int(today.Sub(t).Hours()/24); k >= 0 && k < trafficDays {
			out[k] += v
		}
	}
	return out
}

// searchConsole: Google search impressions per day per verified domain, with clicks in the text.
func (f *Fleet) searchConsole(ctx context.Context) (map[string]Series, error) {
	var sites struct {
		Entries []struct {
			URL        string `json:"siteUrl"`
			Permission string `json:"permissionLevel"`
		} `json:"siteEntry"`
	}
	if err := f.gget(ctx, f.cfg.QuotaProject, "https://www.googleapis.com/webmasters/v3/sites", &sites); err != nil {
		return nil, err
	}
	out := map[string]Series{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	var firstErr error
	today := time.Now()
	body := map[string]any{"startDate": today.AddDate(0, 0, -(trafficDays - 1)).Format("2006-01-02"), "endDate": today.Format("2006-01-02"), "dimensions": []string{"date"}}
	for _, e := range sites.Entries {
		if e.Permission == "siteUnverifiedUser" {
			continue
		}
		domain := strings.TrimPrefix(e.URL, "sc-domain:")
		if u, err := url.Parse(e.URL); err == nil && u.Host != "" {
			domain = u.Host
		}
		domain = strings.TrimPrefix(domain, "www.")
		wg.Add(1)
		go func() {
			defer wg.Done()
			var r struct {
				Rows []struct {
					Keys        []string `json:"keys"`
					Clicks      float64  `json:"clicks"`
					Impressions float64  `json:"impressions"`
				} `json:"rows"`
			}
			err := f.gcp.DoAs(ctx, "POST", "https://www.googleapis.com/webmasters/v3/sites/"+url.PathEscape(e.URL)+"/searchAnalytics/query", f.cfg.QuotaProject, body, &r)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if firstErr == nil {
					firstErr = err
				}
				return
			}
			clicks := 0.0
			for _, x := range r.Rows {
				clicks += x.Clicks
			}
			daily := byDate(len(r.Rows), func(i int) (string, float64) {
				if len(r.Rows[i].Keys) == 0 {
					return "", 0
				}
				return r.Rows[i].Keys[0], r.Rows[i].Impressions
			})
			s := out[domain] // a domain property and a URL-prefix one for the same site: keep the bigger
			if s.Daily == nil || sum(daily) > s.Total {
				out[domain] = Series{Name: "Google search", Source: "Search Console", Daily: daily, Total: sum(daily), Human: true,
					Text: fmt.Sprintf("%s clicks from %s search impressions", compact(clicks), compact(sum(daily)))}
			}
		}()
	}
	wg.Wait()
	if len(out) == 0 && firstErr != nil {
		return nil, firstErr
	}
	return out, nil
}

// doJSON runs a request and decodes a 2xx JSON answer into out.
func doJSON(c *http.Client, req *http.Request, out any) error {
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b[:min(len(b), 200)])))
	}
	return json.Unmarshal(b, out)
}
