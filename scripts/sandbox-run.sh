#!/usr/bin/env bash
# sandbox-run.sh — launch mailbox in an isolated sandbox built from a COPY of the
# live database and config, so verification never touches or interrupts a running
# instance.
#
# It copies ~/.local/share/mailbox and ~/.config/mailbox into a temp dir, points
# the XDG dirs there, runs under a virtual X display (Xvfb), and registers under a
# distinct app id (MAILBOX_APP_ID) on the *real* session bus — so the OS keyring
# still resolves OAuth tokens and the AI key, but launching it starts a fresh
# instance instead of activating the user's running app.
#
# Usage:
#   scripts/sandbox-run.sh [--seconds N] [--shot FILE] [--keep]
# Any MAILBOX_* test hooks (MAILBOX_OPEN_FIRST, MAILBOX_SEARCH, MAILBOX_WIN_SIZE,
# …) set in the environment are passed through to the app.
#
#   scripts/sandbox-run.sh --seconds 16 --shot /tmp/out.png      # screenshot then exit
#   MAILBOX_OPEN_FIRST=1 scripts/sandbox-run.sh --seconds 18 --shot /tmp/out.png
set -euo pipefail

SECONDS_RUN=16
SHOT=""
KEEP=0
while [ $# -gt 0 ]; do
  case "$1" in
    --seconds) SECONDS_RUN="$2"; shift 2 ;;
    --shot)    SHOT="$2"; shift 2 ;;
    --keep)    KEEP=1; shift ;;
    *) echo "unknown arg: $1" >&2; exit 2 ;;
  esac
done

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BIN="$ROOT/bin/mailbox"
[ -x "$BIN" ] || { echo "build first: make build"; exit 1; }

# Session bus + runtime dir (needed for the keyring); fall back to the standard
# per-user paths when the caller's environment doesn't carry them.
export XDG_RUNTIME_DIR="${XDG_RUNTIME_DIR:-/run/user/$(id -u)}"
export DBUS_SESSION_BUS_ADDRESS="${DBUS_SESSION_BUS_ADDRESS:-unix:path=$XDG_RUNTIME_DIR/bus}"

SBX="$(mktemp -d /tmp/mailbox-sandbox.XXXXXX)"
export XDG_DATA_HOME="$SBX/data" XDG_CONFIG_HOME="$SBX/config" XDG_CACHE_HOME="$SBX/cache"
mkdir -p "$XDG_DATA_HOME" "$XDG_CONFIG_HOME" "$XDG_CACHE_HOME"

# Copy the live data + config (DB, accounts.json, view.json, prefs, signature).
LIVE_DATA="${HOME}/.local/share/mailbox"
LIVE_CONFIG="${HOME}/.config/mailbox"
[ -d "$LIVE_DATA" ]   && cp -a "$LIVE_DATA"   "$XDG_DATA_HOME/mailbox"   || mkdir -p "$XDG_DATA_HOME/mailbox"
[ -d "$LIVE_CONFIG" ] && cp -a "$LIVE_CONFIG" "$XDG_CONFIG_HOME/mailbox" || mkdir -p "$XDG_CONFIG_HOME/mailbox"

# Distinct app id → coexists with the running instance on the shared session bus.
export MAILBOX_APP_ID="${MAILBOX_APP_ID:-com.surfly.mailbox.sandbox}"
export GDK_BACKEND=x11 GSK_RENDERER=cairo

DISP=":$(( (RANDOM % 80) + 120 ))"
Xvfb "$DISP" -screen 0 1366x880x24 >/dev/null 2>&1 &
XPID=$!
cleanup() {
  kill "${APP:-}" 2>/dev/null || true
  kill "$XPID" 2>/dev/null || true
  [ "$KEEP" -eq 1 ] || rm -rf "$SBX"
}
trap cleanup EXIT
sleep 1.5

echo "sandbox: dir=$SBX app-id=$MAILBOX_APP_ID display=$DISP"
DISPLAY="$DISP" "$BIN" --trace >/dev/null 2>&1 &
APP=$!
sleep "$SECONDS_RUN"

if [ -n "$SHOT" ]; then
  DISPLAY="$DISP" import -window root "$SHOT" 2>/dev/null && echo "shot: $SHOT"
fi
if [ "$KEEP" -eq 1 ]; then
  echo "leaving sandbox running (pid $APP); dir kept at $SBX"
  trap - EXIT
  wait "$APP"
fi
