package ui

import (
	"context"
	"html"
	"os"
	"strings"
	"time"

	"github.com/diamondburned/gotk4/pkg/gtk/v4"
	"github.com/jsnjack/mailbox/internal/ics"
	"github.com/jsnjack/mailbox/internal/logging"
	"github.com/jsnjack/mailbox/internal/model"
)

// buildInviteCard creates the (hidden) meeting-invite card shown above the
// conversation when a message carries a calendar invite.
func (w *window) buildInviteCard() gtk.Widgetter {
	w.inviteCard = gtk.NewBox(gtk.OrientationVertical, 4)
	w.inviteCard.AddCSSClass("card")
	w.inviteCard.AddCSSClass("invite-card") // inner padding — see theme.go
	setMargins(w.inviteCard, 12, 12, 6, 6)
	w.inviteCard.SetVisible(false)
	return w.inviteCard
}

// isCalendarAttachment reports whether an attachment looks like an iCalendar
// invite (Google/Outlook attach it as invite.ics with a text/calendar type).
func isCalendarAttachment(a model.Attachment) bool {
	mt := strings.ToLower(a.MimeType)
	return strings.HasPrefix(mt, "text/calendar") || strings.HasPrefix(mt, "application/ics") ||
		strings.HasSuffix(strings.ToLower(a.Filename), ".ics")
}

