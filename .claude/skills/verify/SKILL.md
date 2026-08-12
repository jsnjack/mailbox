---
name: verify
description: Drive the mailbox GTK app in a throwaway sandbox to verify a change end-to-end — build, launch under Xvfb with copied live data, drive the flow, screenshot. Use after any change to product code (ui, sync, backend, auth, store).
---

# Verify mailbox changes live

The app is verified in a sandbox (per AGENTS.md): a copy of the live DB +
config under temp XDG dirs, a fresh Xvfb display, and a distinct app id so it
never touches a running real instance. It shares the session DBus, so keyring
tokens and the AI key still resolve — real Gmail fetches work.

## Recipe

```bash
make build                       # bin/mailbox (cgo cached after first build)
SB=$(mktemp -d)/sandbox && mkdir -p $SB/{data,config,cache}
cp -r ~/.local/share/mailbox $SB/data/mailbox
cp -r ~/.config/mailbox   $SB/config/mailbox

Xvfb :77 -screen 0 1400x900x24 &   # pick a free display
sbx_stop                           # ALWAYS, before every launch — see Gotchas

XDG_DATA_HOME=$SB/data XDG_CONFIG_HOME=$SB/config XDG_CACHE_HOME=$SB/cache \
DISPLAY=:77 GDK_BACKEND=x11 GSK_RENDERER=cairo \
MAILBOX_APP_ID=com.jsnjack.mailbox.sandbox MAILBOX_DEMO=1 \
MAILBOX_OPEN_FIRST=1 MAILBOX_WIN_SIZE=1400x900 \
./bin/mailbox --trace > $SB/stdout.log 2>&1 &
```

- Evidence: `DISPLAY=:77 import -window root shot.png` (ImageMagick; no
  xdotool on this machine — drive via test hooks + DB state, not synthetic input).
- `/tmp/mailbox.log` is the trace (truncated each start — including by the
  sandbox, so a real session's log is lost once the sandbox launches).
  `grep` it for the code path you changed; every branch traces.

## Useful hooks

- `MAILBOX_OPEN_FIRST=1` opens the newest message on launch — combined with
  clearing its cached body it exercises the live body-fetch path:
  ```sql
  DELETE FROM message_bodies WHERE message_rowid IN
    (SELECT message_rowid FROM message_labels WHERE label_id='INBOX');
  UPDATE messages SET body_fetched=0 WHERE rowid IN
    (SELECT message_rowid FROM message_labels WHERE label_id='INBOX');
  ```
- `MAILBOX_OPEN_PREFS=1` opens Preferences on launch.
- Simulate a dead/hung network (stuck connection, suspend/resume): launch with
  `HTTPS_PROXY=http://127.0.0.1:3128` pointing at a hanging proxy (a python
  socket server that accepts and never replies). Token refresh + all Gmail
  API calls hang at CONNECT; watch the trace for the bounded-timeout recovery.

## Driving the UI (no xdotool on this machine)

python3-Xlib with XTEST works for clicks/keys/typing — see the drive.py pattern
(fake_input KeyPress/ButtonPress against DISPLAY=:77). Hard-won gotchas:

- Xvfb has NO window manager → X input focus is PointerRoot: **keys go to the
  window under the pointer**, not the newest window. Move/click the pointer
  onto a window before typing at it, or keystrokes land in the main window and
  trigger single-key shortcuts (archive/reply/translate…) on real mail.
- The WebKit reader consumes keys when focused — click a thread-list row first
  if a shortcut doesn't fire.
- Shortcuts are user-rebound via `~/.config/mailbox/shortcuts.json` (copied
  into the sandbox with the config!) — check it before assuming defaults;
  archive is currently `a` only, not `a`/`e`.
- Always screenshot-checkpoint before clicking Send or any destructive button.
- Notifications: the sandbox app id has no .desktop entry, so GNOME drops its
  banners (nothing pops on the real desktop); verify via the trace
  ("ui: notify new mail" / "ui: notification gist") or dbus-monitor with a
  method_call match rule (gdbus monitor only shows signals — useless here).

## Gotchas

- **The keyring is NOT sandboxed** (it rides the shared session DBus, not XDG).
  Preferences → AI mirrors the real keyring: closing the dialog with the key
  row cleared DELETES the real key, and typing a key stores it for real. Before
  driving a prefs-close in a sandbox, check whether a real AI key exists
  (`secret-tool lookup service mailbox-ai` semantics) and don't clear the row.

- **A leftover sandbox instance absorbs your launch.** GApplication is
  single-instance per app id on the session bus: if an earlier sandbox with the
  same `MAILBOX_APP_ID` is still running (even from a past session), a new
  launch just activates it and exits — you screenshot the OLD binary and the
  change appears to be missing/unverified.

  Stopping it is where this goes wrong, in three ways that all fail *silently*:

  - **`/proc/<pid>/exe` reads `…/bin/mailbox (deleted)` after any rebuild**,
    because `go build` replaced the file the process is running. An exact
    string match on the exe path stops matching every instance you started
    before the last build, so the kill quietly does nothing.
  - **`pkill -f 'MAILBOX_APP_ID=…'` matches the killing shell itself** — its own
    command line contains the pattern — so the shell dies (exit 144, no output)
    while the app lives on.
  - **Killing is not instant.** Launch immediately after and the new process
    hands off to the still-dying one.

  Match on the environment instead, and wait for the process to actually go:

  ```bash
  sbx_pids() {   # every sandbox process, whatever happened to its binary
    for p in /proc/[0-9]*; do
      grep -qz 'MAILBOX_APP_ID=com.jsnjack.mailbox.sandbox' "$p/environ" 2>/dev/null &&
        echo "${p#/proc/}"
    done
  }
  sbx_stop() {
    for pid in $(sbx_pids); do kill "$pid" 2>/dev/null; done
    for _ in $(seq 30); do [ -z "$(sbx_pids)" ] && return 0; sleep 1; done
    for pid in $(sbx_pids); do kill -9 "$pid" 2>/dev/null; done; sleep 2
  }
  ```

  It also catches the WebKit helper processes, which inherit the environment.
  Keep `MAILBOX_APP_ID` inline on the launch command — `export` it in your
  driving shell and the shell matches itself again.

  **The symptom to recognise:** an edit has no visible effect no matter what you
  change. Do not start theorising about GTK; prove which binary is on screen by
  putting an absurd marker in the UI (`gtk.NewLabel("ZZZ")`), rebuilding, and
  relaunching. If ZZZ doesn't appear, you are screenshotting a stale instance —
  every before/after comparison you made since the last rebuild is void.

- The sandbox truncates `/tmp/mailbox.log`; if you need the real session's
  trace, copy it before launching.
