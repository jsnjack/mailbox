package store

import (
	"strings"

	"github.com/jsnjack/mailbox/internal/model"
)

// searchFilter is a parsed local-search query: zero or more field operators plus
// the free-text remainder that still goes through FTS.
type searchFilter struct {
	from      []string // from:  → matched against from_addr/from_name (AND across values)
	to        []string // to:    → matched against to_addrs/cc_addrs
	subject   []string // subject: → matched against subject
	inLabels  []string // in:    → label token (system alias or user-label name)
	hasAttach bool     // has:attachment
	freeText  string   // everything else, for FTS MATCH
}

// systemLabelAliases maps the words users type in `in:` to Gmail system label
// ids. Anything not here is treated as a user-label name.
var systemLabelAliases = map[string]string{
	"inbox":     model.LabelInbox,
	"unread":    model.LabelUnread,
	"starred":   model.LabelStarred,
	"important": model.LabelImportant,
	"sent":      model.LabelSent,
	"trash":     model.LabelTrash,
	"spam":      model.LabelSpam,
	"draft":     model.LabelDraft,
	"drafts":    model.LabelDraft,
}

// parseSearch splits a raw query into field operators and free text. A token is
// an operator only when its key is one we recognize and it carries a non-empty
// value (has:attachment being the one valueless form) — so `foo:bar`, a lone
// `:`, and quoted phrases fall through to free text unchanged, preserving the
// prior plain-FTS behavior. Operator values may be double-quoted to include
// spaces (from:"John Doe").
func parseSearch(raw string) searchFilter {
	var f searchFilter
	var free []string
	for _, tok := range splitSearchTokens(raw) {
		key, val, isPair := splitOperator(tok)
		if !isPair {
			free = append(free, tok)
			continue
		}
		switch key {
		case "from":
			f.from = append(f.from, val)
		case "to":
			f.to = append(f.to, val)
		case "subject":
			f.subject = append(f.subject, val)
		case "in":
			f.inLabels = append(f.inLabels, val)
		case "has":
			if v := strings.ToLower(val); v == "attachment" || v == "attachments" {
				f.hasAttach = true
			} else {
				free = append(free, tok) // has:something-else isn't an operator
			}
		default:
			free = append(free, tok)
		}
	}
	f.freeText = strings.Join(free, " ")
	return f
}

// splitOperator parses "key:value" where key is a recognized operator and value
// is non-empty (surrounding double quotes stripped). Returns isPair=false for
// anything else, so the caller keeps it as free text.
func splitOperator(tok string) (key, val string, isPair bool) {
	i := strings.IndexByte(tok, ':')
	if i <= 0 || i == len(tok)-1 {
		return "", "", false // no colon, leading colon, or trailing colon
	}
	key = strings.ToLower(tok[:i])
	switch key {
	case "from", "to", "subject", "in", "has":
	default:
		return "", "", false
	}
	val = strings.Trim(tok[i+1:], `"`)
	if val == "" {
		return "", "", false
	}
	return key, val, true
}

// splitSearchTokens splits on whitespace but keeps double-quoted spans together,
// so from:"John Doe" and "quoted phrase" survive as single tokens.
func splitSearchTokens(raw string) []string {
	var toks []string
	var cur strings.Builder
	inQuote := false
	flush := func() {
		if cur.Len() > 0 {
			toks = append(toks, cur.String())
			cur.Reset()
		}
	}
	for _, r := range raw {
		switch {
		case r == '"':
			inQuote = !inQuote
			cur.WriteRune(r)
		case (r == ' ' || r == '\t' || r == '\n') && !inQuote:
			flush()
		default:
			cur.WriteRune(r)
		}
	}
	flush()
	return toks
}

// buildFilterPredicates turns the operators into SQL predicates (over the
// messages alias `m`) and their bound args. Free text is handled separately via
// FTS by the caller.
func (f searchFilter) buildFilterPredicates() (preds []string, args []any) {
	like := func(v string) string { return "%" + escapeLike(v) + "%" }
	for _, v := range f.from {
		preds = append(preds, "(m.from_addr LIKE ? ESCAPE '\\' OR m.from_name LIKE ? ESCAPE '\\')")
		args = append(args, like(v), like(v))
	}
	for _, v := range f.to {
		preds = append(preds, "(m.to_addrs LIKE ? ESCAPE '\\' OR m.cc_addrs LIKE ? ESCAPE '\\')")
		args = append(args, like(v), like(v))
	}
	for _, v := range f.subject {
		preds = append(preds, "m.subject LIKE ? ESCAPE '\\'")
		args = append(args, like(v))
	}
	if f.hasAttach {
		preds = append(preds, "m.has_attachments = 1")
	}
	for _, v := range f.inLabels {
		if id, ok := systemLabelAliases[strings.ToLower(v)]; ok {
			preds = append(preds, "EXISTS (SELECT 1 FROM message_labels ml WHERE ml.message_rowid = m.rowid AND ml.label_id = ?)")
			args = append(args, id)
		} else {
			preds = append(preds, "EXISTS (SELECT 1 FROM message_labels ml JOIN labels l ON l.account_id = ml.account_id AND l.gmail_id = ml.label_id WHERE ml.message_rowid = m.rowid AND lower(l.name) = lower(?))")
			args = append(args, v)
		}
	}
	return preds, args
}

// escapeLike escapes the LIKE metacharacters so an operator value is matched
// literally (using ESCAPE '\' on the query side).
func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}
