package ui

import (
	"os"
	"runtime"
	"strings"
	"testing"

	"github.com/diamondburned/gotk4/pkg/gtk/v4"
)

func TestRecipientChipsGTK(t *testing.T) {
	if os.Getenv("MAILBOX_TEST_GTK") != "1" {
		t.Skip("requires MAILBOX_TEST_GTK=1 and a display")
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	gtk.Init()
	f := newRecipientField("To", `"Doe, Jane" <jane@example.com>, second@example.com`)
	if len(f.addresses) != 2 || f.entry.Text() != "" {
		t.Fatalf("initial recipients = %v; input %q", f.addresses, f.entry.Text())
	}
	changes := 0
	f.changed = func() { changes++ }
	f.entry.SetText("invalid address,")
	if !f.errorLabel.Visible() || !strings.Contains(f.text(), "invalid address") {
		t.Fatal("invalid recipient was hidden or discarded")
	}
	f.entry.SetText("third@example.com, ")
	if len(f.addresses) != 3 || f.errorLabel.Visible() {
		t.Fatal("valid trailing-comma recipient did not become a chip")
	}
	var visit func(gtk.Widgetter, func(*gtk.Button))
	visit = func(root gtk.Widgetter, fn func(*gtk.Button)) {
		w := gtk.BaseWidget(root)
		if b, ok := w.Cast().(*gtk.Button); ok {
			fn(b)
		}
		for child := w.FirstChild(); child != nil; child = gtk.BaseWidget(child).NextSibling() {
			visit(child, fn)
		}
	}
	var remove *gtk.Button
	visit(f.box, func(b *gtk.Button) {
		if b.TooltipText() == "Remove third@example.com" {
			remove = b
		}
	})
	if remove == nil {
		t.Fatal("chip has no remove control")
	}
	remove.Emit("clicked")
	if len(f.addresses) != 2 || strings.Contains(f.text(), "third@example.com") {
		t.Fatal("chip removal did not update outgoing recipients")
	}
	var edit *gtk.Button
	visit(f.box, func(b *gtk.Button) {
		if b.TooltipText() == "Edit second@example.com" {
			edit = b
		}
	})
	if edit == nil {
		t.Fatal("chip has no edit control")
	}
	edit.Emit("clicked")
	if f.entry.Text() != "second@example.com" || len(f.addresses) != 1 {
		t.Fatal("editing a chip lost or duplicated the recipient")
	}
	if changes == 0 {
		t.Fatal("recipient changes did not notify autosave")
	}
}
