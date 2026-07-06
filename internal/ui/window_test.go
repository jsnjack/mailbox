package ui

import (
	"strings"
	"testing"
	"time"

	"github.com/jsnjack/mailbox/internal/model"
)

func TestRelativeDate(t *testing.T) {
	now := time.Date(2026, time.June, 25, 14, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		when time.Time
		want string
	}{
		{"today shows time", time.Date(2026, time.June, 25, 9, 30, 0, 0, time.UTC), "09:30"},
		{"earlier this week shows weekday", time.Date(2026, time.June, 22, 9, 0, 0, 0, time.UTC), "Mon"},
		{"this year shows month/day", time.Date(2026, time.January, 2, 9, 0, 0, 0, time.UTC), "Jan 2"},
		{"older shows year", time.Date(2024, time.December, 31, 9, 0, 0, 0, time.UTC), "Dec 31, 2024"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := relativeDate(c.when, now); got != c.want {
				t.Fatalf("relativeDate(%v) = %q, want %q", c.when, got, c.want)
			}
		})
	}
	if got := relativeDate(time.Time{}, now); got != "" {
		t.Fatalf("zero time should render empty, got %q", got)
	}
}

func TestReplyAllRecipients(t *testing.T) {
	m := model.Message{
		FromAddr: "alice@x.com",
		ToAddrs:  "Me <me@self.com>, Bob <bob@x.com>",
		CcAddrs:  "carol@x.com, me@self.com",
	}
	to, cc := replyAllRecipients(m, "me@self.com")

	if !strings.Contains(to, "alice@x.com") || !strings.Contains(to, "bob@x.com") {
		t.Fatalf("To missing expected recipients: %q", to)
	}
	if strings.Contains(to, "me@self.com") || strings.Contains(cc, "me@self.com") {
		t.Fatalf("self address should be excluded: to=%q cc=%q", to, cc)
	}
	if cc != "carol@x.com" {
		t.Fatalf("Cc = %q, want carol@x.com", cc)
	}
}

func TestReplyTarget(t *testing.T) {
	// No Reply-To: falls back to From.
	if got := replyTarget(model.Message{FromAddr: "alice@x.com"}); got != "alice@x.com" {
		t.Fatalf("no reply-to: got %q, want alice@x.com", got)
	}
	// Reply-To set: it wins over From.
	if got := replyTarget(model.Message{FromAddr: "no-reply@x.com", ReplyTo: "list@x.com"}); got != "list@x.com" {
		t.Fatalf("reply-to: got %q, want list@x.com", got)
	}
}

func TestReplyAllHonorsReplyTo(t *testing.T) {
	// Reply-To replaces From in the To line; From is not added.
	m := model.Message{
		FromAddr: "no-reply@x.com",
		ReplyTo:  "List <list@x.com>",
		ToAddrs:  "Bob <bob@x.com>",
	}
	to, _ := replyAllRecipients(m, "me@self.com")
	if !strings.Contains(to, "list@x.com") {
		t.Fatalf("To missing Reply-To address: %q", to)
	}
	if strings.Contains(to, "no-reply@x.com") {
		t.Fatalf("From should not be a recipient when Reply-To is set: %q", to)
	}
}

func TestHTMLToText(t *testing.T) {
	got := htmlToText(`<p>Hello <b>Bob</b>,</p><div>It&#39;s &amp; done</div>`)
	if strings.ContainsAny(got, "<>") {
		t.Fatalf("HTML tags remain: %q", got)
	}
	for _, want := range []string{"Hello", "Bob", "It's", "& done"} {
		if !strings.Contains(got, want) {
			t.Fatalf("content lost: missing %q in %q", want, got)
		}
	}
	// Already-plain text is returned cleanly.
	if got := htmlToText("just text"); got != "just text" {
		t.Fatalf("plain text mangled: %q", got)
	}
}

