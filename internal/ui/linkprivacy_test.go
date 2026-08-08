package ui

import (
	"net/url"
	"testing"
)

func TestCleanExternalLinkStripsTrackingParams(t *testing.T) {
	got, stats := cleanExternalLink("https://example.com/page?utm_source=newsletter&id=7&FBCLID=abc&mc_eid=user#details")
	want := "https://example.com/page?id=7#details"
	if got != want {
		t.Fatalf("cleaned link = %q, want %q", got, want)
	}
	if stats.Unwrapped != 0 || stats.StrippedParams != 3 {
		t.Fatalf("stats = %+v", stats)
	}
}

func TestCleanExternalLinkPreservesFunctionalParams(t *testing.T) {
	raw := "https://example.com/reset?campaign=spring&token=secret&next=%2Faccount"
	got, stats := cleanExternalLink(raw)
	if got != raw || stats != (linkCleanStats{}) {
		t.Fatalf("functional link changed: got %q stats=%+v", got, stats)
	}
}

func TestCleanExternalLinkUnwrapsExplicitDestination(t *testing.T) {
	raw := "https://click.mailer.example/redirect?url=https%3A%2F%2Fshop.example%2Fp%3Futm_medium%3Demail%26item%3D42&signature=keep"
	got, stats := cleanExternalLink(raw)
	want := "https://shop.example/p?item=42"
	if got != want {
		t.Fatalf("cleaned link = %q, want %q", got, want)
	}
	if stats.Unwrapped != 1 || stats.StrippedParams != 1 {
		t.Fatalf("stats = %+v", stats)
	}
}

func TestCleanExternalLinkUnwrapsDoubleEncoding(t *testing.T) {
	raw := "https://links.mailer.example/click?target=https%253A%252F%252Fexample.com%252Fdocs%253Fgclid%253Dabc%2526page%253D2"
	got, stats := cleanExternalLink(raw)
	want := "https://example.com/docs?page=2"
	if got != want || stats.Unwrapped != 1 || stats.StrippedParams != 1 {
		t.Fatalf("got %q stats=%+v, want %q", got, stats, want)
	}
}

func TestCleanExternalLinkBoundsNestedWrappers(t *testing.T) {
	target := "https://destination.example/?utm_source=email&ok=1"
	for i := 0; i < maxLinkUnwraps+1; i++ {
		target = "https://click.mailer.example/redirect?url=" + queryEscape(target)
	}
	got, stats := cleanExternalLink(target)
	if stats.Unwrapped != maxLinkUnwraps {
		t.Fatalf("unwrapped %d times, want %d", stats.Unwrapped, maxLinkUnwraps)
	}
	if got == "https://destination.example/?ok=1" {
		t.Fatal("cleaner exceeded its unwrap bound")
	}
}

func TestCleanExternalLinkRejectsAmbiguousWrapper(t *testing.T) {
	raw := "https://click.mailer.example/redirect?url=https%3A%2F%2Fa.example&target=https%3A%2F%2Fb.example"
	got, stats := cleanExternalLink(raw)
	if got != raw || stats != (linkCleanStats{}) {
		t.Fatalf("ambiguous wrapper changed: got %q stats=%+v", got, stats)
	}
}

func TestCleanExternalLinkDoesNotBypassApplicationRedirect(t *testing.T) {
	raw := "https://accounts.example/login/redirect?url=https%3A%2F%2Faccounts.example%2Fdashboard"
	got, stats := cleanExternalLink(raw)
	if got != raw || stats != (linkCleanStats{}) {
		t.Fatalf("same-site application redirect changed: got %q stats=%+v", got, stats)
	}
}

func TestCleanExternalLinkRequiresWrapperSignal(t *testing.T) {
	raw := "https://example.com/article?url=https%3A%2F%2Fsource.example%2Fresearch"
	got, stats := cleanExternalLink(raw)
	if got != raw || stats != (linkCleanStats{}) {
		t.Fatalf("ordinary URL-valued query changed: got %q stats=%+v", got, stats)
	}
}

func TestCleanExternalLinkRejectsNonHTTPDestination(t *testing.T) {
	for _, raw := range []string{
		"https://click.example/redirect?url=javascript%3Aalert%281%29",
		"https://click.example/redirect?url=file%3A%2F%2F%2Fetc%2Fpasswd",
		"mailto:person@example.com",
	} {
		got, stats := cleanExternalLink(raw)
		if got != raw || stats != (linkCleanStats{}) {
			t.Errorf("unsafe/non-HTTP link changed: got %q stats=%+v", got, stats)
		}
	}
}

func queryEscape(raw string) string {
	// Keeping this helper local makes the nested-wrapper fixture readable while
	// still exercising the production decoder at every level.
	return url.QueryEscape(raw)
}
