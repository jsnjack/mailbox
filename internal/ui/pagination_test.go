package ui

import (
	"reflect"
	"testing"
	"time"

	"github.com/jsnjack/mailbox/internal/model"
)

func TestMergeThreadSummariesUpdatesDuplicatesAndKeepsStableNewestOrder(t *testing.T) {
	at := time.Unix(100, 0)
	existing := []model.ThreadSummary{
		{ThreadID: "new", Latest: model.Message{RowID: 4, InternalDate: time.Unix(200, 0)}, Count: 1},
		{ThreadID: "changed", Latest: model.Message{RowID: 2, InternalDate: at}, Count: 1},
	}
	added := []model.ThreadSummary{
		{ThreadID: "older-tie", Latest: model.Message{RowID: 1, InternalDate: at}, Count: 1},
		{ThreadID: "changed", Latest: model.Message{RowID: 2, InternalDate: at}, Count: 3},
	}

	got := mergeThreadSummaries(existing, added)
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3", len(got))
	}
	ids := []string{got[0].ThreadID, got[1].ThreadID, got[2].ThreadID}
	if !reflect.DeepEqual(ids, []string{"new", "changed", "older-tie"}) {
		t.Fatalf("ids = %v", ids)
	}
	if got[1].Count != 3 {
		t.Fatalf("updated duplicate count = %d, want 3", got[1].Count)
	}
}

func TestAppendUniqueStringsPreservesFirstSeenOrder(t *testing.T) {
	got := appendUniqueStrings([]string{"m1", "m2"}, []string{"m2", "", "m3"})
	if !reflect.DeepEqual(got, []string{"m1", "m2", "m3"}) {
		t.Fatalf("ids = %v", got)
	}
}

func TestMergeSearchThreadSummariesOrder(t *testing.T) {
	existing := []model.ThreadSummary{
		{ThreadID: "relevant", Latest: model.Message{InternalDate: time.Unix(100, 0)}},
	}
	added := []model.ThreadSummary{
		{ThreadID: "newer", Latest: model.Message{InternalDate: time.Unix(200, 0)}},
	}
	tests := []struct {
		name   string
		newest bool
		want   []string
	}{
		{name: "relevance", want: []string{"relevant", "newer"}},
		{name: "newest", newest: true, want: []string{"newer", "relevant"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := mergeSearchThreadSummaries(existing, added, tt.newest)
			ids := make([]string, len(got))
			for i := range got {
				ids[i] = got[i].ThreadID
			}
			if !reflect.DeepEqual(ids, tt.want) {
				t.Fatalf("ids = %v, want %v", ids, tt.want)
			}
		})
	}
}

// A refresh that arrives while the identical query is in flight must be
// remembered, not dropped: the in-flight query was issued before the change
// that triggered the refresh (an archive's optimistic label change), so its
// rows are stale.
func TestCoalesceThreadLoadDefersRefreshInsteadOfDroppingIt(t *testing.T) {
	key := threadPageKey{mode: threadPageLabel, accountID: 1, label: "INBOX"}
	w := &window{threadPage: threadPageState{loading: true}, pageRequest: key}

	if !w.coalesceThreadLoad(key) {
		t.Fatal("identical in-flight query was not coalesced")
	}
	if !w.pageReloadPending {
		t.Fatal("refresh was dropped; want a re-run pending once the query lands")
	}
}

func TestCoalesceThreadLoadOnlyMatchesTheSameInFlightQuery(t *testing.T) {
	key := threadPageKey{mode: threadPageLabel, accountID: 1, label: "INBOX"}
	cases := []struct {
		name string
		w    *window
	}{
		{"nothing in flight", &window{pageRequest: key}},
		{"another label in flight", &window{
			threadPage:  threadPageState{loading: true},
			pageRequest: threadPageKey{mode: threadPageLabel, accountID: 1, label: "SENT"},
		}},
		{"another account in flight", &window{
			threadPage:  threadPageState{loading: true},
			pageRequest: threadPageKey{mode: threadPageLabel, accountID: 2, label: "INBOX"},
		}},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			if tt.w.coalesceThreadLoad(key) {
				t.Fatal("coalesced into a query that isn't this one")
			}
			if tt.w.pageReloadPending {
				t.Fatal("pending re-run set for a load that starts normally")
			}
		})
	}
}
