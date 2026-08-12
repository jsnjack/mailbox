// Reader: rendering an open conversation into the WebKit view — body fetch,
// section assembly and caching, inline images, gist cards, and the reader-only
// indicators (trackers, sender authentication, images). The shell page every
// conversation is swapped into lives here too.
package ui

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"log/slog"
	"net/mail"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/diamondburned/gotk4-webkitgtk/pkg/webkit/v6"
	"github.com/jsnjack/mailbox/internal/dispatch"
	"github.com/jsnjack/mailbox/internal/logging"
	"github.com/jsnjack/mailbox/internal/model"
)

// renderConversation fetches each message's body (lazily) and renders the whole
// thread as stacked sections in the reader.
func (w *window) renderConversation(msgs []model.Message) {
	latest := msgs[len(msgs)-1]
	// The pinned header is the thread title: subject plus a message count for a
	// real conversation. Each message's sender/date/recipients live in its own
	// section below (conversationSection), so the header no longer repeats the
	// latest message's sender+date — that duplicated the newest section header.
	subject := strings.TrimSpace(latest.Subject)
	if subject == "" {
		subject = "(no subject)"
	}
	title := "<span size=\"large\" weight=\"bold\">" + html.EscapeString(subject) + "</span>"
	if len(msgs) > 1 {
		title += fmt.Sprintf("\n<span size=\"small\">%d messages</span>", len(msgs))
	}
	w.header.SetMarkup(title)
	// Mirror threadRow's list-row override: once you've had the last word, or a
	// snooze just returned the thread to the inbox, the cached AI category is
	// stale or beside the point — otherwise the reader header could keep
	// showing e.g. "Needs reply" after the list row already moved on.
	threadKey := w.activeCacheKey(w.openThreadID)
	cat := w.threadCats[threadKey]
	category := cat.tag
	outgoing := w.current == model.LabelSent || w.current == model.LabelDraft
	if t, ok := w.threadByID[w.openThreadID]; ok && !outgoing && !cat.manual {
		switch {
		case t.RepliedByMe:
			category = "Replied"
		case t.WokeFromSnooze:
			category = "Snoozed"
		}
	}
	w.setReaderCategory(category, cat.failed)
	// Show a loading placeholder immediately when bodies need fetching (not all
	// cached), so the user sees their click registered instead of staring at the
	// previous message for up to the fetch timeout. When all bodies are cached
	// (the common case) the previous message stays (no flash) and the rendered
	// thread swaps in near-instantly behind the cover.
	needsFetch := false
	if w.deps.FetchBody != nil {
		for _, m := range msgs {
			if !m.BodyFetched {
				needsFetch = true
				break
			}
		}
	}
	if needsFetch {
		w.setReaderHTML(loadingInner)
	}

	threadID := w.openThreadID // guard against a newer thread being opened mid-render
	// Snapshot already-rendered sections on the main thread; the goroutine reuses
	// these and only sanitizes the misses, so re-opening a thread is near-instant.
	cached := w.cachedSectionsFor(msgs)
	// Snapshot the inline-refetch guard on the main thread too: w.inlineRefetched
	// must never be touched from the render goroutine (overlapping renders — rapid
	// j/k, a live thread refresh — would race). The goroutine works on its own
	// copy and the writes are merged back via dispatch.Main below.
	refetched := make(map[string]bool, len(msgs))
	for _, m := range msgs {
		if w.inlineRefetched[cacheKey(m.AccountID, m.GmailID)] {
			refetched[m.GmailID] = true
		}
	}
	loadRemoteImages := w.imagesEnabled
	logging.Trace("ui: render conversation", "thread", threadID, "msgs", len(msgs), "cachedSections", len(cached))
	// Cancel a still-running previous render (rapid thread open, a background
	// refresh, a re-render) so its in-flight body fetches abort immediately
	// instead of consuming a fetch-timeout of stale work.
	if w.renderCancel != nil {
		w.renderCancel()
	}
	renderCtx, cancelRender := context.WithCancel(context.Background())
	w.renderCancel = cancelRender
	// Claim this render's body fetches, so their MessageBodyFetched events don't
	// come back as a "the open thread changed" refresh: that refresh re-enters
	// renderConversation, whose cancel above kills the sibling fetches still in
	// flight. One completed fetch would then abort the rest, turning a single
	// parallel pass over the thread into one network round trip per message —
	// the reader sitting on its loading placeholder the whole time.
	w.renderGen++
	rgen := w.renderGen
	w.renderFetching = make(map[uiCacheKey]bool, len(msgs))
	for _, m := range msgs {
		w.renderFetching[cacheKey(m.AccountID, m.GmailID)] = true
	}
	go func() {
		defer cancelRender()
		start := time.Now()
		// Bodies are fetched with a bounded timeout so a hung network call can't
		// pin the reader on the previous message indefinitely — a dropped
		// connection with no RST would otherwise block until the OS TCP keepalive
		// reaps it (2+ hours). The HTTP client also has its own per-request timeout
		// (see gmailapi.NewService); this render-level deadline recovers faster.
		// renderCtx is cancelled when the user opens another thread, so in-flight
		// fetches abort immediately on navigation.
		// Local cache reads below use a fresh unbounded context: SQLite never
		// blocks on the network, so a timed-out/cancelled fetch must not starve
		// cached-body reads.
		fetchCtx, cancelFetch := context.WithTimeout(renderCtx, 60*time.Second)
		defer cancelFetch()
		fetched := 0
		// fetchFailed marks messages whose body fetch failed or never finished
		// within the deadline; their sections render the snippet fallback and are
		// not cached. It is built once after the bounded wait below, so the reads
		// further down are race-free without a lock.
		fetchFailed := map[string]bool{}
		if w.deps.FetchBody != nil {
			var (
				okMu  sync.Mutex
				okIDs = map[string]bool{} // gmailID → body fetch completed successfully
			)
			sem := make(chan struct{}, 6)
			var wg sync.WaitGroup
			for _, m := range msgs {
				if m.BodyFetched {
					continue
				}
				fetched++
				wg.Add(1)
				go func(m model.Message) {
					defer wg.Done()
					// The slot is acquired inside the goroutine (not the spawn loop)
					// so six stuck fetches can't block spawning — and with it the
					// bounded wait below — indefinitely.
					select {
					case sem <- struct{}{}:
					case <-fetchCtx.Done():
						return
					}
					defer func() { <-sem }()
					logging.Trace("ui: fetch body", "id", m.GmailID, "account", m.AccountID)
					if err := w.deps.FetchBody(fetchCtx, m.AccountID, m.GmailID); err != nil {
						slog.Warn("ui: fetch body", "id", m.GmailID, "err", err)
						return
					}
					okMu.Lock()
					okIDs[m.GmailID] = true
					okMu.Unlock()
				}(m)
			}
			// Bounded wait: a fetch stuck in a layer that ignores cancellation (an
			// OAuth token refresh runs before the request context exists) would
			// otherwise block wg.Wait() forever and pin the reader on the loading
			// placeholder. Past the deadline the fallback renders; stragglers finish
			// (or die) in the background, and the message stays unfetched so the
			// next open retries it.
			fetchDone := make(chan struct{})
			go func() { wg.Wait(); close(fetchDone) }()
			select {
			case <-fetchDone:
			case <-fetchCtx.Done():
				logging.Trace("ui: fetch bodies deadline, rendering fallback", "thread", threadID, "err", fetchCtx.Err())
			}
			okMu.Lock()
			for _, m := range msgs {
				if !m.BodyFetched && !okIDs[m.GmailID] {
					fetchFailed[m.GmailID] = true
				}
			}
			okMu.Unlock()
		}
		// If a newer thread was opened (renderCtx cancelled), bail before the
		// sanitization + UI swap — the newer render owns the reader.
		if renderCtx.Err() != nil {
			logging.Trace("ui: render conversation cancelled", "thread", threadID, "fetched", fetched)
			return
		}
		ctx := context.Background()
		fetchDur := time.Since(start)
		// Persisted one-line gists (the same summary the desktop notification
		// shows) render as a card on each message; messages without one are
		// collected for background generation (newest first, capped so a huge
		// thread doesn't queue dozens of AI calls in one pass — the rest are
		// picked up on a later open).
		ids := make([]string, len(msgs))
		for i, m := range msgs {
			ids[i] = m.GmailID
		}
		gists, err := w.deps.Store.MessageGists(ctx, latest.AccountID, ids)
		if err != nil {
			slog.Warn("ui: load message gists", "err", err)
		}
		var gistTodo []model.Message
		for i := len(msgs) - 1; i >= 0; i-- {
			m := msgs[i]
			if _, ok := gists[m.GmailID]; ok || strings.TrimSpace(m.Snippet) == "" {
				continue
			}
			if len(gistTodo) == gistBatchCap {
				logging.Trace("ui: gist batch capped", "thread", threadID, "cap", gistBatchCap)
				break
			}
			gistTodo = append(gistTodo, m)
		}
		sanitizeStart := time.Now()
		var b strings.Builder
		blocked := 0
		latestAuth, latestHTML := "", ""
		fresh := map[string]cachedSection{} // newly-rendered sections to cache
		// Newest message first (msgs is oldest-first from the store).
		for i := len(msgs) - 1; i >= 0; i-- {
			m := msgs[i]
			// The latest message always needs its body read for the auth/phishing
			// signals, even when its section is cached.
			if m.RowID == latest.RowID {
				body := w.bodyForRender(ctx, m, refetched)
				latestAuth = body.RawHeaders
				latestHTML = body.HTML
				if cs, ok := cached[m.GmailID]; ok {
					b.WriteString(composeSection(cs.head, cs.body, false))
					blocked += cs.trackers
					continue
				}
				head, rest, n := w.conversationSection(m, body, w.cleanHTML, fetchFailed[m.GmailID], gists[m.GmailID])
				// A failure section (snippet + "could not be loaded" notice) is
				// transient — caching it would keep showing the failure after the
				// body becomes fetchable again. Only real bodies are immutable.
				if !fetchFailed[m.GmailID] {
					fresh[m.GmailID] = cachedSection{head: head, body: rest, trackers: n}
				}
				b.WriteString(composeSection(head, rest, false))
				blocked += n
				continue
			}
			var head, rest string
			if cs, ok := cached[m.GmailID]; ok {
				head, rest = cs.head, cs.body
				blocked += cs.trackers
			} else {
				body := w.bodyForRender(ctx, m, refetched)
				h2, r2, n := w.conversationSection(m, body, w.cleanHTML, fetchFailed[m.GmailID], gists[m.GmailID])
				// Transient failure sections are not cached — see the
				// latest-message branch above.
				if !fetchFailed[m.GmailID] {
					fresh[m.GmailID] = cachedSection{head: h2, body: r2, trackers: n}
				}
				head, rest = h2, r2
				blocked += n
			}
			// In longer threads the history opens collapsed (newest message
			// expanded, older ones folded to their header) — a 30-message thread
			// reads as a list, not a wall. Native disclosure, no JS.
			b.WriteString(composeSection(head, rest, len(msgs) > 2))
		}
		out := b.String()
		// Naming the images costs no network (see resolveRemoteImages), so the
		// conversation is swapped in as soon as its text is ready and every image
		// — external and inline alike — arrives afterwards through its scheme
		// handler.
		out, remoteStats, pendingImages := w.resolveRemoteImages(out, loadRemoteImages)
		if renderCtx.Err() != nil {
			logging.Trace("ui: external image pass cancelled", "thread", threadID)
			return
		}
		verdict := parseAuthResults(latestAuth)
		warnings := phishingWarnings(latest, latestHTML)
		// Local reads only: attachment rows and the inline-image index. The
		// downloads behind them happen on demand.
		atts := w.threadAttachments(ctx, msgs)
		inlineImgs := w.inlineImageIndex(ctx, msgs)
		slog.Debug("ui: renderConversation", "msgs", len(msgs), "fetched", fetched,
			"trackers", blocked, "auth", verdict.level, "fetch", fetchDur, "sanitize", time.Since(sanitizeStart))
		logging.Trace("ui: render conversation ready", "thread", threadID, "msgs", len(msgs), "fetched", fetched,
			"newSections", len(fresh), "trackers", blocked, "auth", verdict.level, "warnings", len(warnings),
			"attachments", len(atts), "inlineImages", len(inlineImgs), "remoteImages", remoteStats.Total,
			"blockedRemoteImages", remoteStats.Blocked,
			"bytes", len(out), "html", logging.Body(out),
			"fetch", fetchDur, "sanitize", time.Since(sanitizeStart))
		dispatch.Main(func() {
			// This render no longer owns any body fetch: a later one (an inline
			// re-fetch finishing, a straggler past the deadline) is a genuine
			// change again and must re-render. A superseded render leaves the
			// newer one's claim alone.
			if rgen == w.renderGen {
				w.renderFetching = nil
			}
			w.mergeSectionCache(latest.AccountID, fresh) // cache newly-rendered sections (main thread)
			// Merge the goroutine-local inline-refetch marks back into the main-thread
			// map (even for a discarded render — the re-fetch already happened, so it
			// must not repeat).
			for id := range refetched {
				w.inlineRefetched[cacheKey(latest.AccountID, id)] = true
			}
			if w.openThreadID != threadID {
				logging.Trace("ui: render conversation discarded", "thread", threadID, "openThread", w.openThreadID)
				return // user switched to another conversation while this rendered
			}
			w.applyRemoteImageBanner(remoteStats)
			// Both maps must be in place before the swap: WebKit requests the
			// page's images as soon as it has the markup, and the handlers
			// resolve them through these.
			w.remoteImageURLs = pendingImages // serveRemoteImage fetches against this
			w.inlineByCID = inlineImgs        // serveCID resolves cid: against this
			w.lastFetchFailed = len(fetchFailed) > 0
			w.setTrackerCount(blocked)
			w.setAuthBadge(verdict)
			w.setCaution(warnings)
			w.setReaderHTML(out)
			// Re-assert known gists over the fresh swap: a section may have come
			// from the cache with its placeholder still hidden, or this render's
			// store query may predate a gist persisted mid-render — either way
			// the swap must not un-reveal a card. Filling an already-filled card
			// with the same text is a no-op visually. Once the store query
			// includes a gist, its re-apply copy is no longer needed.
			for _, m := range msgs {
				g, ok := gists[m.GmailID]
				if ok {
					delete(w.appliedGists, cacheKey(m.AccountID, m.GmailID))
				} else {
					g = w.appliedGists[cacheKey(m.AccountID, m.GmailID)]
				}
				if g != "" {
					w.fireGistJS(m.GmailID, g)
				}
			}
			w.showThreadAttachments(atts)
			w.showInviteCard(0, nil) // revealed by detectInviteLater if there is one
			if w.lastFetchFailed {
				w.toast("Couldn't load some message bodies — offline?")
			}
			w.generateGists(threadID, gistTodo)
			w.detectInviteLater(threadID, rgen, atts)
		})
	}()
}

