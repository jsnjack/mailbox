package ui

import (
	"fmt"
	"strings"
	"time"

	"github.com/diamondburned/gotk4/pkg/gtk/v4"
	"github.com/jsnjack/mailbox/internal/model"
)

func searchFormTokens(query string) []string {
	// Preserve quoted free-text phrases while replacing only form-owned operators.
	var tokens []string
	start := 0
	quoted := false
	for i, r := range query {
		if r == '"' {
			quoted = !quoted
		}
		if !quoted && (r == ' ' || r == '\t' || r == '\n') {
			if i > start {
				tokens = append(tokens, query[start:i])
			}
			start = i + 1
		}
	}
	if start < len(query) {
		tokens = append(tokens, query[start:])
	}
	return tokens
}

func searchWithFilters(query, from, after, before string, unread, attachment bool) (string, error) {
	for _, date := range []string{after, before} {
		if date != "" {
			if _, err := time.Parse("2006-01-02", date); err != nil {
				return "", fmt.Errorf("use YYYY-MM-DD for dates")
			}
		}
	}
	if after != "" && before != "" && after >= before {
		return "", fmt.Errorf("before must be later than after")
	}
	if strings.ContainsAny(from, "\"\r\n") {
		return "", fmt.Errorf("enter a sender name or email address without quotation marks")
	}
	tokens := searchFormTokens(query)
	var result []string
	for _, token := range tokens {
		lower := strings.ToLower(token)
		if strings.HasPrefix(lower, "from:") || strings.HasPrefix(lower, "after:") || strings.HasPrefix(lower, "before:") || lower == "is:unread" || lower == "has:attachment" || lower == "has:attachments" {
			continue
		}
		result = append(result, token)
	}
	if from != "" {
		result = append(result, `from:"`+from+`"`)
	}
	if after != "" {
		result = append(result, "after:"+after)
	}
	if before != "" {
		result = append(result, "before:"+before)
	}
	if unread {
		result = append(result, "is:unread")
	}
	if attachment {
		result = append(result, "has:attachment")
	}
	return strings.Join(result, " "), nil
}

