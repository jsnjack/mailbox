// Package gmailapi wraps the Gmail REST client with the throttling and
// resilience the sync engine needs: a per-mailbox concurrency cap (well under
// Gmail's ~50 in-flight ceiling), a proactive quota-unit budget, and
// exponential backoff on rate-limit and server errors. It imports no GTK code.
package gmailapi

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/jsnjack/mailbox/internal/logging"
	gmail "google.golang.org/api/gmail/v1"
	"google.golang.org/api/googleapi"
)

const (
	// maxConcurrent stays under Gmail's ~50 concurrent-requests-per-mailbox ceiling.
	maxConcurrent = 30
	maxRetries    = 5
	backoffBase   = 500 * time.Millisecond
	backoffCap    = 30 * time.Second
	// maxRetryAfter caps how long a server Retry-After hint can delay a retry, so
	// an unreasonable value can't hang an operation.
	maxRetryAfter = 60 * time.Second
)

// Client is a throttled, retrying Gmail client for one account.
type Client struct {
	srv    *gmail.Service
	sem    chan struct{}
	budget *RateBudget
	stats  *Stats
}

// NewClient wraps a Gmail service for one account.
func NewClient(srv *gmail.Service) *Client { return NewClientStats(srv, nil) }

// NewClientStats wraps a Gmail service, recording API usage into stats (shared
// with the service's byte-counting transport from NewService). A nil stats gets
// a private one.
func NewClientStats(srv *gmail.Service, stats *Stats) *Client {
	if stats == nil {
		stats = &Stats{}
	}
	return &Client{
		srv:    srv,
		sem:    make(chan struct{}, maxConcurrent),
		budget: NewRateBudget(),
		stats:  stats,
	}
}

// do runs an idempotent call (reads, label changes, deletes): it is retried on
// transient network failures as well as rate-limit/5xx responses.
func (c *Client) do(ctx context.Context, cost int, fn func() error) error {
	return c.doRetry(ctx, cost, isRetryable, fn)
}

// doSend runs a non-idempotent call (sending mail). It retries only on explicit
// rate-limit/5xx RESPONSES — never on a bare network error, which may mean the
// message was delivered but the response was lost, so retrying would duplicate
// it. Transient send failures are recovered at a higher level (the outbox).
func (c *Client) doSend(ctx context.Context, cost int, fn func() error) error {
	return c.doRetry(ctx, cost, isRetryableResponse, fn)
}

// doRetry reserves quota, acquires a concurrency slot, and runs fn with bounded
// exponential backoff, retrying while retryable(err) holds. Each attempt is a
// real HTTP request, so quota is reserved and the request counted per attempt
// (the first reservation happens before the slot is taken, so a budget wait
// doesn't tie one up); on a retry the server's Retry-After hint is honored when
// it exceeds the computed backoff.
func (c *Client) doRetry(ctx context.Context, cost int, retryable func(error) bool, fn func() error) error {
	if err := c.budget.Reserve(ctx, cost); err != nil {
		logging.TraceContext(ctx, "gmailapi: budget reserve failed", "quota", cost, "err", err)
		return err
	}
	select {
	case c.sem <- struct{}{}:
	case <-ctx.Done():
		logging.TraceContext(ctx, "gmailapi: semaphore wait cancelled", "err", ctx.Err())
		return ctx.Err()
	}
	defer func() { <-c.sem }()

	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			d := backoffDuration(attempt)
			ra := retryAfter(lastErr)
			honored := ra > d
			if honored {
				d = ra
			}
			logging.TraceContext(ctx, "gmailapi: retry backoff", "attempt", attempt, "dur", d, "retry_after", ra, "retry_after_honored", honored, "reason", lastErr)
			if err := sleepCtx(ctx, d); err != nil {
				logging.TraceContext(ctx, "gmailapi: retry sleep cancelled", "attempt", attempt, "err", err)
				return err
			}
			if err := c.budget.Reserve(ctx, cost); err != nil { // a retry is another request
				logging.TraceContext(ctx, "gmailapi: budget reserve failed on retry", "attempt", attempt, "quota", cost, "err", err)
				return err
			}
		}
		c.stats.requests.Add(1)
		c.stats.quotaUnits.Add(int64(cost))
		before := c.stats.Snapshot()
		start := time.Now()
		err := fn()
		dur := time.Since(start)
		after := c.stats.Snapshot()
		bytesIn := after.BytesIn - before.BytesIn
		bytesOut := after.BytesOut - before.BytesOut
		if err != nil {
			retry := retryable(err)
			logging.TraceContext(ctx, "gmailapi: request failed", "attempt", attempt, "dur", dur, "quota", cost, "bytes_in", bytesIn, "bytes_out", bytesOut, "retryable", retry, "err", err)
			if !retry {
				return err
			}
			lastErr = err
			continue
		}
		logging.TraceContext(ctx, "gmailapi: request ok", "attempt", attempt, "dur", dur, "quota", cost, "bytes_in", bytesIn, "bytes_out", bytesOut)
		return nil
	}
	logging.TraceContext(ctx, "gmailapi: request exhausted retries", "attempts", maxRetries+1, "quota", cost, "err", lastErr)
	return fmt.Errorf("gmail call failed after %d attempts: %w", maxRetries+1, lastErr)
}

