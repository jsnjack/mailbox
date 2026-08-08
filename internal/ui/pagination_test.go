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
