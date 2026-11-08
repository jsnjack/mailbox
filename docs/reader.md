# The reader

How a conversation opens and renders: body fetching, the HTML sanitizer and
the hardened path for email CSS, the one persistent WebKit shell page, remote
images and tracker stripping, link cleaning, sender authentication and
phishing checks, calendar invites, unsubscribe, reader actions and zoom,
incoming attachments, and the layout of each message. Read it before changing
`internal/ui/reader.go`, `sanitize.go`, `remoteimages.go`, `linkprivacy.go`,
`authresults.go`, `phishing.go`, `msgactions.go`, `invite.go`,
`unsubscribe.go`, `webviewjs.go` or `internal/remotecache`.

Other docs, all in `docs/`: [architecture.md](architecture.md) (every file),
[accounts.md](accounts.md) (accounts, sync, snooze), [reader.md](reader.md)
(the reader), [threadlist.md](threadlist.md) (list and search),
[compose.md](compose.md) (compose, drafts, outbox), [ai.md](ai.md) (AI
features), [window.md](window.md) (colour, status bar, shortcuts) and
[configuration.md](configuration.md); [SETUP.md](SETUP.md) and
[BENCHMARK.md](BENCHMARK.md) are for people setting up the app or choosing a
model. [AGENTS.md](../AGENTS.md) says which to read before which change.

## Opening a conversation

UI state (implemented): 3-pane shell renders the cached account live; opening a
conversation renders what is cached immediately and completes itself around
that. A Gmail conversation's full server-side message membership is hydrated
once in the background (`hydrateThread`, persisted in `thread_hydrations`, so a
capped backfill cannot leave older messages and their attachments invisible);
when it adds any, the resulting `MessageUpserted` re-renders the open
conversation through the ordinary change path. Only a conversation with nothing
cached waits for that lookup, since there it is the only way to open it at all.

## Fetching and rendering bodies

The reader lazily fetches + sanitizes + renders its bodies (WebKit; remote images load through a hardened local cache,
with a global privacy opt-out). A render fetches the thread's missing bodies
concurrently and claims them (`renderFetching`), so the `MessageBodyFetched`
events it causes are recognised as its own echo: acting on them would re-enter
`renderConversation`, whose cancel of the previous render would abort the
sibling fetches still in flight and turn one parallel pass into a round trip per
message. A body fetched by anything else, or after the render finished, still
re-renders the open conversation.

## Sanitizing and email CSS

