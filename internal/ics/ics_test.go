package ics

import (
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
