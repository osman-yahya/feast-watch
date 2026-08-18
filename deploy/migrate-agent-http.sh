#!/usr/bin/env bash
# Re-point an installed agent from an https:// mother to plain http://.
#
# The mother no longer terminates TLS. An agent installed before that change
# holds MOTHER_URL=https://<mother>:8443 in its config, and there is no channel
# to correct it remotely: the ingest response carries only collectors, interval
# and desired version, and the config is read once at startup. A mismatched
# scheme therefore fails at the transport with no signal in the panel — the
# server simply stops reporting.
#
# Run this on each host only if the mother is NOT fronted by a proxy still
# serving the old https address. If it is, nothing needs to change.
#
#   sudo bash migrate-agent-http.sh            # rewrite and restart
#   sudo bash migrate-agent-http.sh --dry-run  # show what would change
set -euo pipefail

CONF=${CONF:-/etc/feast-watch/agent.conf}

main() {
  local dry_run=0
  [ "${1:-}" = "--dry-run" ] && dry_run=1

  if [ ! -f "$CONF" ]; then
    echo "no agent config at $CONF — nothing to migrate" >&2
    return 0
  fi

  local current
  current=$(grep -E '^MOTHER_URL=' "$CONF" || true)
  if [ -z "$current" ]; then
    echo "no MOTHER_URL in $CONF — leaving it alone" >&2
    return 0
  fi
  case "$current" in
    MOTHER_URL=https://*) ;;
    *) echo "already plain HTTP: $current"; return 0 ;;
  esac

  local replacement="${current/https:\/\//http:\/\/}"
  if [ "$dry_run" -eq 1 ]; then
    echo "would rewrite: $current -> $replacement"
    return 0
  fi

  # Backup first: this file also holds the token, which no endpoint reissues.
  cp -a "$CONF" "$CONF.bak"
  sed -i 's|^MOTHER_URL=https://|MOTHER_URL=http://|' "$CONF"
  # TLS_SKIP_VERIFY and CA_FILE are ignored by current agents; drop them so the
  # file stops describing a trust policy that no longer exists.
  sed -i '/^TLS_SKIP_VERIFY=/d; /^CA_FILE=/d' "$CONF"
  echo "rewrote: $current -> $replacement (backup at $CONF.bak)"

  systemctl restart feast-watch-agent
  echo "agent restarted — the server should report within one push interval."
}

main "$@"