The reader sanitizes with an email-tuned bluemonday policy
(`emailPolicy`, keeps inline styles + tables so HTML mail isn't broken). The
sanitizer drops `<style>`, so each block's CSS is re-added scoped to that
message (`scopeEmailStyleBlocks` → `scopeEmailCSS`), and everything about that
path is built for CSS that is malformed, because email CSS is: blocks are
scoped **one at a time** so an unbalanced brace in one costs only that block;
a stylesheet douceur cannot parse is retried **rule by rule**
(`parseRulesIndividually`, over `splitTopLevelRules`) so only the bad rule is
lost, the way a browser recovers; and every parse goes through the one hardened
door (`parseStylesheet`), which is also the only reason the app cannot be hung
by an email. douceur has exactly one input it makes no progress on — a `;` where
a selector belongs, which `parseQualifiedRule` and `parsePrelude` hand back and
forth forever — so `stripPreludeSemicolons` removes precisely that token
(keeping the ones declarations and `@import` need). It arrives via an
HTML-escaped `<style>`, where character references are not decoded and every
`&gt;` in a child selector leaves a `;` behind: a Ryanair itinerary shipped one
and pinned the render goroutine at 100% of a core for the life of the process,
with the conversation never appearing. `cssParseTimeout` is the backstop for a
pathology we have not seen, and `FuzzScopeEmailCSS` is what should find the next
one instead of a user.

## The shell page

The WebView loads **one persistent shell page** (`readerShellHTML`: styles, CSP, and
a fit-to-width script that scales each over-wide message body — its `.mbfit`
wrapper — to the pane individually, so a fixed-width signature table shrinks
only that email's body, never the app-owned header bands, gist cards, or the
thread's other messages) and every
conversation is swapped into it via script (`setReaderHTML` → `__mbSet`, an
innerHTML swap through a small cgo `evalJS` shim) — **never a navigation**, so
WebKit never tears down its composited surface and the reader cannot flash
black between messages (the failure mode of the earlier per-render `LoadHtml` +
white-cover approaches). JavaScript is enabled **only** for the shell script: a
strict CSP (`script-src` pinned to the shell's nonce, `default-src 'none'`)
plus the sanitizer (inserted innerHTML never executes scripts) mean no
email-supplied script can run or reach the network.

## Remote images and trackers

Remote images load by default, but tracking pixels are stripped before
render (`cleanEmailHTML`/`scopeEmailCSS`: tiny or hidden images and backgrounds,
hidden ancestors, and known open/read/pixel URL patterns across HTML and CSS) and
the count is surfaced as a "🛡 N trackers blocked" indicator. No image is
downloaded before the conversation is on screen: `resolveRemoteImages` rewrites
each reference to `mbcache:<key>` without a request (the key is
`remotecache.Key`, a hash of the normalized URL, so a document can name an image
the cache does not hold yet), and `serveRemoteImage` downloads it when WebKit
asks, finishing the scheme request when it lands — images fill in as they arrive
the way a browser loads a page, and a slow or dead image host delays only itself.
Inline `cid:` attachments work the same way (`inlineImageIndex` reads the
attachment rows, `serveCID` downloads on request), as does the calendar invite
behind a conversation (`detectInviteLater`). Both handlers share a six-slot
fetch semaphore, since our downloads bypass WebKit's own connection limits.
An image that can't be fetched is answered with a transparent pixel rather than
an error, so the reader never draws a broken-image glyph; the failure is traced
and nothing is said in the UI, because an expired campaign URL is not something
the reader can act on. The banner is therefore only ever an offer to load images
that were deliberately withheld — "Show images" behind the privacy opt-out, and
nothing else: there is no count at which the reader stops asking for pictures,
since one image already tells the sender the message was opened, so an
image-heavy newsletter loads like any other page. The image collector
parses CSS declarations and names only image-bearing properties; remote
`@import`, fonts, cursors, and other CSS resources are removed without a request.

## Links

A plain-text body (and the snippet fallback) is HTML-escaped into a `<pre>` with
bare http(s) URLs auto-linkified (`linkifyText` — explicit-scheme match only, so
no false positives or non-http schemes), so links in text-only mail (CI/cron/
monitoring notifications) are clickable and open externally like any other link.
Before a reader link is handed to the browser, `cleanExternalLink` silently strips
well-known non-functional campaign parameters and bypasses an obvious redirect
wrapper only when it carries one unambiguous absolute HTTP(S) destination. The
pass is local-only (it never resolves a redirect), unwraps at most three levels,
and leaves opaque, ambiguous, same-site application, and non-HTTP links alone;
left-click and the WebKit **Open Link in New Window** action share this path.

## Sender authentication and phishing

A sender-authentication badge shows Gmail's SPF/DKIM/DMARC verdict
(`ToBody` captures `Authentication-Results` into `raw_headers`; `parseAuthResults`
→ green verified / amber partial / red possible-spoof), plus deterministic
anti-phishing heuristics (`phishing.go`: display-name spoofing and deceptive
link text, compared at the registrable-domain level — no AI/network) surfaced as
an amber caution line; each message's ⋯ menu's "Check for phishing" additionally
runs an on-demand AI phishing analysis of that message (`analyzeMessage` →
`AnalyzeEmail` — verdict + reasons, fed the auth/heuristic signals, shown in the
shared AI card, titled with the sender when it isn't the newest message),
**persisted** per message
(`store.{SetAnalysis,Analysis}`, `message_analyses` table — the message + its
signals are immutable, so it's reused on re-open without re-running the AI).

## Thread order and quoted text

A thread is rendered newest-message-first, with quoted reply history collapsed
behind a native `<details>` "Show quoted text" toggle (`cleanEmailHTML`, same single HTML pass as tracker stripping, no JS).

## Calendar invites, unsubscribe and headers

**Calendar invites**: an `.ics` attachment (parsed by the dependency-free `internal/ics`) renders an event card above the
conversation with Accept/Maybe/Decline that email the iTIP REPLY to the
organizer via the normal outbox path; CANCEL shows a note. **Unsubscribe**:
`List-Unsubscribe(-Post)` headers are captured at metadata fetch (both
backends, `messages.list_unsubscribe` columns); the reader overflow offers
Unsubscribe (RFC 8058 one-click POST > mailto via outbox > browser URL) and
the thread-list menu's "Subscriptions…" dialog (`store.Subscriptions`) groups
list senders by volume with per-sender unsubscribe. Message headers show the sender's
full address ("Name <addr>"), not just the display name.

## Reader actions and zoom

Reader actions archive / mark-unread / star / move-to-inbox / report-spam (or not-spam in the Spam
folder), plus "Delete forever" in Trash/Spam and an "Empty now" banner that empties
the whole folder (`DeletePermanently`/`EmptyLabel` → `messages.batchDelete`), via
optimistic `ModifyLabels` + Gmail mirror; opening an unread message marks it read;
Ctrl +/-/0 zoom the email bodies (persisted; implemented inside the shell's
fit pass — `__mbZoom` lays each body out at avail/zoom and scales it by
exactly zoom× its fit-to-width size, so every email's text grows while the
reader's own header bands and gist cards keep their size; reflowing mail
re-wraps and never overflows, while a width-locked body grows past the pane
and the page pans horizontally — the html clip flips to auto and a styled
always-visible scrollbar sits at the reader viewport's bottom edge (a
per-body scroller put it at the bottom of the message, screens away on a
tall email) — the browser bargain, chosen over a "cap at the pane" rule
under which zoom silently did nothing on exactly those emails — never
`WebView.SetZoomLevel`, whose page zoom scales the app chrome too). A touchpad
pinch drives the same zoom (`buildReaderPinch`: live while pinching, settled on
the nearest 10% when the fingers lift) and WebKit's own `GtkGestureZoom` is set
to `PhaseNone`: its magnification scaled the whole page, chrome included,
without re-laying it out, so the right edge fell off the pane behind a
horizontal scrollbar, and since the reader never navigates it survived every
conversation and Ctrl+0. Off 100%, a flat "110%" header-bar button
(`zoomBtn`, `refreshZoomBadge`) shows the level and resets it on click;

## Incoming attachments

Incoming attachments are extracted on body fetch (`ReplaceAttachments`) and shown as chips
in the reader. A part is treated as an inline image — rendered in the body, no
chip, no paperclip — only when the body actually references its Content-ID
(`model.ReferencedCIDs`/`IsInlineAttachment`, shared by the store and the
reader): forwarding makes Gmail stamp a Content-ID on every part, so keying on
its presence alone hid a forwarded PDF completely; clicking one downloads it (content-addressed under the cache dir)
and opens it with `xdg-open`.

## Message layout and action scope

Opening a thread renders all its messages stacked in the reader (bodies fetched lazily,
each a sanitized section). A message is one header plus its body: the header
(`.mbhead` — space above it, a hairline, and the sender's name in full-ink
semibold; no fill, since a stack of tinted headers reads as slabs and no email
footer ever puts a bold name straight under a rule — the per-message AI gist
keeps its tinted card, margined to the same width as the body's content box so
it can't overhang the email or reach under the scrollbar) doubles as the `<details>`
summary for every
message but the newest, so folding one shows the same header rather than a
second one introducing it — folded it shows sender + snippet, open it shows
sender + address + recipients (`composeSection`). The band must stay inside the
content width: the fit-to-width pass only rescues message bodies (`.mbfit`), so
app chrome bled past the pane edges is simply clipped by `overflow-x:hidden`.
The whole reader pane sits on one text
column at 16px: filled surfaces (the thread-summary and invite cards outside the
WebView, the header band and gist inside it) are inset 6px and pad 10px — 9px
where a 1px border makes up the difference — while plain rows (subject,
attachment chips, the phishing caution) are simply margined 16px, and the body
(`.mbbody`) pads to the same column with the disclosure arrow hanging in the
band's padding. Change one of those numbers and the pane grows a second left
edge. Links inside a summary follow without toggling the
disclosure (WebKit skips activation for interactive descendants), so the
per-message ⋯ works on a folded message too; the `toggle` event refits, since
folding changes the height the scaled layout baked in. Archive/trash apply to the whole thread, the header
reply button (a SplitButton defaulting to **Reply all** — sender-only Reply and
Forward in its dropdown, `r` = reply all) and star to its newest message, and
each stacked message carries an always-visible dim ⋯ in its header
(`msgMenuIcon`, an inline Adwaita SVG; the `mbaction:menu/<id>` link is
intercepted by `onDecidePolicy` → `showMessageMenu`, a native popover — the
rowmenu pattern — anchored at the pointer via a motion controller on the
WebView) offering Reply all / Reply / Forward / View headers / Check for
phishing targeting that specific message
(`replyToMessage`/`forwardMessage`/`viewMessageHeaders`/`analyzeMessage`), so
any message in the thread can be answered, forwarded, or inspected, not just
the last. **Action-surface scope model**: the reader header bar and its
overflow ⋯ act on the conversation (reply-all = continue the conversation via
its newest message; the overflow holds star/unread/move/spam/trash, Labels…,
Unsubscribe, Print…, remote-images — never message- or sender-scoped items),
each message's ⋯ acts on that message, and clicking a sender name opens the
canonical sender surface (`showSenderActions`: copy address, find emails, and
trust images — the link underlines on hover so it reads as
clickable).
