package ui

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	"github.com/diamondburned/gotk4/pkg/gio/v2"
	"github.com/diamondburned/gotk4/pkg/glib/v2"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
	"github.com/jsnjack/mailbox/internal/model"
	"github.com/jsnjack/mailbox/internal/store"
)

func TestAccountSwitchGTK(t *testing.T) {
	if os.Getenv("MAILBOX_TEST_GTK") != "1" {
		t.Skip("requires MAILBOX_TEST_GTK=1 and a display")
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	for _, key := range []string{"XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_CACHE_HOME"} {
		t.Setenv(key, t.TempDir())
	}
	gtk.Init()
	adw.Init()
	db, err := store.Open(filepath.Join(t.TempDir(), "mail.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	}()
	var accounts []AccountInfo
	for _, email := range []string{"home@example.com", "work@example.com"} {
		id, err := db.UpsertAccount(context.Background(), model.Account{Email: email})
		if err != nil {
			t.Fatal(err)
		}
		accounts = append(accounts, AccountInfo{ID: id, Email: email})
	}
	app := adw.NewApplication("com.jsnjack.mailbox.accounttest", gio.ApplicationNonUnique)
	if err := app.Register(context.Background()); err != nil {
		t.Fatal(err)
	}
	w := newWindow(app, Deps{Store: db, Accounts: accounts})
	defer w.win.Destroy()
	for _, tc := range []struct {
		name  string
		index int
		click bool
	}{
		{"sidebar to work", 1, true},
		{"programmatic to home", 0, false},
		{"programmatic to work", 1, false},
		{"same account", 1, false},
		{"sidebar back to home", 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.click {
				w.accountBox.SelectRow(w.accountBox.RowAtIndex(tc.index))
			} else {
				w.setActiveAccount(accounts[tc.index])
			}
			if w.activeID != accounts[tc.index].ID {
				t.Fatalf("active account = %d, want %d", w.activeID, accounts[tc.index].ID)
			}
			row := w.accountBox.SelectedRow()
			if row == nil || row.Index() != tc.index {
				t.Fatalf("sidebar does not highlight account %d", accounts[tc.index].ID)
			}
			deadline := time.Now().Add(3 * time.Second)
			main := glib.MainContextDefault()
			for w.threadLoad.cancel != nil && time.Now().Before(deadline) {
				for main.Pending() {
					main.Iteration(false)
				}
				time.Sleep(time.Millisecond)
			}
			if w.threadPage.key.accountID != w.activeID {
				t.Fatalf("message list account = %d, active = %d", w.threadPage.key.accountID, w.activeID)
			}
		})
	}
}
