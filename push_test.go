package main

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"image/png"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// browser is a fake push subscriber holding the keys a real browser would keep.
type browser struct {
	priv *ecdh.PrivateKey
	auth []byte
}

func newBrowser(t *testing.T, endpoint string) (browser, PushSub) {
	t.Helper()
	priv, _ := ecdh.P256().GenerateKey(rand.Reader)
	auth := make([]byte, 16)
	rand.Read(auth)
	var s PushSub
	s.Endpoint = endpoint
	s.Keys.P256dh = b64(priv.PublicKey().Bytes())
	s.Keys.Auth = b64(auth)
	return browser{priv, auth}, s
}

// decrypt is RFC 8291 from the receiving side.
func (b browser) decrypt(t *testing.T, body []byte) []byte {
	t.Helper()
	salt, rs, idlen := body[:16], binary.BigEndian.Uint32(body[16:20]), int(body[20])
	if rs != 4096 || idlen != 65 {
		t.Fatalf("header rs=%d idlen=%d", rs, idlen)
	}
	asPub := body[21 : 21+idlen]
	ct := body[21+idlen:]
	pub, err := ecdh.P256().NewPublicKey(asPub)
	if err != nil {
		t.Fatal(err)
	}
	shared, _ := b.priv.ECDH(pub)
	cek, nonce, err := pushKeys(shared, b.auth, b.priv.PublicKey().Bytes(), asPub, salt)
	if err != nil {
		t.Fatal(err)
	}
	blk, _ := aes.NewCipher(cek)
	gcm, _ := cipher.NewGCM(blk)
	pt, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if pt[len(pt)-1] != 0x02 {
		t.Fatalf("missing last-record delimiter")
	}
	return pt[:len(pt)-1]
}

// RFC 8291 appendix A test vector: fixed keys must give the exact published CEK/nonce path.
func TestPushKeysRFC8291Vector(t *testing.T) {
	d := func(s string) []byte { b, _ := unb64(s); return b }
	uaPriv, _ := ecdh.P256().NewPrivateKey(d("q1dXpw3UpT5VOmu_cf_v6ih07Aems3njxI-JWgLcM94"))
	asPub := d("BP4z9KsN6nGRTbVYI_c7VJSPQTBtkgcy27mlmlMoZIIgDll6e3vCYLocInmYWAmS6TlzAC8wEqKK6PBru3jl7A8")
	authSecret := d("BTBZMqHH6r4Tts7J_aSIgg")
	salt := d("DGv6ra1nlYgDCS1FRnbzlw")
	pub, _ := ecdh.P256().NewPublicKey(asPub)
	shared, _ := uaPriv.ECDH(pub)
	cek, nonce, err := pushKeys(shared, authSecret, uaPriv.PublicKey().Bytes(), asPub, salt)
	if err != nil {
		t.Fatal(err)
	}
	blk, _ := aes.NewCipher(cek)
	gcm, _ := cipher.NewGCM(blk)
	ct := d("8pfeW0KbunFT06SuDKoJH9Ql87S1QUrdirN6GcG7sFz1y1sqLgVi1VhjVkHsUoEsbI_0LpXMuGvnzQ")
	pt, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		t.Fatalf("RFC vector did not decrypt: %v", err)
	}
	if got := string(pt[:len(pt)-1]); got != "When I grow up, I want to be a watermelon" {
		t.Errorf("plaintext = %q", got)
	}
}

func TestPushSendEndToEnd(t *testing.T) {
	var got struct {
		body    []byte
		headers http.Header
	}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.body, _ = io.ReadAll(r.Body)
		got.headers = r.Header.Clone()
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	p, err := loadPush(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p.http = srv.Client()
	b, sub := newBrowser(t, srv.URL+"/push/abc")
	msg := PushMessage{Title: "🔔 Doorbell", Body: "Someone's at the door", URL: "/"}
	if err := p.Send(context.Background(), sub, msg); err != nil {
		t.Fatal(err)
	}
	var back PushMessage
	if err := json.Unmarshal(b.decrypt(t, got.body), &back); err != nil || back.Title != msg.Title {
		t.Fatalf("round trip = %+v, %v", back, err)
	}
	if got.headers.Get("Content-Encoding") != "aes128gcm" || got.headers.Get("TTL") == "" {
		t.Errorf("headers = %v", got.headers)
	}

	// The VAPID JWT must verify against the public key in the same header.
	auth := got.headers.Get("Authorization")
	if !strings.HasPrefix(auth, "vapid t=") {
		t.Fatalf("auth = %q", auth)
	}
	parts := strings.SplitN(strings.TrimPrefix(auth, "vapid t="), ", k=", 2)
	jwt, k := parts[0], parts[1]
	seg := strings.Split(jwt, ".")
	claims, _ := unb64(seg[1])
	var c struct {
		Aud string
		Exp int64
		Sub string
	}
	json.Unmarshal(claims, &c)
	if c.Aud != srv.URL || c.Sub != pushSubject || c.Exp < time.Now().Unix() {
		t.Errorf("claims = %+v", c)
	}
	raw, _ := unb64(k)
	x, y := new(big.Int).SetBytes(raw[1:33]), new(big.Int).SetBytes(raw[33:])
	pub := &ecdsa.PublicKey{Curve: p.key.Curve, X: x, Y: y}
	sig, _ := unb64(seg[2])
	h := sha256.Sum256([]byte(seg[0] + "." + seg[1]))
	if !ecdsa.Verify(pub, h[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])) {
		t.Error("VAPID signature does not verify")
	}
}