// setReaderHTML swaps inner (sanitized conversation HTML) into the persistent
// shell page via script — an innerHTML swap, never a navigation, so WebKit
// never tears down its composited surface and the reader cannot flash black
// (see buildReader). Content set before the shell reported ready is queued;
// only the latest queued content matters, so a newer set replaces it.
func (w *window) setReaderHTML(inner string) {
	if !w.readerReady {
		logging.Trace("ui: reader shell not ready; queueing content", "bytes", len(inner))
		w.pendingReaderHTML = &inner
		return
	}
	// json.Marshal produces a valid JS string literal (quotes, escapes, and
	// HTML-unsafe characters like <, U+2028/9 escaped), so the HTML rides into
	// __mbSet as data with nothing to inject.
	quoted, err := json.Marshal(inner)
	if err != nil { // can't happen for a string; keep the reader honest anyway
		slog.Error("ui: set reader html", "err", err)
		return
	}
	evalJS(w.webview, "window.__mbSet("+string(quoted)+");")
}

// shellReadyHandler is the script-message channel the reader shell announces
// itself on once __mbSet is installed; buildReader flushes queued content then.
const shellReadyHandler = "shellready"

// readerRefitScript schedules the shell's fit-to-width pass after WebKit has
// applied a native page-zoom change.
const readerRefitScript = "window.__mbFit&&window.__mbFit();"

