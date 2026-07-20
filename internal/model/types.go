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

// Snooze mirror labels. A snoozed conversation is mirrored to the provider as
// −INBOX plus two labels: SnoozeLabelRoot (stable membership — where snoozed
// mail lives in other clients, e.g. the Gmail phone app) and a
// SnoozeLabelPrefix+stamp child carrying the exact wake time, so any machine
// running this app can wake it on schedule (see internal/snooze).
const (
	SnoozeLabelRoot   = "Snoozed"
	SnoozeLabelPrefix = SnoozeLabelRoot + "/"
)

// IsSnoozeLabel reports whether a label name belongs to the snooze mirror —
// such labels are app bookkeeping and are hidden from label pickers and the
// sidebar (the Snoozed virtual folder is their UI).
func IsSnoozeLabel(name string) bool {
	return name == SnoozeLabelRoot || len(name) > len(SnoozeLabelPrefix) && name[:len(SnoozeLabelPrefix)] == SnoozeLabelPrefix
}

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
	RowID        int64
	AccountID    int64
	GmailID      string
	ThreadID     string
	InternalDate time.Time
	FromName     string
	FromAddr     string
	ReplyTo      string // Reply-To header (raw); replies target this over From when set
	ToAddrs      string
	CcAddrs      string
	// BccAddrs is only ever non-empty on the user's own copies (sent mail,
	// drafts) — a received message never carries the Bcc header.
	BccAddrs       string
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
	// ListUnsubscribe is the List-Unsubscribe header value ("" = none);
	// ListUnsubOneClick reports an RFC 8058 List-Unsubscribe-Post companion.
	ListUnsubscribe   string
	ListUnsubOneClick bool
	Labels            []string
}

// ThreadSummary describes a conversation for the thread list: its newest message
// plus how many messages it holds and how many are unread.
type ThreadSummary struct {
	ThreadID    string
	Latest      Message
	Count       int
	UnreadCount int
	// RepliedByMe is true when the thread's most recent message (any label) was
	// sent by this account — i.e. you had the last word, so it needs no reply.
	RepliedByMe bool
	// WokeFromSnooze is true when this thread's snooze already fired (it
	// returned to the inbox on schedule), so the list can show where it came
	// from instead of a stale or absent AI category.
	WokeFromSnooze bool
	// SnoozedUntil is the wake time (unix seconds) when this summary is shown in
	// the Snoozed view; 0 everywhere else. Rows show it in place of the date.
	SnoozedUntil int64
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
	From       string
	To         string
	Cc         string
	Bcc        string
	Subject    string
	Body       string // plain text
	HTMLBody   string // HTML alternative; when set the message goes out multipart/alternative
	InReplyTo  string // original Message-ID header
	References string // existing References plus the original Message-ID
	ThreadID   string // Gmail threadId, so Gmail files it in the conversation
	DraftID    string // when set, this edits/sends an existing Gmail draft
	// QuoteHTML is compose-side only: the replied-to/forwarded message's
	// sanitized HTML, carried into the compose window so the send can embed the
	// original's real formatting in the HTML alternative's quote. BuildMIME
	// never writes it to the wire.
	QuoteHTML string
	// SkipSignature is compose-side only: true suppresses the configured
	// signature for this compose (e.g. a reply to a GitHub notification, where
	// a personal sign-off is out of place). BuildMIME never reads it.
	SkipSignature bool
	Attachments   []OutgoingAttachment
	// Calendar is an iTIP payload (an .ics REPLY) sent as an inline
	// text/calendar body part inside the multipart/alternative — mail servers
	// (Exchange, Google) auto-process attendee responses only from an inline
	// calendar part, not from an attached .ics file. CalendarMethod is its
	// iTIP method ("REPLY"), echoed in the part's Content-Type.
	Calendar       []byte
	CalendarMethod string
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
	DraftID   string // source draft to delete once the send succeeds
	RFC822    []byte
	State     string // queued | failed
	Attempts  int
	LastError string
	NotBefore int64 // unix seconds; not sendable until now >= NotBefore (0 = ASAP)
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
	// ContentID is the part's Content-ID (without angle brackets) for an inline
	// image referenced by a cid: URL in the body; empty for a regular attachment.
	ContentID string
}
