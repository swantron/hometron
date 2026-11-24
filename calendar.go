package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Family calendar and chore days (trash, recycling).
//
// Config: <state>/calendar.json
//
//	{
//	  "calendars": [{"name": "Family", "url": "https://calendar.google.com/calendar/ical/…/private-…/basic.ics"}],
//	  "chores": [
//	    {"name": "Trash", "weekday": "Tuesday"},
//	    {"name": "Recycling", "weekday": "Tuesday", "weeks": 2, "from": "2026-12-01"}
//	  ],
//	  "remind": "19:00"
//	}
//
// The url is Google Calendar's "Secret address in iCal format" (calendar settings), so no
// extra Google permissions are needed. Chores repeat every `weeks` weeks (default 1)
// counting from `from`; `remind` is when the night-before reminder goes to the feed/alerts.
type CalendarConfig struct {
	Calendars []struct {
		Name string `json:"name"`
		URL  string `json:"url"`
	} `json:"calendars"`
	Chores []Chore `json:"chores"`
	Remind string  `json:"remind,omitempty"`
}

type Chore struct {
	Name    string `json:"name"`
	Weekday string `json:"weekday"`
	Weeks   int    `json:"weeks,omitempty"`
	From    string `json:"from,omitempty"` // a date the chore falls on, YYYY-MM-DD
	At      string `json:"at,omitempty"`   // pickup time, HH:MM; shown and used in the reminder
}

type CalEvent struct {
	Title    string    `json:"title"`
	Start    time.Time `json:"start"`
	End      time.Time `json:"end"`
	AllDay   bool      `json:"allDay,omitempty"`
	Calendar string    `json:"calendar,omitempty"`
	Location string    `json:"location,omitempty"`
	Chore    bool      `json:"chore,omitempty"`
}

type Calendar struct {
	cfg    CalendarConfig
	http   *http.Client
	events *Events

	mu       sync.Mutex
	upcoming []CalEvent
	errs     []string
	reminded map[string]bool
}

func loadCalendar(stateDir string, events *Events) (*Calendar, error) {
	b, err := os.ReadFile(filepath.Join(stateDir, "calendar.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var cfg CalendarConfig
	if err := json.Unmarshal(b, &cfg); err != nil {
		return nil, fmt.Errorf("calendar.json: %w", err)
	}
	for _, c := range cfg.Chores {
		if _, ok := weekdays[strings.ToLower(c.Weekday)]; !ok {
			return nil, fmt.Errorf("calendar.json: chore %q: weekday %q", c.Name, c.Weekday)
		}
		if c.At != "" {
			if _, err := time.Parse("15:04", c.At); err != nil {
				return nil, fmt.Errorf("calendar.json: chore %q: at must be HH:MM", c.Name)
			}
		}
		if c.From != "" {
			if _, err := time.ParseInLocation("2006-01-02", c.From, time.Local); err != nil {
				return nil, fmt.Errorf("calendar.json: chore %q: from must be YYYY-MM-DD", c.Name)
			}
		}
	}
	if cfg.Remind == "" {
		cfg.Remind = "19:00"
	}
	if _, err := time.Parse("15:04", cfg.Remind); err != nil {
		return nil, errors.New("calendar.json: remind must be HH:MM")
	}
	return &Calendar{cfg: cfg, http: &http.Client{Timeout: 30 * time.Second}, events: events, reminded: map[string]bool{}}, nil
}

var weekdays = map[string]time.Weekday{"sunday": 0, "monday": 1, "tuesday": 2, "wednesday": 3, "thursday": 4, "friday": 5, "saturday": 6}

// Run refreshes calendars every 15 minutes and checks chore reminders every minute.
func (c *Calendar) Run(ctx context.Context) {
	c.refresh(ctx)
	last := time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Minute):
		}
		if time.Since(last) >= 15*time.Minute {
			c.refresh(ctx)
			last = time.Now()
		}
		c.remind(time.Now())
	}
}

