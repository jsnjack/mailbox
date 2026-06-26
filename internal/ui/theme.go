package ui

import (
	_ "embed"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/diamondburned/gotk4/pkg/gdk/v4"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
)

// palmTreeSVG is a bundled symbolic icon (no palm tree exists in Adwaita); it is
// written to a cache icon theme dir and registered at startup.
//
//go:embed icons/palm-tree-symbolic.svg
var palmTreeSVG []byte

// registerCustomIcons installs bundled symbolic icons (the palm tree) into a
// cache icon theme that GTK searches, so they can be used by name. Best-effort.
func registerCustomIcons() {
	display := gdk.DisplayGetDefault()
	if display == nil {
		return
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		return
	}
	base := filepath.Join(cache, "mailbox", "icons")
	dir := filepath.Join(base, "hicolor", "scalable", "actions")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		slog.Warn("ui: icon dir", "err", err)
		return
	}
	if err := os.WriteFile(filepath.Join(dir, "palm-tree-symbolic.svg"), palmTreeSVG, 0o644); err != nil {
		slog.Warn("ui: write icon", "err", err)
		return
	}
	gtk.IconThemeGetForDisplay(display).AddSearchPath(base)
}

// appCSS adds a single accent colour on top of stock Adwaita, following GNOME's
// HIG (which also matches Material's one-seed-accent approach): symbolic icons
// stay monochrome — the theme foreground — and colour is reserved for state.
// Only three things are tinted, all from libadwaita's @accent_color family so
// they track the system accent and light/dark automatically: count-badge pills,
// the small unread dot, and the soft accent-tinted summary card. Buttons stay
// symbolic and flat; no chrome and no per-element hues are added.
const appCSS = `
/* Count badge: a quiet outline pill — no fill, just accent text and a faint
   accent border (folder unread counts and per-account unread). */
.badge-pill {
	border-radius: 999px;
	padding: 0 6px;
	color: @accent_color;
	border: 1px solid alpha(@accent_color, 0.40);
}

/* Unread conversations get a small, subtle accent dot alongside their bold
   weight — quiet enough not to compete with the text. */
.unread-dot {
	color: alpha(@accent_color, 0.55);
	font-size: 10px;
}

/* AI inbox-category tag on a list row: a small neutral pill; "Needs reply"
   (the actionable one) picks up the accent. */
.cat-tag {
	font-size: 0.76em;
	padding: 0 6px;
	border-radius: 6px;
	background-color: alpha(@card_fg_color, 0.10);
}
.cat-needsreply {
	background-color: alpha(@accent_color, 0.15);
	color: @accent_color;
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
	registerCustomIcons()
}
