package ui

import (
	"fmt"
	"net/url"

	"github.com/diamondburned/gotk4/pkg/gdk/v4"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"

	"github.com/jsnjack/mailbox/internal/logging"
)

// svgViewMore is an inline copy of Adwaita's view-more-symbolic (16px, CC0),
// recolored to currentColor so it follows the header's text tint. Inlined as
// SVG markup — not fetched — so the reader's default-src 'none' CSP is
// untouched.
const svgViewMore = `<svg viewBox="0 0 16 16" aria-hidden="true"><path fill="currentColor" d="m 7.996094 0 c -1.105469 0 -2 0.894531 -2 2 s 0.894531 2 2 2 c 1.101562 0 2 -0.894531 2 -2 s -0.898438 -2 -2 -2 z m 0 6 c -1.105469 0 -2 0.894531 -2 2 s 0.894531 2 2 2 c 1.101562 0 2 -0.894531 2 -2 s -0.898438 -2 -2 -2 z m 0 6 c -1.105469 0 -2 0.894531 -2 2 s 0.894531 2 2 2 c 1.101562 0 2 -0.894531 2 -2 s -0.898438 -2 -2 -2 z m 0 0"/></svg>`

// msgMenuIcon renders a message header's always-visible ⋯ affordance: one dim
// three-dot link per message that opens the native per-message action menu
// (showMessageMenu) via the mbaction: scheme intercepted by onDecidePolicy.
func msgMenuIcon(gmailID string) string {
	return fmt.Sprintf(`<a class="mbmenu" href="mbaction:menu/%s" title="Message actions" aria-label="Message actions">%s</a>`,
		url.QueryEscape(gmailID), svgViewMore)
}

// showMessageMenu opens the per-message action menu — everything about one
// specific message of the open thread (answer or forward it, inspect its
// headers, analyze it), while the header bar acts on the conversation — as a
// native popover anchored at the pointer (the clicked ⋯). Hand-built flat
// buttons with the menu/rowmenu styling, like showRowMenu: a GtkPopoverMenu
// manually parented to a widget does not reliably activate its items' GActions.
func (w *window) showMessageMenu(gmailID string) {
	m, ok := w.threadMessageByID(gmailID)
	if !ok {
		return
	}
	logging.Trace("ui: show message menu", "id", gmailID, "thread", w.openThreadID, "account", w.activeID)

	pop := gtk.NewPopover()
	pop.SetParent(w.webview)
	pop.SetHasArrow(false)
	pop.SetPosition(gtk.PosBottom)
	// Anchor at the pointer: the click arrived as a WebView navigation (no
	// widget coords), so the position comes from the motion controller that
	// tracks the pointer over the reader (w.readerPtrX/Y).
	rect := gdk.NewRectangle(int(w.readerPtrX), int(w.readerPtrY), 1, 1)
	pop.SetPointingTo(&rect)
	// Detach when dismissed so the WebView isn't left parenting a stale popover.
	pop.ConnectClosed(func() { pop.Unparent() })

	box := gtk.NewBox(gtk.OrientationVertical, 0)
	box.AddCSSClass("menu")
	box.AddCSSClass("rowmenu")
	item := func(label string, fn func()) {
		lbl := gtk.NewLabel(label)
		lbl.SetXAlign(0)
		lbl.SetHExpand(true)
		b := gtk.NewButton()
		b.SetChild(lbl)
		b.AddCSSClass("flat")
		b.ConnectClicked(func() {
			logging.Trace("ui: message menu action", "action", label, "id", gmailID)
			pop.Popdown()
			fn()
		})
		box.Append(b)
	}
	if w.deps.Send != nil {
		item("Reply all", func() { w.replyToMessage(gmailID, true) })
		item("Reply", func() { w.replyToMessage(gmailID, false) })
		item("Forward", func() { w.forwardMessage(gmailID) })
		box.Append(gtk.NewSeparator(gtk.OrientationHorizontal))
	}
	item("View headers", func() { w.viewMessageHeaders(m) })
	if w.deps.Assistant != nil && w.aiPhishing {
		item("Check for phishing", func() { w.analyzeMessage(m) })
	}

	pop.SetChild(box)
	pop.Popup()
}
