package ui

import (
	"context"
	"errors"
	"testing"

	"github.com/jsnjack/mailbox/internal/ai"
)

func TestCollectComposeAI(t *testing.T) {
	for _, tc := range []struct {
		name   string
		chunks []ai.Chunk
		cancel bool
		want   string
		fail   bool
	}{
		{name: "complete", chunks: []ai.Chunk{{Text: "Hello "}, {Text: "world"}}, want: "Hello world"},
		{name: "partial failure", chunks: []ai.Chunk{{Text: "Incomplete"}, {Err: errors.New("connection lost")}}, fail: true},
		{name: "empty", fail: true},
		{name: "cancelled", cancel: true, fail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			ch := make(chan ai.Chunk, len(tc.chunks))
			for _, c := range tc.chunks {
				ch <- c
			}
			close(ch)
			if tc.cancel {
				cancel()
			}
			got, err := collectComposeAI(ctx, ch)
			if (err != nil) != tc.fail || got != tc.want {
				t.Fatalf("got %q, %v; want %q, failure %v", got, err, tc.want, tc.fail)
			}
		})
	}
}

func TestSwitchComposeSignature(t *testing.T) {
	for _, tc := range []struct{ name, body, old, next, want string }{
		{"unchanged", "Hello\n\nWork", "Work", "Personal", "Hello\n\nPersonal"},
		{"edited", "Hello\n\nWork, mobile", "Work", "Personal", "Hello\n\nWork, mobile"},
		{"quoted", "Hello\n\nWork\n\nOn Monday, Pat wrote:\n> Original", "Work", "Personal", "Hello\n\nPersonal\n\nOn Monday, Pat wrote:\n> Original"},
		{"same word within line", "I need Work", "Work", "Personal", "I need Work"},
		{"no previous", "Hello", "", "Personal", "Hello\n\nPersonal"},
		{"remove", "Hello\n\nWork", "Work", "", "Hello"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := switchComposeSignature(tc.body, tc.old, tc.next); got != tc.want {
				t.Fatalf("got %q; want %q", got, tc.want)
			}
		})
	}
}
