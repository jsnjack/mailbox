package ui

import (
	"github.com/diamondburned/gotk4/pkg/gdk/v4"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
)

// appCSS adds colour on top of the stock Adwaita theme without fighting it:
// symbolic folder icons get a recognisable hue each, unread mail picks up the
// accent colour, count badges become accent pills, and the AI summary card and
// tracker indicator get their own tints. Buttons stay symbolic and flat — only
// colour is added, never chrome. Colours reference libadwaita's named theme
// variables (@accent_color etc.) where possible so they track light/dark.
const appCSS = `
/* Sidebar folder icons — a recognisable colour per mailbox. The class sits on
   the GtkImage; recolouring its "color" recolours the symbolic icon. */
.folder-inbox     { color: #3584e4; } /* blue   */
.folder-starred   { color: #e5a50a; } /* gold   */
.folder-important { color: #e5a50a; }
.folder-sent      { color: #2ec27e; } /* green  */
.folder-draft     { color: #9141ac; } /* purple */
.folder-spam      { color: #e66100; } /* orange */
.folder-trash     { color: #c01c28; } /* red    */
.folder-all       { color: #9a9996; } /* grey   */
.folder-label     { color: #62a0ea; }

/* Count badge: an accent pill (label unread counts and per-account unread). */
.badge-pill {
	background-color: @accent_bg_color;
	color: @accent_fg_color;
	border-radius: 999px;
	padding: 0 7px;
	font-weight: bold;
}

/* Unread conversations pick up the accent colour, alongside their bold weight. */
.unread-accent { color: @accent_color; }
.unread-dot {
	color: @accent_color;
	font-size: 13px;
}

/* AI thread-summary card: a soft accent-tinted panel pinned above the thread. */
.summary-card {
	background-color: alpha(@accent_color, 0.08);
	border: 1px solid alpha(@accent_color, 0.28);
	border-radius: 12px;
}
.summary-title { color: @accent_color; }

/* "N trackers blocked" reads as a positive, secure state — tint it green. */
.tracker-shield { color: #2ec27e; }
`

// loadAppCSS registers the application stylesheet on the default display, above
// the theme but below user overrides. It is a safe no-op when there is no
// display (e.g. before the GTK application is activated).
func loadAppCSS() {
	display := gdk.DisplayGetDefault()
	if display == nil {
		return
	}
	provider := gtk.NewCSSProvider()
	provider.LoadFromString(appCSS)
	gtk.StyleContextAddProviderForDisplay(display, provider, gtk.STYLE_PROVIDER_PRIORITY_APPLICATION)
}
