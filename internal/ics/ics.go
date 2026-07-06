// Package ics is a minimal iCalendar (RFC 5545/5546) reader and iTIP REPLY
// writer — just enough to render a meeting-invite card and RSVP to it. It
// deliberately handles only the first VEVENT and ignores recurrence.
package ics

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jsnjack/mailbox/internal/logging"
)

// Event is the subset of a VEVENT an invite card needs.
type Event struct {
	UID       string
	Summary   string
	Location  string
	Organizer string // organizer's email address (mailto: stripped)
	Start     time.Time
	End       time.Time
	AllDay    bool
	Sequence  int
	Method    string // VCALENDAR METHOD: REQUEST, CANCEL, REPLY, …
}

// property is one unfolded content line, split into name, params, and value.
type property struct {
	name   string
	params map[string]string
	value  string
}

// Parse extracts the first VEVENT from iCalendar data. It returns an error
// when no VEVENT is present. VTIMEZONE blocks are collected first (they may
// follow the VEVENT) so a TZID that Go's zone database can't resolve — e.g.
// the Windows names Outlook/Teams emit ("Mountain Standard Time") — is
// resolved from the file's own offset definitions.
func Parse(data []byte) (*Event, error) {
	lines := unfold(string(data))
	tzdb := parseVTimezones(lines)

	ev := &Event{}
	inEvent := false
	seen := false
	for _, line := range lines {
		p, ok := parseProperty(line)
		if !ok {
			continue
		}
		switch p.name {
		case "BEGIN":
			if strings.EqualFold(p.value, "VEVENT") && !seen {
				inEvent = true
				seen = true
			}
		case "END":
			if strings.EqualFold(p.value, "VEVENT") {
				inEvent = false
			}
		case "METHOD":
			if !inEvent {
				ev.Method = strings.ToUpper(strings.TrimSpace(p.value))
			}
		}
		if !inEvent {
			continue
		}
		switch p.name {
		case "UID":
			ev.UID = p.value
		case "SUMMARY":
			ev.Summary = unescapeText(p.value)
		case "LOCATION":
			ev.Location = unescapeText(p.value)
		case "ORGANIZER":
			ev.Organizer = mailtoAddr(p.value)
		case "SEQUENCE":
			if n, err := strconv.Atoi(strings.TrimSpace(p.value)); err == nil {
				ev.Sequence = n
			}
		case "DTSTART":
			ev.Start, ev.AllDay = parseICSTime(p, tzdb)
		case "DTEND":
			ev.End, _ = parseICSTime(p, tzdb)
		}
	}
	if !seen {
		return nil, fmt.Errorf("ics: no VEVENT found")
	}
	return ev, nil
}

// Reply builds an iTIP REPLY VCALENDAR answering ev with partstat (ACCEPTED,
// TENTATIVE, or DECLINED) as attendee. now stamps DTSTAMP (injected so tests
// are deterministic).
func Reply(ev *Event, attendee, attendeeName, partstat string, now time.Time) []byte {
	var b strings.Builder
	line := func(s string) { b.WriteString(s); b.WriteString("\r\n") }
	line("BEGIN:VCALENDAR")
	line("PRODID:-//mailbox//iTIP//EN")
	line("VERSION:2.0")
	line("METHOD:REPLY")
	line("BEGIN:VEVENT")
	line("UID:" + ev.UID)
	line("SEQUENCE:" + strconv.Itoa(ev.Sequence))
	line("DTSTAMP:" + now.UTC().Format("20060102T150405Z"))
	if ev.Organizer != "" {
		line("ORGANIZER:mailto:" + ev.Organizer)
	}
	att := "ATTENDEE;PARTSTAT=" + partstat
	if attendeeName != "" {
		att += ";CN=" + escapeParam(attendeeName)
	}
	line(att + ":mailto:" + attendee)
	if ev.Summary != "" {
		line("SUMMARY:" + escapeText(ev.Summary))
	}
	line("END:VEVENT")
	line("END:VCALENDAR")
	return []byte(b.String())
}

