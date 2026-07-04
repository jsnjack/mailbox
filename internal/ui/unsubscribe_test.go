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
