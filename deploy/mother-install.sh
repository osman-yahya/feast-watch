#!/usr/bin/env bash
# Install the feast-watch mother as a systemd service.
#
#   sudo deploy/mother-install.sh --download
#   sudo deploy/mother-install.sh --download=v1.4.0 --with-agent
#   sudo deploy/mother-install.sh /path/to/feast-watch
#   sudo deploy/mother-install.sh --with-agent /path/to/feast-watch
#   sudo deploy/mother-install.sh --build --with-agent
#   sudo deploy/mother-install.sh --download --skip-go
#
# --download fetches a published mother binary and verifies its SHA-256 before
# installing it. It is the bootstrap and only the bootstrap: a first mother has
# to come from somewhere, and this host — unlike every host it will go on to
# monitor — is the one with a route off the network. Pass a path instead when
# you want a binary you built yourself.
#
# After this, the mother is the source of every binary on the fleet, including
# its own replacement. Producing one means a Go toolchain on THIS host, so this
# installer puts one there — pinned by version and checksum, unless the host
# already has a new-enough Go or --skip-go says not to:
#
#   FW_SOURCE_DIR=/path/to/checkout feast-watch build v1.5.0
#   feast-watch build v1.5.0          # fetches that tag's source itself
#
# --with-agent also installs an agent on this same host, pointed at the mother
# it just installed. That is the deployment the design assumes — the mother
# monitors its own host — and its binary comes from where every other agent's
# does: the mother's own catalogue, or the build sitting beside this script in
# a checkout.
#
# --build compiles the mother from this checkout with the toolchain this script
# just installed, which is the whole of a from-scratch deployment: no published
# release has to exist and nothing has to be built beforehand.
#
# Run with --build, with --download, or pass a binary you built yourself.
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

# SCRIPT_DIR is where the files this installer copies live — the unit files, the
# promote helper, the uninstaller, and the source tree above them.
#
# From BASH_SOURCE rather than $0, because the shell tests source this file to
# exercise one function at a time: sourced, $0 is the calling shell and every
# sibling path resolves against whatever directory the test happened to be run
# from.
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

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

# The Go toolchain this host compiles the fleet with.
#
# It is installed here rather than left to the operator because it is not
# optional any more: the mother produces every binary its agents run, so a
# mother without a compiler is a fleet that can never be updated. Making the
# installer responsible for it means one command leaves a host able to do the
# whole job, instead of a host that looks installed and fails at the first
# `feast-watch build`.
#
# Pinned by version AND by checksum. The version because a toolchain that
# changes under a deployment changes the binaries it produces; the checksum
# because this is an unauthenticated download that becomes the compiler every
# binary on the fleet is built by — the one place where trusting whatever
# answered would be worst.
GO_VERSION="${GO_VERSION:-1.26.7}"
GO_DL_BASE="${GO_DL_BASE:-https://go.dev/dl}"
GO_ROOT="$FW_ROOT/usr/local/go"
GO_LINK="$FW_ROOT/usr/local/bin/go"

# GO_MIN is the oldest Go that can build this tree: the `go` line in go.mod,
# patch included. An existing toolchain at least this new is used as it is.
#
# The patch matters. A host one patch short passes any major.minor test, and
# then every build silently reaches for the matching toolchain over the network
# — which on the mother this is written for is the one thing that must not be
# needed. Update this with go.mod.
GO_MIN=1.26.1

# GO_INSTALLED records whether the toolchain on this host is one we put there.
# The manifest carries it, so the uninstaller removes a compiler this installer
# installed and leaves alone one that was already here — removing somebody
# else's Go would be a footprint we never declared.
GO_INSTALLED=0

# go_sha256 is the published SHA-256 of the tarball for one platform, pinned.
# Updating GO_VERSION means updating these together — from
# https://go.dev/dl/?mode=json — and a pair that does not match is a refused
# install rather than a compiler nobody checked.
go_sha256() {
  case "$1" in
    linux-amd64) echo "ffb5f8de10c62550dfddab66b36b57030721e0a44a3218e9e1181d7b59f121ca" ;;
    linux-arm64) echo "5a4ec883379d51ee9ce1040d5e87f8d35e20387574dd8c947feb01eabc3c1b37" ;;
    *) return 1 ;;
  esac
}

