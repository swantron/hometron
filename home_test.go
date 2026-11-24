package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"testing"
	"time"
)

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.ParseInLocation("2006-01-02 15:04", s, time.Local)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

const testICS = "BEGIN:VCALENDAR\r\n" +
	"BEGIN:VEVENT\r\nUID:once\r\nDTSTART:20261124T170000Z\r\nDTEND:20261124T180000Z\r\nSUMMARY:Dentist\\, Joe\r\nEND:VEVENT\r\n" +
	"BEGIN:VEVENT\r\nUID:allday\r\nDTSTART;VALUE=DATE:20261126\r\nDTEND;VALUE=DATE:20261127\r\nSUMMARY:Thanksgiving\r\nEND:VEVENT\r\n" +
	// Weekly Mon/Wed soccer in Denver time, with one week skipped and one moved.
	"BEGIN:VEVENT\r\nUID:soccer\r\nDTSTART;TZID=America/Denver:20260105T160000\r\nDTEND;TZID=America/Denver:20260105T170000\r\n" +
	"RRULE:FREQ=WEEKLY;BYDAY=MO,WE\r\nEXDATE;TZID=America/Denver:20261125T160000\r\nSUMMARY:Soccer\r\nEND:VEVENT\r\n" +
	"BEGIN:VEVENT\r\nUID:soccer\r\nRECURRENCE-ID;TZID=America/Denver:20261130T160000\r\nDTSTART;TZID=America/Denver:20261130T180000\r\n" +
	"DTEND;TZID=America/Denver:20261130T190000\r\nSUMMARY:Soccer (late)\r\nEND:VEVENT\r\n" +
	// Daily since 2015: must still reach this week without walking 4000 days.
	"BEGIN:VEVENT\r\nUID:pills\r\nDTSTART;TZID=America/Denver:20150101T080000\r\nDURATION:PT15M\r\nRRULE:FREQ=DAILY\r\n" +
	"SUMMARY:Pills\r\nEND:VEVENT\r\n" +
	// Monthly on the last Friday, ended before this week.
	"BEGIN:VEVENT\r\nUID:old\r\nDTSTART:20250131T150000Z\r\nRRULE:FREQ=MONTHLY;BYDAY=-1FR;UNTIL=20260101T000000Z\r\nSUMMARY:Old\r\nEND:VEVENT\r\n" +
	"BEGIN:VEVENT\r\nUID:x\r\nDTSTART:20261124T200000Z\r\nSTATUS:CANCELLED\r\nSUMMARY:Cancelled\r\nEND:VEVENT\r\n" +
	"END:VCALENDAR\r\n"

func TestParseICS(t *testing.T) {
	den, err := time.LoadLocation("America/Denver")
	if err != nil {
		t.Skip("no tzdata")
	}
	from := time.Date(2026, 11, 23, 0, 0, 0, 0, den)
	evs, err := parseICS(strings.NewReader(testICS), from, from.AddDate(0, 0, 8))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int{}
	var soccer []string
	for _, e := range evs {
		got[e.Title]++
		if strings.HasPrefix(e.Title, "Soccer") {
			soccer = append(soccer, e.Start.In(den).Format("Mon 15:04"))
		}
	}
	if got["Dentist, Joe"] != 1 || got["Thanksgiving"] != 1 || got["Pills"] != 8 || got["Old"] != 0 || got["Cancelled"] != 0 {
		t.Errorf("counts = %v", got)
	}
	// Mon 23rd, (Wed 25th skipped), Mon 30th moved to 18:00.
	if strings.Join(soccer, ",") != "Mon 16:00,Mon 18:00" {
		t.Errorf("soccer = %v", soccer)
	}
	for _, e := range evs {
		if e.Title == "Thanksgiving" && (!e.AllDay || e.End.Sub(e.Start) != 24*time.Hour) {
			t.Errorf("all-day = %+v", e)
		}
		if e.Title == "Pills" && e.Start.In(den).Hour() != 8 {
			t.Errorf("pills drifted: %v", e.Start.In(den))
		}
	}
}

func TestMonthlyNthWeekday(t *testing.T) {
	start := time.Date(2026, 1, 13, 19, 0, 0, 0, time.UTC) // 2nd Tuesday
	got := expandRRule(start, "FREQ=MONTHLY;BYDAY=2TU;COUNT=3", start, start.AddDate(1, 0, 0))
	want := []string{"2026-01-13", "2026-02-10", "2026-03-10"}
	if len(got) != 3 {
		t.Fatalf("got %v", got)
	}
	for i := range want {
		if got[i].Format("2006-01-02") != want[i] {
			t.Errorf("%d: %v", i, got[i])
		}
	}
}

