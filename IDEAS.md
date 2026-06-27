# IDEAS

Discoveries and deferred work we want to keep for later — not committed to yet.
Move an item into a commit (and delete it here) when it's done.

## Performance

- **Fully-paged thread list.** The list is a `gtk.ListView` over a `gtk.StringList`
  of thread ids, capped at `threadListCap` (5000), refreshed incrementally
  (`diffThreadModel`). The original plan called for a windowed `gio.ListModel`
  (`GetNItems` = COUNT(*), `GetItem` pages SQL with an LRU). We deferred it: at the
  5000 cap the incremental diff already removes the per-sync churn and the memory
  (~1.5 MB of summaries) is fine, so a custom GListModel GObject subclass isn't worth
  the gotk4 complexity / regression risk. **Revisit if** users routinely have a
  single label / All-Mail far beyond 5000 threads and want them all scrollable, or
  the summary map's memory becomes a problem. Build it behind the existing
  `showThreads`/`diffThreadModel` surface.

- **Rendered-section cache for instant thread re-open.** Opening a thread re-fetches
  each body from SQLite and re-sanitizes it (bluemonday ~10 ms/msg). A small in-memory
  LRU of the rendered section HTML keyed by message rowid + `body_fetched` would make
  re-opening a recently-viewed thread instant. Watch memory; invalidate on body update.

- **Prepared-statement reuse.** The store re-parses each SQL string per call. Caching
  prepared statements for the hot queries would shave parse overhead. Low impact per
  the audit; do only if profiling shows it.

## Features (noticed while working, not requested yet)

- Hover row actions (archive/star on row hover) in the thread list.
- Per-account signature (currently one global signature).
- Capture `Reply-To` and honor it when replying.