func TestStripCodeFence(t *testing.T) {
	cases := []struct{ in, want string }{
		{"```html\n<p>Hi</p>\n```", "<p>Hi</p>"},
		{"```\n<b>x</b>\n```", "<b>x</b>"},
		{"<p>plain</p>", "<p>plain</p>"},
	}
	for _, c := range cases {
		if got := strings.TrimSpace(stripCodeFence(c.in)); got != c.want {
			t.Fatalf("stripCodeFence(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestOutboxHeaders(t *testing.T) {
	raw := []byte("From: me@x.com\r\nTo: Bob <bob@x.com>\r\nSubject: Lunch?\r\n\r\nbody here\r\n")
	to, subject := outboxHeaders(raw)
	if to != "Bob <bob@x.com>" {
		t.Fatalf("to = %q", to)
	}
	if subject != "Lunch?" {
		t.Fatalf("subject = %q", subject)
	}
	// Garbage must not panic; it yields empty strings.
	if to, subject := outboxHeaders([]byte("not a message")); to != "" || subject != "" {
		t.Fatalf("garbage parsed to %q / %q", to, subject)
	}
}

func TestOutboxStatus(t *testing.T) {
	if got := outboxStatus(model.OutboxItem{State: "queued"}); got != "Queued" {
		t.Fatalf("queued status = %q", got)
	}
	got := outboxStatus(model.OutboxItem{State: "failed", Attempts: 2, LastError: "timeout"})
	if got != "Failed (attempt 2): timeout" {
		t.Fatalf("failed status = %q", got)
	}
}

func TestEnsurePrefixes(t *testing.T) {
	if got := ensureRePrefix("Hello"); got != "Re: Hello" {
		t.Fatalf("ensureRePrefix = %q", got)
	}
	if got := ensureRePrefix("Re: Hello"); got != "Re: Hello" {
		t.Fatalf("ensureRePrefix double-prefixed: %q", got)
	}
	if got := ensureFwdPrefix("Hello"); got != "Fwd: Hello" {
		t.Fatalf("ensureFwdPrefix = %q", got)
	}
}

func TestLinkifyText(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"no url", "just some text", "just some text"},
		{
			"plain url",
			"Link https://mrt-wake.surfly.com/build/587143",
			`Link <a href="https://mrt-wake.surfly.com/build/587143">https://mrt-wake.surfly.com/build/587143</a>`,
		},
		{
			"trailing sentence period not in link",
			"See https://x.com/a.",
			`See <a href="https://x.com/a">https://x.com/a</a>.`,
		},
		{
			"unbalanced closing paren left out",
			"(see https://x.com/a)",
			`(see <a href="https://x.com/a">https://x.com/a</a>)`,
		},
		{
			"balanced parens kept",
			"https://en.wikipedia.org/wiki/Foo_(bar)",
			`<a href="https://en.wikipedia.org/wiki/Foo_(bar)">https://en.wikipedia.org/wiki/Foo_(bar)</a>`,
		},
		{
			"query string ampersand escaped in href",
			"https://x.com/p?a=1&b=2",
			`<a href="https://x.com/p?a=1&amp;b=2">https://x.com/p?a=1&amp;b=2</a>`,
		},
		{
			"surrounding text is escaped",
			"a <b> & https://x.com c",
			`a &lt;b&gt; &amp; <a href="https://x.com">https://x.com</a> c`,
		},
		{
			"two urls",
			"https://a.com and https://b.com",
			`<a href="https://a.com">https://a.com</a> and <a href="https://b.com">https://b.com</a>`,
		},
		{"no scheme is not linked", "visit example.com today", "visit example.com today"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := linkifyText(tt.in); got != tt.want {
				t.Fatalf("linkifyText(%q)\n  = %q\nwant %q", tt.in, got, tt.want)
			}
		})
	}
}

// cleanAIContext removes the invisible preheader padding (U+034F + NBSP runs)
// LinkedIn-style mail packs into snippets, and collapses whitespace.
func TestCleanAIContext(t *testing.T) {
	in := "Senior VP of Sales at Itransition Group \u034f\u00a0\u034f\u00a0\u034f\u00a0 \u200b\u200d\ufeff tail"
	if got := cleanAIContext(in); got != "Senior VP of Sales at Itransition Group tail" {
		t.Fatalf("cleanAIContext = %q", got)
	}
	if got := cleanAIContext("  plain   text \n unchanged  "); got != "plain text unchanged" {
		t.Fatalf("whitespace collapse = %q", got)
	}
}
