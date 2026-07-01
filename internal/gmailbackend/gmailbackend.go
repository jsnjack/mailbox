// Package gmailbackend implements backend.Backend over the Gmail REST client. It
// owns the Gmail↔domain conversions and the history-walk that turns Gmail's
// history records into a flat upsert/delete id set, keeping the sync engine free
// of any Gmail-specific concept.
package gmailbackend

import (
	"context"
	"fmt"
	"time"

	"github.com/jsnjack/mailbox/internal/backend"
	"github.com/jsnjack/mailbox/internal/gmailapi"
	"github.com/jsnjack/mailbox/internal/logging"
	"github.com/jsnjack/mailbox/internal/model"
)

// Backend adapts a per-account *gmailapi.Client to backend.Backend.
type Backend struct {
	c         *gmailapi.Client
	accountID int64
}

// New wraps a Gmail client as a backend for the given account.
func New(c *gmailapi.Client, accountID int64) *Backend {
	return &Backend{c: c, accountID: accountID}
}

var _ backend.Backend = (*Backend)(nil)

// Profile returns the account email and the current historyId as the cursor.
func (b *Backend) Profile(ctx context.Context) (backend.Profile, error) {
	start := time.Now()
	logging.TraceContext(ctx, "gmailbackend: Profile", "account", b.accountID)
	p, err := b.c.GetProfile(ctx)
	if err != nil {
		logging.TraceContext(ctx, "gmailbackend: Profile failed", "account", b.accountID, "dur", time.Since(start), "err", err)
		return backend.Profile{}, err
	}
	prof := backend.Profile{Email: p.EmailAddress, Cursor: fmt.Sprintf("%d", p.HistoryId)}
	logging.TraceContext(ctx, "gmailbackend: Profile ok", "account", b.accountID, "email", prof.Email, "historyId", prof.Cursor, "dur", time.Since(start))
	return prof, nil
}

// Labels lists the account's Gmail labels as domain labels.
func (b *Backend) Labels(ctx context.Context) ([]model.Label, error) {
	start := time.Now()
	logging.TraceContext(ctx, "gmailbackend: Labels", "account", b.accountID)
	ls, err := b.c.ListLabels(ctx)
	if err != nil {
		logging.TraceContext(ctx, "gmailbackend: Labels failed", "account", b.accountID, "dur", time.Since(start), "err", err)
		return nil, err
	}
	out := make([]model.Label, len(ls))
	for i, l := range ls {
		out[i] = gmailapi.ToLabel(b.accountID, l)
	}
	logging.TraceContext(ctx, "gmailbackend: Labels ok", "account", b.accountID, "count", len(out), "dur", time.Since(start))
	return out, nil
}

// SearchIDs runs a Gmail server-side search (query is Gmail's q= syntax).
func (b *Backend) SearchIDs(ctx context.Context, query string, max int) ([]string, error) {
	start := time.Now()
	logging.TraceContext(ctx, "gmailbackend: SearchIDs", "account", b.accountID, "query", query, "max", max)
	ids, err := b.c.ListMessageIDs(ctx, query, max)
	if err != nil {
		logging.TraceContext(ctx, "gmailbackend: SearchIDs failed", "account", b.accountID, "query", query, "dur", time.Since(start), "err", err)
		return nil, err
	}
	logging.TraceContext(ctx, "gmailbackend: SearchIDs ok", "account", b.accountID, "query", query, "count", len(ids), "dur", time.Since(start))
	return ids, nil
}

// FetchMetadata fetches a message's headers/flags and converts to the domain model.
func (b *Backend) FetchMetadata(ctx context.Context, id string) (model.Message, error) {
	start := time.Now()
	logging.TraceContext(ctx, "gmailbackend: FetchMetadata", "account", b.accountID, "id", id)
	m, err := b.c.GetMessageMetadata(ctx, id)
	if err != nil {
		logging.TraceContext(ctx, "gmailbackend: FetchMetadata failed", "account", b.accountID, "id", id, "dur", time.Since(start), "err", err)
		return model.Message{}, err
	}
	msg := gmailapi.ToMessage(b.accountID, m)
	logging.TraceContext(ctx, "gmailbackend: FetchMetadata ok", "account", b.accountID, "id", id, "threadID", msg.ThreadID, "from", msg.FromAddr, "subject", msg.Subject, "dur", time.Since(start))
	return msg, nil
}

