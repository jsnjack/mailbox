package imapbackend

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/jsnjack/mailbox/internal/logging"
)

// folderState is the per-folder incremental-sync watermark: the UIDVALIDITY (so a
// folder reset is detected), the CONDSTORE HIGHESTMODSEQ (so only flag-changed
// messages are re-fetched, when the server supports it), and the full set of
// UIDs present at the last sync (range-compressed) — diffing against the current
// set yields new and vanished messages without QRESYNC.
type folderState struct {
	UIDValidity uint32 `json:"uidvalidity"`
	ModSeq      uint64 `json:"modseq,omitempty"`
	// UIDNext is the folder's UIDNEXT at the last snapshot; with it, a cheap
	// STATUS can prove a folder unchanged (see statusUnchanged) and skip the
	// per-tick SELECT + full UID SEARCH. 0 in cursors written by older builds
	// and by SeedCursor — those take the full snapshot once, which fills it.
	UIDNext imap.UID `json:"uidnext,omitempty"`
	UIDs    string   `json:"uids"` // imap.UIDSet.String() form, e.g. "1:5,7"
}

// cursor is the opaque sync watermark serialized into accounts.sync_cursor.
type cursor struct {
	Folders map[string]folderState `json:"folders"`
}

func decodeCursor(s string) cursor {
	c := cursor{Folders: map[string]folderState{}}
	if strings.TrimSpace(s) == "" {
		return c
	}
	_ = json.Unmarshal([]byte(s), &c) // a corrupt cursor degrades to a full diff
	if c.Folders == nil {
		c.Folders = map[string]folderState{}
	}
	return c
}

func (c cursor) encode() string {
	b, _ := json.Marshal(c)
	return string(b)
}

// encodeUIDs range-compresses a UID list to the IMAP set form ("1:5,7,9:12").
func encodeUIDs(uids []imap.UID) string {
	var set imap.UIDSet
	set.AddNum(uids...)
	return set.String()
}

// decodeUIDs parses the IMAP set form back into a UID slice (ascending).
func decodeUIDs(s string) []imap.UID {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	var out []imap.UID
	for _, part := range strings.Split(s, ",") {
		lo, hi, isRange := strings.Cut(part, ":")
		l, err := strconv.ParseUint(strings.TrimSpace(lo), 10, 32)
		if err != nil {
			continue
		}
		if !isRange {
			out = append(out, imap.UID(l))
			continue
		}
		h, err := strconv.ParseUint(strings.TrimSpace(hi), 10, 32)
		if err != nil {
			continue
		}
		for u := l; u <= h; u++ {
			out = append(out, imap.UID(u))
		}
	}
	return out
}

// folders returns the syncable mailboxes (see ensureFolders).
func (b *Backend) folders(c *conn) ([]string, error) {
	if err := b.ensureFolders(c); err != nil {
		return nil, err
	}
	b.folderMu.Lock()
	defer b.folderMu.Unlock()
	return b.synced, nil
}

// snapshot captures a folder's current state (UIDVALIDITY, modseq, full UID set).
// It forces a fresh SELECT (bypassing the conn's cache) so a UIDVALIDITY change
// is observed every sync pass, then refreshes the cache.
func (b *Backend) snapshot(c *conn, folder string) (folderState, []imap.UID, error) {
	// reselect forces a fresh SELECT so a UIDVALIDITY change is observed every
	// pass; CONDSTORE is requested only when the server advertises it.
	sel, err := c.reselect(folder, true)
	if err != nil {
		return folderState{}, nil, err
	}
	start := time.Now()
	sd, err := c.cl.UIDSearch(&imap.SearchCriteria{}, nil).Wait()
	if err != nil {
		logging.Trace("imapbackend: snapshot uid search failed", "folder", folder, "dur", time.Since(start), "err", err)
		return folderState{}, nil, fmt.Errorf("imap uid search %q: %w", folder, err)
	}
	uids := sd.AllUIDs()
	sort.Slice(uids, func(i, j int) bool { return uids[i] < uids[j] })
	logging.Trace("imapbackend: snapshot",
		"folder", folder, "uidvalidity", sel.UIDValidity, "modseq", sel.HighestModSeq, "uidnext", sel.UIDNext, "count", len(uids), "dur", time.Since(start))
	return folderState{UIDValidity: sel.UIDValidity, ModSeq: sel.HighestModSeq, UIDNext: sel.UIDNext, UIDs: encodeUIDs(uids)}, uids, nil
}

