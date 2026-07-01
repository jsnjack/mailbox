package ui

import (
	"strings"

	"github.com/jsnjack/mailbox/internal/logging"
)

// authLevelName maps an authLevel to a colour-coded label for trace logs.
func authLevelName(l authLevel) string {
	switch l {
	case authFail:
		return "red"
	case authPartial:
		return "amber"
	case authPass:
		return "green"
	default:
		return "unknown"
	}
}

// authLevel classifies a message's sender-authentication outcome.
type authLevel int

const (
	authUnknown authLevel = iota // no usable verdict
	authFail                     // a check failed — possible spoofing
	authPartial                  // SPF/DKIM passed, but DMARC didn't
	authPass                     // DMARC passed (authenticated and aligned)
)

// authVerdict is the parsed Authentication-Results outcome.
type authVerdict struct {
	level  authLevel
	detail string // human summary, e.g. "SPF, DKIM, DMARC passed"
}

// parseAuthResults interprets the Authentication-Results header (the verdict the
// receiving server — Gmail — computed for SPF/DKIM/DMARC) into a sender-
// authenticity verdict. An empty/inconclusive header yields authUnknown.
func parseAuthResults(header string) authVerdict {
	v := parseAuthResultsInner(header)
	logging.Trace("ui: auth-results parsed", "verdict", authLevelName(v.level), "detail", v.detail, "header", logging.Body(header))
	return v
}

func parseAuthResultsInner(header string) authVerdict {
	spf := authMethodResult(header, "spf")
	dkim := authMethodResult(header, "dkim")
	dmarc := authMethodResult(header, "dmarc")
	if spf == "" && dkim == "" && dmarc == "" {
		return authVerdict{}
	}

	var passed []string
	if spf == "pass" {
		passed = append(passed, "SPF")
	}
	if dkim == "pass" {
		passed = append(passed, "DKIM")
	}
	if dmarc == "pass" {
		passed = append(passed, "DMARC")
	}
	anyFail := spf == "fail" || dkim == "fail" || dmarc == "fail"

	switch {
	case dmarc == "pass":
		return authVerdict{level: authPass, detail: strings.Join(passed, ", ") + " passed"}
	case len(passed) > 0 && !anyFail:
		return authVerdict{level: authPartial, detail: strings.Join(passed, ", ") + " passed"}
	case anyFail:
		var failed []string
		if spf == "fail" {
			failed = append(failed, "SPF")
		}
		if dkim == "fail" {
			failed = append(failed, "DKIM")
		}
		if dmarc == "fail" {
			failed = append(failed, "DMARC")
		}
		return authVerdict{level: authFail, detail: strings.Join(failed, ", ") + " failed"}
	default:
		return authVerdict{} // only none/neutral/softfail — inconclusive
	}
}

// authMethodResult extracts the result token for an auth method (spf/dkim/dmarc)
// from an Authentication-Results value, e.g. "pass" out of "dkim=pass header.i=…".
func authMethodResult(header, method string) string {
	for _, clause := range strings.Split(header, ";") {
		c := strings.ToLower(strings.TrimSpace(clause))
		prefix := method + "="
		if strings.HasPrefix(c, prefix) {
			v := c[len(prefix):]
			if i := strings.IndexAny(v, " \t("); i >= 0 {
				v = v[:i]
			}
			return v
		}
	}
	return ""
}
