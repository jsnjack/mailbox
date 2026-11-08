# The thread list and search

The middle pane: conversations grouped in a virtualized list that is diffed
rather than rebuilt, local and server search with filters, paging, refreshes
that keep the scroll position and never let a stale result win, selection
mode, bulk actions, and the Undo toasts. Read it before changing
`internal/ui/threadlist.go`, `searchfilters.go`, or the list and search
queries in `internal/store`.

Other docs, all in `docs/`: [architecture.md](architecture.md) (every file),
[accounts.md](accounts.md) (accounts, sync, snooze), [reader.md](reader.md)
(the reader), [threadlist.md](threadlist.md) (list and search),
[compose.md](compose.md) (compose, drafts, outbox), [ai.md](ai.md) (AI
features), [window.md](window.md) (colour, status bar, shortcuts) and
[configuration.md](configuration.md); [SETUP.md](SETUP.md) and
[BENCHMARK.md](BENCHMARK.md) are for people setting up the app or choosing a
model. [AGENTS.md](../AGENTS.md) says which to read before which change.

## The conversation list

The list is grouped by conversation: a virtualized `gtk.ListView` over a
`gtk.StringList` of thread ids (looked up in a `threadByID` map of
`model.ThreadSummary`); rows show the newest message + a count. Refreshes are
incremental (`diffThreadModel`): an unchanged list (the common idle-sync case)
does no work, an in-place change (mark-read, star, a new AI category tag) re-
binds only the affected rows via a per-row signature (`renderSig`), and only a
changed set/order triggers a full splice — so the list keeps its scroll position
instead of rebuilding on every event. `store`
provides `ListThreadsByLabel`/`ListThreadMessages`/`GetThreadSummaries`.

## Search

The thread-list header's search toggle, Ctrl+F, and `/` reveal a
native `GtkSearchBar`; Escape or the toggle hides it, clears the query, and
returns to the current label. Its entry runs instant local FTS5 search
(`store.SearchPage`, sanitized into a quoted prefix MATCH) whose hits are
grouped into threads. The temporary search surface names the current scope and
loaded result count; a Relevant/Newest selector defaults to the FTS/provider
relevance order. Explicit Account and Cached mail/All mail on server selectors
control the scope; Enter keeps the chosen scope. Expanding Filters offers sender,
after/before dates, unread, and attachments, and reads any existing operators
from the query. Local search implements those operators; IMAP maps date/unread
filters to SEARCH criteria and preserves quoted sender names. IMAP attachment
filtering is explicitly cache-only because it has no server SEARCH mapping.

## Paging and refresh

Conversation lists load in bounded 100-item pages as the user approaches the bottom: label and All Mail views use
stable `(internal_date,rowid)` keyset cursors, local FTS advances by raw-hit
offset (several hits may belong to one conversation), and provider search keeps
the provider's continuation token opaque (`SearchPager` → Gmail's
`nextPageToken`; backends without it get a bounded compatibility cursor).
Every page query runs off the GTK thread and is guarded by `refreshGen` so a slow
result cannot overwrite a newer account/folder/query (last request wins); an
in-flight query is context-cancelled only by the replacement that supersedes it
(`startThreadLoad`), and a cancelled result is discarded rather than reported as
a failure. A refresh arriving while the identical query is already running is
neither dropped nor allowed to cancel it (`coalesceThreadLoad`): its answer
predates the change that asked for it — an archive's optimistic label edit would
still be listed — so the load is re-run when the in-flight one lands, and
`afterPopulate` (advancing the selection past an archived conversation) waits for
that re-run. Appending a normal
date-ordered page splices only the new `gtk.StringList` tail, preserving realized
rows, selection, scroll position, and date headers. A failed continuation leaves
the loaded rows intact and reveals a small Retry footer. Live sync refreshes
re-query the depth the user already reached rather than collapsing the list back
to its first page; a server-search refresh rehydrates only its already-loaded
ids and does not restart the provider cursor. Search hits become summaries with
one batched `store.GetThreadSummaries` read (server ids first map through
`store.ThreadIDsForMessages`) rather than a query per hit. When a search has no
local matches, "Search all mail" starts the paged provider search and caches only
each requested page; a reader action "Find emails from sender" uses the same path
with a `from:` query. Server-search results persist a
`serverSearch`/`serverQuery` mode so the debounced search-changed signal and 60s
background refreshes don't clobber them with local FTS results.

## Selection, bulk actions and Undo

A selection-mode toggle turns rows into checkboxes with a bulk-action bar (Archive / Trash / Mark read),
applying the change to every selected conversation in one batched `ModifyLabels`
call (`bulkApply`). Label changes from the bulk bar and the right-click row menu
(`threadModifyAll`) show the same reversing "Undo" toast (`showUndoToast`) as the
reader's archive/trash, so every archive/trash is recoverable. Undo toasts
coalesce: a burst of same-kind changes (rapid j/a triage) replaces the pending
toast with one cumulative "Archived N conversations" whose Undo reverses the
whole burst — ToastOverlay queues toasts, so 80 individual archives would
otherwise drain one 6s toast at a time for minutes; a single change names its
conversation ("Archived “subject…”", `undoTitle`). Switching accounts blanks
the thread list immediately (the reload is async last-request-wins; under
heavy churn the old account's threads would otherwise linger for seconds) and
traces the click→content latency ("switch account content visible").