// statusUnchanged reports whether a cheap STATUS proves folder f identical to
// its stored state, letting the sync tick skip the SELECT + full UID SEARCH
// (which enumerates every message server-side and materializes every UID
// client-side — O(mailbox) per folder per tick even when nothing changed).
// Unchanged means: same UIDVALIDITY, same UIDNEXT (no arrivals), same message
// count (no arrivals ⇒ same count rules out expunges), and on a CONDSTORE
// server the same HIGHESTMODSEQ (no flag changes; a non-CONDSTORE server can't
// surface flag changes on the snapshot path either, so nothing is lost there).
// Any doubt — STATUS failure, missing fields, a cursor without UIDNext, or a
// CONDSTORE server before the modseq watermark is seeded — returns false and
// the caller takes the full snapshot.
func (b *Backend) statusUnchanged(c *conn, folder string, old folderState) bool {
	if old.UIDNext == 0 || old.UIDValidity == 0 {
		return false
	}
	condstore := c.cl.Caps().Has(imap.CapCondStore)
	if condstore && old.ModSeq == 0 {
		return false // take the snapshot so the modseq watermark gets seeded
	}
	opts := &imap.StatusOptions{NumMessages: true, UIDNext: true, UIDValidity: true, HighestModSeq: condstore}
	start := time.Now()
	sd, err := c.cl.Status(folder, opts).Wait()
	if err != nil || sd.NumMessages == nil {
		logging.Trace("imapbackend: status pre-check unavailable", "folder", folder, "err", err)
		return false
	}
	same := sd.UIDValidity == old.UIDValidity &&
		sd.UIDNext == old.UIDNext &&
		int(*sd.NumMessages) == countUIDs(old.UIDs) &&
		(!condstore || sd.HighestModSeq == old.ModSeq)
	logging.Trace("imapbackend: status pre-check", "folder", folder, "unchanged", same,
		"uidvalidity", sd.UIDValidity, "uidnext", sd.UIDNext, "messages", *sd.NumMessages,
		"modseq", sd.HighestModSeq, "dur", time.Since(start))
	return same
}

// changedSince returns which of the current UIDs had their flags changed since
// modseq (CONDSTORE). Empty when the server lacks CONDSTORE (modseq == 0) or
// there are no messages. The folder must already be selected on cl.
func (b *Backend) changedSince(cl *imapclient.Client, modseq uint64, curUIDs []imap.UID) ([]imap.UID, error) {
	if modseq == 0 || len(curUIDs) == 0 {
		logging.Trace("imapbackend: changedsince skipped (full-diff fallback)", "modseq", modseq, "curUIDs", len(curUIDs))
		return nil, nil // no CONDSTORE: flag changes are picked up on a later re-fetch
	}
	var set imap.UIDSet
	set.AddNum(curUIDs...)
	start := time.Now()
	bufs, err := cl.Fetch(set, &imap.FetchOptions{UID: true, ChangedSince: modseq}).Collect()
	if err != nil {
		logging.Trace("imapbackend: changedsince rejected (no deltas)", "modseq", modseq, "dur", time.Since(start), "err", err)
		return nil, nil // a server that rejects CHANGEDSINCE just yields no deltas
	}
	out := make([]imap.UID, 0, len(bufs))
	for _, m := range bufs {
		out = append(out, m.UID)
	}
	logging.Trace("imapbackend: changedsince", "modseq", modseq, "changed", len(out), "dur", time.Since(start))
	return out, nil
}

