// Package gmailbackend implements backend.Backend over the Gmail REST client. It
// owns the Gmail↔domain conversions and the history-walk that turns Gmail's
// history records into a flat upsert/delete id set, keeping the sync engine free
// of any Gmail-specific concept.
package gmailbackend

import (
	"context"
	"fmt"

	"github.com/jsnjack/mailbox/internal/backend"
	"github.com/jsnjack/mailbox/internal/gmailapi"
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
	p, err := b.c.GetProfile(ctx)
	if err != nil {
		return backend.Profile{}, err
	}
	return backend.Profile{Email: p.EmailAddress, Cursor: fmt.Sprintf("%d", p.HistoryId)}, nil
}

// Labels lists the account's Gmail labels as domain labels.
func (b *Backend) Labels(ctx context.Context) ([]model.Label, error) {
	ls, err := b.c.ListLabels(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]model.Label, len(ls))
	for i, l := range ls {
		out[i] = gmailapi.ToLabel(b.accountID, l)
	}
	return out, nil
}

// SearchIDs runs a Gmail server-side search (query is Gmail's q= syntax).
func (b *Backend) SearchIDs(ctx context.Context, query string, max int) ([]string, error) {
	return b.c.ListMessageIDs(ctx, query, max)
}

// FetchMetadata fetches a message's headers/flags and converts to the domain model.
func (b *Backend) FetchMetadata(ctx context.Context, id string) (model.Message, error) {
	m, err := b.c.GetMessageMetadata(ctx, id)
	if err != nil {
		return model.Message{}, err
	}
	return gmailapi.ToMessage(b.accountID, m), nil
}

// FetchBody fetches the full message and extracts body + attachment metadata.
func (b *Backend) FetchBody(ctx context.Context, id string) (model.MessageBody, []model.Attachment, error) {
	full, err := b.c.GetMessageFull(ctx, id)
	if err != nil {
		return model.MessageBody{}, nil, err
	}
	body := gmailapi.ToBody(full)
	// Gmail serves a large part body (e.g. a 1 MB HTML newsletter) via an
	// attachment id rather than inline; fetch it so the message renders as HTML
	// instead of falling back to its text/plain alternative.
	if textAtt, htmlAtt := gmailapi.ExternalBodyParts(full); textAtt != "" || htmlAtt != "" {
		if body.HTML == "" && htmlAtt != "" {
			if data, e := b.c.GetAttachment(ctx, id, htmlAtt); e == nil {
				body.HTML = string(data)
			}
		}
		if body.Text == "" && textAtt != "" {
			if data, e := b.c.GetAttachment(ctx, id, textAtt); e == nil {
				body.Text = string(data)
			}
		}
	}
	return body, gmailapi.AttachmentsFromMessage(full), nil
}

// FetchAttachment downloads one attachment's decoded bytes.
func (b *Backend) FetchAttachment(ctx context.Context, msgID, attID string) ([]byte, error) {
	return b.c.GetAttachment(ctx, msgID, attID)
}

// ApplyLabels mirrors a label delta to Gmail with one batchModify.
func (b *Backend) ApplyLabels(ctx context.Context, ids []string, add, remove []string) error {
	return b.c.BatchModify(ctx, ids, add, remove)
}

// Delete permanently removes messages with batchDelete.
func (b *Backend) Delete(ctx context.Context, ids []string) error {
	return b.c.BatchDelete(ctx, ids)
}

// Changes walks Gmail history since cursor and returns the ids to re-fetch and to
// delete, plus the newest historyId. It maps a too-old cursor to ErrCursorExpired.
func (b *Backend) Changes(ctx context.Context, cursor string) (upserts, deletes []string, next string, err error) {
	records, newest, err := b.c.ListHistory(ctx, cursor)
	if err != nil {
		if gmailapi.IsHistoryExpired(err) {
			return nil, nil, "", backend.ErrCursorExpired
		}
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
	return upserts, deletes, newest, nil
}

// Send transmits raw via Gmail and returns the new message id.
func (b *Backend) Send(ctx context.Context, raw []byte, threadID string) (string, error) {
	return b.c.Send(ctx, raw, threadID)
}

// SaveDraft creates a new Gmail draft.
func (b *Backend) SaveDraft(ctx context.Context, raw []byte, threadID string) (string, error) {
	return b.c.SaveDraft(ctx, raw, threadID)
}

// UpdateDraft replaces an existing Gmail draft.
func (b *Backend) UpdateDraft(ctx context.Context, draftID string, raw []byte, threadID string) (string, error) {
	return b.c.UpdateDraft(ctx, draftID, raw, threadID)
}

// DeleteDraft removes a Gmail draft.
func (b *Backend) DeleteDraft(ctx context.Context, draftID string) error {
	return b.c.DeleteDraft(ctx, draftID)
}

// FindDraftID resolves the Gmail draft resource id backing a message.
func (b *Backend) FindDraftID(ctx context.Context, msgID string) (string, error) {
	return b.c.FindDraftID(ctx, msgID)
}
