package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Renewing the SmartThings personal access token. Samsung's free tier only offers tokens
// that last 24 hours, created on account.smartthings.com/tokens while signed in. hometron
// keeps a dedicated Chrome profile that someone signed into once (on the server's screen,
// with "keep me signed in"), and once a day opens that page headless and asks the page's
// own API for a fresh read-only token. It never sees or stores the Samsung password, and
// when the session runs out it says so instead of trying to log in.
//
// Chrome is driven over the DevTools protocol on a pipe (--remote-debugging-pipe), not a
// port, so nothing else on the machine can attach to it.

const chromeBin = "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"

var errNeedsSignIn = errors.New("the SmartThings browser session has expired: sign in again")

// stTokenPrefix names the tokens hometron creates, so it only ever deletes its own.
const stTokenPrefix = "hometron-auto"

type cdp struct {
	cmd *exec.Cmd
	w   *os.File // to Chrome (its fd 3)
	r   *bufio.Reader

	mu      sync.Mutex
	nextID  int
	pending map[int]chan cdpReply
	done    chan struct{}
}

type cdpReply struct {
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// startChrome launches Chrome with profile dir and a DevTools pipe. headless=false opens a
// normal window (for signing in).
func startChrome(ctx context.Context, bin, profile string, headless bool, startURL string) (*cdp, error) {
	toChromeR, toChromeW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	fromChromeR, fromChromeW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	args := []string{
		"--user-data-dir=" + profile, "--remote-debugging-pipe", "--no-first-run", "--no-default-browser-check",
		// Cookies are encrypted with a fixed key instead of a Keychain entry, so a process
		// started by launchd never stops on a Keychain prompt. The profile dir is 0700.
		"--use-mock-keychain",
	}
	if headless {
		args = append(args, "--headless=new")
	}
	args = append(args, startURL)
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.ExtraFiles = []*os.File{toChromeR, fromChromeW} // become fds 3 and 4
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	toChromeR.Close()
	fromChromeW.Close()
	c := &cdp{cmd: cmd, w: toChromeW, r: bufio.NewReader(fromChromeR), pending: map[int]chan cdpReply{}, done: make(chan struct{})}
	go c.readLoop()
	return c, nil
}

func (c *cdp) readLoop() {
	defer close(c.done)
	for {
		msg, err := c.r.ReadBytes(0)
		if err != nil {
			return
		}
		var m struct {
			ID int `json:"id"`
			cdpReply
		}
		if json.Unmarshal(msg[:len(msg)-1], &m) != nil || m.ID == 0 {
			continue // events: not needed
		}
		c.mu.Lock()
		ch := c.pending[m.ID]
		delete(c.pending, m.ID)
		c.mu.Unlock()
		if ch != nil {
			ch <- m.cdpReply
		}
	}
}

// call sends one DevTools command (to the page's session if sessionID is set).
func (c *cdp) call(ctx context.Context, sessionID, method string, params any, out any) error {
	c.mu.Lock()
	c.nextID++
	id := c.nextID
	ch := make(chan cdpReply, 1)
	c.pending[id] = ch
	c.mu.Unlock()
	msg := map[string]any{"id": id, "method": method, "params": params}
	if sessionID != "" {
		msg["sessionId"] = sessionID
	}
	b, _ := json.Marshal(msg)
	if _, err := c.w.Write(append(b, 0)); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.done:
		return errors.New("chrome exited")
	case r := <-ch:
		if r.Error != nil {
			return fmt.Errorf("%s: %s", method, r.Error.Message)
		}
		if out != nil {
			return json.Unmarshal(r.Result, out)
		}
		return nil
	}
}

func (c *cdp) close() {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	c.call(ctx, "", "Browser.close", map[string]any{}, nil)
	cancel()
	c.w.Close()
	done := make(chan struct{})
	go func() { c.cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		c.cmd.Process.Kill()
	}
}