// buildProfileCursor captures the current state of all synced folders as the
// initial cursor (used to seed an account before its first incremental).
func (b *Backend) buildProfileCursor(c *conn) (string, error) {
	folders, err := b.folders(c)
	if err != nil {
		return "", err
	}
	logging.Trace("imapbackend: build profile cursor", "account", b.cfg.Email, "folders", len(folders))
	cur := cursor{Folders: make(map[string]folderState, len(folders))}
	for _, f := range folders {
		st, _, err := b.snapshot(c, f)
		if err != nil {
			return "", err
		}
		cur.Folders[f] = st
	}
	return cur.encode(), nil
}

// SeedCursor implements backend.CursorSeeder: it builds the initial sync cursor
// from exactly the ids the engine backfilled, so the per-folder UID sets are
// honest — a message the backfill cap skipped is absent from the cursor and
// surfaces as "new" on the next incremental pass, instead of being marked
// already-seen and hidden forever (the failure mode of seeding from Profile's
// full-mailbox snapshot). Folders with nothing backfilled simply have no entry:
// their whole content diffs as new later. ModSeq is left 0, so the first
// incremental uses the full-diff path rather than trusting a pre-backfill
// CONDSTORE watermark.
func (b *Backend) SeedCursor(ctx context.Context, backfilledIDs []string) (string, error) {
	logging.TraceContext(ctx, "imapbackend: seed cursor", "account", b.cfg.Email, "n", len(backfilledIDs))
	cur := cursor{Folders: map[string]folderState{}}
	epochs := map[string]int{} // per-mailbox distinct-epoch count, for the sanity trace
	for key, uids := range groupByFolder(backfilledIDs) {
		epochs[key.mailbox]++
		st, ok := cur.Folders[key.mailbox]
		// A mailbox should only ever appear under one UIDVALIDITY in a single
		// backfill; if a mid-backfill renumber produced two epochs, keep the larger
		// set (the next incremental's UIDVALIDITY check reconciles either way).
		if ok && len(decodeUIDs(st.UIDs)) >= len(uids) {
			continue
		}
		sort.Slice(uids, func(i, j int) bool { return uids[i] < uids[j] })
		cur.Folders[key.mailbox] = folderState{UIDValidity: key.uidv, UIDs: encodeUIDs(uids)}
	}
	for mb, n := range epochs {
		if n > 1 {
			logging.TraceContext(ctx, "imapbackend: seed cursor saw multiple uidvalidity epochs; kept largest", "account", b.cfg.Email, "mailbox", mb, "epochs", n)
		}
	}
	logging.TraceContext(ctx, "imapbackend: seed cursor ok", "account", b.cfg.Email, "folders", len(cur.Folders))
	return cur.encode(), nil
}

