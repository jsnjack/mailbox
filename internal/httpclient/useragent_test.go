package httpclient

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestTransportSetsUserAgent(t *testing.T) {
	orig := UserAgent
	UserAgent = "mailbox/1.2.3"
	defer func() { UserAgent = orig }()

	var got string
	base := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		got = req.Header.Get("User-Agent")
		return httptest.NewRecorder().Result(), nil
	})
	tr := &Transport{Base: base}

	req, _ := http.NewRequest("GET", "http://example.com", nil)
	if _, err := tr.RoundTrip(req); err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	if got != "mailbox/1.2.3" {
		t.Fatalf("User-Agent = %q, want %q", got, "mailbox/1.2.3")
	}
	// The original request must be untouched (RoundTrip clones before mutating).
	if req.Header.Get("User-Agent") != "" {
		t.Fatalf("original request was mutated: User-Agent = %q", req.Header.Get("User-Agent"))
	}
}

func TestTransportKeepsExistingUserAgent(t *testing.T) {
	var got string
	base := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		got = req.Header.Get("User-Agent")
		return httptest.NewRecorder().Result(), nil
	})
	tr := &Transport{Base: base}

	req, _ := http.NewRequest("GET", "http://example.com", nil)
	req.Header.Set("User-Agent", "custom-agent/1.0")
	if _, err := tr.RoundTrip(req); err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	if got != "custom-agent/1.0" {
		t.Fatalf("User-Agent = %q, want caller's value preserved", got)
	}
}