// unfold splits content lines, joining folded continuations (a CRLF or LF
// followed by a space/tab belongs to the previous line — RFC 5545 §3.1).
func unfold(s string) []string {
	raw := strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n")
	var out []string
	for _, l := range raw {
		if (strings.HasPrefix(l, " ") || strings.HasPrefix(l, "\t")) && len(out) > 0 {
			out[len(out)-1] += l[1:]
			continue
		}
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}

// parseProperty splits "NAME;PARAM=V;PARAM="Q":VALUE" — the name/param part
// ends at the first colon outside double quotes.
func parseProperty(line string) (property, bool) {
	inQuotes := false
	for i, r := range line {
		switch r {
		case '"':
			inQuotes = !inQuotes
		case ':':
			if inQuotes {
				continue
			}
			p := property{params: map[string]string{}, value: line[i+1:]}
			head := strings.Split(line[:i], ";")
			p.name = strings.ToUpper(strings.TrimSpace(head[0]))
			for _, kv := range head[1:] {
				k, v, ok := strings.Cut(kv, "=")
				if !ok {
					continue
				}
				p.params[strings.ToUpper(k)] = strings.Trim(v, `"`)
			}
			return p, true
		}
	}
	return property{}, false
}

// parseICSTime handles the three DTSTART/DTEND shapes: UTC ("...Z"), a local
// time qualified by a TZID param, and an all-day VALUE=DATE. A TZID is
// resolved as an IANA zone first, then from the file's own VTIMEZONE
// definition (Windows names), and only then falls back to the machine's zone.
// Timed values are returned in local time — the card shows the user's wall
// clock, not the organizer's.
func parseICSTime(p property, tzdb map[string]vtimezone) (t time.Time, allDay bool) {
	v := strings.TrimSpace(p.value)
	if p.params["VALUE"] == "DATE" || len(v) == 8 {
		t, _ = time.ParseInLocation("20060102", v, time.Local)
		return t, true
	}
	if strings.HasSuffix(v, "Z") {
		t, _ = time.Parse("20060102T150405Z", v)
		return t.Local(), false
	}
	if tzid := p.params["TZID"]; tzid != "" {
		if l, err := time.LoadLocation(tzid); err == nil {
			logging.Trace("ics: tzid resolved", "tzid", tzid, "via", "iana")
			t, _ = time.ParseInLocation("20060102T150405", v, l)
			return t.Local(), false
		}
		if vtz, ok := tzdb[tzid]; ok {
			if naive, err := time.Parse("20060102T150405", v); err == nil {
				off := vtz.offsetAt(naive)
				logging.Trace("ics: tzid resolved", "tzid", tzid, "via", "vtimezone", "offset", off)
				t = time.Date(naive.Year(), naive.Month(), naive.Day(),
					naive.Hour(), naive.Minute(), naive.Second(), 0,
					time.FixedZone(tzid, off))
				return t.Local(), false
			}
		}
		logging.Trace("ics: tzid unresolved; assuming machine zone", "tzid", tzid)
	}
	t, _ = time.ParseInLocation("20060102T150405", v, time.Local)
	return t, false
}

// vtimezone is one VTIMEZONE definition: its STANDARD and DAYLIGHT
// observances. A zone without DST carries only the standard observance.
type vtimezone struct {
	std, dst observance
}

// observance is one STANDARD/DAYLIGHT block: the offset it switches to and a
// yearly transition rule ("nth weekday of month at hh:mm:ss"), the only RRULE
// shape real-world invites use.
type observance struct {
	present bool
	offset  int // seconds east of UTC (TZOFFSETTO)
	month   time.Month
	weekday time.Weekday
	nth     int // 1..5, or -1 for last; 0 = no usable rule
	daySecs int // transition time-of-day, seconds since midnight
}

// offsetAt returns the UTC offset in effect at the (zone-local) time t.
func (z vtimezone) offsetAt(t time.Time) int {
	switch {
	case !z.dst.present:
		return z.std.offset
	case !z.std.present:
		return z.dst.offset
	case z.std.nth == 0 || z.dst.nth == 0:
		// Transition rules we couldn't parse: standard time is the safer guess.
		return z.std.offset
	}
	dstStart := transitionTime(t.Year(), z.dst)
	stdStart := transitionTime(t.Year(), z.std)
	inDST := false
	if dstStart.Before(stdStart) { // northern hemisphere
		inDST = !t.Before(dstStart) && t.Before(stdStart)
	} else { // southern hemisphere: DST spans the year boundary
		inDST = !t.Before(dstStart) || t.Before(stdStart)
	}
	if inDST {
		return z.dst.offset
	}
	return z.std.offset
}

// transitionTime computes the observance's transition instant in year, as a
// naive time comparable with the naive event time.
func transitionTime(year int, o observance) time.Time {
	var day time.Time
	if o.nth > 0 {
		day = time.Date(year, o.month, 1, 0, 0, 0, 0, time.UTC)
		for day.Weekday() != o.weekday {
			day = day.AddDate(0, 0, 1)
		}
		day = day.AddDate(0, 0, 7*(o.nth-1))
	} else {
		day = time.Date(year, o.month+1, 0, 0, 0, 0, 0, time.UTC) // last day of month
		for day.Weekday() != o.weekday {
			day = day.AddDate(0, 0, -1)
		}
		day = day.AddDate(0, 0, 7*(o.nth+1))
	}
	return day.Add(time.Duration(o.daySecs) * time.Second)
}

// parseVTimezones collects every VTIMEZONE block from the unfolded lines,
// keyed by TZID.
func parseVTimezones(lines []string) map[string]vtimezone {
	tzdb := map[string]vtimezone{}
	var (
		inTZ bool
		tzid string
		cur  vtimezone
		sub  string // "STANDARD" | "DAYLIGHT" | ""
		obs  observance
	)
	for _, line := range lines {
		p, ok := parseProperty(line)
		if !ok {
			continue
		}
		switch p.name {
		case "BEGIN":
			switch strings.ToUpper(p.value) {
			case "VTIMEZONE":
				inTZ, tzid, cur = true, "", vtimezone{}
			case "STANDARD", "DAYLIGHT":
				if inTZ {
					sub, obs = strings.ToUpper(p.value), observance{present: true}
				}
			}
		case "END":
			switch strings.ToUpper(p.value) {
			case "VTIMEZONE":
				if inTZ && tzid != "" && (cur.std.present || cur.dst.present) {
					tzdb[tzid] = cur
				}
				inTZ = false
			case "STANDARD":
				if sub == "STANDARD" {
					cur.std = obs
				}
				sub = ""
			case "DAYLIGHT":
				if sub == "DAYLIGHT" {
					cur.dst = obs
				}
				sub = ""
			}
		case "TZID":
			if inTZ && sub == "" {
				tzid = strings.TrimSpace(p.value)
			}
		case "TZOFFSETTO":
			if sub != "" {
				obs.offset = parseUTCOffset(p.value)
			}
		case "DTSTART":
			if sub != "" {
				if t, err := time.Parse("20060102T150405", strings.TrimSpace(p.value)); err == nil {
					obs.daySecs = t.Hour()*3600 + t.Minute()*60 + t.Second()
				}
			}
		case "RRULE":
			if sub != "" {
				obs.month, obs.weekday, obs.nth = parseYearlyRule(p.value)
			}
		}
	}
	return tzdb
}

// parseYearlyRule extracts month + nth-weekday from the yearly RRULE shape
// VTIMEZONEs use ("FREQ=YEARLY;BYDAY=2SU;BYMONTH=3"). nth 0 means the rule is
// something else and the caller should not trust it.
func parseYearlyRule(v string) (time.Month, time.Weekday, int) {
	var (
		month  time.Month
		wd     time.Weekday
		nth    int
		yearly bool
	)
	days := map[string]time.Weekday{"SU": time.Sunday, "MO": time.Monday, "TU": time.Tuesday,
		"WE": time.Wednesday, "TH": time.Thursday, "FR": time.Friday, "SA": time.Saturday}
	for _, kv := range strings.Split(strings.TrimSpace(v), ";") {
		k, val, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		switch strings.ToUpper(k) {
		case "FREQ":
			yearly = strings.EqualFold(val, "YEARLY")
		case "BYMONTH":
			m, _, _ := strings.Cut(val, ",")
			if n, err := strconv.Atoi(m); err == nil && n >= 1 && n <= 12 {
				month = time.Month(n)
			}
		case "BYDAY":
			d, _, _ := strings.Cut(val, ",")
			if len(d) < 3 {
				return 0, 0, 0 // a bare weekday ("SU") has no nth — unusable
			}
			w, ok := days[strings.ToUpper(d[len(d)-2:])]
			if !ok {
				return 0, 0, 0
			}
			n, err := strconv.Atoi(d[:len(d)-2])
			if err != nil || n == 0 || n > 5 || n < -5 {
				return 0, 0, 0
			}
			wd, nth = w, n
		}
	}
	if !yearly || month == 0 || nth == 0 {
		return 0, 0, 0
	}
	return month, wd, nth
}

// parseUTCOffset parses a UTC-OFFSET value ("-0700", "+0200", "+023000") into
// seconds east of UTC.
func parseUTCOffset(v string) int {
	v = strings.TrimSpace(v)
	if len(v) < 5 {
		return 0
	}
	sign := 1
	switch v[0] {
	case '-':
		sign = -1
	case '+':
	default:
		return 0
	}
	digits := v[1:]
	h, err1 := strconv.Atoi(digits[:2])
	m, err2 := strconv.Atoi(digits[2:4])
	s := 0
	if len(digits) >= 6 {
		s, _ = strconv.Atoi(digits[4:6])
	}
	if err1 != nil || err2 != nil {
		return 0
	}
	return sign * (h*3600 + m*60 + s)
}

// mailtoAddr strips a mailto: prefix (any case) from a cal-address value.
func mailtoAddr(v string) string {
	v = strings.TrimSpace(v)
	if len(v) >= 7 && strings.EqualFold(v[:7], "mailto:") {
		return v[7:]
	}
	return v
}

// unescapeText reverses RFC 5545 TEXT escaping (\\ \; \, \n).
func unescapeText(s string) string {
	r := strings.NewReplacer(`\\`, `\`, `\;`, ";", `\,`, ",", `\n`, "\n", `\N`, "\n")
	return r.Replace(s)
}

// escapeText applies RFC 5545 TEXT escaping.
func escapeText(s string) string {
	r := strings.NewReplacer(`\`, `\\`, ";", `\;`, ",", `\,`, "\n", `\n`)
	return r.Replace(s)
}

// escapeParam quotes a param value when it contains separators.
func escapeParam(s string) string {
	if strings.ContainsAny(s, ";:,") {
		return `"` + strings.ReplaceAll(s, `"`, "") + `"`
	}
	return s
}