// GetProfile returns the account profile (email, message count, current historyId).
func (c *Client) GetProfile(ctx context.Context) (*gmail.Profile, error) {
	logging.TraceContext(ctx, "gmailapi: getProfile")
	var p *gmail.Profile
	err := c.do(ctx, costMessageGet, func() error {
		r, e := c.srv.Users.GetProfile("me").Context(ctx).Do()
		p = r
		return e
	})
	if err != nil {
		return nil, fmt.Errorf("get profile: %w", err)
	}
	logging.TraceContext(ctx, "gmailapi: getProfile done", "email", p.EmailAddress, "history_id", p.HistoryId, "messages_total", p.MessagesTotal)
	return p, nil
}

// ListLabels returns all labels for the account.
func (c *Client) ListLabels(ctx context.Context) ([]*gmail.Label, error) {
	logging.TraceContext(ctx, "gmailapi: labels.list")
	var resp *gmail.ListLabelsResponse
	err := c.do(ctx, costLabelsList, func() error {
		r, e := c.srv.Users.Labels.List("me").Context(ctx).Do()
		resp = r
		return e
	})
	if err != nil {
		return nil, fmt.Errorf("list labels: %w", err)
	}
	logging.TraceContext(ctx, "gmailapi: labels.list done", "count", len(resp.Labels))
	return resp.Labels, nil
}

// ListMessageIDs lists message ids matching query (Gmail search syntax; empty for
// all), newest first, up to max (0 = no limit). Each page costs few quota units.
func (c *Client) ListMessageIDs(ctx context.Context, query string, max int) ([]string, error) {
	logging.TraceContext(ctx, "gmailapi: messages.list", "query", query, "max", max)
	var ids []string
	pageToken := ""
	page := 0
	for {
		var resp *gmail.ListMessagesResponse
		err := c.do(ctx, costMessageList, func() error {
			call := c.srv.Users.Messages.List("me").Context(ctx)
			if query != "" {
				call = call.Q(query)
			}
			if pageToken != "" {
				call = call.PageToken(pageToken)
			}
			pageSize := int64(500)
			if max > 0 {
				if remaining := int64(max - len(ids)); remaining < pageSize {
					pageSize = remaining
				}
			}
			r, e := call.MaxResults(pageSize).Do()
			resp = r
			return e
		})
		if err != nil {
			return nil, fmt.Errorf("list messages: %w", err)
		}
		for _, m := range resp.Messages {
			ids = append(ids, m.Id)
		}
		page++
		logging.TraceContext(ctx, "gmailapi: messages.list page", "query", query, "page", page, "page_count", len(resp.Messages), "total", len(ids), "more", resp.NextPageToken != "")
		if resp.NextPageToken == "" || (max > 0 && len(ids) >= max) {
			break
		}
		pageToken = resp.NextPageToken
	}
	if max > 0 && len(ids) > max {
		ids = ids[:max]
	}
	logging.TraceContext(ctx, "gmailapi: messages.list done", "query", query, "count", len(ids), "pages", page)
	return ids, nil
}