# Where a FIRST mother binary is fetched from with --download. Nothing else in
# this project reads it: agents download from their mother, and the mother
# compiles its own replacements. Overridable so e2e/download_test.sh can point
# it at a local tree instead of github.com.
RELEASE_BASE_URL="${RELEASE_BASE_URL:-https://github.com/osman-yahya/feast-watch}"

# fetch_release_binary downloads one published asset and verifies it before it
# is allowed to become anything. Used for the bootstrap mother binary, and for
# the co-located agent when it is taken from the mother's own catalogue — the
# URL shape is the same either way, which is why one function serves both.
#
# --fail is not optional. Without it `curl -o` exits 0 on an HTTP 404 and writes
# the response body, so a missing asset would install an error page as the
# mother, chmod 0755, and systemd would respawn against it every five seconds.
# `set -euo pipefail` does not catch that, because curl succeeded.
#
# The checksum is fetched second but is what decides: a build published without
# one, or one whose bytes do not match, is refused rather than installed. This
# is the same rule the agent installer and the mother's self-update apply, and
# it is what stands between an install and whatever answered the request.
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

# version_at_least compares two "1.26.7" triples numerically. Field by field
# rather than with sort -V, which is not on every host this may run on, and
# never as strings: "1.9" above "1.26" is exactly the comparison that would wave
# through a toolchain three years too old.
version_at_least() {
  local have="$1" want="$2" i h w
  for i in 1 2 3; do
    h=$(echo "$have" | cut -d. -f$i); w=$(echo "$want" | cut -d. -f$i)
    h=${h:-0}; w=${w:-0}
    case "$h$w" in *[!0-9]*) return 1 ;; esac
    [ "$h" -gt "$w" ] && return 0
    [ "$h" -lt "$w" ] && return 1
  done
  return 0
}

