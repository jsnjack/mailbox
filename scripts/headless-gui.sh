#!/usr/bin/env bash
# headless-gui.sh CMD [ARGS...] - run a GUI test suite, smoke check or
# screenshot script so that nothing it opens appears on the desktop and nothing
# it does reaches the desktop session. The command gets:
#
#   - a private X server (xvfb-run) and no Wayland display, so its windows, and
#     any browser or dialog it starts, open there instead of on the desktop;
#   - a private, empty XDG_RUNTIME_DIR, so it cannot reach the desktop's audio,
#     keyring, Wayland or systemd sockets;
#   - a private D-Bus session bus. Services it starts (portals, gvfs) see only
#     the private runtime directory, so a document portal mounts its doc
#     filesystem there instead of over the desktop's /run/user/$UID/doc;
#   - in-memory GSettings, local-only GIO and stub link launchers.
#
# A D-Bus session that starts the document portal while sharing /run/user/$UID
# unmounts the desktop's /run/user/$UID/doc, and Flatpak apps then fail with
# "bwrap: Can't find source path /run/user/$UID/doc/by-app/...". This wrapper
# fails the run if that mount disappeared while it ran.
#
# Copied from https://github.com/jsnjack/standards (headless-gui.sh); see
# "GUI tests" in AGENTS.universal.md.
#
#   HEADLESS_SCREEN           virtual screen size, default 1500x950
#   HEADLESS_GSK_RENDERER     GTK renderer, default cairo: Xvfb has no GL, and
#                             a desktop's GSK_RENDERER=gl must not leak in
#   HEADLESS_SHARE_RUNTIME=1  keep the desktop's XDG_RUNTIME_DIR, for commands
#                             that need it (rootless Podman). The bus then
#                             cannot start any service, so no portal starts.
set -euo pipefail

if [[ $# -eq 0 ]]; then
	echo "usage: headless-gui.sh CMD [ARGS...]" >&2
	exit 2
fi
for tool in xvfb-run dbus-run-session; do
	if ! command -v "$tool" >/dev/null; then
		echo "headless-gui: $tool is not installed. Install it with:" >&2
		echo "  sudo dnf install xorg-x11-server-Xvfb dbus-daemon" >&2
		exit 1
	fi
done

desktop_doc=/run/user/$(id -u)/doc
had_doc=
if mountpoint -q "$desktop_doc" 2>/dev/null; then
	had_doc=1
fi

dir=$(mktemp -d /tmp/headless-gui.XXXXXX)
cleanup() {
	local status=$?
	# Services started on the private bus exit after it does; the document
	# portal among them unmounts the private doc filesystem on its way out.
	for _ in $(seq 50); do
		mountpoint -q "$dir/runtime/doc" 2>/dev/null || break
		sleep 0.1
	done
	if mountpoint -q "$dir/runtime/doc" 2>/dev/null; then
		fusermount3 -u -z "$dir/runtime/doc" 2>/dev/null || true
	fi
	rm -rf --one-file-system -- "$dir" 2>/dev/null || true
	if [[ -n $had_doc ]] && ! mountpoint -q "$desktop_doc"; then
		echo "headless-gui: $desktop_doc was unmounted during this run, so Flatpak apps cannot start." >&2
		echo "headless-gui: restore it with: systemctl --user restart xdg-document-portal.service" >&2
		status=1
	fi
	exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

mkdir -m 0700 "$dir/runtime" "$dir/bin"

# A clicked link must not open a browser; the stubs record what was asked.
for cmd in xdg-open gio gnome-www-browser x-www-browser sensible-browser firefox epiphany; do
	printf '#!/bin/sh\necho "%s $*" >>"%s/opened.log"\n' "$cmd" "$dir" >"$dir/bin/$cmd"
	chmod +x "$dir/bin/$cmd"
done

bus=()
if [[ ${HEADLESS_SHARE_RUNTIME:-} == 1 ]]; then
	# No <servicedir>: nothing can be activated on this bus, so no portal can
	# start in the desktop's runtime directory.
	cat >"$dir/bus.conf" <<-'EOF'
		<!DOCTYPE busconfig PUBLIC "-//freedesktop//DTD D-Bus Bus Configuration 1.0//EN"
		 "http://www.freedesktop.org/standards/dbus/1.0/busconfig.dtd">
		<busconfig>
		  <type>session</type>
		  <keep_umask/>
		  <listen>unix:tmpdir=/tmp</listen>
		  <auth>EXTERNAL</auth>
		  <policy context="default">
		    <allow send_destination="*" eavesdrop="true"/>
		    <allow eavesdrop="true"/>
		    <allow own="*"/>
		  </policy>
		</busconfig>
	EOF
	bus=(--config-file="$dir/bus.conf")
else
	export XDG_RUNTIME_DIR=$dir/runtime
fi
unset WAYLAND_DISPLAY WAYLAND_SOCKET
export PATH="$dir/bin:$PATH" BROWSER="$dir/bin/xdg-open" \
	GDK_BACKEND=x11 GSK_RENDERER="${HEADLESS_GSK_RENDERER:-cairo}" \
	GSETTINGS_BACKEND=memory GIO_USE_VFS=local ADW_DISABLE_PORTAL=1 \
	GTK_A11Y=none NO_AT_BRIDGE=1

dbus-run-session "${bus[@]}" -- \
	xvfb-run -a -s "-screen 0 ${HEADLESS_SCREEN:-1500x950}x24" "$@"
