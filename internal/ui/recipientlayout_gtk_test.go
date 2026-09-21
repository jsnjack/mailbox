package ui

import (
	"fmt"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	"github.com/diamondburned/gotk4/pkg/gdk/v4"
	"github.com/diamondburned/gotk4/pkg/glib/v2"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
)

func TestRecipientLayoutGTK(t *testing.T) {
	if os.Getenv("MAILBOX_TEST_GTK") != "1" {
		t.Skip("requires MAILBOX_TEST_GTK=1 and a display")
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	gtk.Init()
	adw.Init()
	css := gtk.NewCSSProvider()
	css.LoadFromString(appCSS)
	gtk.StyleContextAddProviderForDisplay(gdk.DisplayGetDefault(), css, gtk.STYLE_PROVIDER_PRIORITY_APPLICATION)
	win := adw.NewWindow()
	win.SetDefaultSize(700, 600)
	defer win.Destroy()
	toolbar := adw.NewToolbarView()
	toolbar.AddTopBar(adw.NewHeaderBar())
	box := gtk.NewBox(gtk.OrientationVertical, 6)
	setMargins(box, 12, 12, 12, 12)
	fields := []*recipientField{newRecipientField("To", "first@example.com"), newRecipientField("Cc", "second@example.com"), newRecipientField("Bcc", "third@example.com")}
	for _, field := range fields {
		box.Append(field.box)
	}
	subject := gtk.NewEntry()
	subject.SetText("Compose layout regression")
	box.Append(subject)
	body := gtk.NewTextView()
	body.SetVExpand(true)
	scroll := gtk.NewScrolledWindow()
	scroll.SetVExpand(true)
	scroll.SetChild(body)
	box.Append(scroll)
	toolbar.SetContent(box)
	win.SetContent(toolbar)
	win.Present()
	main := glib.MainContextDefault()
	settle := func() {
		t.Helper()
		until := time.Now().Add(250 * time.Millisecond)
		for time.Now().Before(until) {
			for main.Pending() {
				main.Iteration(false)
			}
			time.Sleep(time.Millisecond)
		}
	}
	settle()
	for _, field := range fields {
		if field.box.Height() > 48 {
			t.Fatalf("single recipient field consumes %dpx", field.box.Height())
		}
	}
	if scroll.Height() < win.Height()/2 {
		t.Fatalf("message editor is only %d of %dpx", scroll.Height(), win.Height())
	}
	heights := []int{fields[0].box.Height(), fields[1].box.Height(), fields[2].box.Height()}
	var many []string
	for i := 0; i < 40; i++ {
		many = append(many, fmt.Sprintf("recipient%d@example.com", i))
	}
	for _, field := range fields {
		field.entry.SetText(strings.Join(many, ", "))
	}
	settle()
	for i, field := range fields {
		if field.box.Height() > heights[i]+16 || field.box.Height() > 48 {
			t.Fatalf("many recipients expanded %s field from %d to %dpx", []string{"To", "Cc", "Bcc"}[i], heights[i], field.box.Height())
		}
		if parsed, err := parseRecipients(field.text()); err != nil || len(parsed) != 40 {
			t.Fatal("overflow layout lost recipients")
		}
	}
	if scroll.Height() < win.Height()/2 {
		t.Fatalf("recipient overflow shrank message editor to %d of %dpx", scroll.Height(), win.Height())
	}
}
