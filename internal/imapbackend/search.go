package imapbackend

import (
	"fmt"
	"strings"

	"github.com/emersion/go-imap/v2"
	"github.com/jsnjack/mailbox/internal/logging"
	"github.com/jsnjack/mailbox/internal/model"
)

// searchQuery is a parsed provider query for SearchIDs: an optional label scope
// (from an in: operator) plus IMAP SEARCH criteria built from the remaining
// operators and free text.
type searchQuery struct {
	label    string              // label id from in:<label>; "" = all synced folders
	criteria imap.SearchCriteria // header/text criteria; zero value matches all
}

// headerOperators maps supported <op>: query operators to the RFC 5322 header
// an IMAP `SEARCH HEADER <field>` runs over.
var headerOperators = map[string]string{
	"from":    "From",
	"to":      "To",
	"cc":      "Cc",
	"bcc":     "Bcc",
	"subject": "Subject",
}

// rejectedOperators are Gmail-style operators IMAP SEARCH has no safe mapping
// for here. A query using one errors instead of silently matching the wrong
// (or every) message.
var rejectedOperators = map[string]bool{
	"is": true, "has": true, "label": true, "rfc822msgid": true,
	"after": true, "before": true, "older": true, "newer": true,
	"older_than": true, "newer_than": true, "filename": true, "list": true,
	"category": true, "deliveredto": true, "size": true, "larger": true, "smaller": true,
}

// parseSearchQuery turns a Gmail-flavoured query into a searchQuery. Supported:
// `in:<label>` scopes the search to the folder(s) mapped to that label;
// `from:`/`to:`/`cc:`/`bcc:`/`subject:` become IMAP `SEARCH HEADER` terms; any
// remaining free-text token becomes an IMAP `SEARCH TEXT` term (server-side
// substring over headers + body, terms ANDed) — the pragmatic mapping, since an
// OR tree over FROM/SUBJECT/TEXT buys nothing TEXT doesn't already cover. A
// known-unsupported operator is an error: SearchIDs must never degrade a scoped
// query into "all messages" (Empty Trash passes "in:trash" and deletes what
// comes back).
func parseSearchQuery(query string) (searchQuery, error) {
	var q searchQuery
	for _, tok := range strings.Fields(query) {
		key, val, hasKey := strings.Cut(tok, ":")
		lkey := strings.ToLower(key)
		switch {
		case hasKey && lkey == "in":
			if q.label != "" {
				return searchQuery{}, fmt.Errorf("imap: multiple in: operators in query %q", query)
			}
			if val == "" {
				return searchQuery{}, fmt.Errorf("imap: empty in: operator in query %q", query)
			}
			q.label = labelIDForQueryName(val)
		case hasKey && headerOperators[lkey] != "":
			if val == "" {
				return searchQuery{}, fmt.Errorf("imap: empty %s: operator in query %q", lkey, query)
			}
			q.criteria.Header = append(q.criteria.Header, imap.SearchCriteriaHeaderField{
				Key: headerOperators[lkey], Value: val,
			})
		case hasKey && rejectedOperators[lkey]:
			return searchQuery{}, fmt.Errorf("imap: unsupported search operator %q in query %q", lkey, query)
		default:
			// Free text (including tokens with a colon that isn't a known operator,
			// e.g. a URL) searches headers + body.
			q.criteria.Text = append(q.criteria.Text, tok)
		}
	}
	logging.Trace("imapbackend: parse search query",
		"query", query, "label", q.label, "headers", len(q.criteria.Header), "text", len(q.criteria.Text))
	return q, nil
}

// labelIDForQueryName maps an in:<name> operand to a label id: Gmail's folder
// names map to the app's system label ids; anything else is taken verbatim (a
// user label id is its folder name).
func labelIDForQueryName(name string) string {
	switch strings.ToLower(name) {
	case "trash":
		return model.LabelTrash
	case "spam", "junk":
		return model.LabelSpam
	case "inbox":
		return model.LabelInbox
	case "sent":
		return model.LabelSent
	case "draft", "drafts":
		return model.LabelDraft
	}
	return name
}

// foldersForLabel returns the synced mailbox(es) mapped to a label id. It
// errors when nothing maps — a scoped search must fail loudly rather than fall
// back to every folder. Caller must have run ensureFolders.
func (b *Backend) foldersForLabel(label string) ([]string, error) {
	b.folderMu.Lock()
	defer b.folderMu.Unlock()
	var out []string
	for _, f := range b.synced {
		if b.folderToLabel[f] == label {
			out = append(out, f)
		}
	}
	if len(out) == 0 {
		logging.Trace("imapbackend: no folder for label", "account", b.cfg.Email, "label", label)
		return nil, fmt.Errorf("imap: no folder mapped to label %q", label)
	}
	logging.Trace("imapbackend: folders for label", "account", b.cfg.Email, "label", label, "folders", out)
	return out, nil
}
