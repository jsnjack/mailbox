// Package model holds the plain domain types shared across the app. It must not
// import any GTK or persistence package so it stays usable from every layer.
package model

import "time"

// LabelType distinguishes Gmail's built-in labels from user-created ones.
type LabelType string

const (
	// LabelSystem marks a Gmail built-in label (INBOX, SENT, STARRED, ...).
	LabelSystem LabelType = "system"
	// LabelUser marks a user-created label.
	LabelUser LabelType = "user"
)

// Well-known Gmail system label IDs used for flag decoding and mutations.
const (
	LabelInbox   = "INBOX"
	LabelUnread  = "UNREAD"
	LabelStarred = "STARRED"
	LabelSent    = "SENT"
	LabelTrash   = "TRASH"
	LabelSpam    = "SPAM"
	LabelDraft   = "DRAFT"
)

// Account is a connected Gmail account. The OAuth refresh token is never stored
// here — it lives in the OS keyring, keyed by Email.
type Account struct {
	ID            int64
	Email         string
	DisplayName   string
	TokenExpiry   time.Time
	Scopes        []string
	LastHistoryID string // Gmail historyId watermark for incremental sync
	BackfilledAt  time.Time
}

// Label is a Gmail label scoped to an account.
type Label struct {
	AccountID   int64
	GmailID     string
	Name        string
	Type        LabelType
	ColorBg     string
	UnreadTotal int
}

// Thread is a Gmail conversation, denormalized for fast list rendering.
type Thread struct {
	AccountID     int64
	GmailID       string
	LastMessageAt time.Time
	Subject       string
	Snippet       string
	MsgCount      int
	UnreadCount   int
}

// Message is the metadata for a single message. The body lives in MessageBody
// and is loaded lazily when the message is opened.
type Message struct {
	RowID          int64
	AccountID      int64
	GmailID        string
	ThreadID       string
	InternalDate   time.Time
	FromName       string
	FromAddr       string
	ToAddrs        string
	CcAddrs        string
	Subject        string
	Snippet        string
	RFC822MsgID    string
	InReplyTo      string
	References     string
	IsUnread       bool
	IsStarred      bool
	HasAttachments bool
	SizeEstimate   int64
	BodyFetched    bool
	Labels         []string
}

// MessageBody holds the lazily-fetched body parts of a message.
type MessageBody struct {
	MessageRowID int64
	Text         string
	HTML         string
	RawHeaders   string
}

// OutgoingMessage is a message to be sent. For replies and forwards the
// threading fields (InReplyTo, References, ThreadID) tie it to the conversation.
type OutgoingMessage struct {
	From       string
	To         string
	Cc         string
	Subject    string
	Body       string // plain text
	InReplyTo  string // original Message-ID header
	References string // existing References plus the original Message-ID
	ThreadID   string // Gmail threadId, so Gmail files it in the conversation
}

// OutboxItem is a queued outgoing message awaiting (re)send.
type OutboxItem struct {
	ID        int64
	LocalUUID string
	AccountID int64
	ThreadID  string
	RFC822    []byte
	State     string // queued | failed
	Attempts  int
	LastError string
}

// Attachment points to an attachment's bytes; the bytes are stored on disk
// (content-addressed by SHA-256), not in the database.
type Attachment struct {
	ID           int64
	MessageRowID int64
	GmailAttID   string
	Filename     string
	MimeType     string
	SizeBytes    int64
	SHA256       string
	DiskPath     string
}