func TestChoreDays(t *testing.T) {
	chores := []Chore{{Name: "Trash", Weekday: "Tuesday"}, {Name: "Recycling", Weekday: "tuesday", Weeks: 2, From: "2026-12-01"}}
	from := mustTime(t, "2026-11-23 00:00")
	var got []string
	for _, e := range choreDays(chores, from, from.AddDate(0, 0, 21)) {
		got = append(got, e.Title+" "+e.Start.Format("01-02"))
	}
	want := "Trash 11-24,Trash 12-01,Trash 12-08,Recycling 12-01"
	if strings.Join(got, ",") != want {
		t.Errorf("got %v", got)
	}
}

func TestChoreReminder(t *testing.T) {
	e := newTestEvents(t)
	c := &Calendar{cfg: CalendarConfig{Chores: []Chore{{Name: "Trash", Weekday: "Tuesday"}}, Remind: "19:00"}, events: e, reminded: map[string]bool{}}
	c.remind(mustTime(t, "2026-11-23 18:59"))
	c.remind(mustTime(t, "2026-11-23 19:01"))
	c.remind(mustTime(t, "2026-11-23 19:05"))
	c.remind(mustTime(t, "2026-11-24 19:01")) // Tuesday evening: nothing due Wednesday
	r := e.Recent()
	if len(r) != 1 || r[0].Kind != "reminder" || r[0].Device != "Trash goes out tonight" {
		t.Errorf("feed = %+v", r)
	}
	// With a pickup time it's a timed entry and the reminder says when.
	c.cfg.Chores[0].At = "08:00"
	c.reminded = map[string]bool{}
	c.remind(mustTime(t, "2026-11-30 19:00"))
	if r := e.Recent(); r[0].Device != "Trash goes out tonight (pickup 8:00 AM)" {
		t.Errorf("feed = %+v", r[0])
	}
	if d := choreDays(c.cfg.Chores, mustTime(t, "2026-12-01 00:00"), mustTime(t, "2026-12-02 00:00")); len(d) != 1 || d[0].AllDay || d[0].Start.Hour() != 8 {
		t.Errorf("timed chore = %+v", d)
	}
}

func TestLaundryDone(t *testing.T) {
	e := newTestEvents(t)
	l := &Laundry{events: e, machines: map[string]*machine{}}
	now := time.Now()
	steps := []string{"POWER_OFF", "RUNNING", "RINSING", "PAUSE", "SPINNING", "END", "END", "POWER_OFF"}
	for i, s := range steps {
		l.update("w1", "Washer", "washer", s, 10, now.Add(time.Duration(i)*time.Minute))
	}
	// A dryer that goes straight into wrinkle care is done then, not when it powers off.
	for i, s := range []string{"DRYING", "COOLING", "WRINKLE_CARE", "END", "POWER_OFF"} {
		l.update("d1", "Dryer", "dryer", s, 5, now.Add(time.Duration(i)*time.Minute))
	}
	var done []string
	for _, ev := range e.Recent() {
		done = append(done, ev.Device+":"+ev.Kind)
	}
	if strings.Join(done, ",") != "Dryer:done,Washer:done" {
		t.Errorf("events = %v", done)
	}
}

func TestParseLaundryState(t *testing.T) {
	washer := `[{"location":{"locationName":"MINI"},"runState":{"currentState":"POWER_OFF"}},
		{"location":{"locationName":"MAIN"},"runState":{"currentState":"RINSING"},"timer":{"remainHour":0,"remainMinute":34}}]`
	if s, m := parseLaundryState(json.RawMessage(washer)); s != "RINSING" || m != 34 {
		t.Errorf("washer = %s %d", s, m)
	}
	dryer := `{"runState":{"currentState":"DRYING"},"timer":{"remainHour":1,"remainMinute":5}}`
	if s, m := parseLaundryState(json.RawMessage(dryer)); s != "DRYING" || m != 65 {
		t.Errorf("dryer = %s %d", s, m)
	}
}

func TestRuntime(t *testing.T) {
	at := func(s string) time.Time { return mustTime(t, s) }
	samples := []Sample{
		{T: at("2026-11-22 23:00"), Hvac: "HEATING"}, // carries over midnight
		{T: at("2026-11-23 00:30"), Hvac: "OFF"},
		{T: at("2026-11-23 06:00"), Hvac: "HEATING"},
		{T: at("2026-11-23 06:45"), Hvac: "OFF"},
	}
	heat, cool := Runtime(samples, nil, at("2026-11-23 00:00"), at("2026-11-23 12:00"))
	if heat != 75*time.Minute || cool != 0 {
		t.Errorf("heat=%v cool=%v", heat, cool)
	}
}