// gistBatchCap bounds how many missing gists one thread render may queue for
// generation; a longer thread's remainder is picked up on a later open.
const gistBatchCap = 10

// gistCard renders a message's one-line AI summary as a card between the
// section header and the body. While no gist exists yet the card is emitted
// hidden, so __mbGist can fill and reveal it in place when generation
// finishes. The tag makes its AI origin explicit.
func gistCard(gmailID, gist string) string {
	g := strings.TrimSpace(gist)
	hidden := ""
	if g == "" {
		hidden = " hidden"
	}
	return `<div class="mbgist" data-mid="` + html.EscapeString(gmailID) + `"` + hidden +
		` title="Summary generated by AI from this message"><span class="mbgist-tag">✦ AI summary</span> <span class="mbgist-text">` +
		html.EscapeString(g) + `</span></div>`
}

// gistContext is the AI input for a message's one-line gist — shared by the
// desktop notification and the reader's summary card so both produce (and
// reuse) the same line.
func gistContext(m model.Message) string {
	return fmt.Sprintf("From: %s\nSubject: %s\n\n%s", displayFrom(m), m.Subject, cleanAIContext(m.Snippet))
}

// generateGists produces (and persists) the one-line AI gist for the given
// messages of the open conversation, sequentially in the background; each
// finished gist is slotted into the rendered thread via applyGist. Gated on
// its own AI-features toggle and deduplicated per session via gistRequested.
// Main thread.
func (w *window) generateGists(threadID string, msgs []model.Message) {
	if w.deps.Assistant == nil || !w.aiGist || len(msgs) == 0 {
		return
	}
	var todo []model.Message
	for _, m := range msgs {
		key := cacheKey(m.AccountID, m.GmailID)
		if w.gistRequested[key] {
			continue
		}
		w.gistRequested[key] = true
		todo = append(todo, m)
	}
	if len(todo) == 0 {
		return
	}
	logging.Trace("ui: generate gists", "thread", threadID, "n", len(todo))
	label := "Summarizing message"
	if len(todo) > 1 {
		label = fmt.Sprintf("Summarizing %d messages", len(todo))
	}
	done := w.aiActivity(label)
	go func() {
		var lastErr error
		for _, m := range todo {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			gist, err := w.deps.Assistant.BriefSummary(ctx, gistContext(m))
			cancel()
			if err != nil || gist == "" {
				logging.Trace("ui: gist failed", "id", m.GmailID, "err", err)
				if err != nil {
					lastErr = err
				}
				// Clear the mark so a later open retries this message.
				dispatch.Main(func() { delete(w.gistRequested, cacheKey(m.AccountID, m.GmailID)) })
				continue
			}
			if serr := w.deps.Store.SetMessageGist(context.Background(), m.AccountID, m.GmailID, gist); serr != nil {
				slog.Warn("ui: persist gist", "id", m.GmailID, "err", serr)
			}
			dispatch.Main(func() { w.applyGist(m.AccountID, m.GmailID, m.ThreadID, gist) })
		}
		done(doneErr(lastErr))
	}()
}

