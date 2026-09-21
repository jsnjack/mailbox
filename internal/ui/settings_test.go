package ui

import (
	"context"
	"testing"
	"time"

	"github.com/jsnjack/mailbox/internal/ai"
	"github.com/jsnjack/mailbox/internal/model"
)

func TestSignInAgePhrase(t *testing.T) {
	now := time.Date(2026, time.July, 17, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		when time.Time
		want string
	}{
		{"same day", now.Add(-3 * time.Hour), "today"},
		{"one day", now.Add(-30 * time.Hour), "yesterday"},
		{"a week", now.Add(-7 * 24 * time.Hour), "7 days ago"},
		{"future clock skew", now.Add(2 * time.Hour), "today"},
	}
	for _, c := range cases {
		if got := signInAgePhrase(c.when, now); got != c.want {
			t.Errorf("%s: signInAgePhrase = %q, want %q", c.name, got, c.want)
		}
	}
}

// TestAILanguages guards the table the Language row is built from.
// The row preselects by matching the stored pref against these values, so a
// duplicate or a missing default would leave the dialog showing a language the
// app is not using.
func TestAILanguages(t *testing.T) {
	if len(aiLanguages) < 2 {
		t.Fatalf("aiLanguages = %d entries, want the default plus at least one choice", len(aiLanguages))
	}
	// The default (English) is the pref's zero value and leads the list.
	if aiLanguages[0].label != "English" || aiLanguages[0].value != "" {
		t.Fatalf("first entry = %+v, want {English, \"\"}", aiLanguages[0])
	}
	seen := map[string]bool{}
	labels := map[string]bool{}
	for _, l := range aiLanguages {
		if l.label == "" {
			t.Fatalf("entry %+v has no label", l)
		}
		if seen[l.value] {
			t.Fatalf("duplicate value %q — the row would preselect the wrong entry", l.value)
		}
		if labels[l.label] {
			t.Fatalf("duplicate label %q", l.label)
		}
		seen[l.value], labels[l.label] = true, true
	}
	// "Same as the email" must carry the sentinel the ai package understands,
	// not a language name a model would try to write in.
	if !seen["match"] {
		t.Fatalf("aiLanguages offers no %q entry", "match")
	}
}

// TestTranslateTarget covers the one place the AI language cannot be taken
// literally: "Same as the email" has no meaning for a translation (a mail into
// its own language is a no-op), so Translate falls back to the default.
func TestTranslateTarget(t *testing.T) {
	for _, tt := range []struct {
		name, lang, want string
	}{
		{"default", "", ai.DefaultLanguage},
		{"chosen language", "Portuguese", "Portuguese"},
		{"same as the email falls back", ai.LanguageMatch, ai.DefaultLanguage},
	} {
		asst := ai.NewAssistant(nil)
		asst.SetLanguage(tt.lang)
		w := &window{deps: Deps{Assistant: asst}}
		if got := w.translateTarget(); got != tt.want {
			t.Errorf("%s: translateTarget = %q, want %q", tt.name, got, tt.want)
		}
	}
	// No assistant at all (AI unconfigured): still a usable answer, never "".
	w := &window{}
	if got := w.translateTarget(); got != ai.DefaultLanguage {
		t.Errorf("no assistant: translateTarget = %q, want %q", got, ai.DefaultLanguage)
	}
}

func TestLanguageChangeTranslationCache(t *testing.T) {
	m := model.Message{AccountID: 1, GmailID: "m1"}
	other := model.Message{AccountID: 2, GmailID: "m1"}
	w := &window{translationCache: map[translationCacheKey]string{
		{cacheKey(1, "m1"), "English"}: "English text",
	}}
	for _, tt := range []struct {
		name, lang string
		msg        model.Message
		want       int
	}{
		{"reuse English", "English", m, 0},
		{"new language", "French", m, 1},
		{"other account", "English", other, 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := w.messagesNeedingTranslation([]model.Message{tt.msg}, tt.lang); len(got) != tt.want {
				t.Fatalf("missing = %v, want %d", got, tt.want)
			}
		})
	}
}

func TestFinishAINoteReset(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	w := &window{
		deps: Deps{RecategorizeInbox: func(accountID int64) {
			if accountID != 0 {
				t.Errorf("refresh account = %d, want all accounts", accountID)
			}
			calls++
		}},
		renderCancel: cancel,
		sectionCache: map[uiCacheKey]cachedSection{
			cacheKey(1, "open"):   {body: "English gist"},
			cacheKey(2, "closed"): {body: "English gist"},
		},
		summaryCache:  map[uiCacheKey]string{cacheKey(1, "thread"): "English summary"},
		appliedGists:  map[uiCacheKey]string{cacheKey(2, "closed"): "English gist"},
		gistRequested: map[uiCacheKey]bool{cacheKey(2, "closed"): true},
	}
	w.finishAINoteReset()
	if ctx.Err() == nil {
		t.Fatal("old render was not cancelled")
	}
	if len(w.sectionCache) != 0 || len(w.summaryCache) != 0 || len(w.appliedGists) != 0 || len(w.gistRequested) != 0 {
		t.Fatal("old notes remain cached")
	}
	if calls != 1 {
		t.Fatalf("worker triggers = %d, want 1 even with no open conversation", calls)
	}
	if w.aiNotesGen == 0 {
		t.Fatal("pending UI callbacks were not invalidated")
	}
}
