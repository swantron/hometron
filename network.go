package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Network lists what's alive on the server's own LAN: IP, MAC, maker, mDNS name, and
// whether it answers like a Tailwind. Mostly a debugging aid for "why can't hometron
// see device X": it only sees the subnet the server's Wi-Fi is on, so a device behind
// a second router shows up as that router (its WAN side), not as itself.
type Network struct {
	dir string // state dir: oui cache + seen history

	mu      sync.Mutex
	last    NetScan
	seen    map[string]NetHost // mac -> history
	oui     map[string]string  // "AABBCC" -> vendor
	running bool
}

type NetScan struct {
	Interface string    `json:"interface"`
	Address   string    `json:"address"` // server's own ip/prefix
	Gateway   string    `json:"gateway"`
	At        time.Time `json:"at"`
	Took      string    `json:"took"`
	Hosts     []NetHost `json:"hosts"`
	Gone      []NetHost `json:"gone"` // seen before, not answering now
	Error     string    `json:"error,omitempty"`
}

type NetHost struct {
	IP        string    `json:"ip"`
	MAC       string    `json:"mac"`
	Vendor    string    `json:"vendor,omitempty"`
	Name      string    `json:"name,omitempty"`
	Web       bool      `json:"web,omitempty"`      // something answers on :80
	Tailwind  bool      `json:"tailwind,omitempty"` // answered the Tailwind /json API
	Private   bool      `json:"private,omitempty"`  // randomized MAC (phones, laptops)
	Self      bool      `json:"self,omitempty"`
	Gateway   bool      `json:"gateway,omitempty"`
	FirstSeen time.Time `json:"firstSeen"`
	LastSeen  time.Time `json:"lastSeen"`
}

func newNetwork(stateDir string) *Network {
	n := &Network{dir: stateDir, seen: map[string]NetHost{}}
	if b, err := os.ReadFile(filepath.Join(stateDir, "network-seen.json")); err == nil {
		_ = json.Unmarshal(b, &n.seen)
	}
	return n
}

// Last returns the most recent scan (zero value if none yet).
func (n *Network) Last() NetScan {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.last
}

// Run scans now and then every interval, so "gone" and "last seen" mean something
// for flaky devices.
func (n *Network) Run(ctx context.Context, every time.Duration) {
	for t := time.NewTicker(every); ctx.Err() == nil; <-t.C {
		n.Scan(ctx)
	}
}

// Scan sweeps the LAN. Concurrent callers get the scan already in progress.
func (n *Network) Scan(ctx context.Context) NetScan {
	n.mu.Lock()
	if n.running {
		n.mu.Unlock()
		for i := 0; i < 60; i++ { // wait for the in-flight one
			time.Sleep(500 * time.Millisecond)
			n.mu.Lock()
			done := !n.running
			n.mu.Unlock()
			if done {
				break
			}
		}
		return n.Last()
	}
	n.running = true
	n.mu.Unlock()

	s := n.scan(ctx)

	n.mu.Lock()
	n.last, n.running = s, false
	b, _ := json.MarshalIndent(n.seen, "", " ")
	n.mu.Unlock()
	_ = os.WriteFile(filepath.Join(n.dir, "network-seen.json"), b, 0o600)
	return s
}

