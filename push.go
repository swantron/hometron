package main

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Push sends Web Push notifications (RFC 8030/8291/8292) straight to the browser
// vendors' push services. No third-party notification service, no SDK: a P-256 VAPID
// key identifies this server, and each message is encrypted for one subscription.
type Push struct {
	dir  string
	key  *ecdsa.PrivateKey
	http *http.Client

	mu   sync.Mutex
	subs map[string]PushSub // endpoint -> subscription
	last map[string]time.Time
}

type PushSub struct {
	Endpoint string `json:"endpoint"`
	Keys     struct {
		P256dh string `json:"p256dh"`
		Auth   string `json:"auth"`
	} `json:"keys"`
	Kinds []string  `json:"kinds"` // event kinds to alert on
	Label string    `json:"label,omitempty"`
	Added time.Time `json:"added"`
}

// pushSubject is the VAPID "sub" claim: how a push service can reach the sender.
const pushSubject = "https://github.com/swantron/hometron"

var defaultAlertKinds = []string{"chime", "person"}

func loadPush(stateDir string) (*Push, error) {
	p := &Push{dir: stateDir, http: &http.Client{Timeout: 15 * time.Second}, subs: map[string]PushSub{}, last: map[string]time.Time{}}
	keyPath := filepath.Join(stateDir, "vapid.json")
	var stored struct {
		PKCS8 string `json:"pkcs8"`
	}
	if err := readJSON(keyPath, &stored); err == nil {
		der, err := base64.StdEncoding.DecodeString(stored.PKCS8)
		if err != nil {
			return nil, fmt.Errorf("vapid.json: %w", err)
		}
		k, err := x509.ParsePKCS8PrivateKey(der)
		if err != nil {
			return nil, fmt.Errorf("vapid.json: %w", err)
		}
		ek, ok := k.(*ecdsa.PrivateKey)
		if !ok {
			return nil, errors.New("vapid.json: not an ECDSA key")
		}
		p.key = ek
	} else if errors.Is(err, os.ErrNotExist) {
		// First run: mint the server's identity. Losing it just means phones re-subscribe.
		k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return nil, err
		}
		der, _ := x509.MarshalPKCS8PrivateKey(k)
		b, _ := json.Marshal(map[string]string{"pkcs8": base64.StdEncoding.EncodeToString(der)})
		if err := os.WriteFile(keyPath, b, 0o600); err != nil {
			return nil, err
		}
		p.key = k
	} else {
		return nil, err
	}
	_ = readJSON(filepath.Join(stateDir, "push.json"), &p.subs)
	return p, nil
}

// PublicKey is the applicationServerKey browsers subscribe with (uncompressed point, base64url).
func (p *Push) PublicKey() string {
	pub, _ := p.key.PublicKey.ECDH()
	return b64(pub.Bytes())
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func unb64(s string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "="))
}

// Only real push services get our POSTs; otherwise the subscribe API would let anyone
// on the tailnet make the server send requests to arbitrary internal URLs.
var pushHosts = []string{"fcm.googleapis.com", ".push.apple.com", "updates.push.services.mozilla.com", ".notify.windows.com"}

func validEndpoint(endpoint string) bool {
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Port() != "" {
		return false
	}
	for _, h := range pushHosts {
		if u.Hostname() == h || (strings.HasPrefix(h, ".") && strings.HasSuffix(u.Hostname(), h)) {
			return true
		}
	}
	return false
}

func (p *Push) Subscribe(s PushSub) error {
	if !validEndpoint(s.Endpoint) {
		return errors.New("not a known push service endpoint")
	}
	if k, err := unb64(s.Keys.P256dh); err != nil || len(k) != 65 {
		return errors.New("bad p256dh key")
	}
	if a, err := unb64(s.Keys.Auth); err != nil || len(a) != 16 {
		return errors.New("bad auth secret")
	}
	if len(s.Kinds) == 0 {
		s.Kinds = defaultAlertKinds
	}
	s.Added = time.Now()
	p.mu.Lock()
	p.subs[s.Endpoint] = s
	p.mu.Unlock()
	return p.save()
}

func (p *Push) Unsubscribe(endpoint string) error {
	p.mu.Lock()
	delete(p.subs, endpoint)
	p.mu.Unlock()
	return p.save()
}

func (p *Push) Get(endpoint string) (PushSub, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	s, ok := p.subs[endpoint]
	return s, ok
}

func (p *Push) save() error {
	p.mu.Lock()
	b, _ := json.MarshalIndent(p.subs, "", " ")
	p.mu.Unlock()
	return os.WriteFile(filepath.Join(p.dir, "push.json"), b, 0o600)
}

// PushMessage is what the service worker turns into a notification.
type PushMessage struct {
	Title string `json:"title"`
	Body  string `json:"body"`
	Tag   string `json:"tag,omitempty"` // same tag replaces the previous notification
	URL   string `json:"url"`
	Time  int64  `json:"ts"`
}

// personCooldown: a person walking around the yard is one alert, not six.
const personCooldown = 3 * time.Minute