// applyGist makes a freshly generated gist visible: the message's cached
// section is invalidated (the next render includes the card filled from the
// store), and when its thread is the one on screen the card is filled and
// revealed in place via __mbGist — a full re-render would reset the reader's
// scroll position. Main thread.
func (w *window) applyGist(accountID int64, gmailID, threadID, gist string) {
	w.invalidateSection(accountID, gmailID)
	w.appliedGists[cacheKey(accountID, gmailID)] = gist
	if w.activeID != accountID || w.openThreadID != threadID {
		return
	}
	w.fireGistJS(gmailID, gist)
}

// fireGistJS fills and reveals a message's gist card in the rendered reader
// (no-op in the shell when the message isn't on screen). Main thread.
func (w *window) fireGistJS(gmailID, gist string) {
	idJSON, _ := json.Marshal(gmailID)
	gistJSON, _ := json.Marshal(gist)
	evalJS(w.webview, "window.__mbGist("+string(idJSON)+","+string(gistJSON)+");")
}

// composeSection joins a message's header and the rest of it. The newest
// message shows both outright; an older one folds into a native <details>
// whose summary is that same header — so a message has exactly one identity
// line, open or closed, instead of a summary and a header introducing it twice
// in a row. CSS decides which parts of the header each state shows.
func composeSection(head, rest string, collapsed bool) string {
	if !collapsed {
		return head + rest
	}
	return `<details class="mbmsg"><summary>` + head + `</summary>` + rest + `</details>`
}

// capCache evicts arbitrary entries until m holds at most max. Main-thread only
// (all session caches are main-thread confined).
func capCache(m map[uiCacheKey]string, max int) {
	for len(m) > max {
		for k := range m {
			delete(m, k)
			break
		}
	}
}

// cachedSectionsFor returns the cached sections for the given messages (main
// thread); the result is handed to the render goroutine, which reuses hits and
// sanitizes only the misses.
func (w *window) cachedSectionsFor(msgs []model.Message) map[string]cachedSection {
	out := make(map[string]cachedSection, len(msgs))
	for _, m := range msgs {
		if cs, ok := w.sectionCache[cacheKey(m.AccountID, m.GmailID)]; ok {
			out[m.GmailID] = cs
		}
	}
	return out
}

// mergeSectionCache stores newly-rendered sections, evicting arbitrary entries
// when over the cap (sections are immutable, so an eviction is just a future
// cache miss). Main-thread only.
func (w *window) mergeSectionCache(accountID int64, fresh map[string]cachedSection) {
	for k, v := range fresh {
		w.sectionCache[cacheKey(accountID, k)] = v
	}
	for len(w.sectionCache) > sectionCacheCap {
		for k := range w.sectionCache {
			delete(w.sectionCache, k)
			break
		}
	}
}

// invalidateSection drops a message's cached section, so a re-synced message
// (changed metadata/body) re-renders. Main-thread only.
func (w *window) invalidateSection(accountID int64, gmailID string) {
	if gmailID != "" {
		delete(w.sectionCache, cacheKey(accountID, gmailID))
	}
}

// formatMsgDate renders a message timestamp, spending the year only when it
// isn't this year.
func formatMsgDate(t, now time.Time) string {
	if t.Year() == now.Year() {
		return t.Format("Jan 2, 15:04")
	}
	return t.Format("Jan 2, 2006 15:04")
}

// formatRecipients renders a recipient list compactly: the user's own
// addresses become "me" (the mail being addressed to you is the routine case),
// others show their display name, and long lists collapse to "+N more". Every
// recipient is a link opening its address card (mbaction:rcpt — copy, search,
// compose; the same pattern as the sender name), and each shows its full
// address on hover. Senders keep their full address inline elsewhere (an
// anti-phishing choice); recipients don't need it.
func (w *window) formatRecipients(list string) string {
	own := make(map[string]bool, len(w.deps.Accounts))
	for _, a := range w.deps.Accounts {
		own[strings.ToLower(a.Email)] = true
	}
	addrs, err := mail.ParseAddressList(list)
	if err != nil || len(addrs) == 0 {
		return html.EscapeString(list) // unparseable — show as-is
	}
	var parts []string
	for _, a := range addrs {
		label := a.Address
		switch {
		case own[strings.ToLower(a.Address)]:
			label = "me"
		case a.Name != "":
			label = a.Name
		}
		// a.String() re-serializes "Name <addr>" so the card gets both parts;
		// QueryEscape keeps the href attribute-safe.
		parts = append(parts, fmt.Sprintf(`<a href="mbaction:rcpt/%s" class="mbrcpt" title="%s">%s</a>`,
			url.QueryEscape(a.String()), html.EscapeString(a.Address), html.EscapeString(label)))
	}
	if len(parts) <= rcptShown {
		return strings.Join(parts, ", ")
	}
	more := fmt.Sprintf("+%d more", len(parts)-rcptShown)
	return strings.Join(parts[:rcptShown], ", ") +
		`<span class="mbrest" hidden>, ` + strings.Join(parts[rcptShown:], ", ") + `</span> ` +
		`<a href="#" class="mbmore" data-more="` + more + `" data-less="show less">` + more + `</a>`
}