var metadataHeaders = []string{"From", "Reply-To", "To", "Cc", "Subject", "Date", "Message-ID", "In-Reply-To", "References"}

// GetMessageMetadata fetches a message in metadata format (headers + labels, no body).
func (c *Client) GetMessageMetadata(ctx context.Context, id string) (*gmail.Message, error) {
	logging.TraceContext(ctx, "gmailapi: messages.get metadata", "id", id)
	var msg *gmail.Message
	err := c.do(ctx, costMessageGet, func() error {
		r, e := c.srv.Users.Messages.Get("me", id).
			Format("metadata").MetadataHeaders(metadataHeaders...).Context(ctx).Do()
		msg = r
		return e
	})
	if err != nil {
		return nil, fmt.Errorf("get message metadata %s: %w", id, err)
	}
	logging.TraceContext(ctx, "gmailapi: messages.get metadata done", "id", id, "thread_id", msg.ThreadId, "size_estimate", msg.SizeEstimate)
	return msg, nil
}

// GetMessageFull fetches a message in full format (payload with body parts).
func (c *Client) GetMessageFull(ctx context.Context, id string) (*gmail.Message, error) {
	logging.TraceContext(ctx, "gmailapi: messages.get full", "id", id)
	var msg *gmail.Message
	err := c.do(ctx, costMessageGet, func() error {
		r, e := c.srv.Users.Messages.Get("me", id).Format("full").Context(ctx).Do()
		msg = r
		return e
	})
	if err != nil {
		return nil, fmt.Errorf("get message full %s: %w", id, err)
	}
	logging.TraceContext(ctx, "gmailapi: messages.get full done", "id", id, "thread_id", msg.ThreadId, "size_estimate", msg.SizeEstimate)
	return msg, nil
}

// Send transmits a raw RFC 5322 message. threadID (optional) files it into an
// existing Gmail conversation. It returns the new message id.
func (c *Client) Send(ctx context.Context, raw []byte, threadID string) (string, error) {
	logging.TraceContext(ctx, "gmailapi: messages.send", "bytes", len(raw), "thread_id", threadID)
	var sent *gmail.Message
	err := c.doSend(ctx, costSend, func() error {
		msg := &gmail.Message{Raw: base64.URLEncoding.EncodeToString(raw)}
		if threadID != "" {
			msg.ThreadId = threadID
		}
		r, e := c.srv.Users.Messages.Send("me", msg).Context(ctx).Do()
		sent = r
		return e
	})
	if err != nil {
		return "", fmt.Errorf("send message: %w", err)
	}
	logging.TraceContext(ctx, "gmailapi: messages.send done", "id", sent.Id, "thread_id", sent.ThreadId)
	return sent.Id, nil
}

// SaveDraft stores a raw RFC 5322 message as a Gmail draft, returning the draft id.
func (c *Client) SaveDraft(ctx context.Context, raw []byte, threadID string) (string, error) {
	logging.TraceContext(ctx, "gmailapi: drafts.create", "bytes", len(raw), "thread_id", threadID)
	var draft *gmail.Draft
	err := c.do(ctx, costMessageGet, func() error {
		msg := &gmail.Message{Raw: base64.URLEncoding.EncodeToString(raw)}
		if threadID != "" {
			msg.ThreadId = threadID
		}
		r, e := c.srv.Users.Drafts.Create("me", &gmail.Draft{Message: msg}).Context(ctx).Do()
		draft = r
		return e
	})
	if err != nil {
		return "", fmt.Errorf("create draft: %w", err)
	}
	logging.TraceContext(ctx, "gmailapi: drafts.create done", "id", draft.Id)
	return draft.Id, nil
}

