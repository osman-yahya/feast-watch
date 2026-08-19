#!/usr/bin/env bash
# Install the feast-watch mother as a systemd service.
#
#   sudo deploy/mother-install.sh /path/to/feast-watch
#   sudo deploy/mother-install.sh --with-agent /path/to/feast-watch
#
# --with-agent also installs an agent on this same host, pointed at the mother
# it just installed. That is the deployment the design assumes — the mother
# monitors its own host — and doing it here avoids the chicken-and-egg of the
# served installer, which downloads the agent from a published GitHub release:
# on this host the binary was just built beside the mother, so nothing is
# fetched and no release has to exist yet.
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

# FW_ROOT prefixes every path. Empty in production; the co-location test sets
# it to a temp tree so this script can be exercised without being root.
FW_ROOT="${FW_ROOT:-}"

BIN_DEST="$FW_ROOT/usr/local/bin/feast-watch"
AGENT_BIN_DEST="$FW_ROOT/usr/local/bin/feast-watch-agent"
CONF_DIR="$FW_ROOT/etc/feast-watch"
ENV_FILE="$CONF_DIR/mother.env"
AGENT_CONF="$CONF_DIR/agent.conf"
UNIT="$FW_ROOT/etc/systemd/system/feast-watch-mother.service"
UNIT_NAME=feast-watch-mother.service
AGENT_UNIT="$FW_ROOT/etc/systemd/system/feast-watch-agent.service"
AGENT_UNIT_NAME=feast-watch-agent.service
AGENT_UNINSTALLER="$FW_ROOT/usr/local/sbin/feast-watch-agent-uninstall"
AGENT_MANIFEST="$CONF_DIR/install-manifest"
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

# install_local_agent registers this host with the mother it was just given and
# installs the agent from the locally built binary.
#
# The mother has to be running first: the token comes from `feast-watch
# generate`, which writes to the same database the service owns, and SQLite
# takes a single writer.
install_local_agent() {
  local source_dir="$1" name
  name="$(hostname -s 2>/dev/null || hostname)"

  local agent_src="$source_dir/feast-watch-agent"
  [ -f "$agent_src" ] || { echo "agent binary not found at $agent_src — run bin/release.sh first" >&2; return 1; }

  echo "-> registering this host as '$name'"
  # generate is create-or-fetch, so re-running the installer does not mint a
  # second server for the same host.
  local token
  token=$(FW_DB_PATH="$FW_ROOT/var/lib/feast-watch/mother.db" \
          "$BIN_DEST" generate --name="$name" |
          sed -n 's|.*/install/\(tk_[0-9a-f]*\)\.sh.*|\1|p')
  [ -n "$token" ] || { echo "could not read a token from feast-watch generate" >&2; return 1; }

  echo "-> installing agent binary"
  install -m 0755 "$agent_src" "$AGENT_BIN_DEST"

  if [ -f "$AGENT_CONF" ]; then
    echo "   kept existing $AGENT_CONF"
  else
    # The mother is reached over the loopback: the agent is on the same host,
    # so its traffic never needs to leave it whatever FW_PUBLIC_URL says.
    local listen port
    listen=$(sed -n 's/^FW_LISTEN=//p' "$ENV_FILE")
    port="${listen##*:}"
    {
      echo "MOTHER_URL=http://127.0.0.1:${port:-8443}"
      echo "TOKEN=$token"
      echo "SERVER_NAME=$name"
    } > "$AGENT_CONF"
    chmod 0600 "$AGENT_CONF"
    echo "   wrote $AGENT_CONF"
  fi

  install_agent_uninstaller

  echo "-> installing agent unit"
  install -m 0644 "$(dirname "$0")/feast-watch-agent.service" "$AGENT_UNIT"
  systemctl daemon-reload
  systemctl enable --now "$AGENT_UNIT_NAME"
}

# install_agent_uninstaller puts the uninstaller and the manifest on disk, the
# way the served installer does.
#
# Without this, the mother's own host is the one machine in the fleet that
# cannot be removed from the panel. Deleting a server is answered on the
# agent's next push, and the agent's response is to exec this exact path
# (agent.DefaultUninstaller); missing, it can only report
# "uninstaller ... is not on this host" and the row sits in "uninstalling"
# for good.
#
# The script is taken from mother/api/uninstall.sh — the same file the mother
# embeds and serves at /uninstall.sh — so a host installed by either path gets
# byte-identical removal behaviour.
install_agent_uninstaller() {
  local src="$(dirname "$0")/../mother/api/uninstall.sh"
  [ -f "$src" ] || { echo "uninstall script not found at $src" >&2; return 1; }

  echo "-> installing agent uninstaller"
  mkdir -p "$(dirname "$AGENT_UNINSTALLER")"
  install -m 0755 "$src" "$AGENT_UNINSTALLER"

  # Same keys as the served installer writes: the uninstaller reads this to
  # clean a host installed by an older version of either path.
  {
    echo "# What this installer created. No secrets: the token lives in agent.conf."
    echo "installed_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
    echo "bin=/usr/local/bin/feast-watch-agent"
    echo "conf=/etc/feast-watch/agent.conf"
    echo "unit=/etc/systemd/system/feast-watch-agent.service"
    echo "uninstaller=/usr/local/sbin/feast-watch-agent-uninstall"
  } > "$AGENT_MANIFEST"
  chmod 0644 "$AGENT_MANIFEST"
}

main() {
  local with_agent=0
  local args=()
  for arg in "$@"; do
    case "$arg" in
      --with-agent) with_agent=1 ;;
      *) args+=("$arg") ;;
    esac
  done

  local source="${args[0]:-$(dirname "$0")/../bin/build/feast-watch}"

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

  if [ "$with_agent" -eq 1 ]; then
    echo
    echo "-> starting the mother so it can mint this host's token"
    systemctl start "$UNIT_NAME"
    install_local_agent "$(dirname "$source")"
    echo
    echo "mother and agent installed. Edit $ENV_FILE (API key, public URL), then:"
    echo "  systemctl restart $UNIT_NAME"
    return 0
  fi

  echo
  echo "installed. Edit $ENV_FILE, then: systemctl start $UNIT_NAME"
}

main "$@"
