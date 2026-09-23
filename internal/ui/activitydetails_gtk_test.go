package ui

import (
	"os"
	"runtime"
	"strings"
	"testing"

	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
)

func TestActivityDetailsGTK(t *testing.T) {
	if os.Getenv("MAILBOX_TEST_GTK") != "1" {
		t.Skip("requires MAILBOX_TEST_GTK=1 and a display")
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	gtk.Init()
	adw.Init()
	t.Run("long error retains trailing quota URL", func(t *testing.T) {
		note := "error: " + strings.Repeat("Google quota exceeded. ", 500) + "\nhttps://console.cloud.google.com/iam-admin/quotas?project=example"
		dialog := newActivityDetails("Checking mail", note)
		var view *gtk.TextView
		var scroll *gtk.ScrolledWindow
		var copyButton *gtk.Button
		var walk func(*gtk.Widget)
		walk = func(widget *gtk.Widget) {
			switch child := widget.Cast().(type) {
			case *gtk.TextView:
				view = child
			case *gtk.ScrolledWindow:
				scroll = child
			case *gtk.Button:
				if child.Label() == "Copy" {
					copyButton = child
				}
			}
			for child := widget.FirstChild(); child != nil; child = gtk.BaseWidget(child).NextSibling() {
				walk(gtk.BaseWidget(child))
			}
		}
		walk(gtk.BaseWidget(dialog.Child()))
		if view == nil || scroll == nil || copyButton == nil {
			t.Fatal("details must provide scrollable text and a Copy button")
		}
		if got := bodyText(view.Buffer()); got != note {
			t.Fatal("details truncated or changed the error message")
		}
		if view.WrapMode() != gtk.WrapWordChar || view.Editable() {
			t.Fatal("details must wrap long URLs and prevent editing")
		}
	})
}