// UpdateDraft replaces the contents of an existing draft, returning its id.
func (c *Client) UpdateDraft(ctx context.Context, draftID string, raw []byte, threadID string) (string, error) {
	logging.TraceContext(ctx, "gmailapi: drafts.update", "id", draftID, "bytes", len(raw), "thread_id", threadID)
	var draft *gmail.Draft
	err := c.do(ctx, costMessageGet, func() error {
		msg := &gmail.Message{Raw: base64.URLEncoding.EncodeToString(raw)}
		if threadID != "" {
			msg.ThreadId = threadID
		}
		r, e := c.srv.Users.Drafts.Update("me", draftID, &gmail.Draft{Message: msg}).Context(ctx).Do()
		draft = r
		return e
	})
	if err != nil {
		return "", fmt.Errorf("update draft: %w", err)
	}
	logging.TraceContext(ctx, "gmailapi: drafts.update done", "id", draft.Id)
	return draft.Id, nil
}

// DeleteDraft permanently removes a draft (used after its edited contents have
// been sent as a normal message).
func (c *Client) DeleteDraft(ctx context.Context, draftID string) error {
	logging.TraceContext(ctx, "gmailapi: drafts.delete", "id", draftID)
	err := c.do(ctx, costMessageGet, func() error {
		return c.srv.Users.Drafts.Delete("me", draftID).Context(ctx).Do()
	})
	if err != nil {
		return fmt.Errorf("delete draft %s: %w", draftID, err)
	}
	logging.TraceContext(ctx, "gmailapi: drafts.delete done", "id", draftID)
	return nil
}

// FindDraftID returns the draft resource id whose underlying message has the
// given message id, or "" if no draft matches. Gmail tracks drafts by a separate
// id from the message id, so editing/sending a synced draft requires this lookup.
func (c *Client) FindDraftID(ctx context.Context, messageID string) (string, error) {
	logging.TraceContext(ctx, "gmailapi: drafts.list find", "message_id", messageID)
	// Each page is its own do() call so quota/requests are charged per page (as in
	// ListMessageIDs); paginating inside a single do() would under-count a
	// multi-page drafts list as one request.
	pageToken := ""
	for page := 1; ; page++ {
		var resp *gmail.ListDraftsResponse
		err := c.do(ctx, costMessageList, func() error {
			call := c.srv.Users.Drafts.List("me").MaxResults(100).Context(ctx)
			if pageToken != "" {
				call = call.PageToken(pageToken)
			}
			r, e := call.Do()
			resp = r
			return e
		})
		if err != nil {
			return "", fmt.Errorf("find draft id: %w", err)
		}
		for _, d := range resp.Drafts {
			if d.Message != nil && d.Message.Id == messageID {
				logging.TraceContext(ctx, "gmailapi: drafts.list find done", "message_id", messageID, "draft_id", d.Id, "found", true, "pages", page)
				return d.Id, nil
			}
		}
		if resp.NextPageToken == "" {
			logging.TraceContext(ctx, "gmailapi: drafts.list find done", "message_id", messageID, "found", false, "pages", page)
			return "", nil
		}
		pageToken = resp.NextPageToken
	}
}

// GetAttachment downloads and decodes an attachment's bytes.
func (c *Client) GetAttachment(ctx context.Context, messageID, attachmentID string) ([]byte, error) {
	logging.TraceContext(ctx, "gmailapi: attachments.get", "id", messageID, "attachment_id", attachmentID)
	var body *gmail.MessagePartBody
	err := c.do(ctx, costMessageGet, func() error {
		r, e := c.srv.Users.Messages.Attachments.Get("me", messageID, attachmentID).Context(ctx).Do()
		body = r
		return e
	})
	if err != nil {
		return nil, fmt.Errorf("get attachment %s: %w", attachmentID, err)
	}
	data, err := base64.URLEncoding.DecodeString(body.Data)
	if err != nil {
		// Gmail attachment data is web-safe base64, sometimes unpadded.
		logging.TraceContext(ctx, "gmailapi: attachments.get decode fallback to raw", "attachment_id", attachmentID)
		data, err = base64.RawURLEncoding.DecodeString(body.Data)
		if err != nil {
			return nil, fmt.Errorf("decode attachment %s: %w", attachmentID, err)
		}
	}
	logging.TraceContext(ctx, "gmailapi: attachments.get done", "attachment_id", attachmentID, "bytes", len(data))
	return data, nil
}

