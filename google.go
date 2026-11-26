package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Credentials is ~/nest/credentials.json: the OAuth client plus project IDs.
type Credentials struct {
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
	GCPProjectID string `json:"gcp_project_id"`
	SDMProjectID string `json:"sdm_project_id"`
	RedirectURI  string `json:"redirect_uri"`
}

// Google holds an OAuth refresh token and hands out short-lived access tokens.
type Google struct {
	creds        Credentials
	refreshToken string
	http         *http.Client

	mu      sync.Mutex
	access  string
	expires time.Time
}

func loadGoogle(stateDir string) (*Google, error) {
	var c Credentials
	if err := readJSON(filepath.Join(stateDir, "credentials.json"), &c); err != nil {
		return nil, err
	}
	var tok struct {
		RefreshToken string `json:"refresh_token"`
	}
	if err := readJSON(filepath.Join(stateDir, "token.json"), &tok); err != nil {
		return nil, err
	}
	if c.ClientID == "" || c.ClientSecret == "" || c.SDMProjectID == "" || c.GCPProjectID == "" {
		return nil, fmt.Errorf("credentials.json is missing client_id, client_secret, sdm_project_id or gcp_project_id")
	}
	if tok.RefreshToken == "" {
		return nil, fmt.Errorf("token.json has no refresh_token; run the OAuth flow again")
	}
	return &Google{creds: c, refreshToken: tok.RefreshToken, http: &http.Client{Timeout: 30 * time.Second}}, nil
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

// Token returns a valid access token, refreshing it a minute before expiry.
func (g *Google) Token(ctx context.Context) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.access != "" && time.Until(g.expires) > time.Minute {
		return g.access, nil
	}
	form := url.Values{
		"client_id":     {g.creds.ClientID},
		"client_secret": {g.creds.ClientSecret},
		"refresh_token": {g.refreshToken},
		"grant_type":    {"refresh_token"},
	}
	req, _ := http.NewRequestWithContext(ctx, "POST", "https://oauth2.googleapis.com/token", bytes.NewBufferString(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := g.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var out struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
		Error       string `json:"error"`
		Description string `json:"error_description"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if out.AccessToken == "" {
		if strings.Contains(out.Description, "invalid_rapt") {
			// A Workspace account under Google Cloud session control: the admin console's
			// reauthentication policy decides how long a sign-in lasts.
			return "", fmt.Errorf("Google wants a fresh sign-in (Workspace reauthentication policy); sign in again or turn reauthentication off for trusted apps in the Admin console")
		}
		return "", fmt.Errorf("token refresh failed: %s %s", out.Error, out.Description)
	}
	g.access = out.AccessToken
	g.expires = time.Now().Add(time.Duration(out.ExpiresIn) * time.Second)
	return g.access, nil
}

// APIError is a non-2xx response from a Google API.
type APIError struct {
	Status int
	Body   string
}

func (e *APIError) Error() string { return fmt.Sprintf("google api %d: %s", e.Status, e.Body) }

// Do sends a JSON request with a bearer token and decodes a JSON response into out (if non-nil).
func (g *Google) Do(ctx context.Context, method, rawURL string, body, out any) error {
	return g.DoAs(ctx, method, rawURL, "", body, out)
}

// DoAs is Do with the quota and billing for the call charged to userProject (when set)
// instead of the OAuth client's own project.
func (g *Google) DoAs(ctx context.Context, method, rawURL, userProject string, body, out any) error {
	tok, err := g.Token(ctx)
	if err != nil {
		return err
	}
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	if userProject != "" {
		req.Header.Set("X-Goog-User-Project", userProject)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := g.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode/100 != 2 {
		return &APIError{Status: resp.StatusCode, Body: string(b)}
	}
	if out != nil && len(b) > 0 {
		return json.Unmarshal(b, out)
	}
	return nil
}

// loadGoogleToken is a second sign-in with the same OAuth client, kept in its own file
// (e.g. fleet-token.json: read-only Cloud access, separate from the Nest token).
func loadGoogleToken(stateDir, file string) (*Google, error) {
	var c Credentials
	if err := readJSON(filepath.Join(stateDir, "credentials.json"), &c); err != nil {
		return nil, err
	}
	var tok struct {
		RefreshToken string `json:"refresh_token"`
	}
	if err := readJSON(filepath.Join(stateDir, file), &tok); err != nil {
		return nil, err
	}
	if tok.RefreshToken == "" {
		return nil, fmt.Errorf("%s has no refresh_token", file)
	}
	return &Google{creds: c, refreshToken: tok.RefreshToken, http: &http.Client{Timeout: 30 * time.Second}}, nil
}
