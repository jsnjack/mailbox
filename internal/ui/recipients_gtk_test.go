package ui

import (
	"os"
	"runtime"
	"testing"

	"github.com/diamondburned/gotk4/pkg/gtk/v4"
)

func TestRecipientFieldsGTK(t *testing.T) {
	if os.Getenv("MAILBOX_TEST_GTK") != "1" {
		t.Skip("requires MAILBOX_TEST_GTK=1 and a display")
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	gtk.Init()
	initial := `"Doe, Jane" <jane@example.com>, second@example.com`
	f := newRecipientField("To", initial)
	if f.entry.Text() != initial || f.text() != initial {
		t.Fatal("initial addresses changed")
	}
	lastChangedText := ""
	f.changed = func() { lastChangedText = f.text() }
	f.entry.SetText("invalid address,")
	if !f.errorLabel.Visible() || f.text() != "invalid address" {
		t.Fatal("invalid recipient was hidden or discarded")
	}
	f.entry.SetText("first@example.com, second@example.com, ")
	if f.errorLabel.Visible() || f.text() != "first@example.com, second@example.com" {
		t.Fatal("valid recipients failed validation")
	}
	f.entry.SetText("second@example.com")
	if f.text() != "second@example.com" {
		t.Fatal("editing did not update outgoing recipients")
	}
	if lastChangedText != "second@example.com" {
		t.Fatalf("autosave received stale recipients: %q", lastChangedText)
	}
	var visit func(gtk.Widgetter)
	visit = func(root gtk.Widgetter) {
		widget := gtk.BaseWidget(root)
		switch widget.Cast().(type) {
		case *gtk.Button, *gtk.FlowBox, *gtk.ScrolledWindow:
			t.Fatal("recipient field still contains chip controls")
		}
		for child := widget.FirstChild(); child != nil; child = gtk.BaseWidget(child).NextSibling() {
			visit(child)
		}
	}
	visit(f.box)
}