// batchModifyMax is Gmail's per-call id limit for batchModify.
const batchModifyMax = 1000

// BatchModify applies a label change to many messages, chunked to Gmail's limit.
func (c *Client) BatchModify(ctx context.Context, ids []string, add, remove []string) error {
	logging.TraceContext(ctx, "gmailapi: messages.batchModify", "count", len(ids), "add", add, "remove", remove)
	for start := 0; start < len(ids); start += batchModifyMax {
		end := start + batchModifyMax
		if end > len(ids) {
			end = len(ids)
		}
		chunk := ids[start:end]
		logging.TraceContext(ctx, "gmailapi: messages.batchModify chunk", "count", len(chunk), "offset", start)
		err := c.do(ctx, costMessageList, func() error {
			return c.srv.Users.Messages.BatchModify("me", &gmail.BatchModifyMessagesRequest{
				Ids:            chunk,
				AddLabelIds:    add,
				RemoveLabelIds: remove,
			}).Context(ctx).Do()
		})
		if err != nil {
			return fmt.Errorf("batch modify: %w", err)
		}
	}
	return nil
}

// BatchDelete permanently deletes messages (bypassing Trash). Chunked to stay
// within Gmail's per-request id limit.
func (c *Client) BatchDelete(ctx context.Context, ids []string) error {
	logging.TraceContext(ctx, "gmailapi: messages.batchDelete", "count", len(ids))
	for start := 0; start < len(ids); start += batchModifyMax {
		end := start + batchModifyMax
		if end > len(ids) {
			end = len(ids)
		}
		chunk := ids[start:end]
		logging.TraceContext(ctx, "gmailapi: messages.batchDelete chunk", "count", len(chunk), "offset", start)
		err := c.do(ctx, costMessageList, func() error {
			return c.srv.Users.Messages.BatchDelete("me", &gmail.BatchDeleteMessagesRequest{Ids: chunk}).Context(ctx).Do()
		})
		if err != nil {
			return fmt.Errorf("batch delete: %w", err)
		}
	}
	return nil
}

// ModifyLabels adds and removes Gmail label ids on a message (e.g. remove UNREAD
// to mark read, remove INBOX to archive, add/remove STARRED).
func (c *Client) ModifyLabels(ctx context.Context, id string, add, remove []string) error {
	logging.TraceContext(ctx, "gmailapi: messages.modify", "id", id, "add", add, "remove", remove)
	return c.do(ctx, costMessageGet, func() error {
		_, e := c.srv.Users.Messages.Modify("me", id, &gmail.ModifyMessageRequest{
			AddLabelIds:    add,
			RemoveLabelIds: remove,
		}).Context(ctx).Do()
		return e
	})
}

// ListHistory returns history records since startHistoryID and the newest
// historyId observed. IsHistoryExpired reports whether startHistoryID was too old.
func (c *Client) ListHistory(ctx context.Context, startHistoryID string) ([]*gmail.History, string, error) {
	logging.TraceContext(ctx, "gmailapi: history.list", "start_history_id", startHistoryID)
	start, err := strconv.ParseUint(startHistoryID, 10, 64)
	if err != nil {
		logging.TraceContext(ctx, "gmailapi: history.list bad start id", "start_history_id", startHistoryID, "err", err)
		return nil, "", fmt.Errorf("parse historyId %q: %w", startHistoryID, err)
	}
	var records []*gmail.History
	pageToken := ""
	newest := startHistoryID
	page := 0
	for {
		var resp *gmail.ListHistoryResponse
		err := c.do(ctx, costHistoryList, func() error {
			call := c.srv.Users.History.List("me").StartHistoryId(start).Context(ctx)
			if pageToken != "" {
				call = call.PageToken(pageToken)
			}
			r, e := call.Do()
			resp = r
			return e
		})
		if err != nil {
			logging.TraceContext(ctx, "gmailapi: history.list failed", "start_history_id", startHistoryID, "expired", IsHistoryExpired(err), "err", err)
			return nil, "", fmt.Errorf("list history: %w", err)
		}
		records = append(records, resp.History...)
		if resp.HistoryId != 0 {
			newest = strconv.FormatUint(resp.HistoryId, 10)
		}
		page++
		logging.TraceContext(ctx, "gmailapi: history.list page", "page", page, "page_records", len(resp.History), "total_records", len(records), "more", resp.NextPageToken != "")
		if resp.NextPageToken == "" {
			break
		}
		pageToken = resp.NextPageToken
	}
	logging.TraceContext(ctx, "gmailapi: history.list done", "start_history_id", startHistoryID, "newest", newest, "records", len(records), "pages", page)
	return records, newest, nil
}

