package store

import (
	"context"
	"testing"

	"github.com/jsnjack/mailbox/internal/model"
)

func TestModifyLabelsBatchAndEnqueue(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	acct := seedAccount(t, s)
	if err := s.UpsertMessages(ctx, []model.Message{{
		AccountID: acct, GmailID: "m1", ThreadID: "t1",
		Labels: []string{model.LabelInbox, model.LabelUnread}, IsUnread: true,
	}}); err != nil {
		t.Fatal(err)
	}
	if err := s.ModifyLabelsBatchAndEnqueue(ctx, acct, []string{"m1"}, []string{model.LabelStarred}, []string{model.LabelInbox}); err != nil {
		t.Fatal(err)
	}
	m, err := s.GetMessage(ctx, acct, "m1")
	if err != nil {
		t.Fatal(err)
	}
	if !m.IsStarred || hasString(m.Labels, model.LabelInbox) {
		t.Fatalf("local labels = %v starred=%v", m.Labels, m.IsStarred)
	}
	ops, err := s.PendingLabelOps(ctx, acct, 10)
	if err != nil || len(ops) != 1 {
		t.Fatalf("pending ops = %+v, %v", ops, err)
	}
	if got := ops[0]; len(got.MessageIDs) != 1 || got.MessageIDs[0] != "m1" || len(got.Add) != 1 || got.Add[0] != model.LabelStarred {
		t.Fatalf("pending op = %+v", got)
	}
	if err := s.FailPendingLabelOp(ctx, ops[0].ID, context.DeadlineExceeded); err != nil {
		t.Fatal(err)
	}
	ops, _ = s.PendingLabelOps(ctx, acct, 10)
	if ops[0].Attempts != 1 || ops[0].LastError == "" {
		t.Fatalf("failed op = %+v", ops[0])
	}
	if err := s.CompletePendingLabelOp(ctx, ops[0].ID); err != nil {
		t.Fatal(err)
	}
	if ops, err = s.PendingLabelOps(ctx, acct, 10); err != nil || len(ops) != 0 {
		t.Fatalf("completed ops = %+v, %v", ops, err)
	}
}

func hasString(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