func (c *Calendar) refresh(ctx context.Context) {
	now := time.Now()
	from := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.Local)
	to := from.AddDate(0, 0, 8)
	var all []CalEvent
	var errs []string
	for _, cal := range c.cfg.Calendars {
		evs, err := c.fetch(ctx, cal.URL, from, to)
		if err != nil {
			log.Printf("calendar %s: %v", cal.Name, err)
			errs = append(errs, cal.Name+": "+err.Error())
			// Keep what we had for this calendar rather than blanking the card.
			c.mu.Lock()
			for _, e := range c.upcoming {
				if e.Calendar == cal.Name && !e.Chore {
					all = append(all, e)
				}
			}
			c.mu.Unlock()
			continue
		}
		for i := range evs {
			evs[i].Calendar = cal.Name
		}
		all = append(all, evs...)
	}
	all = append(all, choreDays(c.cfg.Chores, from, to)...)
	sort.SliceStable(all, func(i, j int) bool {
		if !all[i].Start.Equal(all[j].Start) {
			return all[i].Start.Before(all[j].Start)
		}
		return all[i].AllDay && !all[j].AllDay
	})
	c.mu.Lock()
	c.upcoming, c.errs = all, errs
	c.mu.Unlock()
}

func (c *Calendar) fetch(ctx context.Context, url string, from, to time.Time) ([]CalEvent, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	res, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return nil, fmt.Errorf("HTTP %d", res.StatusCode)
	}
	return parseICS(io.LimitReader(res.Body, 20<<20), from, to)
}

// Upcoming returns events from the start of today for the next week, plus fetch errors.
func (c *Calendar) Upcoming() ([]CalEvent, []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	out := []CalEvent{}
	for _, e := range c.upcoming {
		if e.End.After(now) || (e.AllDay && sameDay(e.Start, now)) {
			out = append(out, e)
		}
	}
	return out, append([]string(nil), c.errs...)
}

// remind posts "Trash goes out tonight" to the feed at the remind time the day before.
func (c *Calendar) remind(now time.Time) {
	hm, _ := time.Parse("15:04", c.cfg.Remind)
	at := time.Date(now.Year(), now.Month(), now.Day(), hm.Hour(), hm.Minute(), 0, 0, time.Local)
	if now.Before(at) || now.Sub(at) > 30*time.Minute {
		return
	}
	tomorrow := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, time.Local)
	for _, e := range choreDays(c.cfg.Chores, tomorrow, tomorrow.AddDate(0, 0, 1)) {
		key := e.Title + tomorrow.Format("2006-01-02")
		c.mu.Lock()
		done := c.reminded[key]
		c.reminded[key] = true
		c.mu.Unlock()
		if !done && c.events != nil {
			msg := e.Title + " goes out tonight"
			if !e.AllDay {
				msg += " (pickup " + e.Start.Format("3:04 PM") + ")"
			}
			c.events.Post(Event{Time: now, DeviceID: "chore:" + strings.ToLower(e.Title), Device: msg, Kind: "reminder"})
		}
	}
}

// choreDays lists chore days in [from, to).
func choreDays(chores []Chore, from, to time.Time) []CalEvent {
	var out []CalEvent
	for _, ch := range chores {
		wd := weekdays[strings.ToLower(ch.Weekday)]
		weeks := max(ch.Weeks, 1)
		var anchor time.Time
		if ch.From != "" {
			anchor, _ = time.ParseInLocation("2006-01-02", ch.From, time.Local)
		}
		for d := from; d.Before(to); d = d.AddDate(0, 0, 1) {
			if d.Weekday() != wd {
				continue
			}
			if weeks > 1 && !anchor.IsZero() {
				days := int(math.Round(dateOnly(d).Sub(dateOnly(anchor)).Hours() / 24)) // DST days are 23/25h
				if days < 0 {
					days = -days
				}
				if (days/7)%weeks != 0 {
					continue
				}
			}
			start := dateOnly(d)
			if at, err := time.Parse("15:04", ch.At); err == nil {
				t := start.Add(time.Duration(at.Hour())*time.Hour + time.Duration(at.Minute())*time.Minute)
				out = append(out, CalEvent{Title: ch.Name, Start: t, End: t.Add(30 * time.Minute), Chore: true})
				continue
			}
			out = append(out, CalEvent{Title: ch.Name, Start: start, End: start.AddDate(0, 0, 1), AllDay: true, Chore: true})
		}
	}
	return out
}

