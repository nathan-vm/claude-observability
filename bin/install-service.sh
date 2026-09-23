#!/usr/bin/env bash
# Installs/uninstalls collector.mjs as a macOS launchd LaunchAgent — the host
# service equivalent of what dashboard-generator and transcript-exporter used
# to be as Docker services. See docker-compose.yaml and README.md's "The
# collector service" for why this can't be a container: usage-truth.mjs needs
# the host's own logged-in `claude` CLI, and transcript scanning needs the
# real Claude Code config directories — neither reachable from node:22-alpine.
#
# There is no systemd here on purpose: this machine is macOS (Darwin), which
# doesn't have it. launchd is the native equivalent.
#
# Usage:
#   bin/install-service.sh install     # write + load the LaunchAgent
#   bin/install-service.sh uninstall   # unload + remove it
#   bin/install-service.sh restart     # unload, then load again (e.g. after
#                                       #   editing your shell rc, or upgrading Node)
#   bin/install-service.sh status      # launchctl state + recent log tail

set -euo pipefail

# The collector's own settings (CLAUDE_DIR, EXPORTER_STREAM, ...) are plain
# env vars bin/agent-setup writes into your shell rc, alongside the OTel
# telemetry vars — there is no config file. launchd does not run a login or
# interactive shell, so it never sees them on its own; this list is what gets
# resolved from your actual shell (see resolve_collector_env below) and baked
# into the plist at install time. Missing values just mean collector.mjs
# falls back to its own built-in defaults.
COLLECTOR_ENV_VARS="CLAUDE_DIR CLAUDE_OBSERVABILITY_EXTRA_DIRS EXPORTER_STREAM RATE_HALFLIFE POLL_SECONDS DASHBOARD_INTERVAL_SECONDS WEEK_START_DAY WEEK_START_HOUR TZ_OFFSET_HOURS"

SOURCE="${BASH_SOURCE[0]}"
while [ -h "$SOURCE" ]; do
  DIR="$(cd -P "$(dirname "$SOURCE")" && pwd)"
  SOURCE="$(readlink "$SOURCE")"
  [[ "$SOURCE" != /* ]] && SOURCE="$DIR/$SOURCE"
done
BIN_DIR="$(cd -P "$(dirname "$SOURCE")" && pwd)"
OBS_ROOT="$(cd "$BIN_DIR/.." && pwd)"
COLLECTOR="$OBS_ROOT/collector-old/collector.mjs"
LABEL="com.agents-observability.collector"
PLIST="$HOME/Library/LaunchAgents/$LABEL.plist"
LOG_DIR="$OBS_ROOT/.state"
LOG_OUT="$LOG_DIR/collector.log"
LOG_ERR="$LOG_DIR/collector.err.log"

require() {
  command -v "$1" >/dev/null || { echo "install-service.sh: $1 not found on PATH" >&2; exit 1; }
}

xml_escape() {
  printf '%s' "$1" | sed 's/&/\&amp;/g; s/</\&lt;/g; s/>/\&gt;/g'
}

# Runs the user's own shell as an interactive shell (so it sources ~/.bashrc,
# ~/.zshrc, or config.fish — the same rc file bin/agent-setup wrote the
# collector's env vars into) and prints the <key>/<string> XML pairs for
# whichever of COLLECTOR_ENV_VARS actually got set. bash/zsh only source
# their rc on an INTERACTIVE shell (a plain non-interactive one would see
# none of this), hence -i; fish sources config.fish either way but accepts
# the same flag.
collector_env_xml() {
  local shell_bin="${SHELL:-/bin/bash}"
  local out
  out="$("$shell_bin" -ic 'env' 2>/dev/null || true)"
  local var value
  for var in $COLLECTOR_ENV_VARS; do
    value="$(printf '%s\n' "$out" | sed -n "s/^${var}=//p" | tail -1)"
    if [ -n "$value" ]; then
      printf '    <key>%s</key><string>%s</string>\n' "$var" "$(xml_escape "$value")"
    fi
  done
  # Under "set -e", "x=$(f)" aborts the script if f's LAST command exited
  # non-zero — which the loop above does whenever the final var in the list
  # isn't set (the common case). Without this, an unset TZ_OFFSET_HOURS alone
  # would silently kill install/restart before the plist was ever written.
  return 0
}

# launchd processes do NOT inherit your shell's PATH or its rc-sourced env
# vars — usage-truth.mjs spawns `claude` as a bare command, so PATH has to be
# resolvable from whatever this plist sets, and CLAUDE_DIR/EXPORTER_STREAM/etc
# have to be baked in directly (see collector_env_xml above). Both resolved
# once, here, at install time — not by collector.mjs itself — so a Homebrew/
# nvm upgrade or an edited shell rc just needs a re-install.
write_plist() {
  require node
  require claude
  local node_bin claude_bin
  node_bin="$(command -v node)"
  claude_bin="$(command -v claude)"
  mkdir -p "$LOG_DIR"

  local env_xml; env_xml="$(collector_env_xml)"
  if [ -z "$env_xml" ]; then
    echo "install-service.sh: warning: none of ($COLLECTOR_ENV_VARS) found in your shell rc —" >&2
    echo "  run bin/agent-setup first, or the collector falls back to its own built-in defaults." >&2
  fi

  cat > "$PLIST" <<PLIST_EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>$LABEL</string>
  <key>ProgramArguments</key>
  <array>
    <string>$node_bin</string>
    <string>$COLLECTOR</string>
  </array>
  <key>WorkingDirectory</key><string>$OBS_ROOT/collector</string>
  <key>EnvironmentVariables</key>
  <dict>
    <key>PATH</key><string>$(dirname "$node_bin"):$(dirname "$claude_bin"):/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin</string>
$env_xml
  </dict>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>StandardOutPath</key><string>$LOG_OUT</string>
  <key>StandardErrorPath</key><string>$LOG_ERR</string>
  <key>ProcessType</key><string>Background</string>
</dict>
</plist>
PLIST_EOF
}

cmd="${1:-}"
case "$cmd" in
  install)
    write_plist
    launchctl bootout "gui/$(id -u)" "$PLIST" 2>/dev/null || true
    launchctl bootstrap "gui/$(id -u)" "$PLIST"
    launchctl enable "gui/$(id -u)/$LABEL"
    echo "installed and started: $LABEL"
    echo "logs: $LOG_OUT / $LOG_ERR"
    ;;
  uninstall)
    launchctl bootout "gui/$(id -u)" "$PLIST" 2>/dev/null || true
    rm -f "$PLIST"
    echo "uninstalled: $LABEL"
    ;;
  restart)
    "$0" uninstall
    "$0" install
    ;;
  status)
    launchctl print "gui/$(id -u)/$LABEL" 2>&1 | head -20 || echo "not loaded"
    echo "--- recent log ---"
    tail -n 20 "$LOG_OUT" 2>/dev/null || echo "(no log yet)"
    ;;
  *)
    echo "Usage: $0 {install|uninstall|restart|status}" >&2
    exit 1
    ;;
esac
