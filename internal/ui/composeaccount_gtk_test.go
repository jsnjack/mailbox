package ui

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	"github.com/diamondburned/gotk4/pkg/glib/v2"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
	"github.com/jsnjack/mailbox/internal/config"
	"github.com/jsnjack/mailbox/internal/model"
	"github.com/jsnjack/mailbox/internal/store"
)

func TestComposeFromGTK(t *testing.T) {
	if os.Getenv("MAILBOX_TEST_GTK") != "1" {
		t.Skip("requires MAILBOX_TEST_GTK=1 and a display")
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	gtk.Init()
	adw.Init()
	db, err := store.Open(filepath.Join(t.TempDir(), "mail.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	}()
	if err := config.SaveAccountSignature("first@example.com", "Work signature"); err != nil {
		t.Fatal(err)
	}
	if err := config.SaveAccountSignature("second@example.com", "Personal signature"); err != nil {
		t.Fatal(err)
	}
	parent := adw.NewWindow()
	overlay := adw.NewToastOverlay()
	overlay.SetChild(gtk.NewBox(gtk.OrientationVertical, 0))
	parent.SetContent(overlay)
	parent.SetDefaultSize(700, 500)
	parent.Present()
	defer parent.Destroy()
	var saved, deleted atomic.Int64
	var savedBody atomic.Value
	w := &window{toastOverlay: overlay, activeID: 1, activeEmail: "first@example.com", deps: Deps{
		Store:    db,
		Accounts: []AccountInfo{{ID: 1, Email: "first@example.com"}, {ID: 2, Email: "second@example.com"}},
		Send:     func(context.Context, int64, model.OutgoingMessage) error { return nil },
		SaveDraft: func(_ context.Context, account int64, msg model.OutgoingMessage) (string, error) {
			if account == 2 && (msg.From != "second@example.com" || (msg.LocalDraftID != "" && msg.LocalDraftID != "second-draft")) {
				t.Errorf("cross-account payload: %#v", msg)
			}
			savedBody.Store(msg.Body)
			saved.Store(account)
			if account == 1 {
				return "first-draft", nil
			}
			return "second-draft", nil
		},
		DeleteDraft: func(_ context.Context, account int64, id string) error {
			if (account == 1 && id != "first-draft") || (account == 2 && id != "second-draft") {
				t.Errorf("wrong deletion: %d/%s", account, id)
			}
			deleted.Store(account)
			return nil
		},
	}}
	w.openComposeOpts(model.OutgoingMessage{To: "recipient@example.com", Subject: "test"}, "", "From regression", composeOpts{})
	var compose *adw.Window
	for _, widget := range gtk.WindowListToplevels() {
		if win, ok := gtk.BaseWidget(widget).Cast().(*adw.Window); ok && win.Title() == "From regression" {
			compose = win
		}
	}
	if compose == nil {
		t.Fatal("compose missing")
	}
	defer compose.Destroy()
	var from *gtk.DropDown
	var editor *gtk.TextView
	var discard, undo *gtk.Button
	var visit func(gtk.Widgetter)
	visit = func(root gtk.Widgetter) {
		widget := gtk.BaseWidget(root)
		switch v := widget.Cast().(type) {
		case *gtk.DropDown:
			from = v
		case *gtk.TextView:
			editor = v
		case *gtk.Button:
			if v.Label() == "Discard" {
				discard = v
			}
			if v.Label() == "Undo" {
				undo = v
			}
		}
		for child := widget.FirstChild(); child != nil; child = gtk.BaseWidget(child).NextSibling() {
			visit(child)
		}
	}
	visit(compose)
	if from == nil || editor == nil {
		t.Fatal("compose fields missing")
	}
	main := glib.MainContextDefault()
	wait := func(ready func() bool) {
		t.Helper()
		until := time.Now().Add(5 * time.Second)
		for time.Now().Before(until) {
			for main.Pending() {
				main.Iteration(false)
			}
			if ready() {
				return
			}
			time.Sleep(time.Millisecond)
		}
		t.Fatal("timed out waiting for compose")
	}
	editor.Buffer().SetText("Keep this message when changing sender.\n\nWork signature")
	wait(func() bool { return saved.Load() == 1 })
	// Drain the save's idle callback too: this is where From used to lock.
	until := time.Now().Add(100 * time.Millisecond)
	wait(func() bool { return time.Now().After(until) })
	if !from.Sensitive() {
		t.Fatal("autosave disabled From")
	}
	from.SetSelected(1)
	wait(func() bool { return deleted.Load() == 1 && from.Sensitive() })
	if saved.Load() != 2 || from.Selected() != 1 {
		t.Fatal("sender switch did not persist")
	}
	if bodyText(editor.Buffer()) != "Keep this message when changing sender.\n\nPersonal signature" {
		t.Fatal("sender switch changed body")
	}
	compose.Close()
	wait(func() bool { return !compose.Visible() })

	w.openComposeOpts(model.OutgoingMessage{To: "recipient@example.com", Subject: "test", Body: "Saved draft\n\nPersonal signature", LocalDraftID: "second-draft"}, "", "Resumed sender regression", composeOpts{fromAccountID: 2})
	for _, widget := range gtk.WindowListToplevels() {
		if win, ok := gtk.BaseWidget(widget).Cast().(*adw.Window); ok && win.Title() == "Resumed sender regression" {
			compose = win
		}
	}
	defer compose.Destroy()
	visit(compose)
	if !from.Sensitive() {
		t.Fatal("resumed draft sender is locked")
	}
	from.SetSelected(0)
	wait(func() bool { return deleted.Load() == 2 && from.Sensitive() })
	if bodyText(editor.Buffer()) != "Saved draft\n\nWork signature" {
		t.Fatal("resumed signature did not follow sender")
	}
	editor.Buffer().SetText("Save these latest edits on close")
	compose.Close()
	wait(func() bool { return !compose.Visible() })
	if savedBody.Load() != "Save these latest edits on close" {
		t.Fatal("closing lost the latest edits")
	}
	w.openComposeOpts(model.OutgoingMessage{To: "recipient@example.com", Body: "Discard and restore this exact writing"}, "", "Discard regression", composeOpts{fromAccountID: 1})
	for _, widget := range gtk.WindowListToplevels() {
		if win, ok := gtk.BaseWidget(widget).Cast().(*adw.Window); ok && win.Title() == "Discard regression" {
			compose = win
		}
	}
	visit(compose)
	if discard == nil {
		t.Fatal("new compose has no persistent Discard action")
	}
	discard.Emit("clicked")
	wait(func() bool { return !compose.Visible() })
	wait(func() bool { visit(overlay); return undo != nil })
	undo.Emit("clicked")
	var restored *adw.Window
	wait(func() bool {
		for _, widget := range gtk.WindowListToplevels() {
			if win, ok := gtk.BaseWidget(widget).Cast().(*adw.Window); ok && win.Title() == "Restored draft" {
				restored = win
			}
		}
		return restored != nil
	})
	visit(restored)
	if bodyText(editor.Buffer()) != "Discard and restore this exact writing" {
		t.Fatal("Undo changed the discarded message")
	}
	restored.Close()
	wait(func() bool { return !restored.Visible() })

}