func (w *window) buildSearchOptions() *gtk.Box {
	box := gtk.NewBox(gtk.OrientationVertical, 4)
	w.searchAccount = gtk.NewDropDownFromStrings(nil)
	a11yLabel(w.searchAccount, "Search account")
	w.searchAccount.SetTooltipText("Search account")
	w.searchAccount.SetHExpand(true)
	w.searchScope = gtk.NewDropDownFromStrings([]string{"Cached mail", "All mail on server"})
	a11yLabel(w.searchScope, "Search scope")
	w.searchScope.SetSensitive(w.canSearchServer())
	row := gtk.NewBox(gtk.OrientationHorizontal, 4)
	row.Append(gtk.NewLabel("Account"))
	row.Append(w.searchAccount)
	box.Append(row)
	scopeRow := gtk.NewBox(gtk.OrientationHorizontal, 4)
	scopeRow.Append(gtk.NewLabel("Search"))
	scopeRow.Append(w.searchScope)
	box.Append(scopeRow)
	w.searchAccount.Connect("notify::selected", func() {
		if w.updatingSearchOptions {
			return
		}
		i := int(w.searchAccount.Selected())
		if i >= len(w.deps.Accounts) {
			return
		}
		account := w.deps.Accounts[i]
		if account.ID == w.activeID {
			return
		}
		query := w.searchEntry.Text()
		server := w.searchScope.Selected() == 1
		w.setActiveAccount(account)
		w.openSearch()
		w.searchEntry.SetText(query)
		w.searchScope.SetSelected(0)
		if server {
			w.searchScope.SetSelected(1)
		} else {
			w.onSearchChanged()
		}
	})
	w.searchScope.Connect("notify::selected", func() {
		if w.updatingSearchOptions {
			return
		}
		if w.searchScope.Selected() == 1 && strings.Contains(strings.ToLower(w.searchEntry.Text()), "has:attachment") {
			for _, account := range w.deps.Accounts {
				if account.ID == w.activeID && account.Type == model.AccountIMAP {
					w.searchScope.SetSelected(0)
					w.toast("Attachment filtering is available in Cached mail for this account")
					return
				}
			}
		}
		w.serverSearch = w.searchScope.Selected() == 1
		w.serverQuery = ""
		w.refreshList(w.searchEntry.Text())
	})
	filters := gtk.NewExpander("Filters")
	form := gtk.NewBox(gtk.OrientationVertical, 6)
	from := gtk.NewEntry()
	from.SetPlaceholderText("Sender name or email")
	a11yLabel(from, "Sender filter")
	after := gtk.NewEntry()
	after.SetPlaceholderText("After: YYYY-MM-DD")
	a11yLabel(after, "After date")
	before := gtk.NewEntry()
	before.SetPlaceholderText("Before: YYYY-MM-DD")
	a11yLabel(before, "Before date")
	unread := gtk.NewCheckButtonWithLabel("Unread only")
	attachments := gtk.NewCheckButtonWithLabel("Has attachments")
	note := gtk.NewLabel("")
	note.SetWrap(true)
	note.SetXAlign(0)
	note.SetVisible(false)
	note.AddCSSClass("error")
	apply := gtk.NewButtonWithLabel("Apply filters")
	clear := gtk.NewButtonWithLabel("Clear filters")
	applyFilters := func() {
		query, err := searchWithFilters(w.searchEntry.Text(), strings.TrimSpace(from.Text()), strings.TrimSpace(after.Text()), strings.TrimSpace(before.Text()), unread.Active(), attachments.Active())
		if err == nil && attachments.Active() && w.searchScope.Selected() == 1 {
			for _, account := range w.deps.Accounts {
				if account.ID == w.activeID && account.Type == model.AccountIMAP {
					err = fmt.Errorf("choose Cached mail to filter attachments for this account")
				}
			}
		}
		note.SetVisible(err != nil)
		if err != nil {
			note.SetText(err.Error())
			return
		}
		w.searchEntry.SetText(query)
		w.onSearchChanged()
	}
	apply.ConnectClicked(applyFilters)
	clear.ConnectClicked(func() {
		from.SetText("")
		after.SetText("")
		before.SetText("")
		unread.SetActive(false)
		attachments.SetActive(false)
		applyFilters()
	})
	form.Append(from)
	form.Append(after)
	form.Append(before)
	form.Append(unread)
	form.Append(attachments)
	form.Append(note)
	buttons := gtk.NewBox(gtk.OrientationHorizontal, 4)
	buttons.Append(apply)
	buttons.Append(clear)
	form.Append(buttons)
	filters.Connect("notify::expanded", func() {
		if !filters.Expanded() {
			return
		}
		from.SetText("")
		after.SetText("")
		before.SetText("")
		unread.SetActive(false)
		attachments.SetActive(false)
		for _, token := range searchFormTokens(w.searchEntry.Text()) {
			key, value, ok := strings.Cut(token, ":")
			if !ok {
				continue
			}
			value = strings.Trim(value, "\"")
			switch strings.ToLower(key) {
			case "from":
				from.SetText(value)
			case "after":
				after.SetText(strings.ReplaceAll(value, "/", "-"))
			case "before":
				before.SetText(strings.ReplaceAll(value, "/", "-"))
			case "is":
				unread.SetActive(strings.EqualFold(value, "unread"))
			case "has":
				attachments.SetActive(strings.EqualFold(value, "attachment") || strings.EqualFold(value, "attachments"))
			}
		}
	})
	filters.SetChild(form)
	box.Append(filters)
	w.syncSearchOptions()
	return box
}

func (w *window) syncSearchOptions() {
	if w.searchAccount == nil {
		return
	}
	w.updatingSearchOptions = true
	defer func() { w.updatingSearchOptions = false }()
	var labels []string
	selected := uint(0)
	for i, a := range w.deps.Accounts {
		labels = append(labels, a.Email)
		if a.ID == w.activeID {
			selected = uint(i)
		}
	}
	signature := strings.Join(labels, "\n")
	if signature != w.searchAccountSignature {
		w.searchAccount.SetModel(gtk.NewStringList(labels))
		w.searchAccountSignature = signature
	}
	if len(labels) > 0 {
		w.searchAccount.SetSelected(selected)
	}
	scope := uint(0)
	if w.serverSearch {
		scope = 1
	}
	w.searchScope.SetSelected(scope)
}
