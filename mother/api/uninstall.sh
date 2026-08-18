#!/usr/bin/env bash
# feast-watch agent uninstaller.
#
# Written to /usr/local/sbin/feast-watch-agent-uninstall by the installer, so a
# host being decommissioned can be cleaned without reaching the mother — which
# may already be retired, or unreachable from a machine on its way out. It is
# also served at <mother>/uninstall.sh for a host that lost its copy.
#
#   feast-watch-agent-uninstall              # remove the service and binary, keep the config
#   feast-watch-agent-uninstall --purge      # also remove /etc/feast-watch (including the token)
#   feast-watch-agent-uninstall --dry-run    # print what would be removed
#
# Every step is idempotent: this has to finish on a half-installed host, and
# under `set -euo pipefail` a systemctl call against a unit that is already gone
# exits non-zero. Each one is guarded rather than blanket-`|| true`'d, so a real
# failure is still a failure.
set -euo pipefail

BIN=/usr/local/bin/feast-watch-agent
CONF_DIR=/etc/feast-watch
UNIT=/etc/systemd/system/feast-watch-agent.service
UNIT_NAME=feast-watch-agent.service
SELF=/usr/local/sbin/feast-watch-agent-uninstall

DRY_RUN=0

say() { echo "-> $*"; }

remove() {
  local path="$1"
  [ -e "$path" ] || [ -L "$path" ] || return 0
  if [ "$DRY_RUN" -eq 1 ]; then
    echo "would remove $path"
    return 0
  fi
  rm -rf "$path"
  say "removed $path"
}

stop_service() {
  command -v systemctl >/dev/null 2>&1 || { say "no systemd; skipping service"; return 0; }
  if [ "$DRY_RUN" -eq 1 ]; then
    echo "would stop and disable $UNIT_NAME"
    return 0
  fi
  # `is-enabled` and `is-active` both exit non-zero for an absent unit, which is
  # exactly the state a re-run is in — so they gate rather than fail.
  if systemctl is-active --quiet "$UNIT_NAME"; then
    systemctl stop "$UNIT_NAME"
    say "stopped $UNIT_NAME"
  fi
  if systemctl is-enabled --quiet "$UNIT_NAME" 2>/dev/null; then
    systemctl disable "$UNIT_NAME"
    say "disabled $UNIT_NAME"
  fi
}

reload_systemd() {
  command -v systemctl >/dev/null 2>&1 || return 0
  [ "$DRY_RUN" -eq 1 ] && return 0
  systemctl daemon-reload
  # A unit that died before being removed stays in "failed" and shows up in
  # `systemctl --failed` forever otherwise.
  systemctl reset-failed "$UNIT_NAME" 2>/dev/null || true
}

main() {
  local purge=0
  for arg in "$@"; do
    case "$arg" in
      --purge) purge=1 ;;
      --dry-run) DRY_RUN=1 ;;
      -h|--help)
        sed -n '2,14p' "$0" | sed 's/^# \{0,1\}//'
        return 0
        ;;
      *) echo "unknown option: $arg" >&2; return 2 ;;
    esac
  done

  if [ "$DRY_RUN" -eq 0 ] && [ "$(id -u)" -ne 0 ]; then
    echo "must run as root" >&2
    return 1
  fi

  stop_service
  remove "$UNIT"
  reload_systemd

  remove "$BIN"
  # A self-update interrupted between writing and renaming leaves a temporary
  # file beside the binary that nothing else ever sweeps.
  for leftover in "$BIN".*.new "$BIN".new; do
    remove "$leftover"
  done

  if [ "$purge" -eq 1 ]; then
    remove "$CONF_DIR"
  else
    say "kept $CONF_DIR (pass --purge to remove it, including the token)"
  fi

  # Removed last: it is the file currently executing. bash has already read the
  # script, so unlinking it here is safe.
  remove "$SELF"

  echo
  echo "feast-watch agent removed. The server will show as offline in the panel"
  echo "until it is deleted there — the agent cannot delete its own record."
}

main "$@"
