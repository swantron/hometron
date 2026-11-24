package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeAccount mimics account.smartthings.com: /tokens needs a session cookie (else it
// redirects to a login page), and /api/tokens needs the XSRF header the real page sends.
type fakeAccount struct {
	mu     sync.Mutex
	tokens []map[string]any
	n      int
}

func (f *fakeAccount) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	signedIn := false
	if c, err := r.Cookie("sess"); err == nil && c.Value == "ok" {
		signedIn = true
	}
	switch {
	case r.URL.Path == "/login":
		// Signing in sets a persistent session, like "keep me signed in".
		http.SetCookie(w, &http.Cookie{Name: "sess", Value: "ok", Path: "/", MaxAge: 3600})
		fmt.Fprint(w, "<html><body>signed in</body></html>")
	case r.URL.Path == "/tokens":
		if !signedIn {
			http.Redirect(w, r, "/login-page", http.StatusFound)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: "XSRF-TOKEN", Value: "x123", Path: "/"})
		fmt.Fprint(w, "<html><body>tokens</body></html>")
	case r.URL.Path == "/login-page":
		fmt.Fprint(w, "<html><body>please log in</body></html>")
	case strings.HasPrefix(r.URL.Path, "/api/tokens"):
		if !signedIn {
			w.WriteHeader(401)
			return
		}
		if r.Method != "GET" && r.Header.Get("X-XSRF-TOKEN") != "x123" {
			w.WriteHeader(403)
			return
		}
		switch r.Method {
		case "GET":
			json.NewEncoder(w).Encode(f.tokens)
		case "POST":
			var body struct {
				DeviceID, DeviceName string
				Scope                []string
			}
			json.NewDecoder(r.Body).Decode(&body)
			if body.DeviceID == "" || len(body.Scope) != 2 {
				w.WriteHeader(400)
				return
			}
			f.n++
			tok := fmt.Sprintf("tok-%d", f.n)
			f.tokens = append(f.tokens, map[string]any{"authKey": fmt.Sprintf("k%d", f.n), "deviceName": body.DeviceName, "token": tok})
			json.NewEncoder(w).Encode(map[string]string{"accessToken": tok})
		case "DELETE":
			key := strings.TrimPrefix(r.URL.Path, "/api/tokens/")
			for i, t := range f.tokens {
				if t["authKey"] == key {
					f.tokens = append(f.tokens[:i], f.tokens[i+1:]...)
					break
				}
			}
		}
	default:
		http.NotFound(w, r)
	}
}

func TestRenewPATWithChrome(t *testing.T) {
	if _, err := os.Stat(chromeBin); err != nil || testing.Short() {
		t.Skip("needs Google Chrome")
	}
	f := &fakeAccount{tokens: []map[string]any{{"authKey": "mine", "deviceName": "Hometron"}}} // the person's own token
	srv := httptest.NewServer(f)
	defer srv.Close()
	profile := t.TempDir()
	ctx := context.Background()

	if _, err := renewPAT(ctx, chromeBin, profile, srv.URL); !errors.Is(err, errNeedsSignIn) {
		t.Fatalf("before sign-in: %v", err)
	}
	// "Sign in": visit the login URL in the same profile, then quit Chrome.
	c, err := startChrome(ctx, chromeBin, profile, true, "about:blank")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.openPage(ctx, srv.URL+"/login"); err != nil {
		t.Fatal(err)
	}
	c.close()
	time.Sleep(500 * time.Millisecond) // let Chrome flush its cookie store

	tok1, err := renewPAT(ctx, chromeBin, profile, srv.URL)
	if err != nil || tok1 != "tok-1" {
		t.Fatalf("first renew = %q, %v", tok1, err)
	}
	tok2, err := renewPAT(ctx, chromeBin, profile, srv.URL)
	if err != nil || tok2 != "tok-2" {
		t.Fatalf("second renew = %q, %v", tok2, err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var keys []string
	for _, tk := range f.tokens {
		keys = append(keys, tk["authKey"].(string))
	}
	// The person's own token stays; only hometron's previous one was deleted.
	if strings.Join(keys, ",") != "mine,k2" {
		t.Errorf("tokens left = %v", keys)
	}
}
