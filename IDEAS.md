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

- **Prepared-statement reuse.** The store re-parses each SQL string per call. Caching
  prepared statements for the hot queries would shave parse overhead. Low impact per
  the audit; do only if profiling shows it.

## Icons

- **Custom translate icon ("A文").** The translate action uses the stock
  `accessories-character-map-symbolic` (a character grid — weak metaphor). A custom
  "A文" glyph would be clearer. First attempt used a stroked SVG (`fill="none"`,
  `stroke=…`) which GTK's *-symbolic recolor pipeline rendered as a tiny blob — it
  needs **filled paths** (like `mail-archive-symbolic`/`palm-tree-symbolic`). Redo
  "A" (with its counter) and "文" as filled outlines and render-test at 16px.

## Features (noticed while working, not requested yet)

- Hover row actions (archive/star on row hover) in the thread list.
- Per-account signature (currently one global signature).
- Capture `Reply-To` and honor it when replying.
