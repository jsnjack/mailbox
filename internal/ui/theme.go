package ui

import (
	"github.com/diamondburned/gotk4/pkg/gdk/v4"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
)

// appCSS adds a single accent colour on top of stock Adwaita, following GNOME's
// HIG (which also matches Material's one-seed-accent approach): symbolic icons
// stay monochrome — the theme foreground — and colour is reserved for state.
// Only three things are tinted, all from libadwaita's @accent_color family so
// they track the system accent and light/dark automatically: count-badge pills,
// the small unread dot, and the soft accent-tinted summary card. Buttons stay
// symbolic and flat; no chrome and no per-element hues are added.
const appCSS = `
/* Count badge: an accent pill (folder unread counts and per-account unread). */
.badge-pill {
	background-color: @accent_bg_color;
	color: @accent_fg_color;
	border-radius: 999px;
	padding: 0 7px;
	font-weight: bold;
}

/* Unread conversations get a small accent dot, alongside their bold weight. */
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