func TestPushGoneSubscriptionDropped(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusGone) }))
	defer srv.Close()
	p, _ := loadPush(t.TempDir())
	p.http = srv.Client()
	_, sub := newBrowser(t, srv.URL+"/x")
	p.subs[sub.Endpoint] = sub
	if err := p.Send(context.Background(), sub, PushMessage{}); err != errGone {
		t.Fatalf("err = %v", err)
	}
	if _, ok := p.Get(sub.Endpoint); ok {
		t.Error("gone subscription kept")
	}
}

func TestPushKeyPersists(t *testing.T) {
	dir := t.TempDir()
	a, _ := loadPush(dir)
	b, _ := loadPush(dir)
	if a.PublicKey() != b.PublicKey() || len(a.PublicKey()) != 87 {
		t.Errorf("key changed across restarts or wrong size: %q", a.PublicKey())
	}
}

func TestSubscribeValidation(t *testing.T) {
	p, _ := loadPush(t.TempDir())
	_, good := newBrowser(t, "https://fcm.googleapis.com/fcm/send/abc")
	if err := p.Subscribe(good); err != nil {
		t.Fatalf("fcm rejected: %v", err)
	}
	if s, _ := p.Get(good.Endpoint); len(s.Kinds) != len(defaultAlertKinds) {
		t.Errorf("default kinds = %v", s.Kinds)
	}
	for _, ep := range []string{
		"http://fcm.googleapis.com/x",         // not https
		"https://192.168.86.1/x",              // internal host: would be SSRF
		"https://evil.test/x",                 // not a push service
		"https://fcm.googleapis.com:8443/x",   // odd port
		"https://notpush.apple.com.evil.test", // suffix trick
		"https://web.push.apple.com/abc",      // fine
	} {
		_, s := newBrowser(t, ep)
		err := p.Subscribe(s)
		if want := ep == "https://web.push.apple.com/abc"; (err == nil) != want {
			t.Errorf("%s: err = %v", ep, err)
		}
	}
	_, bad := newBrowser(t, "https://fcm.googleapis.com/x")
	bad.Keys.Auth = "short"
	if p.Subscribe(bad) == nil {
		t.Error("bad auth secret accepted")
	}
}

func TestAlertHook(t *testing.T) {
	e := newTestEvents(t)
	got := make(chan Event, 10)
	e.OnAlert = func(ev Event) { got <- ev }
	t0 := time.Now()
	e.post(Event{Time: t0, DeviceID: "cam", Device: "Cam", Kind: "motion"}, "th1")
	e.post(Event{Time: t0, DeviceID: "cam", Device: "Cam", Kind: "person"}, "th1") // same session, upgraded
	e.post(Event{Time: t0, DeviceID: "cam", Device: "Cam", Kind: "clip", Media: "x.mp4"}, "th1")
	kinds := []string{}
	timeout := time.After(time.Second)
	for len(kinds) < 2 {
		select {
		case ev := <-got:
			kinds = append(kinds, ev.Kind)
		case <-timeout:
			t.Fatalf("alerts = %v", kinds)
		}
	}
	select {
	case ev := <-got:
		t.Errorf("clip update should not alert again: %+v", ev)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestIcons(t *testing.T) {
	for _, n := range []int{180, 192, 512} {
		img, err := png.Decode(bytes.NewReader(iconPNG(n)))
		if err != nil || img.Bounds().Dx() != n {
			t.Fatalf("icon %d: %v", n, err)
		}
	}
}
