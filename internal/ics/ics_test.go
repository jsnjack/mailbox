package ics

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// A Google-Calendar-style REQUEST parses into the fields the invite card needs,
// including folded lines, escaped text, and a TZID-qualified start.
func TestParseRequest(t *testing.T) {
	data := strings.ReplaceAll(`BEGIN:VCALENDAR
PRODID:-//Google Inc//Google Calendar 70.9054//EN
VERSION:2.0
METHOD:REQUEST
BEGIN:VEVENT
DTSTART;TZID=Europe/Amsterdam:20260710T140000
DTEND;TZID=Europe/Amsterdam:20260710T150000
ORGANIZER;CN=Anna:mailto:anna@example.com
UID:abc123@google.com
SEQUENCE:2
SUMMARY:Team sync\, Q3 planning
  (continued)
LOCATION:Room 4\; floor 2
END:VEVENT
END:VCALENDAR
`, "\n", "\r\n")

	ev, err := Parse([]byte(data))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if ev.Method != "REQUEST" {
		t.Fatalf("Method = %q, want REQUEST", ev.Method)
	}
	if ev.Summary != "Team sync, Q3 planning (continued)" {
		t.Fatalf("Summary = %q", ev.Summary)
	}
	if ev.Location != "Room 4; floor 2" {
		t.Fatalf("Location = %q", ev.Location)
	}
	if ev.Organizer != "anna@example.com" {
		t.Fatalf("Organizer = %q", ev.Organizer)
	}
	if ev.UID != "abc123@google.com" || ev.Sequence != 2 {
		t.Fatalf("UID/Sequence = %q/%d", ev.UID, ev.Sequence)
	}
	ams, _ := time.LoadLocation("Europe/Amsterdam")
	want := time.Date(2026, 7, 10, 14, 0, 0, 0, ams)
	if !ev.Start.Equal(want) {
		t.Fatalf("Start = %v, want %v", ev.Start, want)
	}
	if ev.AllDay {
		t.Fatal("AllDay = true for a timed event")
	}
	if !ev.End.Equal(want.Add(time.Hour)) {
		t.Fatalf("End = %v", ev.End)
	}
}

func TestParseAllDayAndUTC(t *testing.T) {
	allDay := "BEGIN:VCALENDAR\r\nMETHOD:REQUEST\r\nBEGIN:VEVENT\r\nUID:d1\r\nDTSTART;VALUE=DATE:20260801\r\nSUMMARY:Holiday\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	ev, err := Parse([]byte(allDay))
	if err != nil || !ev.AllDay {
		t.Fatalf("all-day parse: %+v, %v", ev, err)
	}
	utc := "BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nUID:d2\r\nDTSTART:20260801T120000Z\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	ev, err = Parse([]byte(utc))
	if err != nil || ev.AllDay {
		t.Fatalf("utc parse: %+v, %v", ev, err)
	}
	if !ev.Start.Equal(time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("utc start = %v", ev.Start)
	}
	if _, err := Parse([]byte("BEGIN:VCALENDAR\r\nEND:VCALENDAR\r\n")); err == nil {
		t.Fatal("no-VEVENT data must error")
	}
}

// A Teams/Outlook-style invite qualifies its times with a Windows TZID that
// Go's zone database can't resolve; the file's own VTIMEZONE block must be
// used instead (offset + DST rules), not the machine's zone.
func TestParseWindowsTZID(t *testing.T) {
	tmpl := strings.ReplaceAll(`BEGIN:VCALENDAR
METHOD:REQUEST
PRODID:Microsoft Exchange Server 2010
VERSION:2.0
BEGIN:VTIMEZONE
TZID:Mountain Standard Time
BEGIN:STANDARD
DTSTART:16010101T020000
TZOFFSETFROM:-0600
TZOFFSETTO:-0700
RRULE:FREQ=YEARLY;INTERVAL=1;BYDAY=1SU;BYMONTH=11
END:STANDARD
BEGIN:DAYLIGHT
DTSTART:16010101T020000
TZOFFSETFROM:-0700
TZOFFSETTO:-0600
RRULE:FREQ=YEARLY;INTERVAL=1;BYDAY=2SU;BYMONTH=3
END:DAYLIGHT
END:VTIMEZONE
BEGIN:VEVENT
ORGANIZER;CN=Ian:mailto:ian@example.com
UID:teams1
SUMMARY:Webfuse intro
DTSTART;TZID=Mountain Standard Time:%s
DTEND;TZID=Mountain Standard Time:%s
END:VEVENT
END:VCALENDAR
`, "\n", "\r\n")

	// July: daylight time, UTC-6 → 07:05 Mountain is 13:05 UTC.
	ev, err := Parse([]byte(fmt.Sprintf(tmpl, "20260710T070500", "20260710T073000")))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if want := time.Date(2026, 7, 10, 13, 5, 0, 0, time.UTC); !ev.Start.Equal(want) {
		t.Fatalf("summer Start = %v, want %v", ev.Start.UTC(), want)
	}
	if want := time.Date(2026, 7, 10, 13, 30, 0, 0, time.UTC); !ev.End.Equal(want) {
		t.Fatalf("summer End = %v, want %v", ev.End.UTC(), want)
	}

	// December: standard time, UTC-7 → 07:05 Mountain is 14:05 UTC.
	ev, err = Parse([]byte(fmt.Sprintf(tmpl, "20261210T070500", "20261210T073000")))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if want := time.Date(2026, 12, 10, 14, 5, 0, 0, time.UTC); !ev.Start.Equal(want) {
		t.Fatalf("winter Start = %v, want %v", ev.Start.UTC(), want)
	}
}

