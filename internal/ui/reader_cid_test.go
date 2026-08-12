package ui

import (
	"testing"

	"github.com/jsnjack/mailbox/internal/model"
)

// A forwarded attachment arrives with a Content-ID that nothing points at:
// Gmail stamps one on every part when a message is forwarded, so only a
// reference from the body makes a part inline.
func TestInlineAttachmentNeedsAReference(t *testing.T) {
	body := `<div><img src="cid:logo123"> text <img src='cid:%3Cphoto%40host%3E'></div>`
	refs := model.ReferencedCIDs(body)

	inlineLogo := model.Attachment{Filename: "logo.png", MimeType: "image/png", ContentID: "logo123"}
	inlinePhoto := model.Attachment{Filename: "p.jpg", MimeType: "image/jpeg", ContentID: "photo@host"}
	forwardedPDF := model.Attachment{Filename: "IND_bijlage.pdf", MimeType: "application/pdf", ContentID: "19ff5632aced085071a1"}
	plainDoc := model.Attachment{Filename: "notes.txt", MimeType: "text/plain"}

	for _, c := range []struct {
		name string
		att  model.Attachment
		want bool
	}{
		{"referenced inline image", inlineLogo, true},
		{"referenced with escaped brackets", inlinePhoto, true},
		{"forwarded attachment Gmail stamped a cid on", forwardedPDF, false},
		{"ordinary attachment", plainDoc, false},
	} {
		if got := model.IsInlineAttachment(c.att, refs); got != c.want {
			t.Errorf("%s: IsInlineAttachment = %v, want %v", c.name, got, c.want)
		}
	}
}
