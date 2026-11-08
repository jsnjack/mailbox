# The window: colour, status bar, layout and shortcuts

The parts of the shell around the panes: the colour rules, the status bar and
its activity log, how the AI model that served a request is shown, the
responsive layout, single-key shortcuts, and the environment variables that
tests and sandboxes use. Read it before changing the shell in
`internal/ui/window.go`, `theme.go`, `statusbar.go`, `activityrow.go`,
`shortcuts.go`, or `internal/activity`.

Other docs, all in `docs/`: [architecture.md](architecture.md) (every file),
[accounts.md](accounts.md) (accounts, sync, snooze), [reader.md](reader.md)
(the reader), [threadlist.md](threadlist.md) (list and search),
[compose.md](compose.md) (compose, drafts, outbox), [ai.md](ai.md) (AI
features), [window.md](window.md) (colour, status bar, shortcuts) and
[configuration.md](configuration.md); [SETUP.md](SETUP.md) and
[BENCHMARK.md](BENCHMARK.md) are for people setting up the app or choosing a
model. [AGENTS.md](../AGENTS.md) says which to read before which change.

## Colour

Colour follows GNOME's HIG (monochrome symbolic icons, one accent reserved for
state): a small application stylesheet (`internal/ui/theme.go`, registered on
the default display in `build`) tints only three things, all from the system
`@accent_color` family so they track the accent and light/dark — count pills
(`countBadge`/`.badge-pill`), the small unread dot on a conversation, and the
soft accent-tinted AI summary card. Folder icons stay the theme foreground and
the account switcher is plain text (no avatars).

## Status bar and activity log

A bottom status bar shows what the app is doing — the current
operation with a spinner/progress bar (left) and live cumulative metrics (right:
bytes transferred, Gmail API requests + quota units, AI requests + AI bytes
(`Assistant.Requests`/`Transferred` — counted in the Assistant so they survive a
live provider swap), DB size, cached-message count) — plus an expandable
structured activity log: one dense line per operation (monospace clock, a
neutral kind chip — SYNC/AI/FETCH/SEND/SEARCH/ATTACH/DRAFT/MAIL — an account
chip, the label, the result note inline and dim, right-aligned duration),
inserted at Start and finished in place (✓/✗/–, error-tinted note on failure;
capped at 200 rows). Completed notes have a Details button that opens the full
text in a wrapping, scrollable dialog with selection and a Copy button, so long
provider errors and URLs remain accessible even when tooltips are clipped.
Every account-scoped event carries `Event.Account` (the
email — `activity.Hub.Begin/Report` take it; app-wide work passes ""); the UI
renders it as the account's display name when one is set. Labels speak in
consistent human phrases, never raw ids or fragments: live operations are
gerunds ("Checking mail", "Summarizing thread" — right both while running in
the bar and as a completed ✓ row), instant reports are past tense ("Archived",
"Snoozed until Jul 20 09:00", "Returned from snooze"), label mutations render
as the action taken with friendly label names (`labelChangeText`: "Archived",
"Marked read", "Filed to Receipts" — user-label ids resolved via
`store.LabelName`), and counts are properly pluralized (`activity.Plural`).
Quiet mail checks ("up to date", every minute per account) keep only their
newest row (`statusQuietRows`), so the log records events rather than
wallpaper. The bottom bar, which has no chips, shows the label plus the
account (`barText`: "Checking mail · Work").
AI notes are suffixed with the model that served the request
(`Assistant.ActiveModel` — with a failover chain, the entry the request
committed to, so a silent fallback is visible; rendered via `ai.ShortModel`,
the bare name shortened to first-10…last-5 runes — the full "model @ host"
stays in traces). A change in the serving model — a failover, a recovery, a
live settings swap — additionally logs its own row
(`Assistant.SetOnModelChange` → "AI model · <new> (was <old>)"), and the
popover's Session section names the current model (`Assistant.ModelStatus`,
marked "(fallback)" when a non-primary chain entry served last).
Every AI op
is bracketed into it via `aiActivity` (including the background categorizer);
label mutations, outbox sweeps that delivered,
retention prunes, and woken snoozes log as instant completed rows via
`activity.Hub.Report`. Background layers report
transient activity to a headless `activity.Hub` (`deps.Activity`) that the bar
drains via `dispatch`; the AI ops the UI brackets directly; metrics come from
per-account `gmailapi.Stats` (a byte-counting transport + request/quota counters)
aggregated by `deps.Stats`.

## Responsive layout, shortcuts and test hooks

The window collapses responsively via `adw.Breakpoint` (3 panes → list+reader below ~860sp → single pane below ~520sp),
with `SetShowContent` driving navigation when collapsed. Single-key shortcuts
(bubble-phase key controller, so text entries keep their input): j/k navigate
threads, r reply, a/e archive, c compose, / focus search, and more ("?" shows
the cheat sheet) — all user-rebindable (Preferences → Keyboard; the
`shortcutDefs` table drives the handler, the cheat sheet, and the editor;
overrides in `~/.config/mailbox/shortcuts.json`). Rebinding rejects conflicts,
names the existing action, and reserves `?` for the shortcuts window; failed
writes do not report success. Test hooks:
`MAILBOX_OPEN_FIRST=1` opens the newest message on launch; `MAILBOX_OPEN_PREFS=1`
opens the Preferences dialog on launch; `MAILBOX_WIN_SIZE=WxH`
overrides the initial window size; `MAILBOX_APP_ID` overrides the GApplication id
so a sandbox instance runs alongside a real one instead of activating it.
