package ai

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// scriptedProvider fails or streams per its script, recording calls.
type scriptedProvider struct {
	requestErr error   // returned by Stream itself
	chunks     []Chunk // streamed otherwise
	calls      int
	gotOpts    *Options
}

func (s *scriptedProvider) Name() string { return "scripted" }

func (s *scriptedProvider) Stream(ctx context.Context, system string, msgs []Msg) (<-chan Chunk, error) {
	return s.StreamOpts(ctx, system, msgs, Options{})
}

func (s *scriptedProvider) StreamOpts(_ context.Context, _ string, _ []Msg, o Options) (<-chan Chunk, error) {
	s.calls++
	s.gotOpts = &o
	if s.requestErr != nil {
		return nil, s.requestErr
	}
	ch := make(chan Chunk, len(s.chunks))
	for _, c := range s.chunks {
		ch <- c
	}
	close(ch)
	return ch, nil
}

func collect(t *testing.T, ch <-chan Chunk) (string, error) {
	t.Helper()
	var b strings.Builder
	for c := range ch {
		if c.Err != nil {
			return b.String(), c.Err
		}
		b.WriteString(c.Text)
	}
	return b.String(), nil
}

// A request error on the primary moves to the backup.
func TestFailoverOnRequestError(t *testing.T) {
	primary := &scriptedProvider{requestErr: errors.New("connection refused")}
	backup := &scriptedProvider{chunks: []Chunk{{Text: "ok"}}}
	f := newFailoverProvider([]Provider{primary, backup}, []string{"p", "b"})

	ch, err := f.Stream(context.Background(), "", []Msg{{Role: RoleUser, Content: "hi"}})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	got, err := collect(t, ch)
	if err != nil || got != "ok" {
		t.Fatalf("got %q, %v", got, err)
	}
	if primary.calls != 1 || backup.calls != 1 {
		t.Fatalf("calls: primary=%d backup=%d", primary.calls, backup.calls)
	}
}

// A stream that errors before yielding any content moves to the backup.
func TestFailoverOnPreContentStreamError(t *testing.T) {
	primary := &scriptedProvider{chunks: []Chunk{{Err: errors.New("api status 429")}}}
	backup := &scriptedProvider{chunks: []Chunk{{Text: "recovered"}}}
	f := newFailoverProvider([]Provider{primary, backup}, []string{"p", "b"})

	ch, err := f.Stream(context.Background(), "", nil)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	got, err := collect(t, ch)
	if err != nil || got != "recovered" {
		t.Fatalf("got %q, %v", got, err)
	}
}

// An error after content has flowed must propagate — retrying would duplicate
// the partial output the caller already consumed.
func TestFailoverMidStreamErrorPropagates(t *testing.T) {
	primary := &scriptedProvider{chunks: []Chunk{{Text: "partial "}, {Err: errors.New("cut off")}}}
	backup := &scriptedProvider{chunks: []Chunk{{Text: "never"}}}
	f := newFailoverProvider([]Provider{primary, backup}, []string{"p", "b"})

	ch, err := f.Stream(context.Background(), "", nil)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	got, err := collect(t, ch)
	if err == nil || got != "partial " {
		t.Fatalf("got %q, err %v — want the partial text and the error", got, err)
	}
	if backup.calls != 0 {
		t.Fatal("backup must not run after content flowed")
	}
}

// When every model fails, the last error is returned.
func TestFailoverAllFail(t *testing.T) {
	p1 := &scriptedProvider{requestErr: errors.New("down 1")}
	p2 := &scriptedProvider{chunks: []Chunk{{Err: errors.New("down 2")}}}
	f := newFailoverProvider([]Provider{p1, p2}, []string{"a", "b"})

	ch, err := f.Stream(context.Background(), "", nil)
	if err == nil {
		// p2's failure arrives as the last provider's first chunk, which is
		// committed (no further fallback) — the error comes through the channel.
		if _, cerr := collect(t, ch); cerr == nil {
			t.Fatal("expected an error from the last provider")
		}
		return
	}
	if !strings.Contains(err.Error(), "down") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// Options pass through the failover chain to the model that serves the request.
func TestFailoverForwardsOptions(t *testing.T) {
	primary := &scriptedProvider{requestErr: errors.New("down")}
	backup := &scriptedProvider{chunks: []Chunk{{Text: "ok"}}}
	f := newFailoverProvider([]Provider{primary, backup}, []string{"p", "b"})

	zero := 0.0
	ch, err := f.StreamOpts(context.Background(), "", nil, Options{Temperature: &zero})
	if err != nil {
		t.Fatalf("StreamOpts: %v", err)
	}
	if _, err := collect(t, ch); err != nil {
		t.Fatalf("collect: %v", err)
	}
	if backup.gotOpts == nil || backup.gotOpts.Temperature == nil || *backup.gotOpts.Temperature != 0 {
		t.Fatalf("options not forwarded: %+v", backup.gotOpts)
	}
}

// SetProvider swaps the provider a live Assistant uses for new requests.
func TestAssistantSetProvider(t *testing.T) {
	old := &fakeProvider{chunks: []Chunk{{Text: "old"}}}
	a := NewAssistant(old)
	next := &fakeProvider{chunks: []Chunk{{Text: `["Receipt"]`}}}
	a.SetProvider(next)

	got, err := a.Categorize(context.Background(), []string{"a"})
	if err != nil || len(got) != 1 || got[0] != "Receipt" {
		t.Fatalf("Categorize after swap = %#v, %v", got, err)
	}
	if old.gotMsgs != nil {
		t.Fatal("old provider used after swap")
	}
}

// transferStub is a Provider whose byte counters are fixed.
type transferStub struct {
	fakeProvider
	in, out int64
}

func (t *transferStub) transfer() (int64, int64) { return t.in, t.out }

// Session AI stats: every op counts a request, and byte counters survive a
// live provider swap (the old provider's total rolls into the baseline).
func TestAssistantSessionStats(t *testing.T) {
	p1 := &transferStub{fakeProvider: fakeProvider{chunks: []Chunk{{Text: `["Receipt"]`}}}, in: 100, out: 40}
	a := NewAssistant(p1)
	if _, err := a.Categorize(context.Background(), []string{"a"}); err != nil {
		t.Fatalf("Categorize: %v", err)
	}
	if got := a.Requests(); got != 1 {
		t.Fatalf("Requests = %d, want 1", got)
	}
	if in, out := a.Transferred(); in != 100 || out != 40 {
		t.Fatalf("Transferred = %d/%d", in, out)
	}

	p2 := &transferStub{fakeProvider: fakeProvider{chunks: []Chunk{{Text: "ok"}}}, in: 7, out: 3}
	a.SetProvider(p2)
	if in, out := a.Transferred(); in != 107 || out != 43 {
		t.Fatalf("Transferred after swap = %d/%d, want 107/43 (baseline + new)", in, out)
	}
	if err := a.Ping(context.Background()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if got := a.Requests(); got != 2 {
		t.Fatalf("Requests after ping = %d, want 2", got)
	}
}