func TestHistoryRecordsChanges(t *testing.T) {
	dir := t.TempDir()
	temp := 70.0
	views := []deviceView{{Kind: "thermostat", TempF: &temp, Hvac: "OFF"}}
	h := newHistory(dir, func(context.Context) ([]deviceView, error) { return views, nil }, nil)
	h.sample(context.Background())
	h.sample(context.Background()) // unchanged: skipped
	views[0].Hvac = "HEATING"
	h.sample(context.Background())
	if got := h.Read(time.Now().Add(-time.Hour)); len(got) != 2 || got[1].Hvac != "HEATING" {
		t.Errorf("samples = %+v", got)
	}
}

func TestInternetOutage(t *testing.T) {
	e := newTestEvents(t)
	in := newInternet(t.TempDir(), e)
	up := true
	in.dial = func(context.Context, string) error {
		if up {
			return nil
		}
		return errors.New("down")
	}
	t0 := mustTime(t, "2026-11-23 10:00")
	in.check(context.Background(), t0)
	up = false
	in.check(context.Background(), t0.Add(time.Minute)) // one miss: not an outage yet
	if !in.View().Up {
		t.Fatal("single failure marked down")
	}
	in.check(context.Background(), t0.Add(2*time.Minute))
	if in.View().Up {
		t.Fatal("two failures still up")
	}
	up = true
	in.check(context.Background(), t0.Add(14*time.Minute))
	v := in.View()
	if !v.Up || len(v.Outages) != 1 || v.Outages[0].End.Sub(v.Outages[0].Start) != 13*time.Minute {
		t.Errorf("state = %+v", v)
	}
	if r := e.Recent(); len(r) != 1 || r[0].Device != "Internet was down 13m" {
		t.Errorf("feed = %+v", r)
	}
	// A blip that recovers before the second check leaves no trace.
	up = false
	in.check(context.Background(), t0.Add(20*time.Minute))
	up = true
	in.check(context.Background(), t0.Add(21*time.Minute))
	if len(in.View().Outages) != 1 {
		t.Error("blip recorded as outage")
	}
}

// ippResponse builds a printer's answer the way an HP does.
func ippResponse() []byte {
	var b bytes.Buffer
	b.Write([]byte{2, 0, 0, 0, 0, 0, 0, 1, 0x01})
	attr := func(tag byte, name string, val []byte) {
		b.WriteByte(tag)
		binary.Write(&b, binary.BigEndian, uint16(len(name)))
		b.WriteString(name)
		binary.Write(&b, binary.BigEndian, uint16(len(val)))
		b.Write(val)
	}
	i32 := func(n int32) []byte { return binary.BigEndian.AppendUint32(nil, uint32(n)) }
	attr(0x47, "attributes-charset", []byte("utf-8"))
	b.WriteByte(0x04)
	attr(0x23, "printer-state", i32(3))
	attr(0x44, "printer-state-reasons", []byte("marker-supply-low-warning"))
	attr(0x41, "printer-make-and-model", []byte("HP OfficeJet Pro 9010"))
	attr(0x42, "marker-names", []byte("Black Cartridge"))
	attr(0x42, "", []byte("Tri-color Cartridge"))
	attr(0x21, "marker-levels", i32(12))
	attr(0x21, "", i32(-3))
	attr(0x42, "marker-colors", []byte("#000000"))
	attr(0x42, "", []byte("#00FFFF#FF00FF#FFFF00"))
	b.WriteByte(0x03)
	return b.Bytes()
}

func TestParseIPP(t *testing.T) {
	a, err := parseIPP(ippResponse())
	if err != nil {
		t.Fatal(err)
	}
	v := printerFrom(a)
	if v.State != "idle" || v.Name != "HP OfficeJet Pro 9010" || len(v.Reasons) != 1 || len(v.Markers) != 2 {
		t.Fatalf("view = %+v", v)
	}
	if m := v.Markers[0]; m.Name != "Black Cartridge" || m.Level != 12 || m.Color != "#000000" {
		t.Errorf("black = %+v", m)
	}
	if m := v.Markers[1]; m.Level != -1 || m.Color != "#00FFFF" {
		t.Errorf("color = %+v", m)
	}
	// Our request must be well-formed enough for our own parser.
	if _, err := parseIPP(ippGetAttrs("ipp://x:631/ipp/print")); err != nil {
		t.Errorf("request: %v", err)
	}
}

func TestCastMessageRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	c := &castConn{conn: fakeConn{&buf}}
	if err := c.send("sender-0", "receiver-0", nsReceiver, map[string]any{"type": "LAUNCH", "appId": defaultApp}); err != nil {
		t.Fatal(err)
	}
	m, err := c.read()
	if err != nil {
		t.Fatal(err)
	}
	if m.Namespace != nsReceiver || !bytes.Contains(m.Payload, []byte(`"appId":"CC1AD845"`)) {
		t.Errorf("msg = %s %s", m.Namespace, m.Payload)
	}
}