// openPage opens url in a new tab and returns the tab's session once it has loaded.
func (c *cdp) openPage(ctx context.Context, url string) (string, error) {
	var t struct {
		TargetID string `json:"targetId"`
	}
	if err := c.call(ctx, "", "Target.createTarget", map[string]any{"url": url}, &t); err != nil {
		return "", err
	}
	var a struct {
		SessionID string `json:"sessionId"`
	}
	if err := c.call(ctx, "", "Target.attachToTarget", map[string]any{"targetId": t.TargetID, "flatten": true}, &a); err != nil {
		return "", err
	}
	// Wait for the page (and any sign-in redirects) to settle.
	last := ""
	for i := 0; i < 60; i++ {
		var r evalResult
		if err := c.call(ctx, a.SessionID, "Runtime.evaluate", map[string]any{"expression": "location.href + ' ' + document.readyState", "returnByValue": true}, &r); err == nil {
			s, _ := r.Result.Value.(string)
			if strings.HasSuffix(s, " complete") && s == last {
				return a.SessionID, nil
			}
			last = s
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	return a.SessionID, nil
}

type evalResult struct {
	Result struct {
		Value any `json:"value"`
	} `json:"result"`
	ExceptionDetails *struct {
		Text string `json:"text"`
	} `json:"exceptionDetails"`
}

func (c *cdp) eval(ctx context.Context, session, js string) (any, error) {
	var r evalResult
	if err := c.call(ctx, session, "Runtime.evaluate", map[string]any{"expression": js, "awaitPromise": true, "returnByValue": true}, &r); err != nil {
		return nil, err
	}
	if r.ExceptionDetails != nil {
		return nil, errors.New("page script: " + r.ExceptionDetails.Text)
	}
	return r.Result.Value, nil
}

// renewJS runs inside account.smartthings.com/tokens: it creates a token the way the page's
// own "Generate" button does (same endpoint, same XSRF header), then deletes older tokens
// hometron made. Returns {token} or {error}.
const renewJS = `(async () => {
  if (!location.pathname.startsWith('/tokens')) return {signin: true, at: location.host};
  const m = document.cookie.match(/(?:^|; )XSRF-TOKEN=([^;]+)/);
  const h = {'Content-Type': 'application/json', 'Accept': 'application/json'};
  if (m) h['X-XSRF-TOKEN'] = decodeURIComponent(m[1]);
  const opts = {headers: h, credentials: 'same-origin'};
  const lr = await fetch('/api/tokens', opts);
  if (lr.status === 401 || lr.status === 403) return {signin: true, at: 'api'};
  const list = lr.ok ? await lr.json() : [];
  const name = %q + '-' + new Date().toISOString().slice(0, 16);
  const r = await fetch('/api/tokens', {...opts, method: 'POST', body: JSON.stringify({deviceId: crypto.randomUUID(), deviceName: name, scope: %s})});
  if (r.status === 401 || r.status === 403) return {signin: true, at: 'create'};
  if (!r.ok) return {error: 'create failed: HTTP ' + r.status};
  const t = await r.json();
  if (!t.accessToken) return {error: 'no token in reply'};
  let deleted = 0;
  for (const old of (Array.isArray(list) ? list : [])) {
    if (old.authKey && JSON.stringify(old).includes('"' + %q)) {
      const d = await fetch('/api/tokens/' + encodeURIComponent(old.authKey), {...opts, method: 'DELETE'});
      if (d.ok) deleted++;
    }
  }
  return {token: t.accessToken, deleted};
})()`

// stScopes: read-only, the minimum the fridge needs.
var stScopes = []string{"r:devices:*", "r:locations:*"}

// renewPAT returns a fresh token, or errNeedsSignIn.
func renewPAT(ctx context.Context, bin, profile, base string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	if _, err := os.Stat(bin); err != nil {
		return "", errors.New("Google Chrome isn't installed on the server")
	}
	if err := os.MkdirAll(profile, 0o700); err != nil {
		return "", err
	}
	c, err := startChrome(ctx, bin, profile, true, "about:blank")
	if err != nil {
		return "", err
	}
	defer c.close()
	session, err := c.openPage(ctx, base+"/tokens")
	if err != nil {
		return "", err
	}
	scopes, _ := json.Marshal(stScopes)
	v, err := c.eval(ctx, session, fmt.Sprintf(renewJS, stTokenPrefix, scopes, stTokenPrefix))
	if err != nil {
		return "", err
	}
	res, _ := v.(map[string]any)
	if res["signin"] == true {
		return "", errNeedsSignIn
	}
	if e, ok := res["error"].(string); ok {
		return "", errors.New(e)
	}
	tok, _ := res["token"].(string)
	if tok == "" {
		return "", errors.New("no token returned")
	}
	if n, _ := res["deleted"].(float64); n > 0 {
		log.Printf("smartthings: replaced token (deleted %d old)", int(n))
	}
	return tok, nil
}

// openSignIn shows a normal Chrome window on the server's screen with the same profile, at
// the tokens page, for someone to sign in (via Screen Sharing). onClosed runs when that
// window is quit, or after 20 minutes.
func openSignIn(bin, profile, base string, onClosed func()) error {
	if _, err := os.Stat(bin); err != nil {
		return errors.New("Google Chrome isn't installed on the server")
	}
	if err := os.MkdirAll(profile, 0o700); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	c, err := startChrome(ctx, bin, profile, false, base+"/tokens")
	if err != nil {
		cancel()
		return err
	}
	go func() {
		select {
		case <-ctx.Done():
		case <-c.done: // they quit Chrome
		}
		c.close()
		cancel()
		onClosed()
	}()
	return nil
}

func stProfileDir(stateDir string) string { return filepath.Join(stateDir, "smartthings-browser") }