// FetchBody fetches the full message and extracts body + attachment metadata.
func (b *Backend) FetchBody(ctx context.Context, id string) (model.MessageBody, []model.Attachment, error) {
	start := time.Now()
	logging.TraceContext(ctx, "gmailbackend: FetchBody", "account", b.accountID, "id", id)
	full, err := b.c.GetMessageFull(ctx, id)
	if err != nil {
		logging.TraceContext(ctx, "gmailbackend: FetchBody failed", "account", b.accountID, "id", id, "dur", time.Since(start), "err", err)
		return model.MessageBody{}, nil, err
	}
	body := gmailapi.ToBody(full)
	// Gmail serves a large part body (e.g. a 1 MB HTML newsletter) via an
	// attachment id rather than inline; fetch it so the message renders as HTML
	// instead of falling back to its text/plain alternative.
	if textAtt, htmlAtt := gmailapi.ExternalBodyParts(full); textAtt != "" || htmlAtt != "" {
		logging.TraceContext(ctx, "gmailbackend: FetchBody externalized parts", "account", b.accountID, "id", id, "hasTextAtt", textAtt != "", "hasHTMLAtt", htmlAtt != "", "haveHTML", body.HTML != "", "haveText", body.Text != "")
		if body.HTML == "" && htmlAtt != "" {
			if data, e := b.c.GetAttachment(ctx, id, htmlAtt); e == nil {
				body.HTML = string(data)
				logging.TraceContext(ctx, "gmailbackend: FetchBody fetched external HTML", "account", b.accountID, "id", id, "bytes", len(data))
			} else {
				logging.TraceContext(ctx, "gmailbackend: FetchBody external HTML failed", "account", b.accountID, "id", id, "err", e)
			}
		}
		if body.Text == "" && textAtt != "" {
			if data, e := b.c.GetAttachment(ctx, id, textAtt); e == nil {
				body.Text = string(data)
				logging.TraceContext(ctx, "gmailbackend: FetchBody fetched external text", "account", b.accountID, "id", id, "bytes", len(data))
			} else {
				logging.TraceContext(ctx, "gmailbackend: FetchBody external text failed", "account", b.accountID, "id", id, "err", e)
			}
		}
	}
	atts := gmailapi.AttachmentsFromMessage(full)
	logging.TraceContext(ctx, "gmailbackend: FetchBody ok", "account", b.accountID, "id", id, "htmlBytes", len(body.HTML), "textBytes", len(body.Text), "attachments", len(atts), "dur", time.Since(start))
	return body, atts, nil
}

// FetchAttachment downloads one attachment's decoded bytes.
func (b *Backend) FetchAttachment(ctx context.Context, msgID, attID string) ([]byte, error) {
	start := time.Now()
	logging.TraceContext(ctx, "gmailbackend: FetchAttachment", "account", b.accountID, "id", msgID, "attID", attID)
	data, err := b.c.GetAttachment(ctx, msgID, attID)
	if err != nil {
		logging.TraceContext(ctx, "gmailbackend: FetchAttachment failed", "account", b.accountID, "id", msgID, "attID", attID, "dur", time.Since(start), "err", err)
		return nil, err
	}
	logging.TraceContext(ctx, "gmailbackend: FetchAttachment ok", "account", b.accountID, "id", msgID, "attID", attID, "bytes", len(data), "dur", time.Since(start))
	return data, nil
}

// ApplyLabels mirrors a label delta to Gmail with one batchModify.
func (b *Backend) ApplyLabels(ctx context.Context, ids []string, add, remove []string) error {
	start := time.Now()
	logging.TraceContext(ctx, "gmailbackend: ApplyLabels", "account", b.accountID, "n", len(ids), "add", add, "remove", remove)
	err := b.c.BatchModify(ctx, ids, add, remove)
	if err != nil {
		logging.TraceContext(ctx, "gmailbackend: ApplyLabels failed", "account", b.accountID, "n", len(ids), "dur", time.Since(start), "err", err)
		return err
	}
	logging.TraceContext(ctx, "gmailbackend: ApplyLabels ok", "account", b.accountID, "n", len(ids), "dur", time.Since(start))
	return nil
}

// Delete permanently removes messages with batchDelete.
func (b *Backend) Delete(ctx context.Context, ids []string) error {
	start := time.Now()
	logging.TraceContext(ctx, "gmailbackend: Delete", "account", b.accountID, "n", len(ids))
	err := b.c.BatchDelete(ctx, ids)
	if err != nil {
		logging.TraceContext(ctx, "gmailbackend: Delete failed", "account", b.accountID, "n", len(ids), "dur", time.Since(start), "err", err)
		return err
	}
	logging.TraceContext(ctx, "gmailbackend: Delete ok", "account", b.accountID, "n", len(ids), "dur", time.Since(start))
	return nil
}