# go_new_enough reports whether this Go can build the tree. `go version` prints
# "go version go1.26.7 linux/amd64".
#
# GOTOOLCHAIN=local is what makes the answer true. Asked inside a module
# directory, a modern `go` silently switches to the toolchain go.mod names —
# downloading it if it must — and then reports THAT version. So an older Go
# answers "go1.26.1" while being nothing of the kind, this check accepts it, and
# every build afterwards pays a toolchain download over the network. Asking for
# the local one is asking what is actually installed.
go_new_enough() {
  local raw
  raw=$(GOTOOLCHAIN=local "$1" version 2>/dev/null) || return 1
  raw=${raw#go version go}
  raw=${raw%% *}
  [ -n "$raw" ] || return 1
  version_at_least "$raw" "$GO_MIN"
}

# usable_go prints the path of a Go new enough to build this project, if the
# host already has one. Ours is preferred over whatever is on PATH: on a host
# where both exist, the one this installer pinned is the one the checksums
# describe.
usable_go() {
  local candidate
  for candidate in "$GO_ROOT/bin/go" "$(command -v go 2>/dev/null || true)"; do
    [ -n "$candidate" ] && [ -x "$candidate" ] || continue
    if go_new_enough "$candidate"; then
      echo "$candidate"
      return 0
    fi
  done
  return 1
}

# install_go downloads the pinned toolchain, verifies it, and puts it where any
# user on this host will find it.
#
# The symlink is the point: `feast-watch build` shells out to `go` and is run as
# the unprivileged service account, whose non-login shell inherits none of the
# PATH edits a profile script would make. /usr/local/bin is on every default
# PATH there is, including that one.
install_go() {
  local platform="$1" want got url tmp
  want=$(go_sha256 "$platform") || {
    echo "no pinned Go build for $platform — install a Go >= $GO_MIN by hand, then re-run with --skip-go" >&2
    return 1
  }
  url="$GO_DL_BASE/go$GO_VERSION.$platform.tar.gz"

  tmp=$(mktemp)
  # shellcheck disable=SC2064  # expand tmp now: it must be removed on any exit
  trap "rm -f '$tmp'" RETURN

  echo "-> downloading Go $GO_VERSION ($platform)"
  curl -fsSL "$url" -o "$tmp" ||
    { echo "could not download $url" >&2; return 1; }

  got=$(sha256sum "$tmp" 2>/dev/null | cut -d' ' -f1 || shasum -a 256 "$tmp" | cut -d' ' -f1)
  if [ "$want" != "$got" ]; then
    echo "checksum mismatch for the Go toolchain: refusing to install it" >&2
    return 1
  fi

  # Replaced whole rather than extracted over: a tarball unpacked on top of an
  # older tree leaves that tree's files behind, and a toolchain half of one
  # version and half of another fails in ways nobody should have to debug.
  rm -rf "$GO_ROOT"
  mkdir -p "$(dirname "$GO_ROOT")" "$(dirname "$GO_LINK")"
  tar -C "$(dirname "$GO_ROOT")" -xzf "$tmp" ||
    { echo "could not extract the Go toolchain" >&2; return 1; }
  ln -sf "$GO_ROOT/bin/go" "$GO_LINK"
  echo "   installed $GO_ROOT, linked as $GO_LINK"
  GO_INSTALLED=1
}

# build_here compiles the mother (and, with --with-agent, the agent) from this
# checkout, using the toolchain ensure_go just guaranteed.
#
# This is what makes a bare host one command from a running mother without a
# published release in the picture at all. It is also the only order that works:
# building needs Go, Go arrives with this installer, so the build cannot be
# something the operator was expected to have done beforehand.
build_here() {
  local both="$1" release_sh
  release_sh="$SCRIPT_DIR/../bin/release.sh"
  [ -f "$release_sh" ] || {
    echo "--build needs the source tree: $release_sh is not there" >&2
    return 1
  }

  echo "-> building from this checkout"
  # The installed toolchain is put on PATH explicitly: the shell running this
  # installer was started before that toolchain existed, so it does not have it.
  local args=(--mother-only)
  [ "$both" -eq 1 ] && args=()
  PATH="$(dirname "$GO_LINK"):$GO_ROOT/bin:$PATH" bash "$release_sh" "${args[@]}" >&2 ||
    { echo "the build failed; nothing was installed" >&2; return 1; }
}

# ensure_go leaves this host with a compiler, or says why it could not.
ensure_go() {
  local existing
  if existing=$(usable_go); then
    echo "-> using the Go toolchain already on this host ($existing)"
    return 0
  fi
  install_go "$1"
}

# mother_port reads the port the mother listens on out of its env file, so the
# agent beside it and the fetch of that agent's binary both address the mother
# that is actually running rather than a default one of them assumed.
mother_port() {
  local listen
  listen=$(sed -n 's/^FW_LISTEN=//p' "$ENV_FILE")
  listen="${listen##*:}"
  echo "${listen:-8443}"
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
    echo "# terminated in front of it. It is also where agents download their"
    echo "# binaries from, because it is the only address they have."
    echo "FW_PUBLIC_URL=http://127.0.0.1:8443"
    echo "FW_API_KEY=change-me"
    echo
    echo "# The build catalogue: what \`feast-watch build\` writes and what this"
    echo "# mother serves to its fleet. Defaults beside the database."
    echo "# FW_BUILD_DIR=/var/lib/feast-watch/builds"
    echo "# Where \`feast-watch build\` takes source from when no FW_SOURCE_DIR"
    echo "# is set: the tag's own archive, over the one route off this network"
    echo "# that only this host needs."
    echo "# FW_SOURCE_URL=https://github.com/osman-yahya/feast-watch"
    echo "# FW_SOURCE_DIR=/opt/feast-watch/src"
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
  src="$SCRIPT_DIR/feast-watch-mother-promote"
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
    if [ "$GO_INSTALLED" -eq 1 ]; then
      echo "go=$GO_ROOT"
      echo "go_link=$GO_LINK"
    fi
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
  local source_dir="$1" platform="$2" name
  name="$(hostname -s 2>/dev/null || hostname)"

  echo "-> registering this host as '$name'"
  # generate is create-or-fetch, so re-running the installer does not mint a
  # second server for the same host.
  local token
  token=$(FW_DB_PATH="$FW_ROOT/var/lib/feast-watch/mother.db" \
          "$BIN_DEST" generate --name="$name" |
          sed -n 's|.*/install/\(tk_[0-9a-f]*\)\.sh.*|\1|p')
  [ -n "$token" ] || { echo "could not read a token from feast-watch generate" >&2; return 1; }

  # Two sources, one destination. From a checkout, the agent built beside the
  # mother; otherwise from the mother that was just started, over the loopback,
  # exactly as every other agent on the fleet gets its binary.
  #
  # The second path needs the mother to have built something. It is a fresh
  # install, so usually it has not — hence the message rather than a bare 404:
  # what the operator has to do next is one command on this same host.
  local agent_src="$source_dir/feast-watch-agent"
  if [ -n "$source_dir" ] && [ -f "$agent_src" ]; then
    echo "-> installing agent binary"
    install -m 0755 "$agent_src" "$AGENT_BIN_DEST"
  else
    echo "-> downloading agent $platform from this mother's catalogue"
    RELEASE_BASE_URL="http://127.0.0.1:$(mother_port)" \
      fetch_release_binary "feast-watch-agent-$platform" "$AGENT_BIN_DEST" latest || {
        echo "this mother has built no agent for $platform yet — run" >&2
        echo "  $BIN_DEST build <version>" >&2
        echo "on this host, then re-run this installer with --with-agent" >&2
        return 1
      }
  fi

  if [ -f "$AGENT_CONF" ]; then
    echo "   kept existing $AGENT_CONF"
  else
    # The mother is reached over the loopback: the agent is on the same host,
    # so its traffic never needs to leave it whatever FW_PUBLIC_URL says. That
    # address is also where its updates come from — an agent has one host.
    {
      echo "MOTHER_URL=http://127.0.0.1:$(mother_port)"
      echo "TOKEN=$token"
      echo "SERVER_NAME=$name"
    } > "$AGENT_CONF"
    chmod 0600 "$AGENT_CONF"
    echo "   wrote $AGENT_CONF"
  fi

  install_agent_uninstaller

  echo "-> installing agent unit"
  install -m 0644 "$SCRIPT_DIR/feast-watch-agent.service" "$AGENT_UNIT"
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
  src="$SCRIPT_DIR/../mother/api/uninstall.sh"
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
  local skip_go=0
  local build=0
  local version=latest
  local args=()
  for arg in "$@"; do
    case "$arg" in
      --with-agent) with_agent=1 ;;
      --download) download=1 ;;
      --skip-go) skip_go=1 ;;
      --build) build=1 ;;
      --download=*) download=1; version="${arg#--download=}" ;;
      *) args+=("$arg") ;;
    esac
  done

  local source="${args[0]:-$SCRIPT_DIR/../bin/build/feast-watch}"

  [ "$(id -u)" -eq 0 ] || { echo "must run as root" >&2; return 1; }

  # The platform names three things: the mother asset to fetch, the agent build
  # to install beside it, and the Go tarball for this machine. Every path below
  # wants at least one of them.
  local platform
  platform=$(release_platform) || return 1

  # The toolchain first, before anything that might need it. --build does; and
  # even without it this host cannot produce a version for its fleet until a
  # compiler is here, so the install is not finished without one either way.
  if [ "$skip_go" -eq 1 ]; then
    echo "-> skipping the Go toolchain as asked; \`feast-watch build\` needs one on PATH"
  else
    ensure_go "$platform" || return 1
  fi

  if [ "$build" -eq 1 ]; then
    build_here "$with_agent" || return 1
    source="$SCRIPT_DIR/../bin/build/feast-watch"
  fi

  # Where a locally built agent would be, when this is a checkout rather than a
  # download. Read before --download replaces `source` with a temp file.
  local agent_source_dir=""
  [ "$download" -eq 1 ] || agent_source_dir="$(dirname "$source")"

  if [ "$download" -eq 1 ]; then
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
  install -m 0644 "$SCRIPT_DIR/feast-watch-mother.service" "$UNIT"
  write_manifest

  systemctl daemon-reload
  systemctl enable "$UNIT_NAME"

  if [ "$with_agent" -eq 1 ]; then
    echo
    echo "-> starting the mother so it can mint this host's token"
    systemctl start "$UNIT_NAME"
    install_local_agent "$agent_source_dir" "$platform"
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