// conversationSection renders one message's header + body and returns the HTML
// plus how many trackers were stripped from it. clean sanitizes+de-tracks HTML.
// fetchFailed, when true, inserts a styled notice that the body couldn't be
// loaded (network error/timeout) so the snippet fallback isn't silent.
// gist is the message's one-line AI summary, rendered as a card between header
// and body; when still empty a hidden placeholder is emitted instead, so a gist
// generated while the thread is open slots in via __mbGist (a targeted DOM
// update — a full re-render would reset the reader's scroll position).
func (w *window) conversationSection(m model.Message, body model.MessageBody, clean func(string) (string, int), fetchFailed bool, gist string) (head, rest string, blocked int) {
	var hb strings.Builder
	// Sender left, date right (flex); the sender is a link to the in-app
	// sender actions (mbaction: is intercepted by onDecidePolicy — it never
	// navigates or leaves the app).
	hb.WriteString(`<div class="mbhead">`)
	hb.WriteString(`<div class="mbline"><span>`)
	// The disclosure triangle lives inside the header, because for every
	// message but the newest this header IS the <details> summary — one
	// identity line per message, whether it is open or closed.
	hb.WriteString(`<span class="mbchev"></span>`)
	fmt.Fprintf(&hb, `<a href="mbaction:sender/%s" style="color:inherit;text-decoration:none" title="Sender actions"><b>%s</b>`,
		url.QueryEscape(m.GmailID), html.EscapeString(displayFrom(m)))
	// Always show the actual sender address, not just the display name. It is
	// the first thing to go when the message is folded shut, where the snippet
	// says more about what the message is.
	if addr := strings.TrimSpace(m.FromAddr); addr != "" && !strings.EqualFold(addr, displayFrom(m)) {
		fmt.Fprintf(&hb, ` <span class="mbaddr">&lt;%s&gt;</span>`, html.EscapeString(addr))
	}
	hb.WriteString(`</a>`)
	if preview := messagePreview(m); preview != "" {
		fmt.Fprintf(&hb, ` <span class="mbprev">%s</span>`, html.EscapeString(preview))
	}
	hb.WriteString(`</span>`)
	hb.WriteString(`<span class="mbdate">`)
	hb.WriteString(formatMsgDate(m.InternalDate, time.Now()))
	// Per-message actions: the header bar acts on the conversation, this ⋯
	// opens the menu of actions on this specific message (reply/forward when
	// sending is available, View headers / phishing analysis always).
	hb.WriteString(msgMenuIcon(m.GmailID))
	hb.WriteString(`</span></div>`)
	if to := strings.TrimSpace(m.ToAddrs); to != "" {
		fmt.Fprintf(&hb, `<div class="mbrcpt-line">to %s</div>`, w.formatRecipients(to))
	}
	if cc := strings.TrimSpace(m.CcAddrs); cc != "" {
		fmt.Fprintf(&hb, `<div class="mbrcpt-line">cc %s</div>`, w.formatRecipients(cc))
	}
	// Only your own copies (sent mail, drafts) ever carry Bcc — shown like
	// Gmail shows it on a sent message.
	if bcc := strings.TrimSpace(m.BccAddrs); bcc != "" {
		fmt.Fprintf(&hb, `<div class="mbrcpt-line">bcc %s</div>`, w.formatRecipients(bcc))
	}
	hb.WriteString(`</div>`)
	header := hb.String()
	// The body is wrapped so it can be padded onto the same text column as the
	// header band and the summary card above it; the page margin is 2px, so this
	// is what puts a message's own text where it belongs.
	body_ := func(inner string) string { return `<div class="mbbody">` + inner + `</div>` }
	rest = gistCard(m.GmailID, gist)
	switch {
	case body.HTML != "":
		cleaned, blocked := clean(body.HTML)
		return header, rest + body_(cleaned), blocked
	case body.Text != "":
		return header, rest + body_("<pre style=\"white-space:pre-wrap\">"+linkifyText(body.Text)+"</pre>"), 0
	default:
		notice := ""
		if fetchFailed {
			notice = `<p style="color:#a00;font-style:italic;margin-bottom:8px">⚠ Message body could not be loaded — you may be offline. Select "Retry loading" from the menu to try again.</p>`
		}
		return header, rest + body_(notice+"<p>"+linkifyText(m.Snippet)+"</p>"), 0
	}
}

// messagePreview is the one-line snippet a folded message shows in place of its
// address — what the message is about, where the open one shows who it is to.
func messagePreview(m model.Message) string {
	preview := strings.TrimSpace(m.Snippet)
	if r := []rune(preview); len(r) > 80 {
		preview = string(r[:79]) + "…"
	}
	return preview
}

// bodyForRender loads a message's body, re-fetching once (this session) when it
// references inline cid: images that weren't captured under older extraction
// logic — so already-cached mail picks up inline images without a manual resync.
// refetched is the render goroutine's private copy of w.inlineRefetched (a
// main-thread snapshot); marks made here are merged back on the main thread.
func (w *window) bodyForRender(ctx context.Context, m model.Message, refetched map[string]bool) model.MessageBody {
	body, _ := w.deps.Store.GetBody(ctx, m.RowID)
	if w.needsInlineRefetch(ctx, m, body, refetched) {
		logging.Trace("ui: inline re-fetch", "id", m.GmailID, "account", m.AccountID)
		refetched[m.GmailID] = true
		// Bound this ad-hoc refetch so a hung network call doesn't block the
		// render indefinitely — ctx is unbounded (local SQLite reads), so a
		// fresh deadline is created here for the one network hop.
		fetchCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
		if err := w.deps.FetchBody(fetchCtx, m.AccountID, m.GmailID); err != nil {
			slog.Warn("ui: inline re-fetch", "id", m.GmailID, "err", err)
		} else {
			body, _ = w.deps.Store.GetBody(ctx, m.RowID)
		}
		cancel()
	}
	return body
}

// needsInlineRefetch reports whether m's body references inline images (cid:) but
// no inline attachment was captured — the signature of a body fetched before
// inline parts were stored. Guarded (via the caller-owned refetched set) so each
// message is re-fetched at most once.
func (w *window) needsInlineRefetch(ctx context.Context, m model.Message, body model.MessageBody, refetched map[string]bool) bool {
	if w.deps.FetchBody == nil || refetched[m.GmailID] {
		return false
	}
	if !strings.Contains(strings.ToLower(body.HTML), "cid:") {
		return false
	}
	atts, err := w.deps.Store.ListAttachments(ctx, m.RowID)
	if err != nil {
		return false
	}
	for _, a := range atts {
		if a.ContentID != "" {
			return false // inline parts already captured
		}
	}
	return true
}

// inlineImage is a cached inline-image file plus its MIME type, served by the
// cid: URI-scheme handler.
// inlineImage locates one inline (cid:) attachment of the open conversation.
// path is filled in once the attachment has been downloaded, so a second request
// (and "Open Image in New Window") is served straight off disk.
type inlineImage struct {
	accountID int64
	gmailID   string
	attID     int64
	mime      string
	path      string
}

// inlineImageIndex maps the thread's inline (cid:) Content-IDs to the attachment
// each one names, reading only the local attachment rows. The download happens
// in serveCID when WebKit asks for the image, so a big inline image delays
// nothing but itself. Embedding these in the HTML as base64 (a 15 MB image →
// ~20 MB page) made WebKit's parse the dominant cost of opening a thread;
// serving them as resources keeps the HTML small.
func (w *window) inlineImageIndex(ctx context.Context, msgs []model.Message) map[string]inlineImage {
	if w.deps.OpenAttach == nil {
		return nil
	}
	out := map[string]inlineImage{}
	for _, m := range msgs {
		atts, err := w.deps.Store.ListAttachments(ctx, m.RowID)
		if err != nil {
			continue
		}
		for _, a := range atts {
			if a.ContentID == "" {
				continue
			}
			if _, done := out[a.ContentID]; done {
				continue
			}
			mime := a.MimeType
			if mime == "" {
				mime = "application/octet-stream"
			}
			out[a.ContentID] = inlineImage{accountID: m.AccountID, gmailID: m.GmailID, attID: a.ID, mime: mime}
		}
	}
	return out
}

