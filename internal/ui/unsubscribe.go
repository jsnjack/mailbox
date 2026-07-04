package ui

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
	"github.com/jsnjack/mailbox/internal/dispatch"
	"github.com/jsnjack/mailbox/internal/logging"
	"github.com/jsnjack/mailbox/internal/model"
)

// unsubTargets is a parsed List-Unsubscribe header: the ways a list offers to
// be left, in preference order (one-click POST > mailto > browser URL).
type unsubTargets struct {
	OneClickURL string // https endpoint to POST "List-Unsubscribe=One-Click" (RFC 8058)
	Mailto      string // address to send an unsubscribe mail to
	MailtoSubj  string // subject the mailto: requested, if any
	URL         string // fallback: page to open in the browser
}

// parseListUnsubscribe splits a List-Unsubscribe value ("<url>, <mailto:…>")
// into actionable targets. oneClick marks the https URL as RFC 8058-capable
// (the sender also sent List-Unsubscribe-Post: List-Unsubscribe=One-Click).
func parseListUnsubscribe(header string, oneClick bool) (unsubTargets, bool) {
	var t unsubTargets
	for _, part := range strings.Split(header, ",") {
		v := strings.TrimSpace(part)
		v = strings.TrimPrefix(v, "<")
		v = strings.TrimSuffix(v, ">")
		switch {
		case strings.HasPrefix(strings.ToLower(v), "mailto:"):
			if t.Mailto != "" {
				continue
			}
			rest := v[len("mailto:"):]
			addr, query, _ := strings.Cut(rest, "?")
			t.Mailto, _ = url.PathUnescape(addr)
			if q, err := url.ParseQuery(query); err == nil {
				t.MailtoSubj = q.Get("subject")
			}
		case strings.HasPrefix(strings.ToLower(v), "https:"), strings.HasPrefix(strings.ToLower(v), "http:"):
			if oneClick && t.OneClickURL == "" && strings.HasPrefix(strings.ToLower(v), "https:") {
				t.OneClickURL = v
			}
			if t.URL == "" {
				t.URL = v
			}
		}
	}
	ok := t.OneClickURL != "" || t.Mailto != "" || t.URL != ""
	return t, ok
}

// performUnsubscribe leaves a list using the best target available: an RFC
// 8058 one-click POST (silent, in-app), an unsubscribe mail through the normal
// send path (undo-able), or opening the list's page in the browser. done (may
// be nil) receives the outcome text shown to the user.
func (w *window) performUnsubscribe(acctID int64, sender string, t unsubTargets, done func(outcome string)) {
	report := func(outcome string) {
		logging.Trace("ui: unsubscribe outcome", "sender", sender, "outcome", outcome)
		w.toast(outcome)
		if done != nil {
			done(outcome)
		}
	}
	switch {
	case t.OneClickURL != "":
		logging.Trace("ui: unsubscribe one-click", "sender", sender, "url", t.OneClickURL)
		go func() {
			client := &http.Client{Timeout: 20 * time.Second}
			resp, err := client.Post(t.OneClickURL, "application/x-www-form-urlencoded",
				strings.NewReader("List-Unsubscribe=One-Click"))
			outcome := "Unsubscribed from " + sender
			if err != nil {
				outcome = "Unsubscribe request failed — try again later"
				logging.Trace("ui: unsubscribe one-click failed", "sender", sender, "err", err)
			} else {
				_ = resp.Body.Close()
				if resp.StatusCode >= 400 {
					outcome = fmt.Sprintf("Unsubscribe request was rejected (HTTP %d)", resp.StatusCode)
					logging.Trace("ui: unsubscribe one-click rejected", "sender", sender, "status", resp.StatusCode)
				}
			}
			dispatch.Main(func() { report(outcome) })
		}()
	case t.Mailto != "":
		email := ""
		for _, a := range w.deps.Accounts {
			if a.ID == acctID {
				email = a.Email
				break
			}
		}
		subj := t.MailtoSubj
		if subj == "" {
			subj = "unsubscribe"
		}
		logging.Trace("ui: unsubscribe mailto", "sender", sender, "to", t.Mailto, "subject", subj)
		w.deferSend(acctID, model.OutgoingMessage{
			From: email, To: t.Mailto, Subject: subj,
			Body: "Please remove this address from your mailing list.",
		})
		report("Unsubscribe request sent to " + sender)
	case t.URL != "":
		logging.Trace("ui: unsubscribe via browser", "sender", sender, "url", t.URL)
		openExternal(t.URL)
		report("Unsubscribe page opened in the browser")
	}
}

// onUnsubscribe is the reader overflow action for the open message.
func (w *window) onUnsubscribe() {
	m := w.openMsg
	t, ok := parseListUnsubscribe(m.ListUnsubscribe, m.ListUnsubOneClick)
	if !ok {
		w.toast("This sender offers no unsubscribe method")
		return
	}
	sender := m.FromName
	if sender == "" {
		sender = m.FromAddr
	}
	w.performUnsubscribe(m.AccountID, sender, t, nil)
}

// openSubscriptions shows the subscriptions dashboard: every sender with a
// List-Unsubscribe header, sorted by mail volume, with a per-sender
// Unsubscribe button.
func (w *window) openSubscriptions() {
	acct := w.activeID
	logging.Trace("ui: open subscriptions", "account", acct)

	list := gtk.NewListBox()
	list.AddCSSClass("boxed-list")
	list.SetSelectionMode(gtk.SelectionNone)

	scroller := gtk.NewScrolledWindow()
	scroller.SetPolicy(gtk.PolicyNever, gtk.PolicyAutomatic)
	scroller.SetChild(list)
	scroller.SetVExpand(true)
	setMargins(scroller, 12, 12, 12, 12)

	tv := adw.NewToolbarView()
	tv.AddTopBar(adw.NewHeaderBar())
	tv.SetContent(scroller)

	dialog := adw.NewDialog()
	dialog.SetTitle("Subscriptions")
	dialog.SetContentWidth(560)
	dialog.SetContentHeight(600)
	dialog.SetChild(tv)
	dialog.Present(w.win)

	go func() {
		subs, err := w.deps.Store.Subscriptions(context.Background(), acct)
		dispatch.Main(func() {
			if err != nil {
				logging.Trace("ui: subscriptions query failed", "err", err)
				list.Append(gtk.NewLabel("Couldn't load subscriptions."))
				return
			}
			if len(subs) == 0 {
				empty := gtk.NewLabel("No mailing lists found.\nSenders appear here as newly synced mail carries unsubscribe headers.")
				empty.SetJustify(gtk.JustifyCenter)
				empty.AddCSSClass("dim-label")
				setMargins(empty, 18, 18, 18, 18)
				list.Append(empty)
				return
			}
			for _, sub := range subs {
				sub := sub
				row := adw.NewActionRow()
				row.SetTitle(sub.FromName)
				row.SetSubtitle(fmt.Sprintf("%d emails · %s", sub.Count, sub.FromAddr))
				btn := gtk.NewButtonWithLabel("Unsubscribe")
				btn.SetVAlign(gtk.AlignCenter)
				t, ok := parseListUnsubscribe(sub.ListUnsubscribe, sub.OneClick)
				if !ok {
					btn.SetSensitive(false)
				}
				btn.ConnectClicked(func() {
					btn.SetSensitive(false)
					btn.SetLabel("Unsubscribing…")
					w.performUnsubscribe(acct, sub.FromName, t, func(outcome string) {
						btn.SetLabel("Done")
						row.SetSubtitle(outcome)
					})
				})
				row.AddSuffix(btn)
				list.Append(row)
			}
		})
	}()
}