func dateOnly(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.Local)
}

func sameDay(a, b time.Time) bool {
	a, b = a.In(time.Local), b.In(time.Local)
	return a.Year() == b.Year() && a.YearDay() == b.YearDay()
}

// ---------- iCalendar (RFC 5545), the parts Google Calendar uses ----------

type icsEvent struct {
	uid, summary, location, status string
	start, end                     time.Time
	allDay                         bool
	rrule                          string
	exdates                        []time.Time
	recurrenceID                   time.Time
	hasDuration                    bool
	duration                       time.Duration
}

// parseICS returns the events (with repeats expanded) that overlap [from, to).
func parseICS(r io.Reader, from, to time.Time) ([]CalEvent, error) {
	lines, err := unfold(r)
	if err != nil {
		return nil, err
	}
	var evs []*icsEvent
	var cur *icsEvent
	for _, l := range lines {
		name, params, value := splitProp(l)
		switch {
		case name == "BEGIN" && value == "VEVENT":
			cur = &icsEvent{}
		case name == "END" && value == "VEVENT":
			if cur != nil && !cur.start.IsZero() {
				evs = append(evs, cur)
			}
			cur = nil
		case cur == nil:
		case name == "UID":
			cur.uid = value
		case name == "SUMMARY":
			cur.summary = icsText(value)
		case name == "LOCATION":
			cur.location = icsText(value)
		case name == "STATUS":
			cur.status = value
		case name == "DTSTART":
			cur.start, cur.allDay = icsTime(value, params)
		case name == "DTEND":
			cur.end, _ = icsTime(value, params)
		case name == "DURATION":
			cur.duration, cur.hasDuration = icsDuration(value)
		case name == "RRULE":
			cur.rrule = value
		case name == "EXDATE":
			for _, v := range strings.Split(value, ",") {
				t, _ := icsTime(v, params)
				cur.exdates = append(cur.exdates, t)
			}
		case name == "RECURRENCE-ID":
			cur.recurrenceID, _ = icsTime(value, params)
		}
	}

	// Moved or cancelled single instances of a repeating event.
	overrides := map[string]map[int64]*icsEvent{}
	for _, e := range evs {
		if !e.recurrenceID.IsZero() {
			if overrides[e.uid] == nil {
				overrides[e.uid] = map[int64]*icsEvent{}
			}
			overrides[e.uid][e.recurrenceID.Unix()] = e
		}
	}

	var out []CalEvent
	emit := func(e *icsEvent, start time.Time) {
		if e.status == "CANCELLED" {
			return
		}
		end := start.Add(e.length())
		if end.After(from) && start.Before(to) {
			out = append(out, CalEvent{Title: e.summary, Start: start, End: end, AllDay: e.allDay, Location: e.location})
		}
	}
	for _, e := range evs {
		if !e.recurrenceID.IsZero() {
			emit(e, e.start)
			continue
		}
		if e.rrule == "" {
			emit(e, e.start)
			continue
		}
		skip := map[int64]bool{}
		for _, x := range e.exdates {
			skip[x.Unix()] = true
		}
		for _, s := range expandRRule(e.start, e.rrule, from.Add(-e.length()), to) {
			if skip[s.Unix()] || overrides[e.uid][s.Unix()] != nil {
				continue // excluded, or replaced by its override (emitted above)
			}
			emit(e, s)
		}
	}
	return out, nil
}

func (e *icsEvent) length() time.Duration {
	switch {
	case e.hasDuration:
		return e.duration
	case !e.end.IsZero() && e.end.After(e.start):
		return e.end.Sub(e.start)
	case e.allDay:
		return 24 * time.Hour
	}
	return 0
}

func unfold(r io.Reader) ([]string, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	var lines []string
	for sc.Scan() {
		l := strings.TrimRight(sc.Text(), "\r")
		if (strings.HasPrefix(l, " ") || strings.HasPrefix(l, "\t")) && len(lines) > 0 {
			lines[len(lines)-1] += l[1:]
			continue
		}
		lines = append(lines, l)
	}
	return lines, sc.Err()
}

