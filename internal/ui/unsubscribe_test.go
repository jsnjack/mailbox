package ui

import "testing"

func TestParseListUnsubscribe(t *testing.T) {
	cases := []struct {
		name     string
		header   string
		oneClick bool
		want     unsubTargets
		ok       bool
	}{
		{
			name:     "one-click https plus mailto",
			header:   "<https://list.example.com/unsub?u=1>, <mailto:leave@example.com?subject=stop>",
			oneClick: true,
			want: unsubTargets{
				OneClickURL: "https://list.example.com/unsub?u=1",
				URL:         "https://list.example.com/unsub?u=1",
				Mailto:      "leave@example.com",
				MailtoSubj:  "stop",
			},
			ok: true,
		},
		{
			name:   "https without one-click stays a browser URL",
			header: "<https://list.example.com/unsub>",
			want:   unsubTargets{URL: "https://list.example.com/unsub"},
			ok:     true,
		},
		{
			name:   "mailto only",
			header: "<mailto:unsubscribe@list.example.com>",
			want:   unsubTargets{Mailto: "unsubscribe@list.example.com"},
			ok:     true,
		},
		{
			// One-click must never target an http: URL (RFC 8058 requires https).
			name:     "http never one-click",
			header:   "<http://list.example.com/unsub>",
			oneClick: true,
			want:     unsubTargets{URL: "http://list.example.com/unsub"},
			ok:       true,
		},
		{name: "empty", header: "", ok: false},
		{name: "garbage", header: "just text", ok: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseListUnsubscribe(tc.header, tc.oneClick)
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v", ok, tc.ok)
			}
			if got != tc.want {
				t.Fatalf("targets = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// formatRawHeaders must label Gmail's bare Authentication-Results value and
// split its clauses, while passing proper header lines through unfolded.
func TestFormatRawHeaders(t *testing.T) {
	bare := "mx.google.com;       dkim=pass header.i=@bol.com header.s=bol_com header.b=uX4wl6xQ;       spf=pass (google.com: domain of automail@bol.com designates 185.14.169.141 as permitted sender) smtp.mailfrom=automail@bol.com;       dmarc=pass (p=QUARANTINE sp=QUARANTINE dis=NONE) header.from=bol.com"
	got := formatRawHeaders(bare)
	want := "Authentication-Results:\n" +
		"  mx.google.com;\n" +
		"  dkim=pass header.i=@bol.com header.s=bol_com header.b=uX4wl6xQ;\n" +
		"  spf=pass (google.com: domain of automail@bol.com designates 185.14.169.141 as permitted sender) smtp.mailfrom=automail@bol.com;\n" +
		"  dmarc=pass (p=QUARANTINE sp=QUARANTINE dis=NONE) header.from=bol.com"
	if got != want {
		t.Fatalf("bare value formatting:\n%s", got)
	}

	named := "Authentication-Results: mx.google.com;\n dkim=pass\nList-Unsubscribe: <https://x>"
	got = formatRawHeaders(named)
	want = "Authentication-Results: mx.google.com; dkim=pass\nList-Unsubscribe: <https://x>"
	if got != want {
		t.Fatalf("named headers formatting:\n%s", got)
	}
}