func (n *Network) scan(ctx context.Context) NetScan {
	start := time.Now()
	s := NetScan{At: start}
	ifName, self, ipnet, err := lanInterface()
	if err != nil {
		s.Error = err.Error()
		return s
	}
	s.Interface, s.Address, s.Gateway = ifName, ipnet.String(), defaultGateway()
	ones, _ := ipnet.Mask.Size()
	if ones < 22 {
		s.Error = fmt.Sprintf("subnet %s is too big to sweep", ipnet)
		return s
	}

	// Poke every address so the kernel ARPs for it. A TCP dial to :80 doubles as the
	// "has a web server" check. Two passes: sleepy Wi-Fi gadgets often miss the first.
	ips := hostsIn(ipnet)
	web := map[string]bool{}
	var wmu sync.Mutex
	for pass := 0; pass < 2; pass++ {
		var wg sync.WaitGroup
		sem := make(chan struct{}, 128)
		for _, ip := range ips {
			if ip.Equal(self) {
				continue
			}
			wg.Add(1)
			sem <- struct{}{}
			go func(ip string) {
				defer func() { <-sem; wg.Done() }()
				d := net.Dialer{Timeout: 800 * time.Millisecond}
				if c, err := d.DialContext(ctx, "tcp", ip+":80"); err == nil {
					c.Close()
					wmu.Lock()
					web[ip] = true
					wmu.Unlock()
				}
				if c, err := net.Dial("udp", ip+":9"); err == nil { // discard port; just triggers ARP
					c.Write([]byte{0})
					c.Close()
				}
			}(ip.String())
		}
		wg.Wait()
		time.Sleep(300 * time.Millisecond)
	}

	arp, err := readARP(ifName)
	if err != nil {
		s.Error = "arp: " + err.Error()
		return s
	}
	ownMAC := ""
	if ifc, err := net.InterfaceByName(ifName); err == nil {
		ownMAC = normMAC(ifc.HardwareAddr.String())
	}
	arp[self.String()] = ownMAC

	ipList := make([]string, 0, len(arp))
	for ip := range arp {
		ipList = append(ipList, ip)
	}
	names := mdnsNames(ctx, ipList, 2*time.Second)
	tw := probeTailwind(ctx, keysOf(web))
	oui := n.vendors(ctx)

	now := time.Now()
	n.mu.Lock()
	alive := map[string]bool{}
	for ip, mac := range arp {
		h := NetHost{IP: ip, MAC: mac, Name: names[ip], Web: web[ip], Tailwind: tw[ip],
			Self: ip == self.String(), Gateway: ip == s.Gateway, LastSeen: now, FirstSeen: now}
		h.Private = isRandomMAC(mac)
		if !h.Private {
			h.Vendor = oui[strings.ToUpper(strings.ReplaceAll(mac, ":", ""))[:6]]
		}
		if old, ok := n.seen[mac]; ok {
			h.FirstSeen = old.FirstSeen
			if h.Name == "" {
				h.Name = old.Name
			}
		}
		n.seen[mac] = h
		alive[mac] = true
		s.Hosts = append(s.Hosts, h)
	}
	for mac, old := range n.seen {
		if !alive[mac] && now.Sub(old.LastSeen) < 14*24*time.Hour {
			g := old
			g.Self, g.Gateway = false, false
			s.Gone = append(s.Gone, g)
		}
	}
	n.mu.Unlock()

	sort.Slice(s.Hosts, func(i, j int) bool { return ipLess(s.Hosts[i].IP, s.Hosts[j].IP) })
	sort.Slice(s.Gone, func(i, j int) bool { return s.Gone[i].LastSeen.After(s.Gone[j].LastSeen) })
	s.Took = time.Since(start).Round(100 * time.Millisecond).String()
	log.Printf("network: %d hosts on %s in %s", len(s.Hosts), s.Address, s.Took)
	return s
}

// lanInterface picks the interface holding the default route's IPv4 address
// (not Tailscale, not loopback).
func lanInterface() (string, net.IP, *net.IPNet, error) {
	ifs, err := net.Interfaces()
	if err != nil {
		return "", nil, nil, err
	}
	gw := net.ParseIP(defaultGateway())
	var fallback struct {
		name string
		ip   net.IP
		net  *net.IPNet
	}
	for _, ifc := range ifs {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 || strings.HasPrefix(ifc.Name, "utun") {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok || ipn.IP.To4() == nil || !ipn.IP.IsPrivate() {
				continue
			}
			nw := &net.IPNet{IP: ipn.IP.Mask(ipn.Mask), Mask: ipn.Mask}
			if gw != nil && nw.Contains(gw) {
				return ifc.Name, ipn.IP.To4(), nw, nil
			}
			if fallback.name == "" {
				fallback.name, fallback.ip, fallback.net = ifc.Name, ipn.IP.To4(), nw
			}
		}
	}
	if fallback.name == "" {
		return "", nil, nil, errors.New("no private IPv4 LAN interface")
	}
	return fallback.name, fallback.ip, fallback.net, nil
}

func defaultGateway() string {
	out, err := exec.Command("/sbin/route", "-n", "get", "default").Output()
	if err != nil {
		return ""
	}
	for _, l := range strings.Split(string(out), "\n") {
		if f := strings.Fields(l); len(f) == 2 && f[0] == "gateway:" {
			return f[1]
		}
	}
	return ""
}

func hostsIn(n *net.IPNet) []net.IP {
	base := binary.BigEndian.Uint32(n.IP.To4())
	ones, bits := n.Mask.Size()
	size := uint32(1) << (bits - ones)
	var out []net.IP
	for i := uint32(1); i < size-1; i++ {
		ip := make(net.IP, 4)
		binary.BigEndian.PutUint32(ip, base+i)
		out = append(out, ip)
	}
	return out
}

