package ai

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// feedAll streams the pieces through the filter the way streamSSE does, one SSE
// chunk at a time, and returns everything the user would have seen.
func feedAll(pieces ...string) string {
	var f thinkFilter
	out := ""
	for _, p := range pieces {
		out += f.feed(p)
	}
	return out + f.flush()
}

func TestThinkFilterStripsInlineReasoning(t *testing.T) {
	cases := []struct {
		name   string
		pieces []string
		want   string
	}{
		{"no tags at all", []string{"Hi Elena,", " the 11th works."}, "Hi Elena, the 11th works."},
		{"whole block in one chunk",
			[]string{"<think>maybe Receipt?</think>[\"Notification\"]"}, "[\"Notification\"]"},
		{"reasoning before answer, chunked",
			[]string{"<think>", "hmm, ", "the sender is a bot", "</think>", "Hi,", " thanks"},
			"Hi, thanks"},
		// The case that motivates a stateful filter: the tag is split across SSE
		// chunks, so a per-chunk regex would leak "<thi" then the reasoning.
		{"open tag split across chunks",
			[]string{"<thi", "nk>secret reasoning</thi", "nk>visible"}, "visible"},
		{"close tag split across chunks",
			[]string{"<think>reason", "ing</th", "ink>answer"}, "answer"},
		{"thinking variant", []string{"<thinking>x</thinking>ok"}, "ok"},
		{"reasoning variant", []string{"<reasoning>x</reasoning>ok"}, "ok"},
		{"two blocks", []string{"<think>a</think>one<think>b</think>two"}, "onetwo"},
		{"text that merely looks like a tag start is released",
			[]string{"compare a < b and c"}, "compare a < b and c"},
		{"lone angle bracket at end is released by flush",
			[]string{"ends with <"}, "ends with <"},
		{"unterminated think block yields nothing",
			[]string{"<think>still going and going"}, ""},
		{"content before an opening tag is kept",
			[]string{"Answer: ", "<think>why</think>", "42"}, "Answer: 42"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := feedAll(c.pieces...); got != c.want {
				t.Fatalf("feedAll(%q) = %q, want %q", c.pieces, got, c.want)
			}
		})
	}
}

// A single byte at a time is the worst case for a streaming filter: every tag
// boundary lands between chunks.
func TestThinkFilterByteAtATime(t *testing.T) {
	in := "before<think>hidden</think>after"
	var pieces []string
	for _, r := range in {
		pieces = append(pieces, string(r))
	}
	if got, want := feedAll(pieces...), "beforeafter"; got != want {
		t.Fatalf("byte-at-a-time = %q, want %q", got, want)
	}
}

// Exercises StreamOpts against a real HTTP server and inspects the JSON that
// actually went over the wire — the point of the option is the request body.
func TestReasoningSuppressedByDefault(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts Options
		want bool
	}{
		// Suppression is ON by default and only lifted by an explicit opt-in, so
		// that a new op cannot accidentally inherit 13-second drafts.
		{"suppressed by default", Options{}, true},
		{"lifted when reasoning is allowed", Options{AllowReasoning: true}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got map[string]any
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				_ = json.Unmarshal(body, &got)
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte("data: [DONE]\n"))
			}))
			defer srv.Close()

			p := newOpenAIProvider(srv.URL, "", "m")
			ch, err := p.StreamOpts(context.Background(), "sys",
				[]Msg{{Role: RoleUser, Content: "hi"}}, tc.opts)
			if err != nil {
				t.Fatalf("StreamOpts: %v", err)
			}
			for range ch { // drain
			}

			effort, hasEffort := got["reasoning_effort"]
			kwargs, hasKwargs := got["chat_template_kwargs"].(map[string]any)
			if hasEffort != tc.want || hasKwargs != tc.want {
				t.Fatalf("reasoning_effort=%v chat_template_kwargs=%v, want present=%v",
					hasEffort, hasKwargs, tc.want)
			}
			if tc.want {
				if effort != "none" {
					t.Errorf("reasoning_effort = %v, want \"none\"", effort)
				}
				if kwargs["enable_thinking"] != false {
					t.Errorf("enable_thinking = %v, want false", kwargs["enable_thinking"])
				}
			}
		})
	}
}