// attachmentFetchTimeout bounds one on-demand attachment download (an inline
// image the reader asked for, or a calendar invite being read).
const attachmentFetchTimeout = 60 * time.Second

// serveCID answers a cid: image request from the reader with the matching inline
// attachment, downloading it first when the cache doesn't have it yet and
// finishing the request when it lands (WebKit permits a deferred answer). The
// handler itself must not block: it runs on the main thread.
func (w *window) serveCID(req *webkit.URISchemeRequest) {
	cid := strings.TrimPrefix(req.URI(), "cid:")
	if dec, err := url.PathUnescape(cid); err == nil {
		cid = dec
	}
	cid = strings.Trim(cid, "<>")
	img, ok := w.inlineByCID[cid]
	if !ok {
		logging.Trace("ui: inline image not in the open conversation", "cid", cid)
		finishBlankImage(req)
		return
	}
	if img.path != "" {
		w.finishImageRequest(req, img.path, img.mime)
		return
	}
	logging.Trace("ui: inline image fetch on demand", "cid", cid, "id", img.gmailID)
	go func() {
		path, err := w.fetchInlineImage(img)
		dispatch.Main(func() {
			if err != nil {
				slog.Warn("ui: inline image fetch", "cid", cid, "err", err)
				finishBlankImage(req)
				return
			}
			// Remember the file so a re-render (or opening the image
			// externally) is served without another download.
			if cur, ok := w.inlineByCID[cid]; ok && cur.attID == img.attID {
				cur.path = path
				w.inlineByCID[cid] = cur
			}
			w.finishImageRequest(req, path, img.mime)
		})
	}()
}

// fetchInlineImage downloads one inline attachment to the local cache and
// returns its path. Safe off the main thread.
func (w *window) fetchInlineImage(img inlineImage) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), attachmentFetchTimeout)
	defer cancel()
	select {
	case imageFetchSlots <- struct{}{}:
		defer func() { <-imageFetchSlots }()
	case <-ctx.Done():
		return "", ctx.Err()
	}
	return w.deps.OpenAttach(ctx, img.accountID, img.gmailID, img.attID)
}

// urlPattern matches an explicit http/https URL. Deliberately narrow (a scheme is
// required, and the URL stops at whitespace or a char that would break out of an
// attribute) so linkifyText never fabricates a non-http link or turns an ordinary
// word into one — fewer false positives than a www./bare-domain matcher.
var urlPattern = regexp.MustCompile(`https?://[^\s<>"]+`)

// linkifyText renders a plain-text email body (or snippet) as safe HTML: every
// segment is HTML-escaped, and bare http(s) URLs become <a> links. The reader's
// navigation policy opens those externally (xdg-open), so no extra plumbing is
// needed. Escaping both the href and the link text — and matching a scheme that
// cannot contain a quote — means email text can't inject markup or a bad scheme.
func linkifyText(text string) string {
	var b strings.Builder
	last := 0
	for _, loc := range urlPattern.FindAllStringIndex(text, -1) {
		b.WriteString(html.EscapeString(text[last:loc[0]]))
		raw := trimURLTrailing(text[loc[0]:loc[1]])
		esc := html.EscapeString(raw)
		fmt.Fprintf(&b, `<a href="%s">%s</a>`, esc, esc)
		last = loc[0] + len(raw) // any trimmed tail falls back into plain text
	}
	b.WriteString(html.EscapeString(text[last:]))
	return b.String()
}

// trimURLTrailing strips punctuation that commonly abuts a URL in prose but isn't
// part of it — a sentence's ".,;:!?'", and a closing ) or ] only when it isn't
// balanced inside the URL (so "(see https://x/a)" drops the ")" while
// ".../Foo_(bar)" keeps it).
func trimURLTrailing(u string) string {
	for {
		t := strings.TrimRight(u, ".,;:!?'")
		if strings.HasSuffix(t, ")") && strings.Count(t, "(") < strings.Count(t, ")") {
			t = t[:len(t)-1]
		} else if strings.HasSuffix(t, "]") && strings.Count(t, "[") < strings.Count(t, "]") {
			t = t[:len(t)-1]
		}
		if t == u {
			return t
		}
		u = t
	}
}

// cleanHTML sanitizes email body HTML then strips tracking pixels and collapses
// quoted history in one pass, returning the cleaned HTML and how many trackers
// were removed.
func (w *window) cleanHTML(h string) (string, int) {
	clean, n := cleanEmailHTML(w.sanitizer.Sanitize(h))
	// The sanitizer strips <style>; re-add it scoped to a unique wrapper so an
	// email's class-based layout renders (with its own cascade intact) without
	// bleeding onto other messages in the thread or the reader chrome.
	css := extractStyleCSS(h)
	if strings.TrimSpace(css) == "" {
		return clean, n
	}
	scope := "mbx-" + randNonce()[:12]
	scoped, cssTrackers := scopeEmailCSS(css, "."+scope)
	if scoped == "" {
		return clean, n
	}
	return `<div class="` + scope + `"><style>` + scoped + `</style>` + clean + `</div>`, n + cssTrackers
}

// setReaderCategory shows the thread's category pill in the reader header
// (hidden when uncategorized), mirroring the list row's tag styling. When
// category is "" and categorizeFailed is true, shows a distinct "Categorize
// failed" pill instead of hiding it — so a stuck AI attempt isn't
// indistinguishable from a settled "no category".
func (w *window) setReaderCategory(category string, categorizeFailed bool) {
	for _, c := range []string{"cat-needsreply", "cat-replied", "cat-discount", "cat-snoozed", "cat-failed"} {
		w.readerCatTag.RemoveCSSClass(c)
	}
	if category == "" && categorizeFailed {
		category = "Categorize failed"
	}
	if category == "" {
		w.readerCatTag.SetVisible(false)
		return
	}
	switch category {
	case "Needs reply":
		w.readerCatTag.AddCSSClass("cat-needsreply")
	case "Replied":
		w.readerCatTag.AddCSSClass("cat-replied")
	case "Discount":
		w.readerCatTag.AddCSSClass("cat-discount")
	case "Snoozed":
		w.readerCatTag.AddCSSClass("cat-snoozed")
	case "Categorize failed":
		w.readerCatTag.AddCSSClass("cat-failed")
	}
	w.readerCatTag.SetText(category)
	w.readerCatTag.SetVisible(true)
}

