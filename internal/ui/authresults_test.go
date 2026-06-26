package ui

import "testing"

func TestParseAuthResults(t *testing.T) {
	tests := []struct {
		name   string
		header string
		level  authLevel
	}{
		{"empty", "", authUnknown},
		{
			"full pass",
			"mx.google.com; dkim=pass header.i=@x.com; spf=pass (google.com: domain of a@x.com) smtp.mailfrom=a@x.com; dmarc=pass (p=REJECT) header.from=x.com",
			authPass,
		},
		{
			"dmarc fail",
			"mx.google.com; dkim=fail header.i=@x.com; spf=softfail; dmarc=fail (p=NONE) header.from=x.com",
			authFail,
		},
		{
			"spf pass, no dmarc",
			"mx.google.com; spf=pass smtp.mailfrom=a@x.com; dkim=none",
			authPartial,
		},
		{
			"all none/neutral",
			"mx.google.com; spf=none; dkim=none; dmarc=none",
			authUnknown,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := parseAuthResults(tt.header); got.level != tt.level {
				t.Fatalf("parseAuthResults level = %d, want %d (detail %q)", got.level, tt.level, got.detail)
			}
		})
	}
}
