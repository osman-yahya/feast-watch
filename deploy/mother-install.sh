#!/usr/bin/env bash
# Install the feast-watch mother as a systemd service.
#
#   sudo deploy/mother-install.sh --download
#   sudo deploy/mother-install.sh --download=v1.4.0 --with-agent
#   sudo deploy/mother-install.sh /path/to/feast-watch
#   sudo deploy/mother-install.sh --with-agent /path/to/feast-watch
#
# --download fetches the published mother binary from GitHub Releases and
# verifies its SHA-256 before installing it, exactly as the served agent
# installer does and as the mother's own self-update does. Nothing else has to
# be on the host: no Go toolchain, no checkout, no build. The mother is a
# statically linked pure-Go binary — it shells out to nothing and compiles
# nothing at runtime — so a host that can run it needs no compiler afterwards
# either. Pass a path instead when you want a binary you built yourself.
#
# --with-agent also installs an agent on this same host, pointed at the mother
# it just installed. That is the deployment the design assumes — the mother
# monitors its own host.
#
# Where its agent binary comes from follows the mother's: with --download, from
# the same published release, which by then demonstrably exists because the
# mother was just fetched from it; from a checkout, from the build sitting
# beside the mother, so nothing is fetched and no release has to exist yet.
# Either way it sidesteps the chicken-and-egg of the served installer, which
# can only download from a published release.
#
# Run with --download, or from a checkout after `bin/release.sh`, or pass the
# binary explicitly.
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
PROMOTE_DEST="$FW_ROOT/usr/local/sbin/feast-watch-mother-promote"
AGENT_MANIFEST="$CONF_DIR/install-manifest"
SERVICE_USER=feast-watch

# Where published builds come from. Overridable so e2e/download_test.sh can
# point it at a local tree instead of github.com.
RELEASE_BASE_URL="${RELEASE_BASE_URL:-https://github.com/osman-yahya/feast-watch}"

# fetch_release_binary downloads one published asset and verifies it before it
# is allowed to become anything.
#
# --fail is not optional. Without it `curl -o` exits 0 on an HTTP 404 and writes
# the response body, so a missing asset would install an error page as the
# mother, chmod 0755, and systemd would respawn against it every five seconds.
# `set -euo pipefail` does not catch that, because curl succeeded.
#
# The checksum is fetched second but is what decides: a build published without
# one, or one whose bytes do not match, is refused rather than installed. This
# is the same rule the agent installer and the mother's self-update apply, and
# it is the only thing standing between a rollout and whatever a CDN handed us.
fetch_release_binary() {
  local asset="$1" dest="$2" version="$3" url
  if [ "$version" = "latest" ]; then
    url="$RELEASE_BASE_URL/releases/latest/download/$asset"
  else
    url="$RELEASE_BASE_URL/releases/download/$version/$asset"
  fi

  local tmp
  tmp=$(mktemp)
  # shellcheck disable=SC2064  # expand tmp now: it must be removed on any exit
  trap "rm -f '$tmp' '$tmp.sha256'" RETURN

  curl -fsSL "$url" -o "$tmp" ||
    { echo "could not download $asset ($version) from $RELEASE_BASE_URL" >&2; return 1; }
  curl -fsSL "$url.sha256" -o "$tmp.sha256" ||
    { echo "$asset ($version) has no published checksum; refusing to install it" >&2; return 1; }

  local want got
  want=$(cut -d' ' -f1 < "$tmp.sha256")
  got=$(sha256sum "$tmp" 2>/dev/null | cut -d' ' -f1 || shasum -a 256 "$tmp" | cut -d' ' -f1)
  if [ "$want" != "$got" ]; then
    echo "checksum mismatch for $asset: refusing to install" >&2
    return 1
  fi

  install -m 0755 "$tmp" "$dest"
}

# release_platform is the "<goos>-<goarch>" half of an asset name for this host.
#
# Always linux: this script is bash plus systemd. The GOOS is still part of the
# name because a release carries builds for other operating systems, and an
# architecture alone does not identify a runnable binary.
release_platform() {
  local machine
  machine=$(uname -m)
  case "$machine" in
    x86_64|amd64) echo "linux-amd64" ;;
    aarch64|arm64) echo "linux-arm64" ;;
    *)
      echo "no published mother build for $machine — build one and pass its path instead" >&2
      return 1
      ;;
  esac
}

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

# install_promote_helper puts the root half of the self-update on disk.
#
# Without it the mother reports `unsupported` and the panel's update control is
# disabled — honest, but this is the deployment where self-update is meant to
# work, and the unit this installer writes already names the path. Installing
# the two together is what keeps that reference from dangling.
install_promote_helper() {
  local src
  src="$(dirname "$0")/feast-watch-mother-promote"
  [ -f "$src" ] || { echo "promote helper not found at $src" >&2; return 1; }

  echo "-> installing promote helper"
  mkdir -p "$(dirname "$PROMOTE_DEST")"
  install -m 0755 "$src" "$PROMOTE_DEST"
}

