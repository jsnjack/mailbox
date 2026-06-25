package ui

import (
	"strings"
	"testing"

	"github.com/jsnjack/mailbox/internal/model"
)

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
