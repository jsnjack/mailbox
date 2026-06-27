// Command seed-demo populates a synthetic, PII-free SQLite mailbox database
// for screenshotting: a fake account, system labels, and a handful of threads
// with messages and one HTML body. It writes to the path given as the sole arg.
//
// Usage: go run ./cmd/seed-demo /path/to/mailbox.db
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/jsnjack/mailbox/internal/model"
	"github.com/jsnjack/mailbox/internal/store"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: seed-demo <db-path>")
		os.Exit(2)
	}
	ctx := context.Background()
	st, err := store.Open(os.Args[1])
	if err != nil {
		fmt.Fprintf(os.Stderr, "open: %v\n", err)
		os.Exit(1)
	}
	defer func() { _ = st.Close() }()

	accID, err := st.UpsertAccount(ctx, model.Account{
		Email:         "ava.mercer@acme.example",
		DisplayName:   "Ava Mercer",
		Scopes:        []string{"https://mail.google.com/"},
		LastHistoryID: "1001",
		BackfilledAt:  time.Now(),
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "account: %v\n", err)
		os.Exit(1)
	}

	systemLabels := []struct{ id, name string }{
		{model.LabelInbox, "Inbox"},
		{model.LabelStarred, "Starred"},
		{model.LabelSent, "Sent"},
		{model.LabelDraft, "Drafts"},
		{model.LabelTrash, "Trash"},
		{model.LabelSpam, "Spam"},
		{"IMPORTANT", "Important"},
	}
	for _, l := range systemLabels {
		if err := st.UpsertLabel(ctx, model.Label{AccountID: accID, GmailID: l.id, Name: l.name, Type: model.LabelSystem}); err != nil {
			fmt.Fprintf(os.Stderr, "label %s: %v\n", l.id, err)
			os.Exit(1)
		}
	}
	if err := st.UpsertLabel(ctx, model.Label{AccountID: accID, GmailID: "Label_1", Name: "Travel", Type: model.LabelUser, ColorBg: "#1e63ec"}); err != nil {
		fmt.Fprintf(os.Stderr, "user label: %v\n", err)
		os.Exit(1)
	}

	now := time.Now().UTC()
	msgs := []model.Message{
		{AccountID: accID, GmailID: "m1", ThreadID: "t1",
			InternalDate: now.Add(-2 * time.Hour), FromName: "Lena Hoff", FromAddr: "lena.hoff@acme.example",
			ToAddrs: "Ava Mercer <ava.mercer@acme.example>", Subject: "Q3 roadmap review — agenda attached",
			Snippet:  "Hi Ava, here's the proposed agenda for next week's roadmap review. I'd like to spend most of the time on the platform migration…",
			IsUnread: true, Labels: []string{model.LabelInbox, "IMPORTANT"}},
		{AccountID: accID, GmailID: "m2", ThreadID: "t2",
			InternalDate: now.Add(-5 * time.Hour), FromName: "Stripe", FromAddr: "receipts@stripe.example",
			ToAddrs: "ava.mercer@acme.example", Subject: "Receipt for your subscription — $24.00",
			Snippet:        "Thanks for your business. This is a receipt for your monthly subscription charged today.",
			HasAttachments: true, Labels: []string{model.LabelInbox}},
		{AccountID: accID, GmailID: "m3", ThreadID: "t3",
			InternalDate: now.Add(-9 * time.Hour), FromName: "Hugo Lindqvist", FromAddr: "hugo@nordictravel.example",
			ToAddrs: "ava.mercer@acme.example", Subject: "Itinerary: Stockholm → Oslo, July 18",
			Snippet:  "Your booking is confirmed. Flight SK863 departs Stockholm Arlanda at 08:55 and arrives in Oslo at 09:55…",
			IsUnread: true, IsStarred: true, Labels: []string{model.LabelInbox, "Label_1"}},
		{AccountID: accID, GmailID: "m4", ThreadID: "t4",
			InternalDate: now.Add(-26 * time.Hour), FromName: "DevOps Alerts", FromAddr: "no-reply@ci.acme.example",
			ToAddrs: "ava.mercer@acme.example", Subject: "✅ Build #4821 passed on main",
			Snippet: "Pipeline succeeded in 4m 12s. All 318 tests passed, coverage is at 84%.",
			Labels:  []string{model.LabelInbox}},
		{AccountID: accID, GmailID: "m5", ThreadID: "t5",
			InternalDate: now.Add(-2*24*time.Hour - 3*time.Hour), FromName: "Mara Osei", FromAddr: "mara.osei@acme.example",
			ToAddrs: "ava.mercer@acme.example", CcAddrs: "design@acme.example", Subject: "Re: Onboarding flow redesign",
			Snippet: "Nice work on the latest mockups, Ava. I've left a few comments inline — mostly about the spacing on the welcome screen.",
			Labels:  []string{model.LabelInbox}},
		{AccountID: accID, GmailID: "m6", ThreadID: "t6",
			InternalDate: now.Add(-3*24*time.Hour - 6*time.Hour), FromName: "Library Newsletter", FromAddr: "news@citylib.example",
			ToAddrs: "ava.mercer@acme.example", Subject: "New arrivals this week at your local branch",
			Snippet: "Browse the new fiction, children's books, and audiobooks that just arrived. Reserve online and pick up at the desk.",
			Labels:  []string{model.LabelInbox}},
		{AccountID: accID, GmailID: "m7", ThreadID: "t1",
			InternalDate: now.Add(-1 * time.Hour), FromName: "Ava Mercer", FromAddr: "ava.mercer@acme.example",
			ToAddrs: "lena.hoff@acme.example", Subject: "Re: Q3 roadmap review — agenda attached",
			Snippet: "Thanks Lena, this looks good. Can we push the platform migration discussion to a separate session? I'd rather we…",
			Labels:  []string{model.LabelInbox, model.LabelSent}},
	}

	rowids := make(map[string]int64, len(msgs))
	for _, m := range msgs {
		rid, err := st.UpsertMessage(ctx, m)
		if err != nil {
			fmt.Fprintf(os.Stderr, "message %s: %v\n", m.GmailID, err)
			os.Exit(1)
		}
		rowids[m.GmailID] = rid
	}

	// Body for the open message so the reader has realistic content to render.
	body := model.MessageBody{
		MessageRowID: rowids["m7"],
		Text: "Thanks Lena, this looks good.\n\nCan we push the platform migration " +
			"discussion to a separate session? I'd rather we keep this one focused on " +
			"the API work and the two hires we still need to close for Q3.\n\nI'll " +
			"send out a shared doc ahead of time so people can add questions.\n\n— Ava",
		HTML: `<div style="font-family:sans-serif;font-size:14px;line-height:1.6;color:#1e1e1e">` +
			`<p>Thanks Lena, this looks good.</p>` +
			`<p>Can we push the platform migration discussion to a separate session? ` +
			`I'd rather we keep this one focused on the API work and the two hires we ` +
			`still need to close for Q3.</p>` +
			`<p>I'll send out a shared doc ahead of time so people can add questions.</p>` +
			`<p>— Ava</p></div>`,
		RawHeaders: "Authentication-Results: mx.acme.example;\n  spf=pass smtp.mailfrom=lena.hoff@acme.example;\n  dkim=pass header.d=acme.example;\n  dmarc=pass",
	}
	if err := st.UpsertBody(ctx, body); err != nil {
		fmt.Fprintf(os.Stderr, "body: %v\n", err)
		os.Exit(1)
	}
	if err := st.SetBackfilledAt(ctx, accID, time.Now()); err != nil {
		fmt.Fprintf(os.Stderr, "backfill mark: %v\n", err)
		os.Exit(1)
	}

	n, _ := st.Count(ctx)
	fmt.Printf("seeded %d messages into %s\n", n, os.Args[1])
}
