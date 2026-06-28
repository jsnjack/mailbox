package imapbackend

import "testing"

func TestXOAUTH2InitialResponse(t *testing.T) {
	c := xoauth2Client("user@example.com", "tok123")
	mech, ir, err := c.Start()
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if mech != "XOAUTH2" {
		t.Errorf("mechanism = %q, want XOAUTH2", mech)
	}
	want := "user=user@example.com\x01auth=Bearer tok123\x01\x01"
	if string(ir) != want {
		t.Errorf("initial response = %q, want %q", ir, want)
	}
	// A server challenge means the token was rejected → abort, not respond.
	if _, err := c.Next([]byte("eyJ...")); err == nil {
		t.Error("Next should error on a challenge (auth failure)")
	}
}