// detectInvite finds and parses a calendar invite among the thread's
// attachments, preferring the newest message's (atts are gathered
// oldest-message-first). Runs off the main thread: the .ics is downloaded into
// the attachment cache and parsed there. Nil when the thread carries none.
func (w *window) detectInvite(ctx context.Context, atts []threadAttachment) (*ics.Event, int64) {
	for i := len(atts) - 1; i >= 0; i-- {
		ta := atts[i]
		if !isCalendarAttachment(ta.att) {
			continue
		}
		path, err := w.deps.OpenAttach(ctx, ta.accountID, ta.gmailID, ta.att.ID)
		if err != nil {
			logging.Trace("ui: invite fetch failed", "id", ta.gmailID, "att", ta.att.Filename, "err", err)
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		ev, err := ics.Parse(data)
		if err != nil {
			logging.Trace("ui: invite parse failed", "att", ta.att.Filename, "err", err)
			continue
		}
		logging.Trace("ui: invite detected", "summary", ev.Summary, "method", ev.Method, "organizer", ev.Organizer)
		return ev, ta.accountID
	}
	return nil, 0
}

// showInviteCard renders ev into the invite card (nil hides it). A REQUEST
// gets Accept / Maybe / Decline buttons that send the iTIP reply; a CANCEL
// shows a cancellation note.
func (w *window) showInviteCard(accountID int64, ev *ics.Event) {
	for child := w.inviteCard.FirstChild(); child != nil; child = w.inviteCard.FirstChild() {
		w.inviteCard.Remove(child)
	}
	if ev == nil {
		w.inviteCard.SetVisible(false)
		return
	}

	title := gtk.NewLabel("")
	title.SetMarkup("📅 <b>" + html.EscapeString(ev.Summary) + "</b>")
	title.SetXAlign(0)
	title.SetWrap(true)
	w.inviteCard.Append(title)

	addLine := func(s string) {
		if s == "" {
			return
		}
		l := gtk.NewLabel(s)
		l.SetXAlign(0)
		l.SetWrap(true)
		l.AddCSSClass("caption")
		w.inviteCard.Append(l)
	}
	addLine(formatEventTime(ev))
	if ev.Location != "" {
		addLine("Location: " + ev.Location)
	}
	if ev.Organizer != "" {
		addLine("Organizer: " + ev.Organizer)
	}

	if ev.Method == "CANCEL" {
		addLine("This event was cancelled.")
		w.inviteCard.SetVisible(true)
		return
	}

	btns := gtk.NewBox(gtk.OrientationHorizontal, 6)
	btns.SetMarginTop(6)
	answered := gtk.NewLabel("")
	answered.SetXAlign(0)
	answered.SetVisible(false)
	// An emailed iTIP reply updates the organizer's calendar, not the copy the
	// user's own calendar service keeps — only its own UI/API can mark that
	// one. Say what actually happened so "accepted" isn't over-promised.
	answeredNote := gtk.NewLabel("The reply was emailed to the organizer. Your own calendar may still show the event as unanswered.")
	answeredNote.SetXAlign(0)
	answeredNote.SetWrap(true)
	answeredNote.AddCSSClass("caption")
	answeredNote.AddCSSClass("dim-label")
	answeredNote.SetVisible(false)
	rsvpBtn := func(label, partstat, verb, confirmation string, suggested bool) {
		b := gtk.NewButtonWithLabel(label)
		if suggested {
			b.AddCSSClass("suggested-action")
		}
		b.ConnectClicked(func() {
			logging.Trace("ui: invite rsvp", "partstat", partstat, "summary", ev.Summary)
			if !w.rsvp(accountID, ev, partstat, verb) {
				return
			}
			btns.SetVisible(false)
			answered.SetText(confirmation)
			answered.SetVisible(true)
			answeredNote.SetVisible(true)
		})
		btns.Append(b)
	}
	rsvpBtn("Accept", "ACCEPTED", "Accepted", "You accepted this invitation.", true)
	rsvpBtn("Maybe", "TENTATIVE", "Tentative", "You tentatively accepted this invitation.", false)
	rsvpBtn("Decline", "DECLINED", "Declined", "You declined this invitation.", false)
	w.inviteCard.Append(btns)
	w.inviteCard.Append(answered)
	w.inviteCard.Append(answeredNote)
	w.inviteCard.SetVisible(true)
}

// rsvp emails the iTIP REPLY to the organizer through the normal send path
// (outbox-first, with the undo toast). Reports whether a send was started.
func (w *window) rsvp(accountID int64, ev *ics.Event, partstat, verb string) bool {
	if ev.Organizer == "" {
		w.toast("The invite has no organizer address to reply to")
		return false
	}
	email := ""
	for _, a := range w.deps.Accounts {
		if a.ID == accountID {
			email = a.Email
			break
		}
	}
	if email == "" {
		return false
	}
	name := w.accountNames[email]
	who := name
	if who == "" {
		who = email
	}
	action := map[string]string{
		"ACCEPTED":  "has accepted the invitation",
		"TENTATIVE": "has tentatively accepted the invitation",
		"DECLINED":  "has declined the invitation",
	}[partstat]
	reply := ics.Reply(ev, email, name, partstat, time.Now())
	msg := model.OutgoingMessage{
		From:    email,
		To:      ev.Organizer,
		Subject: verb + ": " + ev.Summary,
		Body:    who + " " + action + ": " + ev.Summary,
		// The response goes out the way Gmail/Outlook send RSVPs: an inline
		// text/calendar; method=REPLY body part (the only shape Exchange and
		// Google auto-process into the organizer's calendar) plus an .ics
		// attachment copy for clients that only look at attachments.
		Calendar:       reply,
		CalendarMethod: "REPLY",
		Attachments: []model.OutgoingAttachment{{
			Filename: "response.ics",
			MimeType: "application/ics; name=\"response.ics\"",
			Data:     reply,
		}},
	}
	w.deferSend(accountID, msg)
	return true
}

// formatEventTime renders the event's time range compactly: all-day dates,
// same-day ranges as one date + two times, else a full range.
func formatEventTime(ev *ics.Event) string {
	if ev.Start.IsZero() {
		return ""
	}
	if ev.AllDay {
		// DTEND of an all-day event is exclusive; show the last included day.
		if !ev.End.IsZero() && ev.End.Sub(ev.Start) > 24*time.Hour {
			return ev.Start.Format("Mon, Jan 2") + " – " + ev.End.AddDate(0, 0, -1).Format("Mon, Jan 2")
		}
		return ev.Start.Format("Mon, Jan 2") + " (all day)"
	}
	s := ev.Start.Format("Mon, Jan 2 15:04")
	switch {
	case ev.End.IsZero():
		return s
	case ev.Start.Year() == ev.End.Year() && ev.Start.YearDay() == ev.End.YearDay():
		return s + " – " + ev.End.Format("15:04")
	default:
		return s + " – " + ev.End.Format("Mon, Jan 2 15:04")
	}
}
