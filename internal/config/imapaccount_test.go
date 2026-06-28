package config

import "testing"

func TestIMAPAccountRoundTrip(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	if all, err := LoadIMAPAccounts(); err != nil || len(all) != 0 {
		t.Fatalf("fresh load: %v, %d entries", err, len(all))
	}

	a := IMAPAccount{
		Email: "me@fastmail.com", Username: "me@fastmail.com",
		IMAPHost: "imap.fastmail.com", IMAPPort: 993, IMAPSecurity: "tls",
		SMTPHost: "smtp.fastmail.com", SMTPPort: 465, SMTPSecurity: "tls",
		Auth: AuthPassword,
	}
	if err := SaveIMAPAccount(a); err != nil {
		t.Fatalf("SaveIMAPAccount: %v", err)
	}
	got, ok, err := LoadIMAPAccount("me@fastmail.com")
	if err != nil || !ok {
		t.Fatalf("LoadIMAPAccount: ok=%v err=%v", ok, err)
	}
	if got != a {
		t.Fatalf("round-trip mismatch:\n got %+v\nwant %+v", got, a)
	}

	if err := DeleteIMAPAccount("me@fastmail.com"); err != nil {
		t.Fatalf("DeleteIMAPAccount: %v", err)
	}
	if _, ok, _ := LoadIMAPAccount("me@fastmail.com"); ok {
		t.Fatal("account still present after delete")
	}
}

func TestPresets(t *testing.T) {
	if _, ok := PresetByID("gmail"); !ok {
		t.Error("gmail preset missing")
	}
	if _, ok := PresetByID("nope"); ok {
		t.Error("unknown preset should not resolve")
	}
	for _, p := range Presets {
		if p.ID == "" || p.Name == "" {
			t.Errorf("preset with empty id/name: %+v", p)
		}
		// Every non-"other" preset prefills its servers.
		if p.ID != "other" && p.Auth != AuthGmailREST && p.IMAPHost == "" {
			t.Errorf("preset %q missing IMAP host", p.ID)
		}
	}
}
