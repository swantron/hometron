package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Renewing the SmartThings personal access token. Samsung's free tier only offers tokens
// that last 24 hours, made on account.smartthings.com/tokens while signed in. SmartThings'
// login lives in session-only cookies (gone when Chrome quits) and Samsung's own login
// idles out after 30 minutes, so hometron keeps ONE Chrome running on the server's screen
// with a dedicated profile: someone signs in there once (Screen Sharing), hometron reloads
// the tokens page every 10 minutes to keep the session alive, and about once a day asks the
// page's own API for a fresh read-only token. It never sees or stores the Samsung password;
// when the session ends (a reboot, Samsung logging it out) it says so.
//
// Chrome runs detached from hometron, so restarting hometron doesn't sign it out. hometron
// talks to it over the DevTools port, which Chrome binds to 127.0.0.1 only.

const chromeBin = "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"

var errNeedsSignIn = errors.New("the SmartThings browser session has expired: sign in again")

// stTokenPrefix names the tokens hometron creates, so it only ever deletes its own.
const stTokenPrefix = "hometron-auto"

// ---------- a minimal DevTools client over WebSocket ----------

type cdp struct {
	conn net.Conn
	br   *bufio.Reader
	wmu  sync.Mutex

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

// dialCDP opens the browser-level DevTools WebSocket at ws://127.0.0.1:port/path.
func dialCDP(ctx context.Context, wsURL string) (*cdp, error) {
	u, err := url.Parse(wsURL)
	if err != nil {
		return nil, err
	}
	if h := u.Hostname(); h != "127.0.0.1" && h != "localhost" {
		return nil, errors.New("refusing a non-local DevTools address")
	}
	d := net.Dialer{Timeout: 5 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", u.Host)
	if err != nil {
		return nil, err
	}
	key := make([]byte, 16)
	rand.Read(key)
	k := base64.StdEncoding.EncodeToString(key)
	fmt.Fprintf(conn, "GET %s HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\n\r\n", u.RequestURI(), u.Host, k)
	br := bufio.NewReader(conn)
	res, err := http.ReadResponse(br, nil)
	if err != nil {
		conn.Close()
		return nil, err
	}
	sum := sha1.Sum([]byte(k + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	if res.StatusCode != 101 || res.Header.Get("Sec-WebSocket-Accept") != base64.StdEncoding.EncodeToString(sum[:]) {
		conn.Close()
		return nil, fmt.Errorf("DevTools handshake: HTTP %d", res.StatusCode)
	}
	c := &cdp{conn: conn, br: br, pending: map[int]chan cdpReply{}, done: make(chan struct{})}
	go c.readLoop()
	return c, nil
}

// writeFrame sends one masked text frame (clients must mask).
func (c *cdp) writeFrame(payload []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	hdr := []byte{0x81} // FIN + text
	n := len(payload)
	switch {
	case n < 126:
		hdr = append(hdr, 0x80|byte(n))
	case n < 1<<16:
		hdr = append(hdr, 0x80|126)
		hdr = binary.BigEndian.AppendUint16(hdr, uint16(n))
	default:
		hdr = append(hdr, 0x80|127)
		hdr = binary.BigEndian.AppendUint64(hdr, uint64(n))
	}
	mask := make([]byte, 4)
	rand.Read(mask)
	hdr = append(hdr, mask...)
	masked := make([]byte, n)
	for i := range payload {
		masked[i] = payload[i] ^ mask[i%4]
	}
	_, err := c.conn.Write(append(hdr, masked...))
	return err
}

// readMessage reads one (possibly fragmented) server message.
func (c *cdp) readMessage() ([]byte, error) {
	var msg []byte
	for {
		h := make([]byte, 2)
		if _, err := io.ReadFull(c.br, h); err != nil {
			return nil, err
		}
		fin, op := h[0]&0x80 != 0, h[0]&0x0f
		n := uint64(h[1] & 0x7f)
		switch n {
		case 126:
			b := make([]byte, 2)
			if _, err := io.ReadFull(c.br, b); err != nil {
				return nil, err
			}
			n = uint64(binary.BigEndian.Uint16(b))
		case 127:
			b := make([]byte, 8)
			if _, err := io.ReadFull(c.br, b); err != nil {
				return nil, err
			}
			n = binary.BigEndian.Uint64(b)
		}
		if n > 64<<20 {
			return nil, errors.New("DevTools message too large")
		}
		var mask []byte
		if h[1]&0x80 != 0 { // servers don't mask, but be lenient
			mask = make([]byte, 4)
			if _, err := io.ReadFull(c.br, mask); err != nil {
				return nil, err
			}
		}
		p := make([]byte, n)
		if _, err := io.ReadFull(c.br, p); err != nil {
			return nil, err
		}
		for i := range p {
			if mask != nil {
				p[i] ^= mask[i%4]
			}
		}
		switch op {
		case 0x8: // close
			return nil, io.EOF
		case 0x9, 0xA: // ping/pong
			continue
		}
		msg = append(msg, p...)
		if fin {
			return msg, nil
		}
	}
}

func (c *cdp) readLoop() {
	defer close(c.done)
	for {
		msg, err := c.readMessage()
		if err != nil {
			return
		}
		var m struct {
			ID int `json:"id"`
			cdpReply
		}
		if json.Unmarshal(msg, &m) != nil || m.ID == 0 {
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

// call sends one DevTools command (to a tab's session if sessionID is set).
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
	if err := c.writeFrame(b); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.done:
		return errors.New("lost the connection to Chrome")
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

func (c *cdp) disconnect() { c.conn.Close() }

// openPage opens url in a new tab; returns the tab id and its session once it has loaded.
func (c *cdp) openPage(ctx context.Context, url string, background bool) (string, string, error) {
	var t struct {
		TargetID string `json:"targetId"`
	}
	if err := c.call(ctx, "", "Target.createTarget", map[string]any{"url": url, "background": background}, &t); err != nil {
		return "", "", err
	}
	var a struct {
		SessionID string `json:"sessionId"`
	}
	if err := c.call(ctx, "", "Target.attachToTarget", map[string]any{"targetId": t.TargetID, "flatten": true}, &a); err != nil {
		return t.TargetID, "", err
	}
	// Wait for the page (and any sign-in redirects) to settle.
	last := ""
	for i := 0; i < 60; i++ {
		var r evalResult
		if err := c.call(ctx, a.SessionID, "Runtime.evaluate", map[string]any{"expression": "location.href + ' ' + document.readyState", "returnByValue": true}, &r); err == nil {
			s, _ := r.Result.Value.(string)
			if strings.HasSuffix(s, " complete") && s == last {
				break
			}
			last = s
		}
		select {
		case <-ctx.Done():
			return t.TargetID, "", ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	return t.TargetID, a.SessionID, nil
}

func (c *cdp) closeTab(targetID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c.call(ctx, "", "Target.closeTarget", map[string]any{"targetId": targetID}, nil)
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

// ---------- the long-lived browser ----------

// stBrowser owns (or reattaches to) the one Chrome holding the SmartThings session.
type stBrowser struct {
	bin, profile, base string
	headless           bool // tests only: the real one needs a window to sign in

	mu sync.Mutex // one task in the browser at a time
}

// connect attaches to the running Chrome for this profile, starting it if asked.
// Chrome writes its DevTools port to <profile>/DevToolsActivePort.
func (b *stBrowser) connect(ctx context.Context, start bool) (*cdp, error) {
	if c, err := b.attach(ctx); err == nil {
		return c, nil
	} else if !start {
		return nil, err
	}
	if _, err := os.Stat(b.bin); err != nil {
		return nil, errors.New("Google Chrome isn't installed on the server")
	}
	if err := os.MkdirAll(b.profile, 0o700); err != nil {
		return nil, err
	}
	os.Remove(filepath.Join(b.profile, "DevToolsActivePort"))
	args := []string{
		"--user-data-dir=" + b.profile,
		"--remote-debugging-port=0", "--remote-debugging-address=127.0.0.1", // Chrome picks a free port
		"--no-first-run", "--no-default-browser-check",
		// Cookies are encrypted with a fixed key instead of a Keychain entry, so a process
		// started by launchd never stops on a Keychain prompt. The profile dir is 0700.
		"--use-mock-keychain",
	}
	if b.headless {
		args = append(args, "--headless=new")
	}
	args = append(args, "about:blank")
	cmd := exec.Command(b.bin, args...)
	// Its own session, so it outlives hometron restarts (the launchd job also sets
	// AbandonProcessGroup so launchd doesn't kill it on restart).
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	go cmd.Wait() // reap it whenever it exits
	for i := 0; i < 60; i++ {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
		if c, err := b.attach(ctx); err == nil {
			return c, nil
		}
	}
	return nil, errors.New("Chrome started but its DevTools port never came up")
}

func (b *stBrowser) attach(ctx context.Context) (*cdp, error) {
	raw, err := os.ReadFile(filepath.Join(b.profile, "DevToolsActivePort"))
	if err != nil {
		return nil, err
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) < 2 {
		return nil, errors.New("DevToolsActivePort incomplete")
	}
	return dialCDP(ctx, "ws://127.0.0.1:"+strings.TrimSpace(lines[0])+strings.TrimSpace(lines[1]))
}

// checkJS: are we on the tokens page with a working session?
const checkJS = `(async () => {
  if (!location.pathname.startsWith('/tokens')) return false;
  const r = await fetch('/api/tokens', {credentials: 'same-origin', headers: {'Accept': 'application/json'}});
  return r.ok;
})()`

// renewJS runs inside account.smartthings.com/tokens: it creates a token the way the page's
// own "Generate" button does (same endpoint, same XSRF header), then deletes older tokens
// hometron made. Returns {token} or {signin} or {error}.
const renewJS = `(async () => {
  if (!location.pathname.startsWith('/tokens')) return {signin: true};
  const m = document.cookie.match(/(?:^|; )XSRF-TOKEN=([^;]+)/);
  const h = {'Content-Type': 'application/json', 'Accept': 'application/json'};
  if (m) h['X-XSRF-TOKEN'] = decodeURIComponent(m[1]);
  const opts = {headers: h, credentials: 'same-origin'};
  const lr = await fetch('/api/tokens', opts);
  if (lr.status === 401 || lr.status === 403) return {signin: true};
  const list = lr.ok ? await lr.json() : [];
  const name = %q + '-' + new Date().toISOString().slice(0, 16);
  const r = await fetch('/api/tokens', {...opts, method: 'POST', body: JSON.stringify({deviceId: crypto.randomUUID(), deviceName: name, scope: %s})});
  if (r.status === 401 || r.status === 403) return {signin: true};
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

// inTab opens the tokens page in a background tab of the running browser, runs js, and
// closes the tab. If the browser isn't running, that's a sign-in problem: a fresh Chrome
// has no session.
func (b *stBrowser) inTab(ctx context.Context, js string) (any, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	c, err := b.connect(ctx, false)
	if err != nil {
		return nil, errNeedsSignIn
	}
	defer c.disconnect()
	tab, session, err := c.openPage(ctx, b.base+"/tokens", true)
	if tab != "" {
		defer c.closeTab(tab)
	}
	if err != nil {
		return nil, err
	}
	return c.eval(ctx, session, js)
}

// KeepAlive reloads the tokens page so neither Samsung's nor SmartThings' session idles
// out. Returns errNeedsSignIn if it already has.
func (b *stBrowser) KeepAlive(ctx context.Context) error {
	v, err := b.inTab(ctx, checkJS)
	if err != nil {
		return err
	}
	if v != true {
		return errNeedsSignIn
	}
	return nil
}

// Renew returns a fresh token, or errNeedsSignIn.
func (b *stBrowser) Renew(ctx context.Context) (string, error) {
	scopes, _ := json.Marshal(stScopes)
	v, err := b.inTab(ctx, fmt.Sprintf(renewJS, stTokenPrefix, scopes, stTokenPrefix))
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

// ShowSignIn starts the browser if needed and puts the tokens page in front, for someone
// to sign in over Screen Sharing.
func (b *stBrowser) ShowSignIn(ctx context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	c, err := b.connect(ctx, true)
	if err != nil {
		return err
	}
	defer c.disconnect()
	tab, _, err := c.openPage(ctx, b.base+"/tokens", false)
	if err == nil && tab != "" {
		c.call(ctx, "", "Target.activateTarget", map[string]any{"targetId": tab}, nil)
	}
	return err
}

// Quit closes the session browser (tests).
func (b *stBrowser) Quit() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if c, err := b.connect(ctx, false); err == nil {
		c.call(ctx, "", "Browser.close", map[string]any{}, nil)
		select { // wait for Chrome to drop the connection as it exits
		case <-c.done:
		case <-ctx.Done():
		}
		c.disconnect()
		time.Sleep(500 * time.Millisecond) // and finish writing its profile
	}
}

func stProfileDir(stateDir string) string { return filepath.Join(stateDir, "smartthings-browser") }