// computeChanges diffs every synced folder against the cursor and returns the
// upserted/deleted message ids plus the next cursor. New = current\stored,
// vanished = stored\current; a UIDVALIDITY change replaces the whole folder; flag
// changes (CONDSTORE) are folded into upserts. Caller holds mu.
func (b *Backend) computeChanges(c *conn, prev cursor) (upserts, deletes []string, next cursor, err error) {
	folders, err := b.folders(c)
	if err != nil {
		return nil, nil, cursor{}, err
	}
	next = cursor{Folders: make(map[string]folderState, len(folders))}
	up := map[string]bool{} // dedup new + flag-changed
	addUp := func(id string) {
		if !up[id] {
			up[id] = true
			upserts = append(upserts, id)
		}
	}
	for _, f := range folders {
		// Cheap unchanged check first: one STATUS round-trip instead of
		// SELECT + UID SEARCH ALL, which is what makes an idle 60s tick nearly
		// free. Skipped for the mailbox currently SELECTed on this connection
		// (STATUS on the selected mailbox is unreliable on some servers).
		if old, has := prev.Folders[f]; has && c.selected != f && b.statusUnchanged(c, f, old) {
			next.Folders[f] = old
			continue
		}
		st, curUIDs, serr := b.snapshot(c, f)
		if serr != nil {
			return nil, nil, cursor{}, serr
		}
		next.Folders[f] = st
		old := prev.Folders[f]

		if old.UIDValidity != 0 && old.UIDValidity != st.UIDValidity {
			// Folder reset: the old UIDs are meaningless now. Drop them and re-add all.
			oldUIDs := decodeUIDs(old.UIDs)
			logging.Trace("imapbackend: folder uidvalidity change (full re-sync)",
				"folder", f, "old_uidvalidity", old.UIDValidity, "new_uidvalidity", st.UIDValidity,
				"drop", len(oldUIDs), "readd", len(curUIDs))
			for _, u := range oldUIDs {
				deletes = append(deletes, msgID(f, old.UIDValidity, u))
			}
			for _, u := range curUIDs {
				addUp(msgID(f, st.UIDValidity, u))
			}
			continue
		}

		oldUIDs := decodeUIDs(old.UIDs)
		oldSet := uidSet(oldUIDs)
		curSet := uidSet(curUIDs)
		newN, vanishedN := 0, 0
		for _, u := range curUIDs { // new = current \ stored
			if !oldSet[u] {
				addUp(msgID(f, st.UIDValidity, u))
				newN++
			}
		}
		for _, u := range oldUIDs { // vanished = stored \ current
			if !curSet[u] {
				deletes = append(deletes, msgID(f, old.UIDValidity, u))
				vanishedN++
			}
		}
		// Flag changes since the stored modseq (re-fetch to update read/star).
		changed, cerr := b.changedSince(c.cl, old.ModSeq, curUIDs)
		if cerr != nil {
			return nil, nil, cursor{}, cerr
		}
		flagN := 0
		for _, u := range changed {
			if curSet[u] { // ignore changes to messages that also vanished
				addUp(msgID(f, st.UIDValidity, u))
				flagN++
			}
		}
		logging.Trace("imapbackend: folder diff",
			"folder", f, "uidvalidity", st.UIDValidity, "stored", len(oldUIDs), "current", len(curUIDs),
			"new", newN, "vanished", vanishedN, "flag_changed", flagN,
			"path", condStorePath(old.ModSeq))
	}
	logging.Trace("imapbackend: compute changes done", "account", b.cfg.Email, "upserts", len(upserts), "deletes", len(deletes))
	return upserts, deletes, next, nil
}

// condStorePath names which flag-change detection branch a folder diff used, for
// tracing: CONDSTORE CHANGEDSINCE when a stored modseq is present, else the
// full-diff fallback.
func condStorePath(modseq uint64) string {
	if modseq == 0 {
		return "full-diff"
	}
	return "condstore-changedsince"
}

func uidSet(uids []imap.UID) map[imap.UID]bool {
	m := make(map[imap.UID]bool, len(uids))
	for _, u := range uids {
		m[u] = true
	}
	return m
}

// countUIDs returns how many UIDs a range-compressed set string holds, without
// materializing them (the statusUnchanged pre-check runs every tick; decoding
// "1:100000" into a slice there would defeat its purpose).
func countUIDs(s string) int {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	n := 0
	for _, part := range strings.Split(s, ",") {
		lo, hi, isRange := strings.Cut(part, ":")
		l, err := strconv.ParseUint(strings.TrimSpace(lo), 10, 32)
		if err != nil {
			continue
		}
		if !isRange {
			n++
			continue
		}
		h, err := strconv.ParseUint(strings.TrimSpace(hi), 10, 32)
		if err != nil || h < l {
			continue
		}
		n += int(h - l + 1)
	}
	return n
}