// A last-weekday rule (BYDAY=-1SU, the European shape) and a DST period that
// spans the year boundary (southern hemisphere) resolve correctly.
func TestParseVTimezoneEdges(t *testing.T) {
	data := strings.ReplaceAll(`BEGIN:VCALENDAR
METHOD:REQUEST
BEGIN:VTIMEZONE
TZID:AUS Eastern Standard Time
BEGIN:STANDARD
DTSTART:16010101T030000
TZOFFSETFROM:+1100
TZOFFSETTO:+1000
RRULE:FREQ=YEARLY;INTERVAL=1;BYDAY=1SU;BYMONTH=4
END:STANDARD
BEGIN:DAYLIGHT
DTSTART:16010101T020000
TZOFFSETFROM:+1000
TZOFFSETTO:+1100
RRULE:FREQ=YEARLY;INTERVAL=1;BYDAY=1SU;BYMONTH=10
END:DAYLIGHT
END:VTIMEZONE
BEGIN:VEVENT
UID:aus1
DTSTART;TZID=AUS Eastern Standard Time:20260115T100000
END:VEVENT
END:VCALENDAR
`, "\n", "\r\n")
	ev, err := Parse([]byte(data))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	// Mid-January in Sydney is daylight time (UTC+11): 10:00 → 23:00 UTC prev day.
	if want := time.Date(2026, 1, 14, 23, 0, 0, 0, time.UTC); !ev.Start.Equal(want) {
		t.Fatalf("Start = %v, want %v", ev.Start.UTC(), want)
	}

	// Last-Sunday rule: Europe-style custom TZID, late July is daylight (+2).
	data2 := strings.ReplaceAll(`BEGIN:VCALENDAR
METHOD:REQUEST
BEGIN:VTIMEZONE
TZID:Custom Central Europe
BEGIN:STANDARD
DTSTART:16010101T030000
TZOFFSETFROM:+0200
TZOFFSETTO:+0100
RRULE:FREQ=YEARLY;BYDAY=-1SU;BYMONTH=10
END:STANDARD
BEGIN:DAYLIGHT
DTSTART:16010101T020000
TZOFFSETFROM:+0100
TZOFFSETTO:+0200
RRULE:FREQ=YEARLY;BYDAY=-1SU;BYMONTH=3
END:DAYLIGHT
END:VTIMEZONE
BEGIN:VEVENT
UID:eu1
DTSTART;TZID=Custom Central Europe:20261101T100000
END:VEVENT
END:VCALENDAR
`, "\n", "\r\n")
	ev, err = Parse([]byte(data2))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	// Nov 1 is past the last Sunday of October: standard time (+1) → 09:00 UTC.
	if want := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC); !ev.Start.Equal(want) {
		t.Fatalf("Start = %v, want %v", ev.Start.UTC(), want)
	}
}

// A VTIMEZONE-resolved zone without usable transition rules still applies the
// standard offset (better a fixed hour off in summer than the machine's zone).
func TestParseVTimezoneNoRules(t *testing.T) {
	data := strings.ReplaceAll(`BEGIN:VCALENDAR
METHOD:REQUEST
BEGIN:VTIMEZONE
TZID:Fixed Zone
BEGIN:STANDARD
DTSTART:16010101T000000
TZOFFSETFROM:+0530
TZOFFSETTO:+0530
END:STANDARD
END:VTIMEZONE
BEGIN:VEVENT
UID:fx1
DTSTART;TZID=Fixed Zone:20260710T100000
END:VEVENT
END:VCALENDAR
`, "\n", "\r\n")
	ev, err := Parse([]byte(data))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if want := time.Date(2026, 7, 10, 4, 30, 0, 0, time.UTC); !ev.Start.Equal(want) {
		t.Fatalf("Start = %v, want %v", ev.Start.UTC(), want)
	}
}

// The REPLY carries the identity (UID, SEQUENCE), the answering attendee with
// the chosen PARTSTAT, and the organizer — what iTIP consumers key on.
func TestReply(t *testing.T) {
	ev := &Event{UID: "abc123@google.com", Sequence: 2, Organizer: "anna@example.com", Summary: "Team sync, Q3"}
	now := time.Date(2026, 7, 4, 10, 0, 0, 0, time.UTC)
	out := string(Reply(ev, "me@example.com", "Yauhen", "ACCEPTED", now))

	for _, want := range []string{
		"METHOD:REPLY",
		"UID:abc123@google.com",
		"SEQUENCE:2",
		"DTSTAMP:20260704T100000Z",
		"ORGANIZER:mailto:anna@example.com",
		"ATTENDEE;PARTSTAT=ACCEPTED;CN=Yauhen:mailto:me@example.com",
		`SUMMARY:Team sync\, Q3`,
	} {
		if !strings.Contains(out, want+"\r\n") {
			t.Fatalf("reply missing %q:\n%s", want, out)
		}
	}
	// The reply must itself parse.
	back, err := Parse([]byte(out))
	if err != nil || back.Method != "REPLY" || back.UID != ev.UID {
		t.Fatalf("reply re-parse: %+v, %v", back, err)
	}
}
