package ui

import (
	"context"
	"errors"
	"os"
	"runtime"
	"testing"
	"time"

	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	"github.com/diamondburned/gotk4/pkg/glib/v2"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
)

func TestComposePreviewGTK(t *testing.T) {
	if os.Getenv("MAILBOX_TEST_GTK") != "1" {
		t.Skip("requires MAILBOX_TEST_GTK=1 and a display")
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	gtk.Init()
	adw.Init()
	parent := adw.NewWindow()
	parent.SetContent(gtk.NewBox(gtk.OrientationVertical, 0))
	parent.SetDefaultSize(800, 600)
	parent.Present()
	defer parent.Destroy()
	main := glib.MainContextDefault()
	wait := func(t *testing.T, ready func() bool) {
		t.Helper()
		until := time.Now().Add(3 * time.Second)
		for time.Now().Before(until) {
			for main.Pending() {
				main.Iteration(false)
			}
			if ready() {
				return
			}
			time.Sleep(time.Millisecond)
		}
		t.Fatal("preview timed out")
	}
	var find func(gtk.Widgetter, string) *gtk.Button
	find = func(root gtk.Widgetter, label string) *gtk.Button {
		widget := gtk.BaseWidget(root)
		if b, ok := widget.Cast().(*gtk.Button); ok && b.Label() == label {
			return b
		}
		for child := widget.FirstChild(); child != nil; child = gtk.BaseWidget(child).NextSibling() {
			if b := find(child, label); b != nil {
				return b
			}
		}
		return nil
	}
	for _, tc := range []struct {
		name                  string
		failure, edit, cancel bool
	}{{name: "apply and undo"}, {name: "cancel", cancel: true}, {name: "failure", failure: true}, {name: "intervening edit", edit: true}} {
		t.Run(tc.name, func(t *testing.T) {
			buf := gtk.NewTextBuffer(nil)
			buf.SetText("Original writing")
			buf.SetEnableUndo(true)
			completed := false
			release := make(chan struct{})
			d := previewComposeAI(parent, context.Background(), buf, "Preview test", func(ctx context.Context) (string, error) {
				select {
				case <-release:
				case <-ctx.Done():
					return "", ctx.Err()
				}
				if tc.failure {
					return "", errors.New("interrupted generation")
				}
				return "Proposed writing", nil
			}, func(string) { completed = true })
			apply, cancel := find(d.Child(), "Apply"), find(d.Child(), "Cancel")
			if apply == nil || cancel == nil {
				t.Fatal("preview controls missing")
			}
			if apply.Sensitive() || bodyText(buf) != "Original writing" {
				t.Fatal("generation changed source")
			}
			if tc.edit {
				buf.InsertAtCursor(" edited")
			}
			original := bodyText(buf)
			close(release)
			wait(t, func() bool { return completed })
			if bodyText(buf) != original {
				t.Fatal("preview changed source without approval")
			}
			if tc.failure {
				if apply.Sensitive() {
					t.Fatal("failed result is applicable")
				}
				cancel.Emit("clicked")
			} else if tc.cancel {
				cancel.Emit("clicked")
			} else {
				apply.Emit("clicked")
				wait(t, func() bool { return bodyText(buf) == "Proposed writing" || !apply.Sensitive() })
				if tc.edit {
					if bodyText(buf) != original {
						t.Fatal("new edits overwritten")
					}
					cancel.Emit("clicked")
				} else {
					if !buf.CanUndo() {
						t.Fatal("Apply cannot be undone")
					}
					buf.Undo()
					if bodyText(buf) != "Original writing" {
						t.Fatalf("one undo did not restore source: %q", bodyText(buf))
					}
				}
			}
			d.Close()
			wait(t, func() bool { return completed })
		})
	}
}
