package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Doorbell chime on Google/Nest speakers, over the local Cast protocol (the same one a
// phone uses to send audio to them). No Google cloud, no Assistant: hometron serves a
// WAV on the LAN and tells the speaker to play it.
//
// Config: <state>/cast.json
//
//	{"speakers": ["Kitchen speaker"], "volume": 0.6, "say": "Someone's at the front door"}
//
// speakers are names as set in the Google Home app. volume (0-1) is used for the chime
// and the speaker's own level is put back afterwards. say is spoken after the chime with
// macOS's `say`; leave it out for the chime alone.
type CastConfig struct {
	Speakers []string `json:"speakers"`
	Volume   float64  `json:"volume,omitempty"`
	Say      string   `json:"say,omitempty"`
	Port     int      `json:"port,omitempty"` // LAN port the speakers fetch audio from (default 8081)
}

type Caster struct {
	cfg CastConfig
	lan *Network

	audio   []byte // the chime WAV, built once
	mediaIP string // our LAN address, as the speakers see it

	mu      sync.Mutex
	names   map[string]string // speaker name -> ip, from the last discovery
	found   time.Time
	ringing bool
	last    time.Time
}

func loadCaster(stateDir string, lan *Network) (*Caster, error) {
	b, err := os.ReadFile(filepath.Join(stateDir, "cast.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var cfg CastConfig
	if err := json.Unmarshal(b, &cfg); err != nil {
		return nil, fmt.Errorf("cast.json: %w", err)
	}
	if len(cfg.Speakers) == 0 {
		return nil, errors.New("cast.json: no speakers listed")
	}
	if cfg.Port == 0 {
		cfg.Port = 8081
	}
	if cfg.Volume < 0 || cfg.Volume > 1 {
		return nil, errors.New("cast.json: volume is 0 to 1")
	}
	return &Caster{cfg: cfg, lan: lan, audio: chimeWAV(cfg.Say)}, nil
}

// Serve listens on the LAN address for the speakers to fetch the chime. Only the audio
// is served here; the dashboard stays on Tailscale.
func (c *Caster) Serve(ctx context.Context) error {
	_, ip, _, err := lanInterface()
	if err != nil {
		return err
	}
	c.mediaIP = ip.String()
	ln, err := net.Listen("tcp", net.JoinHostPort(c.mediaIP, fmt.Sprint(c.cfg.Port)))
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /cast/chime.wav", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/wav")
		http.ServeContent(w, r, "chime.wav", time.Time{}, bytes.NewReader(c.audio))
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { <-ctx.Done(); srv.Close() }()
	log.Printf("cast: chime served to speakers from http://%s/cast/chime.wav", ln.Addr())
	return srv.Serve(ln)
}

// OnEvent rings the speakers for a doorbell press.
func (c *Caster) OnEvent(ev Event) {
	if ev.Kind != "chime" {
		return
	}
	c.mu.Lock()
	// One ring per press: Nest sometimes sends the chime twice, and a person holding the
	// button shouldn't loop the kitchen.
	if c.ringing || time.Since(c.last) < 20*time.Second {
		c.mu.Unlock()
		return
	}
	c.ringing, c.last = true, time.Now()
	c.mu.Unlock()
	defer func() { c.mu.Lock(); c.ringing = false; c.mu.Unlock() }()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := c.Ring(ctx); err != nil {
		log.Printf("cast: %v", err)
	}
}

// Ring plays the chime on every configured speaker at once.
func (c *Caster) Ring(ctx context.Context) error {
	if c.mediaIP == "" {
		return errors.New("chime audio isn't being served on the LAN")
	}
	ips, missing := c.resolve(ctx)
	if len(ips) == 0 {
		return fmt.Errorf("no speakers found (looked for %s)", strings.Join(missing, ", "))
	}
	if len(missing) > 0 {
		log.Printf("cast: speakers not found: %s", strings.Join(missing, ", "))
	}
	url := fmt.Sprintf("http://%s/cast/chime.wav", net.JoinHostPort(c.mediaIP, fmt.Sprint(c.cfg.Port)))
	var wg sync.WaitGroup
	errs := make([]error, len(ips))
	i := 0
	for name, ip := range ips {
		wg.Add(1)
		go func(i int, name, ip string) {
			defer wg.Done()
			if err := castPlay(ctx, ip, url, c.cfg.Volume); err != nil {
				errs[i] = fmt.Errorf("%s: %w", name, err)
			}
		}(i, name, ip)
		i++
	}
	wg.Wait()
	return errors.Join(errs...)
}

// Speakers lists what discovery found, for the setup hint.
func (c *Caster) Speakers(ctx context.Context) map[string]string {
	c.resolve(ctx)
	c.mu.Lock()
	defer c.mu.Unlock()
	out := map[string]string{}
	for k, v := range c.names {
		out[k] = v
	}
	return out
}

// resolve maps configured speaker names to IPs. Cast devices answer eureka_info with
// their Home-app name, so we ask every Google device the network scan found.
func (c *Caster) resolve(ctx context.Context) (map[string]string, []string) {
	c.mu.Lock()
	stale := time.Since(c.found) > 10*time.Minute
	c.mu.Unlock()
	if stale {
		names := discoverCast(ctx, c.lan.Last().Hosts)
		c.mu.Lock()
		c.names, c.found = names, time.Now()
		c.mu.Unlock()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	got, missing := map[string]string{}, []string{}
	for _, want := range c.cfg.Speakers {
		if ip, ok := c.names[strings.ToLower(want)]; ok {
			got[want] = ip
		} else {
			missing = append(missing, want)
		}
	}
	return got, missing
}

func discoverCast(ctx context.Context, hosts []NetHost) map[string]string {
	cl := &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{
		// Speakers present a self-signed certificate; this only reads the device's name.
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}}
	var mu sync.Mutex
	var wg sync.WaitGroup
	out := map[string]string{}
	for _, h := range hosts {
		if !strings.HasPrefix(h.Vendor, "Google") && !strings.HasPrefix(h.Vendor, "Nest") {
			continue
		}
		wg.Add(1)
		go func(ip string) {
			defer wg.Done()
			req, _ := http.NewRequestWithContext(ctx, "GET", "https://"+ip+":8443/setup/eureka_info?params=name", nil)
			res, err := cl.Do(req)
			if err != nil {
				return
			}
			defer res.Body.Close()
			var info struct{ Name string }
			if json.NewDecoder(io.LimitReader(res.Body, 64<<10)).Decode(&info) == nil && info.Name != "" {
				mu.Lock()
				out[strings.ToLower(info.Name)] = ip
				mu.Unlock()
			}
		}(h.IP)
	}
	wg.Wait()
	return out
}

// ---------- Cast v2 protocol ----------

const (
	nsConn      = "urn:x-cast:com.google.cast.tp.connection"
	nsHeartbeat = "urn:x-cast:com.google.cast.tp.heartbeat"
	nsReceiver  = "urn:x-cast:com.google.cast.receiver"
	nsMedia     = "urn:x-cast:com.google.cast.media"
	defaultApp  = "CC1AD845" // Default Media Receiver
)

type castConn struct {
	conn  net.Conn
	reqID int
}

// castMessage is the CastMessage protobuf, hand-encoded: six fields, all strings or
// enums, is not worth a protobuf dependency.
func (c *castConn) send(src, dst, ns string, payload any) error {
	b, _ := json.Marshal(payload)
	var m []byte
	m = append(m, 0x08, 0x00) // 1: protocol_version = CASTV2_1_0
	m = pbString(m, 2, src)
	m = pbString(m, 3, dst)
	m = pbString(m, 4, ns)
	m = append(m, 0x28, 0x00) // 5: payload_type = STRING
	m = pbString(m, 6, string(b))
	frame := binary.BigEndian.AppendUint32(nil, uint32(len(m)))
	_, err := c.conn.Write(append(frame, m...))
	return err
}

func pbString(b []byte, field int, s string) []byte {
	b = append(b, byte(field<<3|2))
	b = binary.AppendUvarint(b, uint64(len(s)))
	return append(b, s...)
}

type castMsg struct {
	Namespace string
	Payload   []byte
}

func (c *castConn) read() (castMsg, error) {
	var n uint32
	if err := binary.Read(c.conn, binary.BigEndian, &n); err != nil {
		return castMsg{}, err
	}
	if n > 1<<20 {
		return castMsg{}, errors.New("cast: oversized message")
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(c.conn, buf); err != nil {
		return castMsg{}, err
	}
	return parseCastMessage(buf)
}

func parseCastMessage(b []byte) (castMsg, error) {
	var m castMsg
	for len(b) > 0 {
		key, n := binary.Uvarint(b)
		if n <= 0 {
			return m, errors.New("cast: bad tag")
		}
		b = b[n:]
		field, wire := key>>3, key&7
		switch wire {
		case 0:
			_, n = binary.Uvarint(b)
			if n <= 0 {
				return m, errors.New("cast: bad varint")
			}
			b = b[n:]
		case 2:
			l, n := binary.Uvarint(b)
			if n <= 0 || uint64(len(b)-n) < l {
				return m, errors.New("cast: bad length")
			}
			v := b[n : n+int(l)]
			b = b[n+int(l):]
			switch field {
			case 4:
				m.Namespace = string(v)
			case 6:
				m.Payload = v
			}
		default:
			return m, fmt.Errorf("cast: unexpected wire type %d", wire)
		}
	}
	return m, nil
}

// castPlay plays url on the speaker at ip and waits for it to finish. If volume > 0 the
// speaker is set to it for the chime and put back to its old level afterwards.
// Whatever the speaker was playing stops: Cast has one app at a time.
func castPlay(ctx context.Context, ip, url string, volume float64) error {
	d := tls.Dialer{NetDialer: &net.Dialer{Timeout: 5 * time.Second},
		Config: &tls.Config{InsecureSkipVerify: true}} // speakers use self-signed certs
	nc, err := d.DialContext(ctx, "tcp", net.JoinHostPort(ip, "8009"))
	if err != nil {
		return err
	}
	defer nc.Close()
	if dl, ok := ctx.Deadline(); ok {
		nc.SetDeadline(dl)
	}
	c := &castConn{conn: nc}

	// Messages arrive on their own schedule; one reader hands them out by namespace.
	msgs := make(chan castMsg, 32)
	go func() {
		defer close(msgs)
		for {
			m, err := c.read()
			if err != nil {
				return
			}
			if m.Namespace == nsHeartbeat && bytes.Contains(m.Payload, []byte(`"PING"`)) {
				c.send("receiver-0", "receiver-0", nsHeartbeat, map[string]string{"type": "PONG"})
				continue
			}
			msgs <- m
		}
	}()
	// await returns the next message in ns whose type is one of types.
	await := func(ns string, types ...string) (map[string]json.RawMessage, error) {
		for {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case m, ok := <-msgs:
				if !ok {
					return nil, errors.New("speaker closed the connection")
				}
				if m.Namespace != ns {
					continue
				}
				var p map[string]json.RawMessage
				if json.Unmarshal(m.Payload, &p) != nil {
					continue
				}
				var t string
				json.Unmarshal(p["type"], &t)
				for _, want := range types {
					if t == want {
						return p, nil
					}
				}
			}
		}
	}
	next := func() int { c.reqID++; return c.reqID }

	const me = "sender-hometron"
	if err := c.send(me, "receiver-0", nsConn, map[string]string{"type": "CONNECT"}); err != nil {
		return err
	}
	c.send(me, "receiver-0", nsReceiver, map[string]any{"type": "GET_STATUS", "requestId": next()})
	st, err := await(nsReceiver, "RECEIVER_STATUS")
	if err != nil {
		return err
	}
	oldVol := receiverVolume(st)

	if volume > 0 {
		c.send(me, "receiver-0", nsReceiver, map[string]any{"type": "SET_VOLUME", "volume": map[string]float64{"level": volume}, "requestId": next()})
		defer func() {
			if oldVol >= 0 {
				c.send(me, "receiver-0", nsReceiver, map[string]any{"type": "SET_VOLUME", "volume": map[string]float64{"level": oldVol}, "requestId": next()})
				time.Sleep(300 * time.Millisecond) // let it land before the connection closes
			}
		}()
	}

	c.send(me, "receiver-0", nsReceiver, map[string]any{"type": "LAUNCH", "appId": defaultApp, "requestId": next()})
	var transport, session string
	for transport == "" {
		st, err := await(nsReceiver, "RECEIVER_STATUS", "LAUNCH_ERROR")
		if err != nil {
			return err
		}
		if _, bad := st["reason"]; bad {
			return fmt.Errorf("launch failed: %s", st["reason"])
		}
		transport, session = appTransport(st, defaultApp)
	}

	c.send(me, transport, nsConn, map[string]string{"type": "CONNECT"})
	c.send(me, transport, nsMedia, map[string]any{
		"type": "LOAD", "requestId": next(), "autoplay": true,
		"media": map[string]any{
			"contentId": url, "contentType": "audio/wav", "streamType": "BUFFERED",
			"metadata": map[string]any{"metadataType": 0, "title": "Doorbell"},
		},
	})
	// Played once it goes PLAYING then IDLE; LOAD_FAILED if the speaker couldn't fetch it.
	played := false
	for {
		m, err := await(nsMedia, "MEDIA_STATUS", "LOAD_FAILED", "LOAD_CANCELLED", "INVALID_REQUEST")
		if err != nil {
			return err
		}
		var t string
		json.Unmarshal(m["type"], &t)
		if t != "MEDIA_STATUS" {
			return fmt.Errorf("speaker refused the chime: %s", t)
		}
		state, reason := playerState(m)
		if state == "PLAYING" || state == "BUFFERING" {
			played = true
		}
		if state == "IDLE" && (played || reason != "") {
			if reason == "ERROR" {
				return errors.New("speaker could not play the chime")
			}
			break
		}
	}
	// Close the receiver app so the speaker goes back to its idle screen/state.
	c.send(me, "receiver-0", nsReceiver, map[string]any{"type": "STOP", "sessionId": session, "requestId": next()})
	return nil
}

func receiverVolume(st map[string]json.RawMessage) float64 {
	var s struct {
		Status struct {
			Volume struct {
				Level *float64 `json:"level"`
			} `json:"volume"`
		} `json:"status"`
	}
	b, _ := json.Marshal(st)
	if json.Unmarshal(b, &s) != nil || s.Status.Volume.Level == nil {
		return -1
	}
	return *s.Status.Volume.Level
}

func appTransport(st map[string]json.RawMessage, appID string) (transport, session string) {
	var s struct {
		Status struct {
			Applications []struct {
				AppID       string `json:"appId"`
				TransportID string `json:"transportId"`
				SessionID   string `json:"sessionId"`
			} `json:"applications"`
		} `json:"status"`
	}
	b, _ := json.Marshal(st)
	json.Unmarshal(b, &s)
	for _, a := range s.Status.Applications {
		if a.AppID == appID {
			return a.TransportID, a.SessionID
		}
	}
	return "", ""
}

func playerState(m map[string]json.RawMessage) (state, idleReason string) {
	var s []struct {
		PlayerState string `json:"playerState"`
		IdleReason  string `json:"idleReason"`
	}
	json.Unmarshal(m["status"], &s)
	if len(s) == 0 {
		return "", ""
	}
	return s[0].PlayerState, s[0].IdleReason
}

// ---------- the sound ----------

const chimeRate = 22050

// chimeWAV synthesizes a two-note "ding-dong" and appends a spoken line if say is set.
func chimeWAV(say string) []byte {
	pcm := chimePCM()
	if say != "" {
		if speech, err := speak(say); err == nil {
			pcm = append(pcm, make([]byte, chimeRate/4*2)...) // quarter-second pause
			pcm = append(pcm, speech...)
		} else {
			log.Printf("cast: say: %v (chime only)", err)
		}
	}
	return wav(pcm)
}

func chimePCM() []byte {
	notes := []struct{ freq, at float64 }{{659.25, 0}, {523.25, 0.55}} // E5 then C5
	total := int(2.4 * chimeRate)
	out := make([]byte, 0, total*2)
	for i := 0; i < total; i++ {
		t := float64(i) / chimeRate
		var v float64
		for _, n := range notes {
			dt := t - n.at
			if dt < 0 {
				continue
			}
			env := math.Exp(-dt*2.2) * math.Min(1, dt*200) // soft attack, bell-like decay
			v += env * (math.Sin(2*math.Pi*n.freq*dt) + 0.35*math.Sin(2*math.Pi*n.freq*2.01*dt) + 0.15*math.Sin(2*math.Pi*n.freq*3.02*dt))
		}
		s := int16(math.Max(-1, math.Min(1, v*0.45)) * 32767)
		out = binary.LittleEndian.AppendUint16(out, uint16(s))
	}
	return out
}

// speak renders text with macOS's built-in voice to raw 16-bit mono PCM at chimeRate.
func speak(text string) ([]byte, error) {
	f, err := os.CreateTemp("", "hometron-say-*.wav")
	if err != nil {
		return nil, err
	}
	f.Close()
	defer os.Remove(f.Name())
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/usr/bin/say", "-o", f.Name(), "--file-format=WAVE", fmt.Sprintf("--data-format=LEI16@%d", chimeRate), "--", text)
	if out, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("%v: %s", err, out)
	}
	b, err := os.ReadFile(f.Name())
	if err != nil {
		return nil, err
	}
	return wavData(b)
}

// wavData returns the samples in a RIFF/WAVE file's data chunk.
func wavData(b []byte) ([]byte, error) {
	if len(b) < 12 || string(b[:4]) != "RIFF" || string(b[8:12]) != "WAVE" {
		return nil, errors.New("not a WAV file")
	}
	for p := 12; p+8 <= len(b); {
		id, n := string(b[p:p+4]), int(binary.LittleEndian.Uint32(b[p+4:]))
		p += 8
		if p+n > len(b) {
			n = len(b) - p
		}
		if id == "data" {
			return b[p : p+n], nil
		}
		p += n + n%2
	}
	return nil, errors.New("WAV has no data chunk")
}

func wav(pcm []byte) []byte {
	var b bytes.Buffer
	w := func(v any) { binary.Write(&b, binary.LittleEndian, v) }
	b.WriteString("RIFF")
	w(uint32(36 + len(pcm)))
	b.WriteString("WAVEfmt ")
	w(uint32(16))
	w(uint16(1)) // PCM
	w(uint16(1)) // mono
	w(uint32(chimeRate))
	w(uint32(chimeRate * 2))
	w(uint16(2))
	w(uint16(16))
	b.WriteString("data")
	w(uint32(len(pcm)))
	b.Write(pcm)
	return b.Bytes()
}
