package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/jsnjack/mailbox/internal/logging"
)

// Prefs holds general user preferences that don't belong in the [ai] config or
// the per-window state.
type Prefs struct {
	// BlockRemoteImages, when true, stops the reader loading remote images by
	// default (the per-message toggle can still override). Default true for a
	// missing preference file; an existing explicit false remains honored.
	BlockRemoteImages bool `json:"block_remote_images"`
	// RemoteImagesConfigured distinguishes an explicit opt-in from the old
	// pre-privacy default (whose serialized false meant "load automatically").
	// Old files without this marker migrate to blocked on their next load.
	RemoteImagesConfigured bool `json:"remote_images_configured,omitempty"`
	// DisableInboxCategories, when true, turns off the automatic AI categorization
	// of inbox mail. Default false (categorization on), stored inverted so the
	// out-of-the-box behaviour is the zero value's opposite.
	DisableInboxCategories bool `json:"disable_inbox_categories"`
	// The following DisableXxx fields gate the remaining individual AI features,
	// each following the same inverted convention (false = feature on).
	DisableGist              bool `json:"disable_gist"`
	DisableAIDraft           bool `json:"disable_ai_draft"`
	DisableSmartReplies      bool `json:"disable_smart_replies"`
	DisableProofread         bool `json:"disable_proofread"`
	DisableRefine            bool `json:"disable_refine"`
	DisableGenerateSubject   bool `json:"disable_generate_subject"`
	DisableSummarize         bool `json:"disable_summarize"`
	DisableTranslate         bool `json:"disable_translate"`
	DisablePhishingAnalysis  bool `json:"disable_phishing_analysis"`
	DisableSnoozeSuggestions bool `json:"disable_snooze_suggestions"`
	// BodyRetentionDays prunes cached message bodies older than this many days
	// (metadata is kept forever; a pruned body is re-fetched on open). 0 — the
	// default — keeps bodies forever.
	BodyRetentionDays int `json:"body_retention_days,omitempty"`
	// SendUndoSeconds is how long a sent message is held behind the Undo toast
	// before it goes out. 0 means the default (5s).
	SendUndoSeconds int `json:"send_undo_seconds,omitempty"`
	// TrustedImageSenders are addresses whose remote images always load, even
	// when BlockRemoteImages is on (people trust senders, not messages).
	TrustedImageSenders []string `json:"trusted_image_senders,omitempty"`
}

func prefsPath() (string, error) {
	dir, err := ConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "prefs.json"), nil
}

// LoadPrefs reads the general preferences. A missing or unparseable file returns
// the zero value (defaults), not an error.
func LoadPrefs() (Prefs, error) {
	path, err := prefsPath()
	if err != nil {
		return Prefs{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			logging.Trace("config: load prefs (defaults)", "path", path)
			return defaultPrefs(), nil
		}
		logging.Trace("config: load prefs failed", "path", path, "err", err)
		return Prefs{}, fmt.Errorf("read prefs: %w", err)
	}
	p := defaultPrefs()
	if err := json.Unmarshal(data, &p); err != nil {
		logging.Trace("config: load prefs corrupt (ignored)", "path", path, "err", err)
		return defaultPrefs(), nil // ignore a corrupt file
	}
	if !p.RemoteImagesConfigured {
		p.BlockRemoteImages = true
	}
	logging.Trace("config: load prefs", "path", path, "blockRemoteImages", p.BlockRemoteImages,
		"disableInboxCategories", p.DisableInboxCategories, "disableGist", p.DisableGist,
		"disableAIDraft", p.DisableAIDraft, "disableSmartReplies", p.DisableSmartReplies,
		"disableProofread", p.DisableProofread, "disableGenerateSubject", p.DisableGenerateSubject,
		"disableSummarize", p.DisableSummarize, "disableTranslate", p.DisableTranslate,
		"disablePhishingAnalysis", p.DisablePhishingAnalysis, "disableSnoozeSuggestions", p.DisableSnoozeSuggestions)
	return p, nil
}

func defaultPrefs() Prefs { return Prefs{BlockRemoteImages: true} }

// SavePrefs persists the general preferences, creating the config dir if needed.
func SavePrefs(p Prefs) error {
	dir, err := ConfigDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	data, err := json.Marshal(p)
	if err != nil {
		return fmt.Errorf("marshal prefs: %w", err)
	}
	if err := WriteFileAtomic(filepath.Join(dir, "prefs.json"), data, 0o600); err != nil {
		logging.Trace("config: save prefs failed", "err", err)
		return fmt.Errorf("write prefs: %w", err)
	}
	logging.Trace("config: save prefs", "blockRemoteImages", p.BlockRemoteImages,
		"disableInboxCategories", p.DisableInboxCategories, "disableGist", p.DisableGist,
		"disableAIDraft", p.DisableAIDraft, "disableSmartReplies", p.DisableSmartReplies,
		"disableProofread", p.DisableProofread, "disableGenerateSubject", p.DisableGenerateSubject,
		"disableSummarize", p.DisableSummarize, "disableTranslate", p.DisableTranslate,
		"disablePhishingAnalysis", p.DisablePhishingAnalysis, "disableSnoozeSuggestions", p.DisableSnoozeSuggestions)
	return nil
}