// setTrackerCount shows "N trackers blocked" in the reader (hidden when none).
func (w *window) setTrackerCount(n int) {
	if n <= 0 {
		w.trackerLabel.SetVisible(false)
		return
	}
	noun := "tracker"
	if n != 1 {
		noun = "trackers"
	}
	w.trackerLabel.SetText(fmt.Sprintf("%d %s blocked", n, noun))
	w.trackerLabel.SetVisible(true)
}

// setAuthBadge shows the sender-authentication verdict (SPF/DKIM/DMARC, as
// computed by Gmail) with semantic colour; an inconclusive verdict hides it.
func (w *window) setAuthBadge(v authVerdict) {
	w.authIcon.RemoveCSSClass("success")
	w.authIcon.RemoveCSSClass("warning")
	w.authIcon.RemoveCSSClass("error")
	switch v.level {
	case authPass:
		// Indicate exceptions, not norms: a verified sender is the routine
		// case, so no badge — the shield appears only when something is off.
		w.authIcon.SetVisible(false)
	case authPartial:
		w.authIcon.SetFromIconName("security-medium-symbolic")
		w.authIcon.AddCSSClass("warning")
		w.authIcon.SetTooltipText("Partially verified · " + v.detail)
		w.authIcon.SetVisible(true)
	case authFail:
		w.authIcon.SetFromIconName("security-low-symbolic")
		w.authIcon.AddCSSClass("error")
		w.authIcon.SetTooltipText("Authentication failed — sender may be spoofed (" + v.detail + ")")
		w.authIcon.SetVisible(true)
	default:
		w.authIcon.SetVisible(false)
	}
}

// setCaution shows anti-phishing heuristic warnings (hidden when there are none).
func (w *window) setCaution(warnings []string) {
	if len(warnings) == 0 {
		w.cautionLabel.SetVisible(false)
		return
	}
	w.cautionLabel.SetText("⚠ " + strings.Join(warnings, " "))
	w.cautionLabel.SetVisible(true)
}

// setImagesEnabled toggles remote-image loading and re-renders the open thread
// (keeping a shown translation shown — see rerenderOpenThread).
func (w *window) setImagesEnabled(on bool) {
	w.imagesEnabled = on
	w.webview.Settings().SetAutoLoadImages(on)
	if len(w.openThreadMsgs) > 0 {
		w.rerenderOpenThread() // re-render only; keep summary as-is
	}
}

// rerenderOpenThread repaints the open conversation in whichever view it is in:
// translated threads re-apply their translation (cached messages are free; a new
// reply is translated before the swap), everything else renders the originals.
// Without this, a background re-render would silently revert a translated thread
// while the "Showing translation" banner stayed up.
func (w *window) rerenderOpenThread() {
	if w.translationShown {
		logging.Trace("ui: re-render keeps translation", "thread", w.openThreadID)
		w.onTranslate()
		return
	}
	w.renderConversation(w.openThreadMsgs)
}

// onRetryLoading re-opens the current thread to re-fetch bodies that failed on
// the last render (network timeout, offline). Section cache entries for the
// thread's messages are invalidated so the error-notice sections don't persist.
func (w *window) onRetryLoading() {
	if w.openThreadID == "" || len(w.openThreadMsgs) == 0 {
		return
	}
	logging.Trace("ui: retry loading", "thread", w.openThreadID, "msgs", len(w.openThreadMsgs))
	for _, m := range w.openThreadMsgs {
		w.invalidateSection(m.AccountID, m.GmailID)
	}
	w.lastFetchFailed = false
	w.renderConversation(w.openThreadMsgs)
}

