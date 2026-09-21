package ui

import (
	"os"
	"runtime"
	"testing"

	"github.com/diamondburned/gotk4/pkg/gtk/v4"
)

func TestSearchOptionsGTK(t *testing.T) {
	if os.Getenv("MAILBOX_TEST_GTK") != "1" {
		t.Skip("requires MAILBOX_TEST_GTK=1 and a display")
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	gtk.Init()
	for count := 0; count <= 2; count++ {
		accounts := []AccountInfo{{ID: 1, Email: "first@example.com"}, {ID: 2, Email: "second@example.com"}}
		w := &window{activeID: 1, deps: Deps{Accounts: accounts[:count]}, searchEntry: gtk.NewSearchEntry()}
		options := w.buildSearchOptions()
		if options == nil || w.searchAccount == nil || w.searchScope == nil {
			t.Fatalf("%d accounts: search controls missing", count)
		}
		if count > 0 && w.searchAccount.Selected() != 0 {
			t.Fatalf("%d accounts: wrong initial selection", count)
		}
		if count == 2 {
			w.activeID = 2
			w.syncSearchOptions()
			if w.searchAccount.Selected() != 1 {
				t.Fatal("account selector did not follow active account")
			}
		}
		w.serverSearch = true
		w.syncSearchOptions()
		if w.searchScope.Selected() != 1 {
			t.Fatal("scope did not follow server search")
		}
	}
}
