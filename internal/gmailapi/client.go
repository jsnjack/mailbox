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
	"math"
	"math/rand"
	"strconv"
	"time"

	gmail "google.golang.org/api/gmail/v1"
	"google.golang.org/api/googleapi"
)

const (
	// maxConcurrent stays under Gmail's ~50 concurrent-requests-per-mailbox ceiling.
	maxConcurrent = 30
	maxRetries    = 5
	backoffBase   = 500 * time.Millisecond
	backoffCap    = 30 * time.Second
)

// Client is a throttled, retrying Gmail client for one account.
type Client struct {
	srv    *gmail.Service
	sem    chan struct{}
	budget *RateBudget
}

// NewClient wraps a Gmail service for one account.
func NewClient(srv *gmail.Service) *Client {
	return &Client{
		srv:    srv,
		sem:    make(chan struct{}, maxConcurrent),
		budget: NewRateBudget(),
	}
}

// do reserves quota, acquires a concurrency slot, and runs fn with retry/backoff.
func (c *Client) do(ctx context.Context, cost int, fn func() error) error {
	if err := c.budget.Reserve(ctx, cost); err != nil {
		return err
	}
	select {
	case c.sem <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-c.sem }()

	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			if err := sleepCtx(ctx, backoffDuration(attempt)); err != nil {
				return err
			}
		}
		if err := fn(); err != nil {
			if !isRetryable(err) {
				return err
			}
			lastErr = err
			continue
		}
		return nil
	}
	return fmt.Errorf("gmail call failed after %d attempts: %w", maxRetries+1, lastErr)
}

// GetProfile returns the account profile (email, message count, current historyId).
func (c *Client) GetProfile(ctx context.Context) (*gmail.Profile, error) {
	var p *gmail.Profile
	err := c.do(ctx, costMessageGet, func() error {
		r, e := c.srv.Users.GetProfile("me").Context(ctx).Do()
		p = r
		return e
	})
	if err != nil {
		return nil, fmt.Errorf("get profile: %w", err)
	}
	return p, nil
}

// ListLabels returns all labels for the account.
func (c *Client) ListLabels(ctx context.Context) ([]*gmail.Label, error) {
	var resp *gmail.ListLabelsResponse
	err := c.do(ctx, costLabelsList, func() error {
		r, e := c.srv.Users.Labels.List("me").Context(ctx).Do()
		resp = r
		return e
	})
	if err != nil {
		return nil, fmt.Errorf("list labels: %w", err)
	}
	return resp.Labels, nil
}

// ListMessageIDs lists message ids matching query (Gmail search syntax; empty for
// all), newest first, up to max (0 = no limit). Each page costs few quota units.
func (c *Client) ListMessageIDs(ctx context.Context, query string, max int) ([]string, error) {
	var ids []string
	pageToken := ""
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
		if resp.NextPageToken == "" || (max > 0 && len(ids) >= max) {
			break
		}
		pageToken = resp.NextPageToken
	}
	if max > 0 && len(ids) > max {
		ids = ids[:max]
	}
	return ids, nil
}

var metadataHeaders = []string{"From", "To", "Cc", "Subject", "Date", "Message-ID", "In-Reply-To", "References"}

// GetMessageMetadata fetches a message in metadata format (headers + labels, no body).
func (c *Client) GetMessageMetadata(ctx context.Context, id string) (*gmail.Message, error) {
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
	return msg, nil
}

// GetMessageFull fetches a message in full format (payload with body parts).
func (c *Client) GetMessageFull(ctx context.Context, id string) (*gmail.Message, error) {
	var msg *gmail.Message
	err := c.do(ctx, costMessageGet, func() error {
		r, e := c.srv.Users.Messages.Get("me", id).Format("full").Context(ctx).Do()
		msg = r
		return e
	})
	if err != nil {
		return nil, fmt.Errorf("get message full %s: %w", id, err)
	}
	return msg, nil
}

// Send transmits a raw RFC 5322 message. threadID (optional) files it into an
// existing Gmail conversation. It returns the new message id.
func (c *Client) Send(ctx context.Context, raw []byte, threadID string) (string, error) {
	var sent *gmail.Message
	err := c.do(ctx, costSend, func() error {
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
	return sent.Id, nil
}

// ModifyLabels adds and removes Gmail label ids on a message (e.g. remove UNREAD
// to mark read, remove INBOX to archive, add/remove STARRED).
func (c *Client) ModifyLabels(ctx context.Context, id string, add, remove []string) error {
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
	start, err := strconv.ParseUint(startHistoryID, 10, 64)
	if err != nil {
		return nil, "", fmt.Errorf("parse historyId %q: %w", startHistoryID, err)
	}
	var records []*gmail.History
	pageToken := ""
	newest := startHistoryID
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
			return nil, "", fmt.Errorf("list history: %w", err)
		}
		records = append(records, resp.History...)
		if resp.HistoryId != 0 {
			newest = strconv.FormatUint(resp.HistoryId, 10)
		}
		if resp.NextPageToken == "" {
			break
		}
		pageToken = resp.NextPageToken
	}
	return records, newest, nil
}

// IsHistoryExpired reports whether err indicates the startHistoryId is older than
// Gmail's retention window (a 404), requiring a full resync.
func IsHistoryExpired(err error) bool {
	var gerr *googleapi.Error
	return errors.As(err, &gerr) && gerr.Code == 404
}

func isRetryable(err error) bool {
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

func backoffDuration(attempt int) time.Duration {
	d := time.Duration(float64(backoffBase) * math.Pow(2, float64(attempt-1)))
	if d > backoffCap {
		d = backoffCap
	}
	return d + time.Duration(rand.Int63n(int64(d)/2+1))
}
