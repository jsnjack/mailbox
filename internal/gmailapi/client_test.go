package gmailapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"testing"
	"time"

	"google.golang.org/api/googleapi"
)

func TestIsRetryable(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"429", &googleapi.Error{Code: 429}, true},
		{"503", &googleapi.Error{Code: 503}, true},
		{"403 rate limit", &googleapi.Error{Code: 403, Errors: []googleapi.ErrorItem{{Reason: "userRateLimitExceeded"}}}, true},
		{"403 other", &googleapi.Error{Code: 403, Errors: []googleapi.ErrorItem{{Reason: "insufficientPermissions"}}}, false},
		{"404", &googleapi.Error{Code: 404}, false},
		{"400", &googleapi.Error{Code: 400}, false},
		{"plain error", errors.New("boom"), false},
		{"net conn refused", &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}, true},
		{"wrapped net error", fmt.Errorf("get: %w", &net.OpError{Op: "read", Err: errors.New("connection reset by peer")}), true},
		{"io.EOF", io.EOF, true},
		{"io.ErrUnexpectedEOF", io.ErrUnexpectedEOF, true},
		{"context canceled", context.Canceled, false},
		{"wrapped context canceled", fmt.Errorf("call: %w", context.Canceled), false},
		{"context deadline", context.DeadlineExceeded, false},
		{"nil", nil, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isRetryable(tc.err); got != tc.want {
				t.Fatalf("isRetryable = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestIsHistoryExpired(t *testing.T) {
	if !IsHistoryExpired(&googleapi.Error{Code: 404}) {
		t.Fatal("404 should be history-expired")
	}
	if IsHistoryExpired(&googleapi.Error{Code: 429}) {
		t.Fatal("429 should not be history-expired")
	}
	if IsHistoryExpired(errors.New("x")) {
		t.Fatal("plain error should not be history-expired")
	}
}

func TestBackoffDurationWithinCap(t *testing.T) {
	for attempt := 1; attempt <= 12; attempt++ {
		d := backoffDuration(attempt)
		if d <= 0 {
			t.Fatalf("attempt %d: non-positive backoff %v", attempt, d)
		}
		if d > backoffCap+backoffCap/2 {
			t.Fatalf("attempt %d: backoff %v exceeds cap+jitter", attempt, d)
		}
	}
	_ = time.Second
}
