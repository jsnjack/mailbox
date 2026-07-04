// Package ics is a minimal iCalendar (RFC 5545/5546) reader and iTIP REPLY
// writer — just enough to render a meeting-invite card and RSVP to it. It
// deliberately handles only the first VEVENT and ignores recurrence.
package ics

import (
	"fmt"
	"strconv"
	"strings"
	"time"
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
// when no VEVENT is present.
func Parse(data []byte) (*Event, error) {
	ev := &Event{}
	inEvent := false
	seen := false
	for _, line := range unfold(string(data)) {
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
			ev.Method = strings.ToUpper(strings.TrimSpace(p.value))
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
			ev.Start, ev.AllDay = parseICSTime(p)
		case "DTEND":
			ev.End, _ = parseICSTime(p)
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
// time qualified by a TZID param (unresolvable TZIDs — e.g. Windows names —
// fall back to the machine's zone), and an all-day VALUE=DATE.
func parseICSTime(p property) (t time.Time, allDay bool) {
	v := strings.TrimSpace(p.value)
	if p.params["VALUE"] == "DATE" || len(v) == 8 {
		t, _ = time.ParseInLocation("20060102", v, time.Local)
		return t, true
	}
	if strings.HasSuffix(v, "Z") {
		t, _ = time.Parse("20060102T150405Z", v)
		return t.Local(), false
	}
	loc := time.Local
	if tzid := p.params["TZID"]; tzid != "" {
		if l, err := time.LoadLocation(tzid); err == nil {
			loc = l
		}
	}
	t, _ = time.ParseInLocation("20060102T150405", v, loc)
	return t, false
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