var arpLine = regexp.MustCompile(`\((\d+\.\d+\.\d+\.\d+)\) at ([0-9a-fA-F:]+) on (\S+)`)

// readARP parses `arp -an` (macOS) for one interface: ip -> mac.
func readARP(ifName string) (map[string]string, error) {
	out, err := exec.Command("/usr/sbin/arp", "-an").Output()
	if err != nil {
		return nil, err
	}
	return parseARP(string(out), ifName), nil
}

func parseARP(out, ifName string) map[string]string {
	m := map[string]string{}
	for _, l := range strings.Split(out, "\n") {
		sm := arpLine.FindStringSubmatch(l)
		if sm == nil || sm[3] != ifName {
			continue
		}
		ip := net.ParseIP(sm[1]).To4()
		if ip == nil || ip[3] == 255 || ip.IsMulticast() || ip.IsLinkLocalUnicast() {
			continue
		}
		m[sm[1]] = normMAC(sm[2])
	}
	return m
}

// normMAC turns macOS's "0:a:c4:..." into "00:0a:c4:...".
func normMAC(s string) string {
	parts := strings.Split(strings.ToLower(s), ":")
	if len(parts) != 6 {
		return strings.ToLower(s)
	}
	for i, p := range parts {
		if len(p) == 1 {
			parts[i] = "0" + p
		}
	}
	return strings.Join(parts, ":")
}

// isRandomMAC: the locally-administered bit is set on phones/laptops using private Wi-Fi addresses.
func isRandomMAC(mac string) bool {
	var b byte
	_, err := fmt.Sscanf(mac[:2], "%02x", &b)
	return err == nil && b&0x02 != 0
}

func ipLess(a, b string) bool {
	x, y := net.ParseIP(a).To4(), net.ParseIP(b).To4()
	if x == nil || y == nil {
		return a < b
	}
	return binary.BigEndian.Uint32(x) < binary.BigEndian.Uint32(y)
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// probeTailwind asks each web host for Tailwind status with a dummy key. A Tailwind
// answers {"result":"token fail"} (older firmware) or {"result":"Fail","info":"token check fail"}
// (10.4x, with HTTP 400); anything else won't speak this dialect.
func probeTailwind(ctx context.Context, ips []string) map[string]bool {
	found := map[string]bool{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	c := &http.Client{Timeout: 2 * time.Second}
	for _, ip := range ips {
		wg.Add(1)
		go func(ip string) {
			defer wg.Done()
			body := `{"version":"0.1","data":{"type":"get","name":"dev_st"}}`
			req, _ := http.NewRequestWithContext(ctx, "POST", "http://"+ip+"/json", strings.NewReader(body))
			req.Header.Set("TOKEN", "000000")
			resp, err := c.Do(req)
			if err != nil {
				return
			}
			defer resp.Body.Close()
			var r struct {
				Result string `json:"result"`
			}
			if json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&r) == nil && (r.Result == "OK" || r.Result == "Fail" || r.Result == "token fail") {
				mu.Lock()
				found[ip] = true
				mu.Unlock()
			}
		}(ip)
	}
	wg.Wait()
	return found
}

// ---- mDNS reverse lookups (RFC 6762 one-shot queries; replies come back unicast) ----

func mdnsNames(ctx context.Context, ips []string, wait time.Duration) map[string]string {
	names := map[string]string{}
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{})
	if err != nil {
		return names
	}
	defer conn.Close()
	dst := &net.UDPAddr{IP: net.IPv4(224, 0, 0, 251), Port: 5353}
	want := map[string]string{} // reverse name -> ip
	for _, ip := range ips {
		p := strings.Split(ip, ".")
		if len(p) != 4 {
			continue
		}
		rev := p[3] + "." + p[2] + "." + p[1] + "." + p[0] + ".in-addr.arpa"
		want[rev] = ip
		conn.WriteTo(dnsQuery(rev, 12), dst)
	}
	conn.SetReadDeadline(time.Now().Add(wait))
	buf := make([]byte, 9000)
	for ctx.Err() == nil {
		n, _, err := conn.ReadFrom(buf)
		if err != nil {
			break
		}
		for owner, target := range parsePTRs(buf[:n]) {
			if ip, ok := want[strings.ToLower(owner)]; ok {
				names[ip] = strings.TrimSuffix(target, ".")
			}
		}
	}
	return names
}