// Changes walks Gmail history since cursor and returns the ids to re-fetch and to
// delete, plus the newest historyId. It maps a too-old cursor to ErrCursorExpired.
func (b *Backend) Changes(ctx context.Context, cursor string) (upserts, deletes []string, next string, err error) {
	start := time.Now()
	logging.TraceContext(ctx, "gmailbackend: Changes", "account", b.accountID, "cursor", cursor)
	records, newest, err := b.c.ListHistory(ctx, cursor)
	if err != nil {
		if gmailapi.IsHistoryExpired(err) {
			logging.TraceContext(ctx, "gmailbackend: Changes history expired", "account", b.accountID, "cursor", cursor, "dur", time.Since(start), "err", err)
			return nil, nil, "", backend.ErrCursorExpired
		}
		logging.TraceContext(ctx, "gmailbackend: Changes failed", "account", b.accountID, "cursor", cursor, "dur", time.Since(start), "err", err)
		return nil, nil, "", err
	}
	refetch := make(map[string]bool)
	del := make(map[string]bool)
	for _, r := range records {
		for _, x := range r.MessagesAdded {
			if x.Message != nil {
				refetch[x.Message.Id] = true
			}
		}
		for _, x := range r.LabelsAdded {
			if x.Message != nil {
				refetch[x.Message.Id] = true
			}
		}
		for _, x := range r.LabelsRemoved {
			if x.Message != nil {
				refetch[x.Message.Id] = true
			}
		}
		for _, x := range r.MessagesDeleted {
			if x.Message != nil {
				del[x.Message.Id] = true
				delete(refetch, x.Message.Id)
			}
		}
	}
	upserts = make([]string, 0, len(refetch))
	for id := range refetch {
		upserts = append(upserts, id)
	}
	deletes = make([]string, 0, len(del))
	for id := range del {
		deletes = append(deletes, id)
	}
	logging.TraceContext(ctx, "gmailbackend: Changes ok",
		"account", b.accountID, "cursor", cursor, "next", newest,
		"records", len(records), "upserts", len(upserts), "deletes", len(deletes), "dur", time.Since(start))
	return upserts, deletes, newest, nil
}

// Send transmits raw via Gmail and returns the new message id.
func (b *Backend) Send(ctx context.Context, raw []byte, threadID string) (string, error) {
	start := time.Now()
	logging.TraceContext(ctx, "gmailbackend: Send", "account", b.accountID, "bytes", len(raw), "threadID", threadID)
	id, err := b.c.Send(ctx, raw, threadID)
	if err != nil {
		logging.TraceContext(ctx, "gmailbackend: Send failed", "account", b.accountID, "threadID", threadID, "dur", time.Since(start), "err", err)
		return "", err
	}
	logging.TraceContext(ctx, "gmailbackend: Send ok", "account", b.accountID, "id", id, "threadID", threadID, "dur", time.Since(start))
	return id, nil
}

// SaveDraft creates a new Gmail draft.
func (b *Backend) SaveDraft(ctx context.Context, raw []byte, threadID string) (string, error) {
	start := time.Now()
	logging.TraceContext(ctx, "gmailbackend: SaveDraft", "account", b.accountID, "bytes", len(raw), "threadID", threadID)
	id, err := b.c.SaveDraft(ctx, raw, threadID)
	if err != nil {
		logging.TraceContext(ctx, "gmailbackend: SaveDraft failed", "account", b.accountID, "threadID", threadID, "dur", time.Since(start), "err", err)
		return "", err
	}
	logging.TraceContext(ctx, "gmailbackend: SaveDraft ok", "account", b.accountID, "draftID", id, "threadID", threadID, "dur", time.Since(start))
	return id, nil
}

// UpdateDraft replaces an existing Gmail draft.
func (b *Backend) UpdateDraft(ctx context.Context, draftID string, raw []byte, threadID string) (string, error) {
	start := time.Now()
	logging.TraceContext(ctx, "gmailbackend: UpdateDraft", "account", b.accountID, "draftID", draftID, "bytes", len(raw), "threadID", threadID)
	id, err := b.c.UpdateDraft(ctx, draftID, raw, threadID)
	if err != nil {
		logging.TraceContext(ctx, "gmailbackend: UpdateDraft failed", "account", b.accountID, "draftID", draftID, "dur", time.Since(start), "err", err)
		return "", err
	}
	logging.TraceContext(ctx, "gmailbackend: UpdateDraft ok", "account", b.accountID, "draftID", id, "threadID", threadID, "dur", time.Since(start))
	return id, nil
}

// DeleteDraft removes a Gmail draft.
func (b *Backend) DeleteDraft(ctx context.Context, draftID string) error {
	start := time.Now()
	logging.TraceContext(ctx, "gmailbackend: DeleteDraft", "account", b.accountID, "draftID", draftID)
	err := b.c.DeleteDraft(ctx, draftID)
	if err != nil {
		logging.TraceContext(ctx, "gmailbackend: DeleteDraft failed", "account", b.accountID, "draftID", draftID, "dur", time.Since(start), "err", err)
		return err
	}
	logging.TraceContext(ctx, "gmailbackend: DeleteDraft ok", "account", b.accountID, "draftID", draftID, "dur", time.Since(start))
	return nil
}

// FindDraftID resolves the Gmail draft resource id backing a message.
func (b *Backend) FindDraftID(ctx context.Context, msgID string) (string, error) {
	start := time.Now()
	logging.TraceContext(ctx, "gmailbackend: FindDraftID", "account", b.accountID, "id", msgID)
	id, err := b.c.FindDraftID(ctx, msgID)
	if err != nil {
		logging.TraceContext(ctx, "gmailbackend: FindDraftID failed", "account", b.accountID, "id", msgID, "dur", time.Since(start), "err", err)
		return "", err
	}
	logging.TraceContext(ctx, "gmailbackend: FindDraftID ok", "account", b.accountID, "id", msgID, "draftID", id, "dur", time.Since(start))
	return id, nil
}
