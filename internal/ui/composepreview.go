package ui

import (
	"context"
	"fmt"
	"strings"

	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
	"github.com/jsnjack/mailbox/internal/ai"
	"github.com/jsnjack/mailbox/internal/dispatch"
)

// collectComposeAI never returns partial writing as a usable result.
func collectComposeAI(ctx context.Context, ch <-chan ai.Chunk) (string, error) {
	var result strings.Builder
	for {
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("generation cancelled: %w", ctx.Err())
		case chunk, ok := <-ch:
			if !ok {
				if ctx.Err() != nil {
					return "", fmt.Errorf("generation cancelled: %w", ctx.Err())
				}
				text := strings.TrimSpace(result.String())
				if text == "" {
					return "", fmt.Errorf("the model returned no text")
				}
				return text, nil
			}
			if chunk.Err != nil {
				return "", fmt.Errorf("generation failed: %w", chunk.Err)
			}
			result.WriteString(chunk.Text)
		}
	}
}

func replaceComposeText(buf *gtk.TextBuffer, text string) {
	buf.BeginUserAction()
	start, end := buf.Bounds()
	buf.Delete(start, end)
	buf.Insert(buf.StartIter(), text)
	buf.EndUserAction()
}

// previewComposeAI owns a request and applies only against its unchanged source.
func previewComposeAI(parent gtk.Widgetter, lifetime context.Context, buf *gtk.TextBuffer, title string, request func(context.Context) (string, error), done func(string)) *adw.Dialog {
	original := bodyText(buf)
	ctx, cancel := context.WithCancel(lifetime)
	dialog := adw.NewDialog()
	dialog.SetTitle(title)
	dialog.SetContentWidth(600)
	dialog.SetContentHeight(480)
	dialog.SetFollowsContentSize(false)
	dialog.ConnectClosed(cancel)
	header := adw.NewHeaderBar()
	header.SetShowStartTitleButtons(false)
	header.SetShowEndTitleButtons(false)
	cancelBtn := gtk.NewButtonWithLabel("Cancel")
	cancelBtn.ConnectClicked(func() { cancel(); dialog.Close() })
	header.PackStart(cancelBtn)
	apply := gtk.NewButtonWithLabel("Apply")
	apply.AddCSSClass("suggested-action")
	apply.SetSensitive(false)
	header.PackEnd(apply)
	view := gtk.NewTextView()
	view.SetEditable(false)
	view.SetCursorVisible(false)
	view.SetWrapMode(gtk.WrapWordChar)
	view.SetLeftMargin(16)
	view.SetRightMargin(16)
	view.SetTopMargin(16)
	view.SetBottomMargin(16)
	a11yLabel(view, "Proposed message")
	scroll := gtk.NewScrolledWindow()
	scroll.SetVExpand(true)
	scroll.SetChild(view)
	note := gtk.NewLabel("Generating preview… Your message is unchanged.")
	note.SetWrap(true)
	setMargins(note, 12, 12, 12, 12)
	box := gtk.NewBox(gtk.OrientationVertical, 0)
	box.Append(scroll)
	box.Append(note)
	toolbar := adw.NewToolbarView()
	toolbar.AddTopBar(header)
	toolbar.SetContent(box)
	dialog.SetChild(toolbar)
	var candidate string
	apply.ConnectClicked(func() {
		if ctx.Err() != nil || candidate == "" {
			return
		}
		if bodyText(buf) != original {
			apply.SetSensitive(false)
			note.SetText("Your message changed while this preview was open. Cancel and generate a new preview to keep your edits.")
			return
		}
		replaceComposeText(buf, candidate)
		cancel()
		dialog.Close()
	})
	dialog.Present(parent)
	go func() {
		text, err := request(ctx)
		dispatch.Main(func() {
			if ctx.Err() != nil {
				done("Cancelled")
				return
			}
			if err != nil {
				note.SetText(err.Error())
				done(doneErr(err))
				return
			}
			candidate = text
			view.Buffer().SetText(text)
			note.SetText("Review the proposed message. Apply can be undone with Ctrl+Z.")
			apply.SetSensitive(true)
			done("")
		})
	}()
	return dialog
}
