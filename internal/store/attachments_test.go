package store

import (
	"context"
	"testing"

	"github.com/jsnjack/mailbox/internal/model"
)

func TestAttachmentsRoundTrip(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	acc := seedAccount(t, s)

	rowID, err := s.UpsertMessage(ctx, model.Message{AccountID: acc, GmailID: "m1", ThreadID: "t1", Subject: "with files"})
	if err != nil {
		t.Fatalf("upsert message: %v", err)
	}

	atts := []model.Attachment{
		{GmailAttID: "att-1", Filename: "report.pdf", MimeType: "application/pdf", SizeBytes: 1024},
		{GmailAttID: "att-2", Filename: "photo.jpg", MimeType: "image/jpeg", SizeBytes: 2048},
		// An inline image (no filename) referenced by a cid: URL in the body.
		{GmailAttID: "att-3", MimeType: "image/png", SizeBytes: 512, ContentID: "logo123"},
	}
	if err := s.ReplaceAttachments(ctx, rowID, atts); err != nil {
		t.Fatalf("ReplaceAttachments: %v", err)
	}

	got, err := s.ListAttachments(ctx, rowID)
	if err != nil {
		t.Fatalf("ListAttachments: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d attachments, want 3", len(got))
	}
	if got[0].Filename != "report.pdf" || got[0].SizeBytes != 1024 {
		t.Fatalf("unexpected first attachment: %+v", got[0])
	}
	// The Content-ID round-trips so a cid: body image can be resolved.
	if got[2].ContentID != "logo123" {
		t.Fatalf("ContentID not stored: %+v", got[2])
	}
	if got[0].DiskPath != "" {
		t.Fatal("expected empty disk path before download")
	}

	// Mark downloaded and verify it sticks.
	if err := s.SetAttachmentDownloaded(ctx, got[0].ID, "deadbeef", "/cache/de/deadbeef.pdf"); err != nil {
		t.Fatalf("SetAttachmentDownloaded: %v", err)
	}
	one, err := s.GetAttachmentByID(ctx, got[0].ID)
	if err != nil {
		t.Fatalf("GetAttachmentByID: %v", err)
	}
	if one.SHA256 != "deadbeef" || one.DiskPath != "/cache/de/deadbeef.pdf" {
		t.Fatalf("download fields not persisted: %+v", one)
	}

	// ReplaceAttachments replaces, not appends.
	if err := s.ReplaceAttachments(ctx, rowID, atts[:1]); err != nil {
		t.Fatalf("re-replace: %v", err)
	}
	if got, _ := s.ListAttachments(ctx, rowID); len(got) != 1 {
		t.Fatalf("after replace got %d, want 1", len(got))
	}
}
