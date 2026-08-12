package ui

import (
	"testing"
	"time"
)

func TestNotableFinish(t *testing.T) {
	cases := []struct {
		op, note string
		want     bool
	}{
		// The minute-by-minute mail check that found nothing is the app
		// breathing: no checkmark, no flash, straight back to resting.
		{"sync", "up to date", false},
		{"sync", "", false},
		// A sync that actually did something is an event.
		{"sync", "1 change", true},
		{"sync", "error: connection refused", true},
		// The user caused a cancel; announcing it back to them is noise.
		{"ai", noteCancelled, false},
		{"ai", "2.1 KB · granite", true},
		{"mail", "3 messages", true},
		{"send", "", true},
	}
	for _, c := range cases {
		if got := notableFinish(c.op, c.note); got != c.want {
			t.Errorf("notableFinish(%q, %q) = %v, want %v", c.op, c.note, got, c.want)
		}
	}
}

func TestFinishText(t *testing.T) {
	cases := []struct{ label, note, want string }{
		{"Checking mail", "1 change", "Mail checked · 1 change"},
		{"Archiving", "2 messages", "Archived · 2 messages"},
		{"Summarizing thread", "", "Thread summarized"},
		// A failure keeps the original phrase: past tense would claim work that
		// did not happen.
		{"Summarizing thread", "error: connection refused", "Summarizing thread failed · connection refused"},
		// A Report has no running phase, so its label is already past tense.
		{"Returned from snooze", "2 conversations", "Returned from snooze · 2 conversations"},
		// A result with no operation behind it still reads as itself.
		{"", "error: no account", "no account"},
	}
	for _, c := range cases {
		if got := finishText(c.label, c.note); got != c.want {
			t.Errorf("finishText(%q, %q) = %q, want %q", c.label, c.note, got, c.want)
		}
	}
}

func TestWorthFlashing(t *testing.T) {
	cases := []struct {
		name           string
		ok, hadRunning bool
		ran            time.Duration
		want           bool
	}{
		// Triaging with the "a" key: each archive settles instantly and has its
		// own undo toast, so the corner of the window must not strobe.
		{"quick archive", true, true, 120 * time.Millisecond, false},
		{"slow AI draft", true, true, 4 * time.Second, true},
		{"failure", false, true, 50 * time.Millisecond, true},
		// A snooze waking, a queued message going out: it happened to the app
		// rather than being watched, so it earns the flash whatever its timing.
		{"instant report", true, false, 0, true},
	}
	for _, c := range cases {
		if got := worthFlashing(c.ok, c.hadRunning, c.ran); got != c.want {
			t.Errorf("%s: worthFlashing = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestRestingText(t *testing.T) {
	now := time.Date(2026, 8, 11, 15, 4, 0, 0, time.UTC)
	cases := []struct {
		name    string
		last    time.Time
		failing bool
		want    string
	}{
		{"never synced", time.Time{}, false, "Not synced yet"},
		{"just synced", now.Add(-5 * time.Second), false, "Up to date · just now"},
		// Past the "just now" window the line settles on a clock time, so it
		// stops changing width as the minutes pass.
		{"a while ago", now.Add(-20 * time.Minute), false, "Up to date · 14:44"},
		// A failing provider owns the line: this is the whole of that state's UI.
		{"ai down", now.Add(-5 * time.Second), true, "AI provider unavailable"},
	}
	for _, c := range cases {
		if got := restingText(c.last, now, c.failing); got != c.want {
			t.Errorf("%s: restingText = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestHumanDuration(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want string
	}{
		{400 * time.Millisecond, "0.4s"},
		{12300 * time.Millisecond, "12.3s"},
		{125 * time.Second, "2m05s"},
	}
	for _, c := range cases {
		if got := humanDuration(c.in); got != c.want {
			t.Errorf("humanDuration(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestBarText(t *testing.T) {
	if got := barText("Work", "Checking mail"); got != "Checking mail · Work" {
		t.Errorf("barText with account = %q", got)
	}
	if got := barText("", "Pruned old bodies"); got != "Pruned old bodies" {
		t.Errorf("barText app-wide = %q", got)
	}
}