// Notify alerts every subscription that wants this kind of event.
func (p *Push) Notify(ev Event) {
	var msg PushMessage
	switch ev.Kind {
	case "chime":
		msg = PushMessage{Title: "🔔 Doorbell", Body: "Someone's at the door · " + ev.Device}
	case "person":
		msg = PushMessage{Title: "Person", Body: "Seen on " + ev.Device}
	case "opened", "closed":
		msg = PushMessage{Title: "Garage " + ev.Kind, Body: ev.Device}
	default:
		msg = PushMessage{Title: ev.Kind, Body: ev.Device}
	}
	msg.Tag, msg.URL, msg.Time = ev.Kind+"-"+ev.DeviceID, "/", ev.Time.UnixMilli()

	p.mu.Lock()
	if ev.Kind == "person" {
		k := ev.Kind + ev.DeviceID
		if time.Since(p.last[k]) < personCooldown {
			p.mu.Unlock()
			return
		}
		p.last[k] = time.Now()
	}
	var targets []PushSub
	for _, s := range p.subs {
		for _, k := range s.Kinds {
			if k == ev.Kind {
				targets = append(targets, s)
				break
			}
		}
	}
	p.mu.Unlock()

	for _, s := range targets {
		go func(s PushSub) {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			if err := p.Send(ctx, s, msg); err != nil {
				log.Printf("push %s: %v", s.Label, err)
			}
		}(s)
	}
}

var errGone = errors.New("subscription expired")

// Send delivers one message. Subscriptions the push service reports gone are dropped.
func (p *Push) Send(ctx context.Context, s PushSub, msg PushMessage) error {
	payload, _ := json.Marshal(msg)
	body, err := encryptPush(s, payload)
	if err != nil {
		return err
	}
	auth, err := p.vapidHeader(s.Endpoint, time.Now())
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", s.Endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Encoding", "aes128gcm")
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("TTL", "300") // a 5-minute-old doorbell ring is still worth seeing; an hour-old one isn't
	req.Header.Set("Urgency", "high")
	req.Header.Set("Authorization", auth)
	resp, err := p.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
		_ = p.Unsubscribe(s.Endpoint)
		return errGone
	case resp.StatusCode/100 != 2:
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	return nil
}

// vapidHeader builds "vapid t=<ES256 JWT>, k=<public key>" (RFC 8292).
func (p *Push) vapidHeader(endpoint string, now time.Time) (string, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return "", err
	}
	header := b64([]byte(`{"typ":"JWT","alg":"ES256"}`))
	claims, _ := json.Marshal(map[string]any{
		"aud": u.Scheme + "://" + u.Host,
		"exp": now.Add(12 * time.Hour).Unix(),
		"sub": pushSubject,
	})
	signing := header + "." + b64(claims)
	h := sha256.Sum256([]byte(signing))
	r, s, err := ecdsa.Sign(rand.Reader, p.key, h[:])
	if err != nil {
		return "", err
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return "vapid t=" + signing + "." + b64(sig) + ", k=" + p.PublicKey(), nil
}

// encryptPush implements RFC 8291 (aes128gcm) for a single record.
func encryptPush(s PushSub, plaintext []byte) ([]byte, error) {
	uaRaw, err := unb64(s.Keys.P256dh)
	if err != nil {
		return nil, err
	}
	authSecret, err := unb64(s.Keys.Auth)
	if err != nil {
		return nil, err
	}
	uaPub, err := ecdh.P256().NewPublicKey(uaRaw)
	if err != nil {
		return nil, err
	}
	asPriv, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	shared, err := asPriv.ECDH(uaPub)
	if err != nil {
		return nil, err
	}
	asPub := asPriv.PublicKey().Bytes()
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	cek, nonce, err := pushKeys(shared, authSecret, uaRaw, asPub, salt)
	if err != nil {
		return nil, err
	}
	block, _ := aes.NewCipher(cek)
	gcm, _ := cipher.NewGCM(block)
	ct := gcm.Seal(nil, nonce, append(append([]byte{}, plaintext...), 0x02), nil) // 0x02: last record

	var out bytes.Buffer
	out.Write(salt)
	binary.Write(&out, binary.BigEndian, uint32(4096)) // record size
	out.WriteByte(byte(len(asPub)))
	out.Write(asPub)
	out.Write(ct)
	return out.Bytes(), nil
}

// pushKeys derives the content key and nonce (RFC 8291 section 3.4).
func pushKeys(shared, authSecret, uaPub, asPub, salt []byte) (cek, nonce []byte, err error) {
	prkKey, err := hkdf.Extract(sha256.New, shared, authSecret)
	if err != nil {
		return nil, nil, err
	}
	keyInfo := "WebPush: info\x00" + string(uaPub) + string(asPub)
	ikm, err := hkdf.Expand(sha256.New, prkKey, keyInfo, 32)
	if err != nil {
		return nil, nil, err
	}
	prk, err := hkdf.Extract(sha256.New, ikm, salt)
	if err != nil {
		return nil, nil, err
	}
	if cek, err = hkdf.Expand(sha256.New, prk, "Content-Encoding: aes128gcm\x00", 16); err != nil {
		return nil, nil, err
	}
	nonce, err = hkdf.Expand(sha256.New, prk, "Content-Encoding: nonce\x00", 12)
	return cek, nonce, err
}
