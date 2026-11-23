package main

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
)

func TestParseARP(t *testing.T) {
	out := `? (192.168.0.1) at 00:11:22:33:44:55 on en0 ifscope [ethernet]
? (192.168.0.11) at 00:11:22:4:a:d8 on en0 ifscope [ethernet]
? (192.168.0.12) at (incomplete) on en0 ifscope [ethernet]
? (169.254.1.2) at 02:00:00:00:00:01 on en0 [ethernet]
? (100.64.0.1) at 1:2:3:4:5:6 on utun3 [ethernet]
mdns.mcast.net (224.0.0.251) at 1:0:5e:0:0:fb on en0 ifscope permanent [ethernet]
? (192.168.0.255) at ff:ff:ff:ff:ff:ff on en0 ifscope [ethernet]`
	got := parseARP(out, "en0")
	want := map[string]string{"192.168.0.1": "00:11:22:33:44:55", "192.168.0.11": "00:11:22:04:0a:d8"}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
}

func TestRandomMAC(t *testing.T) {
	if !isRandomMAC("de:00:00:00:00:02") || isRandomMAC("24:0a:c4:00:00:01") {
		t.Error("locally-administered bit check is wrong")
	}
}

func TestParseManuf(t *testing.T) {
	m := parseManuf(strings.NewReader("# comment\n24:0A:C4\tEspressif\tEspressif Inc.\n00:1B:C5:00:00/36\tX\tY\nAC:DE:48\tPrivate\n28:BD:89         \tGoogle      \tGoogle, Inc.\n"))
	if m["240AC4"] != "Espressif Inc." || m["ACDE48"] != "Private" || m["28BD89"] != "Google, Inc." || len(m) != 3 {
		t.Errorf("manuf = %v", m)
	}
}

func TestPTRRoundTrip(t *testing.T) {
	q := dnsQuery("57.0.168.192.in-addr.arpa", 12)
	// Build a reply: header with 1 question + 1 answer, the question copied, then an answer
	// whose owner is a compression pointer back to the question name (offset 12).
	var b bytes.Buffer
	b.Write([]byte{0, 0, 0x84, 0, 0, 1, 0, 1, 0, 0, 0, 0})
	b.Write(q[12:])
	b.Write([]byte{0xC0, 12})
	binary.Write(&b, binary.BigEndian, []uint16{12, 1})
	binary.Write(&b, binary.BigEndian, uint32(120))
	target := []byte("\x0ftailwind-abc123\x05local\x00")
	binary.Write(&b, binary.BigEndian, uint16(len(target)))
	b.Write(target)

	got := parsePTRs(b.Bytes())
	if got["57.0.168.192.in-addr.arpa"] != "tailwind-abc123.local" {
		t.Errorf("ptrs = %v", got)
	}
	if len(parsePTRs([]byte{0xC0, 0xC0, 1})) != 0 || len(parsePTRs(b.Bytes()[:30])) != 0 {
		t.Error("truncated input should parse to nothing")
	}
}
