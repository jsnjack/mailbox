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
	LabelInbox     = "INBOX"
	LabelUnread    = "UNREAD"
	LabelStarred   = "STARRED"
	LabelImportant = "IMPORTANT"
	LabelSent      = "SENT"
	LabelTrash     = "TRASH"
	LabelSpam      = "SPAM"
	LabelDraft     = "DRAFT"
)

// Account types: which backend an account syncs through.
const (
	AccountGmail = "gmail" // Gmail REST API
	AccountIMAP  = "imap"  // generic IMAP/SMTP
)

// Account is a connected mail account. Secrets (OAuth refresh token or IMAP
// password) are never stored here — they live in the OS keyring, keyed by Email.
type Account struct {
	ID          int64
	Email       string
	DisplayName string
	Type        string // backend: AccountGmail | AccountIMAP (empty treated as Gmail)
	TokenExpiry time.Time
	Scopes      []string
	// SyncCursor is the opaque incremental-sync watermark: a Gmail historyId, or
	// an IMAP per-folder UIDVALIDITY/MODSEQ summary. The provider interprets it.
	SyncCursor   string
	BackfilledAt time.Time
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
	ReplyTo        string // Reply-To header (raw); replies target this over From when set
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

// ThreadSummary describes a conversation for the thread list: its newest message
// plus how many messages it holds and how many are unread.
type ThreadSummary struct {
	ThreadID    string
	Latest      Message
	Count       int
	UnreadCount int
}

// MessageBody holds the lazily-fetched body parts of a message.
type MessageBody struct {
	MessageRowID int64
	Text         string
	HTML         string
	RawHeaders   string
}

// OutgoingAttachment is a file to attach to an outgoing message.
type OutgoingAttachment struct {
	Filename string
	MimeType string
	Data     []byte
}

// OutgoingMessage is a message to be sent. For replies and forwards the
// threading fields (InReplyTo, References, ThreadID) tie it to the conversation.
type OutgoingMessage struct {
	From        string
	To          string
	Cc          string
	Bcc         string
	Subject     string
	Body        string // plain text
	InReplyTo   string // original Message-ID header
	References  string // existing References plus the original Message-ID
	ThreadID    string // Gmail threadId, so Gmail files it in the conversation
	DraftID     string // when set, this edits/sends an existing Gmail draft
	Attachments []OutgoingAttachment
}

// Contact is a correspondent derived from cached mail, used for recipient
// autocomplete. Count and LastSeen drive ranking (most/recently used first).
type Contact struct {
	Name     string
	Address  string
	Count    int
	LastSeen time.Time
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
