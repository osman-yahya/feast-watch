#!/usr/bin/env bash
# Remove the feast-watch mother installed by deploy/mother-install.sh.
#
#   sudo deploy/mother-uninstall.sh              # service and binary; keep data and config
#   sudo deploy/mother-uninstall.sh --purge      # also remove the database, config and user
#   sudo deploy/mother-uninstall.sh --dry-run    # print what would be removed
#
# Without --purge the database survives, because it is the only copy of every
# server's history and of every agent token — and a token cannot be reissued,
# only replaced by reinstalling that agent.
#
# Every step is idempotent: this has to finish on a half-installed host, and
# under `set -euo pipefail` a systemctl call against an absent unit exits
# non-zero. Each one is guarded rather than blanket-`|| true`'d, so a real
# failure is still a failure.
set -euo pipefail

# FW_ROOT prefixes every path. It is empty in production; the co-location test
# sets it to a temp tree so this script can be exercised without being root.
FW_ROOT="${FW_ROOT:-}"

BIN="$FW_ROOT/usr/local/bin/feast-watch"
# The self-update's footprint: the root helper the unit runs before every start,
# and the previous binary it keeps for a rollback. The staging directory lives
# inside STATE_DIR and goes with it.
PROMOTE="$FW_ROOT/usr/local/sbin/feast-watch-mother-promote"
BACKUP="$BIN.bak"
CONF_DIR="$FW_ROOT/etc/feast-watch"
UNIT="$FW_ROOT/etc/systemd/system/feast-watch-mother.service"
UNIT_NAME=feast-watch-mother.service
STATE_DIR="$FW_ROOT/var/lib/feast-watch"
SERVICE_USER=feast-watch

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
  # is-active and is-enabled both exit non-zero for an absent unit, which is
  # exactly the state a re-run is in — so they gate rather than fail.
  if systemctl is-active --quiet "$UNIT_NAME"; then
    systemctl stop "$UNIT_NAME" || echo "could not stop $UNIT_NAME; removing its files anyway" >&2
    say "stopped $UNIT_NAME"
  fi
  if systemctl is-enabled --quiet "$UNIT_NAME" 2>/dev/null; then
    systemctl disable "$UNIT_NAME" || echo "could not disable $UNIT_NAME; removing its files anyway" >&2
    say "disabled $UNIT_NAME"
  fi
}

# prune_conf_dir removes the shared config directory only when nothing else
# lives in it, so a co-installed agent keeps its token.
prune_conf_dir() {
  [ -d "$CONF_DIR" ] || return 0
  if [ "$DRY_RUN" -eq 1 ]; then
    echo "would remove $CONF_DIR if empty"
    return 0
  fi
  if rmdir "$CONF_DIR" 2>/dev/null; then
    say "removed $CONF_DIR"
  else
    say "kept $CONF_DIR — it still holds files (an agent on this host?)"
  fi
}

reload_systemd() {
  command -v systemctl >/dev/null 2>&1 || return 0
  [ "$DRY_RUN" -eq 1 ] && return 0
  # Tolerated, not required. By the time this runs the unit file is already
  # gone, so a reload that cannot happen — no privilege, no running systemd,
  # a container — means systemd was not told, not that the removal failed.
  # Aborting here would leave a host half-cleaned over a notification, which is
  # strictly worse than a stale unit in systemd's memory until the next reload.
  systemctl daemon-reload || echo "could not reload systemd; the unit files are already removed" >&2
  # A unit that died before being removed otherwise lingers in
  # `systemctl --failed` forever.
  systemctl reset-failed "$UNIT_NAME" 2>/dev/null || true
}

main() {
  local purge=0
  for arg in "$@"; do
    case "$arg" in
      --purge) purge=1 ;;
      --dry-run) DRY_RUN=1 ;;
      *) echo "unknown option: $arg" >&2; return 2 ;;
    esac
  done

  # Root is required for the real paths only. With FW_ROOT set we are acting on
  # a test tree, not the system.
  if [ "$DRY_RUN" -eq 0 ] && [ -z "$FW_ROOT" ] && [ "$(id -u)" -ne 0 ]; then
    echo "must run as root" >&2
    return 1
  fi

  stop_service
  remove "$UNIT"
  reload_systemd
  remove "$BIN"
  remove "$BACKUP"
  remove "$PROMOTE"

  if [ "$purge" -eq 1 ]; then
    remove "$STATE_DIR"
    # Only the mother's own files. An agent is expected to run on this same
    # host — the mother monitors itself — and its agent.conf lives in this very
    # directory, holding a token that no endpoint reissues.
    remove "$CONF_DIR/mother.env"
    remove "$CONF_DIR/mother-manifest"
    prune_conf_dir
    if [ "$DRY_RUN" -eq 0 ] && id "$SERVICE_USER" >/dev/null 2>&1; then
      userdel "$SERVICE_USER"
      say "removed user $SERVICE_USER"
    fi
  else
    say "kept $STATE_DIR and $CONF_DIR"
    say "the database holds every server's history and its tokens; pass --purge to remove it"
  fi

  echo
  echo "feast-watch mother removed. Agents on monitored hosts keep running and"
  echo "keep failing their pushes — run feast-watch-agent-uninstall on each."
}

main "$@"
