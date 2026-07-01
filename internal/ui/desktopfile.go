package ui

import (
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/jsnjack/mailbox/internal/logging"
)

// desktopFileName is <app-id>.desktop. GNOME maps a GApplication's id to an
// installed desktop entry of this name to display notifications and to know the
// app's name/icon; without it, g_application_send_notification is silently
// dropped.
const desktopFileName = appID + ".desktop"

// desktopEntry mirrors packaging/com.jsnjack.mailbox.desktop. The Exec line is
// rewritten to the running binary's absolute path when self-installed for
// development (the packaged entry uses the on-PATH name). MimeType registers the
// app as a mailto handler so it appears under GNOME's Default Apps → Mail; the
// "%u" passes the clicked mailto: URI to the running app (see composeFromMailto).
const desktopEntry = `[Desktop Entry]
Type=Application
Name=Mailbox
GenericName=Email Client
Comment=A native, fast Gmail client
Exec=mailbox %u
Icon=com.jsnjack.mailbox
Terminal=false
Categories=Network;Email;GTK;GNOME;
Keywords=Email;Gmail;Mail;
MimeType=x-scheme-handler/mailto;
StartupNotify=true
`

// ensureDesktopFile makes sure a desktop entry for this app exists where the
// desktop environment can find it, so notifications actually appear. GNOME
// refuses to show notifications from an application id it can't resolve to an
// installed .desktop file, so a binary run straight from ./bin (development)
// gets no notifications until one is installed.
//
// It is a no-op when a system package already installed the entry (the RPM ships
// it under /usr/share/applications) or when a user entry already exists, so it
// never shadows or clobbers a real install. Best-effort: failures are logged and
// ignored — they only cost notifications, not functionality.
func ensureDesktopFile() {
	logging.Trace("ui: ensure desktop file", "name", desktopFileName)
	for _, dir := range systemAppDirs() {
		if fileExists(filepath.Join(dir, desktopFileName)) {
			logging.Trace("ui: desktop file skip", "reason", "system entry exists", "dir", dir)
			return // a packaged install already handles notifications
		}
	}
	dest := filepath.Join(userAppDir(), desktopFileName)
	if existing, err := os.ReadFile(dest); err == nil {
		// An entry already exists. Leave it alone unless it's one we wrote (carries
		// our Comment) but predates the mailto registration — then refresh it so an
		// older self-install still becomes the default mail handler. A user-edited
		// entry (without our marker) is never touched.
		if !isStaleOwnEntry(existing) {
			logging.Trace("ui: desktop file skip", "reason", "user entry exists", "path", dest)
			return
		}
		slog.Info("ui: refreshing stale user desktop entry (adds mailto handler)", "path", dest)
		logging.Trace("ui: desktop file refresh stale own entry", "path", dest)
	}
	exe, err := os.Executable()
	if err != nil {
		slog.Debug("ui: locate executable for desktop entry", "err", err)
		return
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		slog.Debug("ui: create applications dir", "err", err)
		return
	}
	entry := strings.Replace(desktopEntry, "Exec=mailbox", "Exec="+exe, 1)
	if err := os.WriteFile(dest, []byte(entry), 0o644); err != nil {
		slog.Debug("ui: write desktop entry", "err", err)
		return
	}
	slog.Info("ui: installed user desktop entry so notifications resolve", "path", dest)
	logging.Trace("ui: desktop file installed", "path", dest, "exec", exe)
	// Rebuild the user mimeinfo cache so the mailto registration takes effect
	// (otherwise the app won't show under Default Apps → Mail until the next login
	// or a manual refresh). Best-effort; the binary may not exist on minimal hosts.
	if bin, err := exec.LookPath("update-desktop-database"); err == nil {
		if err := exec.Command(bin, filepath.Dir(dest)).Run(); err != nil {
			slog.Debug("ui: update-desktop-database", "err", err)
			logging.Trace("ui: update-desktop-database failed", "err", err)
		} else {
			logging.Trace("ui: ran update-desktop-database", "dir", filepath.Dir(dest))
		}
	} else {
		logging.Trace("ui: update-desktop-database not found")
	}
}

// isStaleOwnEntry reports whether an existing desktop entry is one we previously
// self-installed (it carries our Comment line) but lacks the mailto handler
// registration — i.e. it should be refreshed. A user-customised entry without
// our marker returns false so it is never overwritten.
func isStaleOwnEntry(content []byte) bool {
	s := string(content)
	return strings.Contains(s, "Comment=A native, fast Gmail client") &&
		!strings.Contains(s, "x-scheme-handler/mailto")
}

// userAppDir is the per-user applications directory (XDG_DATA_HOME aware).
func userAppDir() string {
	if x := os.Getenv("XDG_DATA_HOME"); x != "" {
		return filepath.Join(x, "applications")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".local", "share", "applications")
	}
	return filepath.Join(home, ".local", "share", "applications")
}

// systemAppDirs are the system applications directories (XDG_DATA_DIRS aware).
func systemAppDirs() []string {
	dirs := os.Getenv("XDG_DATA_DIRS")
	if dirs == "" {
		dirs = "/usr/local/share:/usr/share"
	}
	var out []string
	for _, d := range strings.Split(dirs, ":") {
		if d != "" {
			out = append(out, filepath.Join(d, "applications"))
		}
	}
	return out
}

// fileExists reports whether path exists (any stat error is treated as absent).
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
