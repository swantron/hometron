package main

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Vercel Web Analytics for the Traffic table. <state>/vercel.json {"token": "..."}: hometron
// finds the teams, their projects and each project's domains itself, and reads 30 days of
// visitors and pageviews for any project serving one of fleet.json's domains.

type vercelConfig struct {
	Token string `json:"token"`
}

func (f *Fleet) vget(ctx context.Context, path string, q url.Values, out any) error {
	u := "https://api.vercel.com" + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+f.vercel.Token)
	return doJSON(f.http, req, out)
}

type vercelProject struct {
	ID, Name, Team string
	Domains        []string
}

// vercelProjects lists every project in every scope the token can see, with its domains.
func (f *Fleet) vercelProjects(ctx context.Context) ([]vercelProject, error) {
	var teams struct {
		Teams []struct {
			ID string `json:"id"`
		} `json:"teams"`
	}
	if err := f.vget(ctx, "/v2/teams", nil, &teams); err != nil {
		return nil, err
	}
	scopes := []string{""} // personal account
	for _, t := range teams.Teams {
		scopes = append(scopes, t.ID)
	}
	var out []vercelProject
	var mu sync.Mutex
	var wg sync.WaitGroup
	var firstErr error
	seen := map[string]bool{}
	for _, team := range scopes {
		q := url.Values{"limit": {"100"}}
		if team != "" {
			q.Set("teamId", team)
		}
		var ps struct {
			Projects []struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"projects"`
		}
		if err := f.vget(ctx, "/v10/projects", q, &ps); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		for _, p := range ps.Projects {
			mu.Lock()
			dup := seen[p.ID]
			seen[p.ID] = true
			mu.Unlock()
			if dup {
				continue
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				var ds struct {
					Domains []struct {
						Name     string `json:"name"`
						Redirect string `json:"redirect"`
					} `json:"domains"`
				}
				dq := url.Values{}
				if team != "" {
					dq.Set("teamId", team)
				}
				f.vget(ctx, "/v9/projects/"+url.PathEscape(p.ID)+"/domains", dq, &ds)
				vp := vercelProject{ID: p.ID, Name: p.Name, Team: team}
				for _, d := range ds.Domains {
					if d.Redirect == "" {
						vp.Domains = append(vp.Domains, d.Name)
					}
				}
				mu.Lock()
				out = append(out, vp)
				mu.Unlock()
			}()
		}
	}
	wg.Wait()
	if len(out) == 0 && firstErr != nil {
		return nil, firstErr
	}
	return out, nil
}

// vercelTraffic: visitors per day (and pageviews in the text) for each fleet domain on Vercel.
// Sites whose project has Web Analytics off come back in off.
func (f *Fleet) vercelTraffic(ctx context.Context) (out map[string]Series, off map[string]bool, notes []string) {
	projects, err := f.vercelProjects(ctx)
	if err != nil {
		return nil, nil, []string{"Vercel: " + shortErr(err)}
	}
	want := map[string]bool{}
	for _, d := range f.cfg.Domains {
		want[d.Name] = true
	}
	out, off = map[string]Series{}, map[string]bool{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	today := time.Now()
	for _, p := range projects {
		site := ""
		for _, d := range p.Domains {
			if d := strings.TrimPrefix(d, "www."); want[d] {
				site = d
				break
			}
		}
		if site == "" {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			q := url.Values{"projectId": {p.ID}, "by": {"day"},
				"since": {today.AddDate(0, 0, -(trafficDays - 1)).Format("2006-01-02")}, "until": {today.Format("2006-01-02")}}
			if p.Team != "" {
				q.Set("teamId", p.Team)
			}
			var r struct {
				Data []struct {
					Timestamp string  `json:"timestamp"`
					Pageviews float64 `json:"pageviews"`
					Visitors  float64 `json:"visitors"`
				} `json:"data"`
			}
			err := f.vget(ctx, "/v1/query/web-analytics/visits/aggregate", q, &r)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err != nil && strings.Contains(err.Error(), "web_analytics_not_enabled"):
				off[site] = true
				return
			case err != nil:
				notes = append(notes, "Vercel Web Analytics ("+p.Name+"): "+shortErr(err))
				return
			}
			views := 0.0
			for _, x := range r.Data {
				views += x.Pageviews
			}
			daily := byDate(len(r.Data), func(i int) (string, float64) {
				return strings.SplitN(r.Data[i].Timestamp, "T", 2)[0], r.Data[i].Visitors
			})
			out[site] = Series{Name: "visitors", Source: "Vercel Web Analytics", Daily: daily, Human: true,
				Text: compact(sum(daily)) + " visitors · " + compact(views) + " pageviews"}
		}()
	}
	wg.Wait()
	return out, off, notes
}