write_manifest() {
  {
    echo "# What this installer created. No secrets."
    echo "installed_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
    echo "bin=$BIN_DEST"
    echo "env=$ENV_FILE"
    echo "unit=$UNIT"
    echo "promote=$PROMOTE_DEST"
    echo "backup=$BIN_DEST.bak"
    echo "staged=/var/lib/feast-watch/update"
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
  local source_dir="$1" platform="$2" version="$3" name
  name="$(hostname -s 2>/dev/null || hostname)"

  echo "-> registering this host as '$name'"
  # generate is create-or-fetch, so re-running the installer does not mint a
  # second server for the same host.
  local token
  token=$(FW_DB_PATH="$FW_ROOT/var/lib/feast-watch/mother.db" \
          "$BIN_DEST" generate --name="$name" |
          sed -n 's|.*/install/\(tk_[0-9a-f]*\)\.sh.*|\1|p')
  [ -n "$token" ] || { echo "could not read a token from feast-watch generate" >&2; return 1; }

  # Two sources, one destination. With --download the agent comes from the same
  # release as the mother, which is also what removes the old chicken-and-egg
  # here: the served installer downloads from a published release, and with
  # --download one demonstrably exists — we just fetched the mother from it.
  if [ -n "$platform" ]; then
    echo "-> downloading agent $version ($platform)"
    fetch_release_binary "feast-watch-agent-$platform" "$AGENT_BIN_DEST" "$version" || return 1
  else
    local agent_src="$source_dir/feast-watch-agent"
    [ -f "$agent_src" ] || {
      echo "agent binary not found at $agent_src — run bin/release.sh first, or pass --download" >&2
      return 1
    }
    echo "-> installing agent binary"
    install -m 0755 "$agent_src" "$AGENT_BIN_DEST"
  fi

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
  local src
  src="$(dirname "$0")/../mother/api/uninstall.sh"
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

# The unit was already running before this install started, which makes this an
# UPGRADE rather than a first install. It has to be observed before the binary
# is replaced, because afterwards there is nothing left on disk to tell the two
# apart — and the difference decides whether the new binary ever runs.
was_active() {
  command -v systemctl >/dev/null 2>&1 || return 1
  systemctl is-active --quiet "$UNIT_NAME"
}

main() {
  local with_agent=0
  local download=0
  local version=latest
  local args=()
  for arg in "$@"; do
    case "$arg" in
      --with-agent) with_agent=1 ;;
      --download) download=1 ;;
      --download=*) download=1; version="${arg#--download=}" ;;
      *) args+=("$arg") ;;
    esac
  done

  local source="${args[0]:-$(dirname "$0")/../bin/build/feast-watch}"

  [ "$(id -u)" -eq 0 ] || { echo "must run as root" >&2; return 1; }

  local platform=""
  if [ "$download" -eq 1 ]; then
    platform=$(release_platform) || return 1
    # Downloaded into a temp file rather than straight onto BIN_DEST: the
    # install below is what replaces the binary, and doing it in one place
    # keeps a failed download from ever touching a running mother.
    source=$(mktemp)
    # shellcheck disable=SC2064  # expand source now: it must be removed on exit
    trap "rm -f '$source'" EXIT
    echo "-> downloading mother $version ($platform)"
    fetch_release_binary "feast-watch-mother-$platform" "$source" "$version" || return 1
  fi

  [ -f "$source" ] || {
    echo "mother binary not found at $source" >&2
    echo "pass --download to fetch the published build instead of building one" >&2
    return 1
  }

  # A system account with no login and no home: the mother needs neither, and
  # the unit refuses to start without the user existing.
  if ! id "$SERVICE_USER" >/dev/null 2>&1; then
    echo "-> creating $SERVICE_USER"
    useradd --system --no-create-home --shell /usr/sbin/nologin "$SERVICE_USER"
  fi

  local upgrading=0
  was_active && upgrading=1

  echo "-> installing binary"
  install -m 0755 "$source" "$BIN_DEST"
  install_promote_helper

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
    if [ "$download" -eq 1 ]; then
      install_local_agent "" "$platform" "$version"
    else
      install_local_agent "$(dirname "$source")" "" ""
    fi
    echo
    if [ "$upgrading" -eq 1 ]; then
      # Same reason as the plain path below: the env file is already filled in
      # on an upgrade, so the only thing standing between the operator and the
      # new binary is the restart nobody was doing.
      echo "-> restarting $UNIT_NAME to pick up the new binary"
      systemctl restart "$UNIT_NAME"
      echo "upgraded. $ENV_FILE was left as it is."
      return 0
    fi
    echo "mother and agent installed. Edit $ENV_FILE (API key, public URL), then:"
    echo "  systemctl restart $UNIT_NAME"
    return 0
  fi

  # `install` unlinks the destination before writing the new file, so replacing
  # the binary under a running mother succeeds silently — and the process keeps
  # executing the old, now-unlinked inode with the old compiled-in version. The
  # closing line used to be `systemctl start`, which on an active unit exits 0
  # and does nothing: the operator saw a clean upgrade and the panel kept
  # reporting the previous mother version indefinitely.
  if [ "$upgrading" -eq 1 ]; then
    echo "-> restarting $UNIT_NAME to pick up the new binary"
    systemctl restart "$UNIT_NAME"
    echo
    echo "upgraded. $ENV_FILE was left as it is."
    return 0
  fi

  echo
  echo "installed. Edit $ENV_FILE, then: systemctl start $UNIT_NAME"
}

# Run main only when executed. e2e/download_test.sh sources this file to
# exercise the download-and-verify half, which needs neither root nor systemd —
# and sourcing a script that installs on sight would be a test that tries to
# deploy a mother onto the machine running it.
if [ "${BASH_SOURCE[0]}" = "$0" ]; then
  main "$@"
fi