func dnsQuery(name string, qtype uint16) []byte {
	var b bytes.Buffer
	b.Write([]byte{0, 0, 0, 0, 0, 1, 0, 0, 0, 0, 0, 0}) // id 0, flags 0, 1 question
	for _, l := range strings.Split(name, ".") {
		b.WriteByte(byte(len(l)))
		b.WriteString(l)
	}
	b.WriteByte(0)
	binary.Write(&b, binary.BigEndian, qtype)
	binary.Write(&b, binary.BigEndian, uint16(0x8001)) // class IN, unicast-response bit
	return b.Bytes()
}

// parsePTRs returns owner -> target for every PTR record in a DNS message.
func parsePTRs(msg []byte) map[string]string {
	out := map[string]string{}
	if len(msg) < 12 {
		return out
	}
	qd := int(binary.BigEndian.Uint16(msg[4:]))
	rr := int(binary.BigEndian.Uint16(msg[6:])) + int(binary.BigEndian.Uint16(msg[8:])) + int(binary.BigEndian.Uint16(msg[10:]))
	off := 12
	for i := 0; i < qd; i++ {
		_, n, ok := readName(msg, off)
		if !ok || n+4 > len(msg) {
			return out
		}
		off = n + 4
	}
	for i := 0; i < rr; i++ {
		owner, n, ok := readName(msg, off)
		if !ok || n+10 > len(msg) {
			return out
		}
		typ := binary.BigEndian.Uint16(msg[n:])
		rdlen := int(binary.BigEndian.Uint16(msg[n+8:]))
		rd := n + 10
		if rd+rdlen > len(msg) {
			return out
		}
		if typ == 12 {
			if target, _, ok := readName(msg, rd); ok {
				out[owner] = target
			}
		}
		off = rd + rdlen
	}
	return out
}

// readName decodes a (possibly compressed) DNS name at off; returns the name and the offset after it.
func readName(msg []byte, off int) (string, int, bool) {
	var labels []string
	end := -1
	for hops := 0; hops < 32; hops++ {
		if off >= len(msg) {
			return "", 0, false
		}
		l := int(msg[off])
		switch {
		case l == 0:
			if end < 0 {
				end = off + 1
			}
			return strings.Join(labels, "."), end, true
		case l&0xC0 == 0xC0:
			if off+1 >= len(msg) {
				return "", 0, false
			}
			if end < 0 {
				end = off + 2
			}
			off = int(binary.BigEndian.Uint16(msg[off:]) & 0x3FFF)
		default:
			if off+1+l > len(msg) {
				return "", 0, false
			}
			labels = append(labels, string(msg[off+1:off+1+l]))
			off += 1 + l
		}
	}
	return "", 0, false
}

// ---- MAC vendors ----

// manufURL is Wireshark's OUI table: plain text, tab-separated, refreshed nightly.
const manufURL = "https://www.wireshark.org/download/automated/data/manuf"

// vendors loads the OUI table, downloading it at most once a month into the state dir.
func (n *Network) vendors(ctx context.Context) map[string]string {
	n.mu.Lock()
	if n.oui != nil {
		defer n.mu.Unlock()
		return n.oui
	}
	n.mu.Unlock()

	path := filepath.Join(n.dir, "manuf.txt")
	if fi, err := os.Stat(path); err != nil || time.Since(fi.ModTime()) > 30*24*time.Hour {
		if err := download(ctx, manufURL, path); err != nil {
			log.Printf("network: vendor table: %v", err)
		}
	}
	m := map[string]string{}
	if f, err := os.Open(path); err == nil {
		m = parseManuf(f)
		f.Close()
	}
	n.mu.Lock()
	n.oui = m
	n.mu.Unlock()
	return m
}

func download(ctx context.Context, url, path string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("%s: %s", url, resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 20<<20))
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}

// parseManuf reads lines like "24:0A:C4   \tEspressif   \tEspressif Inc." (24-bit prefixes only).
func parseManuf(r io.Reader) map[string]string {
	m := map[string]string{}
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		l := sc.Text()
		if l == "" || l[0] == '#' {
			continue
		}
		f := strings.Split(l, "\t")
		for i := range f {
			f[i] = strings.TrimSpace(f[i]) // columns are space-padded before the tab
		}
		if len(f) < 2 || len(f[0]) != 8 {
			continue
		}
		name := f[1]
		if len(f) >= 3 && f[2] != "" {
			name = f[2]
		}
		m[strings.ToUpper(strings.ReplaceAll(f[0], ":", ""))] = name
	}
	return m
}
