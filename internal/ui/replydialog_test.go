package ui

import (
	"context"
	"encoding/json"
	"os"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	"github.com/diamondburned/gotk4/pkg/glib/v2"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
	"github.com/jsnjack/mailbox/internal/ai"
)

type replyDialogProvider struct {
	calls   atomic.Int32
	results chan []string
}

func (p *replyDialogProvider) Name() string { return "reply-dialog-test" }
func (p *replyDialogProvider) Stream(ctx context.Context, _ string, _ []ai.Msg) (<-chan ai.Chunk, error) {
	p.calls.Add(1)
	ch := make(chan ai.Chunk, 1)
	go func() {
		defer close(ch)
		select {
		case replies := <-p.results:
			data, err := json.Marshal(replies)
			ch <- ai.Chunk{Text: string(data), Err: err}
		case <-ctx.Done():
			ch <- ai.Chunk{Err: ctx.Err()}
		}
	}()
	return ch, nil
}

// Run under Xvfb with MAILBOX_TEST_GTK=1. Ordinary headless checks skip native
// widgets; this exercises allocations and asynchronous state on the GTK thread.
func TestReplyDialogGTK(t *testing.T) {
	if os.Getenv("MAILBOX_TEST_GTK") != "1" {
		t.Skip("requires MAILBOX_TEST_GTK=1 and a display")
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	gtk.Init()
	adw.Init()
	parent := adw.NewWindow()
	parent.SetDefaultSize(900, 700)
	parent.SetContent(gtk.NewBox(gtk.OrientationVertical, 0))
	parent.Present()
	defer parent.Destroy()
	main := glib.MainContextDefault()
	pump := func() {
		for main.Pending() {
			main.Iteration(false)
		}
	}
	wait := func(ready func() bool) {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
		for !ready() && time.Now().Before(deadline) {
			pump()
			time.Sleep(time.Millisecond)
		}
		if !ready() {
			t.Fatal("timed out waiting for dialog")
		}
		pump()
	}
	var visit func(gtk.Widgetter, func(*gtk.Widget))
	visit = func(root gtk.Widgetter, fn func(*gtk.Widget)) {
		widget := gtk.BaseWidget(root)
		fn(widget)
		for child := widget.FirstChild(); child != nil; child = gtk.BaseWidget(child).NextSibling() {
			visit(child, fn)
		}
	}
	button := func(d *adw.Dialog, label string) *gtk.Button {
		t.Helper()
		var result *gtk.Button
		visit(d.Child(), func(widget *gtk.Widget) {
			if b, ok := widget.Cast().(*gtk.Button); ok && b.Label() == label {
				result = b
			}
		})
		if result == nil {
			t.Fatalf("button %q missing", label)
		}
		return result
	}
	editor := func(d *adw.Dialog) *gtk.TextView {
		var entry *gtk.TextView
		visit(d.Child(), func(widget *gtk.Widget) {
			switch v := widget.Cast().(type) {
			case *gtk.StackSwitcher, *gtk.FlowBox:
				t.Fatal("dialog still contains tabs or presets")
			case *gtk.TextView:
				entry = v
			}
		})
		return entry
	}
	choices := func(d *adw.Dialog) []*gtk.CheckButton {
		var rows []*gtk.CheckButton
		visit(d.Child(), func(widget *gtk.Widget) {
			if row, ok := widget.Cast().(*gtk.CheckButton); ok {
				rows = append(rows, row)
			}
		})
		return rows
	}
	provider := &replyDialogProvider{results: make(chan []string, 1)}
	w := &window{deps: Deps{Assistant: ai.NewAssistant(provider)}, aiSmartReplies: true}
	instruction, chosen := "", ""
	selections := 0
	open := func(text string, draft func(string)) *adw.Dialog {
		return w.askAIIntent(parent, true, text, draft, func(reply string) { chosen = reply; selections++ })
	}
	draft := func(text string) { instruction = text }

	d := open("original conversation", draft)
	entry := editor(d)
	wait(func() bool { return d.Width() > 0 && d.Height() > 0 && provider.calls.Load() == 1 })
	width, height := d.Width(), d.Height()
	if button(d, "Create draft").Sensitive() {
		t.Fatal("empty instruction enabled Create draft")
	}
	entry.Buffer().SetText("Decline and offer Tuesday.")
	provider.results <- []string{"Thanks for the update.", strings.Repeat("A much longer suggestion that wraps over several lines. ", 30), "Let's discuss this tomorrow."}
	wait(func() bool { return len(choices(d)) == 3 })
	deadline := time.Now().Add(100 * time.Millisecond)
	for time.Now().Before(deadline) {
		pump()
		time.Sleep(time.Millisecond)
	}
	if d.Width() != width || d.Height() != height {
		t.Fatalf("dialog resized from %dx%d to %dx%d", width, height, d.Width(), d.Height())
	}
	if bodyText(entry.Buffer()) != "Decline and offer Tuesday." {
		t.Fatal("arriving suggestions replaced the instruction")
	}
	closed := false
	d.ConnectClosed(func() { closed = true })
	row := choices(d)[0]
	row.SetActive(true)
	if chosen != "Thanks for the update." || selections != 1 {
		t.Fatalf("selection did not immediately open reply: %q, calls=%d", chosen, selections)
	}
	row.SetActive(false)
	row.SetActive(true)
	if selections != 1 {
		t.Fatal("closing dialog opened a second compose")
	}
	wait(func() bool { return closed })

	d = open("original conversation", draft)
	entry = editor(d)
	if len(choices(d)) != 3 || provider.calls.Load() != 1 {
		t.Fatal("reopening unchanged conversation did not reuse suggestions")
	}
	button(d, "Try again").Emit("clicked")
	wait(func() bool { return provider.calls.Load() == 2 })
	provider.results <- []string{"   "}
	wait(func() bool { return button(d, "Try again").Sensitive() })
	if len(choices(d)) != 0 || button(d, "Create draft").Sensitive() {
		t.Fatal("blank result became selectable")
	}
	entry.Buffer().SetText("Offer Wednesday instead.")
	button(d, "Create draft").Emit("clicked")
	if instruction != "Offer Wednesday instead." {
		t.Fatalf("instruction = %q", instruction)
	}
	pump()

	d = open("conversation with a new message", nil)
	wait(func() bool { return provider.calls.Load() == 3 })
	if editor(d) != nil {
		t.Fatal("suggestions-only dialog has an instruction editor")
	}
	if button(d, "Create draft").Visible() {
		t.Fatal("suggestions-only dialog exposes an unused confirmation button")
	}
	button(d, "Cancel").Emit("clicked")
	pump()
}