// readerShellHTML is the reader's single, persistent page: styles, CSP, and the
// script that owns content swaps (__mbSet) and fit-to-width scaling. It loads
// once in buildReader; conversations are swapped in via setReaderHTML — never a
// navigation, so WebKit's composited surface survives and nothing flashes.
func readerShellHTML() string {
	// CSS keeps the common overflow culprits in check (images capped to the
	// width, long URLs wrapped); the script then scales down anything still too
	// wide — chiefly fixed-width newsletter tables that CSS cannot shrink below
	// their min-content — so email fits the reader with neither a horizontal
	// scrollbar nor cropping.
	const style = `
html{overflow-x:hidden}
body{font-family:sans-serif;margin:8px 6px 16px;color:#222;line-height:1.4;overflow-x:hidden;overflow-wrap:anywhere}
table{table-layout:auto}
td,th{overflow-wrap:break-word;word-break:normal}
.mbwrap>.mbhead:first-child,.mbwrap>details.mbmsg:first-child{margin-top:0}
.mbmenu{color:inherit;text-decoration:none;margin-left:12px;opacity:.55}
.mbmenu:hover{opacity:1}
.mbmenu svg{width:15px;height:15px;vertical-align:-3px}
a[href^="mbaction:sender/"]:hover{text-decoration:underline!important}
a.mbrcpt{color:inherit;text-decoration:none}
a.mbrcpt:hover{text-decoration:underline}
a.mbmore{color:#1a5fb4;text-decoration:none;white-space:nowrap}
a.mbmore:hover{text-decoration:underline}
img,video{max-width:100%!important;height:auto!important}
img[hidden],video[hidden]{display:none!important}
pre{font-family:monospace;white-space:pre-wrap}
/* One message = one header. For every message but the newest that header is
   also the <details> summary, so opening a message reveals its body without
   introducing the sender a second time. The header sits on a faint band bled to
   the pane edges: the reader cannot draw a boundary an email is unable to
   imitate, but it can draw a surface. */
/* One text column, at 16px from the pane edge — where body text has always
   sat. Each surface reaches nearly to the edge (the page margin is 2px) and
   pads its own text back out to the column, so the header, the summary card
   and the message all start on the same line, and no tinted box has text
   against its edge. Padding, never negative margins: a surface wider than the
   wrap makes the fit-to-width script scale the conversation (see AGENTS.md). */
.mbhead{color:#555;font-size:90%;margin:0 0 12px}
/* The tint belongs to the identity line alone — recipients are detail, and
   including them made a three-line slab of the newest message's header. */
.mbline{position:relative;display:flex;justify-content:space-between;gap:12px;
  flex-wrap:wrap;background:rgba(0,0,0,.035);border-radius:6px;padding:6px 10px}
.mbdate{color:#888;white-space:nowrap}
.mbaddr{color:#888}
.mbrcpt-line{color:#888;padding:4px 10px 0}
.mbbody{padding:0 10px}
.mbprev{color:#888;display:none}
.mbchev{display:none}
/* Messages are set apart by the space between them as much as by the band. */
details.mbmsg{margin-top:26px}
details.mbmsg>summary{cursor:pointer;list-style:none}
details.mbmsg>summary::-webkit-details-marker{display:none}
details.mbmsg>summary>.mbhead{margin-bottom:0}
details.mbmsg[open]>summary>.mbhead{margin-bottom:12px}
details.mbmsg .mbchev{display:block;position:absolute;left:1px;top:6px;color:#999}
details.mbmsg .mbchev::before{content:"▸"}
details.mbmsg[open] .mbchev::before{content:"▾"}
/* Folded, the header says who wrote and what about; open, it says who wrote,
   from which address, and to whom. */
details.mbmsg:not([open]) .mbaddr,details.mbmsg:not([open]) .mbrcpt-line{display:none}
details.mbmsg:not([open]) .mbprev{display:inline}
.mbgist{background:rgba(53,132,228,.08);border:1px solid rgba(53,132,228,.16);border-radius:8px;padding:6px 9px;margin:12px 0 2px;color:#333;font-size:92%;line-height:1.35}
.mbgist-tag{color:#1a5fb4;font-weight:600;font-size:78%;text-transform:uppercase;letter-spacing:.07em;margin-right:6px;white-space:nowrap}`

	// Fit-to-width: scale wide content down to fit the reader. WebKitGTK ignores
	// CSS `zoom`, so content lives in a wrap div scaled with transform:scale
	// (origin top-left). Because transform doesn't shrink the layout box, the
	// wrapper is pinned to its natural width, the body height is collapsed to
	// the scaled height (no trailing gap), and overflow-x is clipped. Measured
	// before scaling so it never feeds back on itself; re-runs on resize, after
	// every content swap, and as each image of the swapped content loads (a
	// navigation's window.load re-ran it before; a swap has no load event).
	//
	// __mbSet is the app's content channel (setReaderHTML): an innerHTML swap of
	// sanitized HTML — inserted markup never executes scripts, and the CSP keeps
	// covering everything the content references. The ready postMessage lets the
	// app know __mbSet exists before it starts swapping.
	nonce := randNonce()
	script := `<script nonce="` + nonce + `">(function(){var wrap,fitFrame=0;function fit(){var b=document.body;if(!b||!wrap)return;` +
		`b.style.height='';wrap.style.transform='none';wrap.style.width='auto';var avail=b.clientWidth,natural=wrap.scrollWidth;` +
		`if(natural>avail+1&&natural>0){var s=avail/natural;wrap.style.width=natural+'px';wrap.style.transformOrigin='top left';wrap.style.transform='scale('+s+')';b.style.height=(wrap.offsetHeight*s)+'px';}else{b.style.height='';}}` +
		`function requestFit(){if(fitFrame)cancelAnimationFrame(fitFrame);fitFrame=requestAnimationFrame(function(){fitFrame=0;fit();});}` +
		`function setup(){var b=document.body;if(!b)return;wrap=document.createElement('div');wrap.className='mbwrap';b.appendChild(wrap);window.__mbFit=requestFit;window.addEventListener('resize',requestFit);` +
		`function hideBroken(i){i.hidden=true;i.removeAttribute('width');i.removeAttribute('height');` +
		`var p=i.parentElement,d=0;while(p&&p!==wrap&&d++<4){p.removeAttribute('height');p.style.removeProperty('height');p.style.removeProperty('min-height');` +
		`if((p.textContent||'').trim()||p.querySelector('img:not([hidden]),video:not([hidden])'))break;p=p.parentElement;}requestFit();}` +
		`window.__mbSet=function(h){wrap.innerHTML=h;window.scrollTo(0,0);fit();` +
		`wrap.querySelectorAll('img').forEach(function(i){if(!i.getAttribute('src')){hideBroken(i);return;}` +
		`i.addEventListener('error',function(){hideBroken(i);},{once:true});if(!i.complete){i.addEventListener('load',requestFit,{once:true});}});};` +
		// __mbGist fills a message's hidden AI-summary placeholder in place (no
		// content swap, so the scroll position survives). The text rides in as a
		// JS string and lands via textContent — nothing to inject. Placeholders
		// are matched by dataset comparison, not a selector, so a message id
		// never needs CSS escaping.
		`window.__mbGist=function(id,text){if(!wrap)return;wrap.querySelectorAll('.mbgist').forEach(function(el){` +
		`if(el.dataset.mid!==id)return;var t=el.querySelector('.mbgist-text');if(t){t.textContent=text;}el.hidden=false;});requestFit();};` +
		// "+N more" on a To/Cc line toggles the collapsed recipients in place
		// (delegated: header HTML arrives later via __mbSet innerHTML swaps).
		`document.addEventListener('click',function(e){var t=e.target&&e.target.closest?e.target.closest('a.mbmore'):null;if(!t)return;` +
		`e.preventDefault();var r=t.parentNode.querySelector('.mbrest');if(!r)return;r.hidden=!r.hidden;` +
		`t.textContent=r.hidden?t.dataset.more:t.dataset.less;requestFit();},true);` +
		// Folding a message changes the document height, which the scaled-down
		// layout has baked into body.style.height — refit or the content below
		// is clipped.
		`document.addEventListener('toggle',function(){requestFit();},true);` +
		`try{window.webkit.messageHandlers.` + shellReadyHandler + `.postMessage(true);}catch(e){}}` +
		`if(document.readyState!=='loading'){setup();}else{document.addEventListener('DOMContentLoaded',setup);}})();</script>`

	csp := "default-src 'none'; img-src data: cid: mbcache:; media-src data: cid: mbcache:; " +
		"style-src 'unsafe-inline'; script-src 'nonce-" + nonce + "'; font-src data:"

	return `<!doctype html><html><head><meta charset="utf-8">` +
		`<meta http-equiv="Content-Security-Policy" content="` + csp + `">` +
		`<style>` + style + `</style></head><body>` + script + `</body></html>`
}

// randNonce returns a random CSP nonce so only our injected script may run.
func randNonce() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "mailboxfit" // non-secret fallback; CSP still restricts to this value
	}
	return hex.EncodeToString(b[:])
}
