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

## Gotchas

- Kill by PID you saved at launch; `pgrep -x mailbox` can also match a real
  running instance — check `/proc/<pid>/environ` for `MAILBOX_APP_ID` first.
- The sandbox truncates `/tmp/mailbox.log`; if you need the real session's
  trace, copy it before launching.
