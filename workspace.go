package main

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Google Workspace seats: who has an account, whether they use it, and what that costs.
// Read-only Admin SDK (Directory users + Reports usage), on the same sign-in as the
// Google Cloud inventory. Google has no billing API for Workspace bought directly, so
// the price per seat comes from fleet.json: "workspace": {"plan": "...", "usdPerUser": 8.4}.

type workspaceConfig struct {
	Plan       string  `json:"plan"`
	USDPerUser float64 `json:"usdPerUser"`
}

type WorkspaceView struct {
	Plan    string   `json:"plan,omitempty"`
	PerUser float64  `json:"usdPerUser,omitempty"`
	USD     float64  `json:"usd,omitempty"` // monthly: billed seats × price
	Seats   int      `json:"seats"`         // billed accounts (suspended ones still count)
	Users   []WSUser `json:"users"`
	AsOf    string   `json:"asOf,omitempty"` // date of the activity report (Google lags 2–4 days)
}

type WSUser struct {
	Email      string            `json:"email"`
	Name       string            `json:"name"`
	Admin      bool              `json:"admin,omitempty"`
	Suspended  bool              `json:"suspended,omitempty"`
	TwoStep    bool              `json:"twoStep"`
	LastLogin  string            `json:"lastLogin,omitempty"` // "" = never
	Created    string            `json:"created"`
	LastActive map[string]string `json:"lastActive,omitempty"` // app -> date, from the usage report
}

const adminAPI = "https://admin.googleapis.com/admin/"

func (f *Fleet) workspace(ctx context.Context) (*WorkspaceView, error) {
	qp := f.cfg.QuotaProject
	var dir struct {
		Users []struct {
			Email string `json:"primaryEmail"`
			Name  struct {
				Full string `json:"fullName"`
			} `json:"name"`
			Admin     bool   `json:"isAdmin"`
			Suspended bool   `json:"suspended"`
			Archived  bool   `json:"archived"`
			TwoStep   bool   `json:"isEnrolledIn2Sv"`
			LastLogin string `json:"lastLoginTime"`
			Created   string `json:"creationTime"`
		} `json:"users"`
	}
	if err := f.gget(ctx, qp, adminAPI+"directory/v1/users?customer=my_customer&maxResults=500&projection=basic", &dir); err != nil {
		return nil, err
	}
	w := &WorkspaceView{}
	if c := f.cfg.Workspace; c != nil {
		w.Plan, w.PerUser = c.Plan, c.USDPerUser
	}
	for _, u := range dir.Users {
		if u.Archived {
			continue // archived users are billed separately (Archived User licenses)
		}
		wu := WSUser{Email: u.Email, Name: u.Name.Full, Admin: u.Admin, Suspended: u.Suspended, TwoStep: u.TwoStep, Created: day(u.Created)}
		if t, err := time.Parse(time.RFC3339, u.LastLogin); err == nil && t.Year() > 1970 {
			wu.LastLogin = t.Local().Format("2006-01-02")
		}
		w.Users = append(w.Users, wu)
	}
	w.Seats = len(w.Users)
	w.USD = float64(w.Seats) * w.PerUser
	sort.Slice(w.Users, func(i, j int) bool { return w.Users[i].Email < w.Users[j].Email })

	// Usage reports trail by a few days; walk back to the newest day that has data.
	for back := 2; back <= 6; back++ {
		date := time.Now().AddDate(0, 0, -back).Format("2006-01-02")
		var rep struct {
			Usage []struct {
				Entity struct {
					Email string `json:"userEmail"`
				} `json:"entity"`
				Params []struct {
					Name string `json:"name"`
					Date string `json:"datetimeValue"`
				} `json:"parameters"`
			} `json:"usageReports"`
		}
		if f.gget(ctx, qp, adminAPI+"reports/v1/usage/users/all/dates/"+date, &rep) != nil || len(rep.Usage) == 0 {
			continue
		}
		w.AsOf = date
		for _, r := range rep.Usage {
			for i := range w.Users {
				if !strings.EqualFold(w.Users[i].Email, r.Entity.Email) {
					continue
				}
				for _, p := range r.Params {
					app, field, ok := strings.Cut(p.Name, ":")
					if !ok || p.Date == "" || !strings.HasPrefix(field, "last_") || app == "accounts" {
						continue
					}
					t, err := time.Parse(time.RFC3339, p.Date)
					if err != nil || t.Year() < 2000 {
						continue
					}
					d := t.Local().Format("2006-01-02")
					if w.Users[i].LastActive == nil {
						w.Users[i].LastActive = map[string]string{}
					}
					if d > w.Users[i].LastActive[app] {
						w.Users[i].LastActive[app] = d
					}
				}
			}
		}
		break
	}
	return w, nil
}

// workspaceSuggestions: unused or risky seats.
func workspaceSuggestions(w *WorkspaceView) []Suggestion {
	if w == nil {
		return nil
	}
	var out []Suggestion
	price := ""
	if w.PerUser > 0 {
		price = fmt.Sprintf(" A seat costs $%.2f/mo.", w.PerUser)
	}
	for _, u := range w.Users {
		switch {
		case u.Suspended:
			out = append(out, Suggestion{Level: "kill", Text: "Workspace user " + u.Email + " is suspended but still a paid seat",
				Why: "Suspended accounts are billed like active ones. Delete it (transfer the data first) or move it to an Archived User license." + price})
		case u.LastLogin == "":
			out = append(out, Suggestion{Level: "check", Text: "Workspace user " + u.Email + " has never signed in", Why: strings.TrimSpace(price)})
		case olderThan(u.LastLogin, 60):
			out = append(out, Suggestion{Level: "check", Text: "Workspace user " + u.Email + " hasn't signed in since " + u.LastLogin,
				Why: "Mail may still be arriving there (check the last Gmail activity)." + price})
		}
		if !u.Suspended && !u.TwoStep {
			lvl := "check"
			if u.Admin {
				lvl = "fix"
			}
			out = append(out, Suggestion{Level: lvl, Text: "Workspace user " + u.Email + " has no 2-step verification" + map[bool]string{true: " (and is an admin)", false: ""}[u.Admin],
				Why: "A password alone protects this account."})
		}
	}
	return out
}
