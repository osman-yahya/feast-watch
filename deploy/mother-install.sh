#!/usr/bin/env bash
# Install the feast-watch mother as a systemd service.
#
#   sudo deploy/mother-install.sh /path/to/feast-watch
#
# Run from a checkout after `bin/release.sh`, or pass the binary explicitly.
# Everything this creates is listed in /etc/feast-watch/mother-manifest and is
# removed by deploy/mother-uninstall.sh — the two are meant to be read together.
#
# Before this existed the mother had no unit, no installer and no defined
# footprint: QUICKSTART handed the whole deployment to the operator as "run it
# with these env vars", so what a given host actually had was whatever that
# operator improvised — and nothing could be cleanly removed.
set -euo pipefail

BIN_DEST=/usr/local/bin/feast-watch
CONF_DIR=/etc/feast-watch
ENV_FILE="$CONF_DIR/mother.env"
UNIT=/etc/systemd/system/feast-watch-mother.service
UNIT_NAME=feast-watch-mother.service
SERVICE_USER=feast-watch

seed_env_file() {
  # Seeded, never overwritten: re-running the installer to upgrade the binary
  # must not wipe the API key.
  if [ -f "$ENV_FILE" ]; then
    echo "   kept existing $ENV_FILE"
    return 0
  fi
  {
    echo "FW_DB_PATH=/var/lib/feast-watch/mother.db"
    echo "FW_LISTEN=:8443"
    echo "# The URL agents are told to reach the mother on, scheme included. The"
    echo "# mother serves plain HTTP; name a fronting proxy here if TLS is"
    echo "# terminated in front of it."
    echo "FW_PUBLIC_URL=http://127.0.0.1:8443"
    echo "FW_API_KEY=change-me"
  } > "$ENV_FILE"
  echo "   wrote $ENV_FILE — set FW_API_KEY and FW_PUBLIC_URL before starting"
}

write_manifest() {
  {
    echo "# What this installer created. No secrets."
    echo "installed_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
    echo "bin=$BIN_DEST"
    echo "env=$ENV_FILE"
    echo "unit=$UNIT"
    echo "state=/var/lib/feast-watch"
    echo "user=$SERVICE_USER"
  } > "$CONF_DIR/mother-manifest"
  chmod 0644 "$CONF_DIR/mother-manifest"
}

main() {
  local source="${1:-$(dirname "$0")/../bin/build/feast-watch}"

  [ "$(id -u)" -eq 0 ] || { echo "must run as root" >&2; return 1; }
  [ -f "$source" ] || { echo "mother binary not found at $source" >&2; return 1; }

  # A system account with no login and no home: the mother needs neither, and
  # the unit refuses to start without the user existing.
  if ! id "$SERVICE_USER" >/dev/null 2>&1; then
    echo "-> creating $SERVICE_USER"
    useradd --system --no-create-home --shell /usr/sbin/nologin "$SERVICE_USER"
  fi

  echo "-> installing binary"
  install -m 0755 "$source" "$BIN_DEST"

  echo "-> preparing $CONF_DIR"
  mkdir -p "$CONF_DIR"
  seed_env_file
  # The API key lives here, so it is not world-readable.
  chmod 0640 "$ENV_FILE"
  chown root:"$SERVICE_USER" "$ENV_FILE"

  echo "-> installing unit"
  install -m 0644 "$(dirname "$0")/feast-watch-mother.service" "$UNIT"
  write_manifest

  systemctl daemon-reload
  systemctl enable "$UNIT_NAME"
  echo
  echo "installed. Edit $ENV_FILE, then: systemctl start $UNIT_NAME"
}

main "$@"