// splitProp splits `NAME;P1=a;P2="b:c":value`.
func splitProp(l string) (name string, params map[string]string, value string) {
	inQuote, colon := false, -1
	for i, r := range l {
		if r == '"' {
			inQuote = !inQuote
		} else if r == ':' && !inQuote {
			colon = i
			break
		}
	}
	if colon < 0 {
		return strings.ToUpper(l), nil, ""
	}
	parts := strings.Split(l[:colon], ";")
	params = map[string]string{}
	for _, p := range parts[1:] {
		if k, v, ok := strings.Cut(p, "="); ok {
			params[strings.ToUpper(k)] = strings.Trim(v, `"`)
		}
	}
	return strings.ToUpper(parts[0]), params, l[colon+1:]
}

func icsText(s string) string {
	return strings.NewReplacer(`\n`, "\n", `\N`, "\n", `\,`, ",", `\;`, ";", `\\`, `\`).Replace(s)
}

// icsTime handles DATE, UTC DATE-TIME, DATE-TIME with TZID, and floating DATE-TIME.
func icsTime(v string, params map[string]string) (time.Time, bool) {
	v = strings.TrimSpace(v)
	if params["VALUE"] == "DATE" || len(v) == 8 {
		t, err := time.ParseInLocation("20060102", v, time.Local)
		if err != nil {
			return time.Time{}, false
		}
		return t, true
	}
	if strings.HasSuffix(v, "Z") {
		t, _ := time.Parse("20060102T150405Z", v)
		return t.In(time.Local), false
	}
	loc := time.Local
	if tz := params["TZID"]; tz != "" {
		if l, err := time.LoadLocation(tz); err == nil {
			loc = l
		}
	}
	t, _ := time.ParseInLocation("20060102T150405", v, loc)
	return t, false
}

// icsDuration parses the common forms: P1D, PT1H30M, P1W.
func icsDuration(v string) (time.Duration, bool) {
	v = strings.TrimPrefix(strings.TrimPrefix(v, "+"), "P")
	var d time.Duration
	num := ""
	inTime := false
	for _, r := range v {
		switch {
		case r >= '0' && r <= '9':
			num += string(r)
		case r == 'T':
			inTime = true
		default:
			n, _ := strconv.Atoi(num)
			num = ""
			switch {
			case r == 'W':
				d += time.Duration(n) * 7 * 24 * time.Hour
			case r == 'D':
				d += time.Duration(n) * 24 * time.Hour
			case r == 'H' && inTime:
				d += time.Duration(n) * time.Hour
			case r == 'M' && inTime:
				d += time.Duration(n) * time.Minute
			case r == 'S' && inTime:
				d += time.Duration(n) * time.Second
			default:
				return 0, false
			}
		}
	}
	return d, true
}

var icsDays = map[string]time.Weekday{"SU": 0, "MO": 1, "TU": 2, "WE": 3, "TH": 4, "FR": 5, "SA": 6}

// expandRRule lists occurrence starts from dtstart up to `until` (exclusive), honoring
// FREQ (DAILY/WEEKLY/MONTHLY/YEARLY), INTERVAL, COUNT, UNTIL, BYDAY (incl. "2TU", "-1FR"
// for monthly) and BYMONTHDAY. Times keep dtstart's wall clock in its zone, so a 9am
// event stays at 9am across daylight-saving changes.
func expandRRule(dtstart time.Time, rule string, from, until time.Time) []time.Time {
	p := map[string]string{}
	for _, kv := range strings.Split(rule, ";") {
		if k, v, ok := strings.Cut(kv, "="); ok {
			p[strings.ToUpper(k)] = strings.ToUpper(v)
		}
	}
	interval, _ := strconv.Atoi(p["INTERVAL"])
	interval = max(interval, 1)
	count, _ := strconv.Atoi(p["COUNT"])
	if u := p["UNTIL"]; u != "" {
		t, _ := icsTime(u, map[string]string{})
		if !t.IsZero() {
			// UNTIL is inclusive; an all-day UNTIL date includes that whole day.
			if len(u) == 8 {
				t = t.AddDate(0, 0, 1)
			} else {
				t = t.Add(time.Second)
			}
			if t.Before(until) {
				until = t
			}
		}
	}
	type byDay struct {
		n  int
		wd time.Weekday
	}
	var days []byDay
	for _, d := range strings.Split(p["BYDAY"], ",") {
		if len(d) < 2 {
			continue
		}
		wd, ok := icsDays[d[len(d)-2:]]
		if !ok {
			continue
		}
		n, _ := strconv.Atoi(d[:len(d)-2])
		days = append(days, byDay{n, wd})
	}
	var monthDays []int
	for _, d := range strings.Split(p["BYMONTHDAY"], ",") {
		if n, err := strconv.Atoi(d); err == nil && n != 0 {
			monthDays = append(monthDays, n)
		}
	}

	loc := dtstart.Location()
	h, mi, s := dtstart.Clock()
	at := func(y int, m time.Month, d int) time.Time { return time.Date(y, m, d, h, mi, s, 0, loc) }

	var out []time.Time
	n := 0
	// add appends t if it's a valid occurrence; returns false once we're done.
	add := func(t time.Time) bool {
		if t.Before(dtstart) {
			return true
		}
		if !t.Before(until) || (count > 0 && n >= count) {
			return false
		}
		n++
		out = append(out, t)
		return true
	}

	y, m, d := dtstart.Date()
	// Without COUNT, nothing before `from` matters: jump close to it instead of walking
	// years of a daily event one day at a time.
	first := 0
	if count == 0 && from.After(dtstart) {
		days := int(from.Sub(dtstart).Hours() / 24)
		switch p["FREQ"] {
		case "DAILY":
			first = days/interval - 1
		case "WEEKLY":
			first = days/(7*interval) - 1
		case "MONTHLY":
			first = days/(31*interval) - 1
		}
		first = max(first, 0)
	}
	for i := first; i < first+5000; i++ {
		var cands []time.Time
		switch p["FREQ"] {
		case "DAILY":
			cands = []time.Time{at(y, m, d+i*interval)}
		case "WEEKLY":
			// Weeks start Monday (RFC default WKST=MO).
			base := at(y, m, d-((int(dtstart.Weekday())+6)%7)+i*7*interval)
			if len(days) == 0 {
				days = []byDay{{0, dtstart.Weekday()}}
			}
			for _, bd := range days {
				cands = append(cands, base.AddDate(0, 0, (int(bd.wd)+6)%7))
			}
		case "MONTHLY":
			mstart := time.Date(y, m+time.Month(i*interval), 1, 0, 0, 0, 0, loc)
			fy, fm, _ := mstart.Date()
			last := mstart.AddDate(0, 1, -1).Day()
			switch {
			case len(days) > 0:
				for _, bd := range days {
					for _, dd := range nthWeekdays(fy, fm, last, bd.n, bd.wd, loc) {
						cands = append(cands, at(fy, fm, dd))
					}
				}
			case len(monthDays) > 0:
				for _, md := range monthDays {
					if md < 0 {
						md = last + 1 + md
					}
					if md >= 1 && md <= last {
						cands = append(cands, at(fy, fm, md))
					}
				}
			default:
				if d <= last {
					cands = append(cands, at(fy, fm, d))
				}
			}
		case "YEARLY":
			yy := y + i*interval
			if t := at(yy, m, d); t.Day() == d { // skip Feb 29 in non-leap years
				cands = append(cands, t)
			}
		default:
			return []time.Time{dtstart}
		}
		sort.Slice(cands, func(a, b int) bool { return cands[a].Before(cands[b]) })
		for _, c := range cands {
			if !add(c) {
				return out
			}
		}
	}
	return out
}

// nthWeekdays returns the days of month matching weekday wd: all of them if n == 0,
// the nth if n > 0, or the nth from the end if n < 0.
func nthWeekdays(y int, m time.Month, last, n int, wd time.Weekday, loc *time.Location) []int {
	var all []int
	for d := 1; d <= last; d++ {
		if time.Date(y, m, d, 12, 0, 0, 0, loc).Weekday() == wd {
			all = append(all, d)
		}
	}
	switch {
	case n == 0:
		return all
	case n > 0 && n <= len(all):
		return []int{all[n-1]}
	case n < 0 && -n <= len(all):
		return []int{all[len(all)+n]}
	}
	return nil
}