// IsHistoryExpired reports whether err indicates the startHistoryId is older than
// Gmail's retention window (a 404), requiring a full resync.
func IsHistoryExpired(err error) bool {
	var gerr *googleapi.Error
	return errors.As(err, &gerr) && gerr.Code == 404
}

// IsNotFound reports whether err is a Gmail 404 for a specific resource — used to
// tell a message that has genuinely vanished (safe to skip during sync) from a
// transient fetch failure. Same HTTP code as IsHistoryExpired but a distinct
// call site (a message get, not history.list).
func IsNotFound(err error) bool {
	var gerr *googleapi.Error
	return errors.As(err, &gerr) && gerr.Code == 404
}

// isRetryableResponse reports whether err is a retryable HTTP error RESPONSE:
// the server received the request and returned a rate-limit or transient server
// error. It never matches bare network failures, so it's safe for
// non-idempotent calls (a network error there may mean the request succeeded).
func isRetryableResponse(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var gerr *googleapi.Error
	if !errors.As(err, &gerr) {
		return false
	}
	switch gerr.Code {
	case 429, 500, 502, 503, 504:
		return true
	case 403:
		for _, e := range gerr.Errors {
			if e.Reason == "rateLimitExceeded" || e.Reason == "userRateLimitExceeded" {
				return true
			}
		}
	}
	return false
}

// isRetryable reports whether an idempotent call should be retried: a retryable
// HTTP response, or a transient transport failure (connection reset/refused,
// timeout, dropped connection) that has no HTTP response — failing a whole sync
// or body fetch on a momentary blip is worse than a backed-off retry.
func isRetryable(err error) bool {
	if isRetryableResponse(err) {
		return true
	}
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var gerr *googleapi.Error
	if errors.As(err, &gerr) {
		return false // an HTTP response already classified non-retryable (400/401/404/…)
	}
	var nerr net.Error
	if errors.As(err, &nerr) {
		return true
	}
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
}

// retryAfter returns the server's Retry-After delay from err's HTTP response, or
// 0 when absent or unparseable. It accepts both delta-seconds and an HTTP-date,
// and is capped (maxRetryAfter) so an unreasonable hint can't hang an operation
// — the bounded retry count still applies on top.
func retryAfter(err error) time.Duration {
	var gerr *googleapi.Error
	if !errors.As(err, &gerr) || gerr.Header == nil {
		return 0
	}
	v := gerr.Header.Get("Retry-After")
	if v == "" {
		return 0
	}
	var d time.Duration
	if secs, e := strconv.Atoi(v); e == nil {
		d = time.Duration(secs) * time.Second
	} else if t, e := http.ParseTime(v); e == nil {
		d = time.Until(t)
	}
	if d < 0 {
		d = 0
	}
	if d > maxRetryAfter {
		d = maxRetryAfter
	}
	return d
}

func backoffDuration(attempt int) time.Duration {
	d := time.Duration(float64(backoffBase) * math.Pow(2, float64(attempt-1)))
	if d > backoffCap {
		d = backoffCap
	}
	return d + time.Duration(rand.Int63n(int64(d)/2+1))
}