func TestChimeWAV(t *testing.T) {
	b := chimeWAV("")
	pcm, err := wavData(b)
	if err != nil || len(pcm) != int(2.4*chimeRate)*2 {
		t.Fatalf("pcm %d bytes, %v", len(pcm), err)
	}
	if binary.LittleEndian.Uint32(b[24:]) != chimeRate {
		t.Error("sample rate")
	}
}

type fakeConn struct{ *bytes.Buffer }

func (fakeConn) Close() error                     { return nil }
func (fakeConn) LocalAddr() net.Addr              { return nil }
func (fakeConn) RemoteAddr() net.Addr             { return nil }
func (fakeConn) SetDeadline(time.Time) error      { return nil }
func (fakeConn) SetReadDeadline(time.Time) error  { return nil }
func (fakeConn) SetWriteDeadline(time.Time) error { return nil }

func TestLaundryReminder(t *testing.T) {
	newL := func(t *testing.T) (*Laundry, *[]string) {
		ev, _ := newEvents(nil, t.TempDir())
		l := &Laundry{machines: map[string]*machine{}, events: ev}
		said := &[]string{}
		l.SetReminder(LaundryReminderConfig{}, t.TempDir(), func(_ context.Context, s string) error { *said = append(*said, s); return nil })
		return l, said
	}
	day := func(h, m int) time.Time { return time.Date(2026, 11, 24, h, m, 0, 0, time.Local) }
	ctx := context.Background()

	// Washer finishes at 2:00; nobody moves it; at 3:00 the speaker says so, once.
	l, said := newL(t)
	l.update("w", "Washer", "washer", "RUNNING", 10, day(13, 50))
	l.update("w", "Washer", "washer", "END", 0, day(14, 0))
	l.checkReminder(ctx, day(14, 59))
	if len(*said) != 0 {
		t.Fatalf("too early: %v", *said)
	}
	l.update("w", "Washer", "washer", "POWER_OFF", 0, day(14, 10)) // LG turns itself off: still full
	l.checkReminder(ctx, day(15, 1))
	l.checkReminder(ctx, day(15, 30))
	if len(*said) != 1 || !strings.Contains((*said)[0], "finished 61 minutes ago") {
		t.Fatalf("said %v", *said)
	}

	// The dryer starting means the load moved.
	l, said = newL(t)
	l.update("w", "Washer", "washer", "RUNNING", 10, day(13, 50))
	l.update("w", "Washer", "washer", "END", 0, day(14, 0))
	l.update("d", "Dryer", "dryer", "POWER_OFF", 0, day(14, 5))
	l.update("d", "Dryer", "dryer", "RUNNING", 60, day(14, 20))
	l.checkReminder(ctx, day(15, 30))
	if len(*said) != 0 {
		t.Fatalf("moved to the dryer, still said %v", *said)
	}

	// "Moved it" from the dashboard.
	l, said = newL(t)
	l.update("w", "Washer", "washer", "RUNNING", 10, day(13, 50))
	l.update("w", "Washer", "washer", "END", 0, day(14, 0))
	l.Emptied()
	l.checkReminder(ctx, day(15, 30))
	if len(*said) != 0 {
		t.Fatalf("emptied, still said %v", *said)
	}

	// Finishing at 8:30 PM: nothing at 9:30 PM (quiet hours), then once at 8 AM.
	l, said = newL(t)
	l.update("w", "Washer", "washer", "RUNNING", 10, day(20, 20))
	l.update("w", "Washer", "washer", "END", 0, day(20, 30))
	l.checkReminder(ctx, day(21, 31))
	l.checkReminder(ctx, day(23, 59))
	if len(*said) != 0 {
		t.Fatalf("spoke during quiet hours: %v", *said)
	}
	morning := day(8, 1).AddDate(0, 0, 1)
	l.checkReminder(ctx, morning)
	if len(*said) != 1 || !strings.Contains((*said)[0], "last night") {
		t.Fatalf("morning said %v", *said)
	}
}

func TestQuietHours(t *testing.T) {
	at := func(h, m int) time.Time { return time.Date(2026, 1, 1, h, m, 0, 0, time.Local) }
	for _, c := range []struct {
		spec string
		t    time.Time
		want bool
	}{
		{"21:00-08:00", at(21, 0), true}, {"21:00-08:00", at(2, 0), true}, {"21:00-08:00", at(8, 0), false},
		{"21:00-08:00", at(20, 59), false}, {"13:00-14:00", at(13, 30), true}, {"13:00-14:00", at(14, 0), false}, {"junk", at(1, 0), false},
	} {
		if got := quietHours(c.spec, c.t); got != c.want {
			t.Errorf("%s at %s = %v", c.spec, c.t.Format("15:04"), got)
		}
	}
}
