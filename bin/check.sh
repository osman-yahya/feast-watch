#!/usr/bin/env bash
# Everything that used to run in CI, in one command.
#
# The GitHub workflows are gone: this project no longer depends on GitHub for
# anything — not for the binaries, which the mother compiles from source
# (mother/build), and not for the tag that names them. What went with them was
# the only thing that ran these checks on a schedule, so they live here instead,
# where anyone can run them and nothing has to be reachable.
#
# The checks themselves are not ceremony. In one afternoon they caught: a lint
# whose failure had silently skipped the check behind it for eleven runs; an
# uninstaller that died half-way through removing a host when systemd refused an
# unprivileged call; and a `grep -q` that killed the process feeding it, which
# passed on a laptop and failed on one branch of two.
#
#   bin/check.sh          # everything
#   bin/check.sh go       # just the Go suite
#   bin/check.sh shell    # just the shell suites
set -uo pipefail

cd "$(dirname "$0")/.." || exit 1

WHAT="${1:-all}"
FAILED=()

run() {
  local name="$1"; shift
  echo
  echo "==> $name"
  if "$@"; then
    return 0
  fi
  FAILED+=("$name")
  return 0   # keep going: one red check must not hide the next
}

# shellcheck disable=SC2329  # invoked by name through run(), which shellcheck
# cannot follow.
gofmt_check() {
  local unformatted
  unformatted=$(gofmt -l .)
  [ -z "$unformatted" ] && return 0
  echo "gofmt needed:"; echo "$unformatted"
  return 1
}

# shellcheck disable=SC2329  # invoked by name through run(), as above.
install_template() {
  # install.sh.tmpl is a Go template, so it is not valid shell until rendered.
  # The rendered script is piped into `sudo bash` on every monitored host, which
  # makes a syntax error here a broken install across the fleet, not a lint nit.
  go run ./.github/tools/rendertmpl mother/api/install.sh.tmpl /tmp/install.sh &&
    bash -n /tmp/install.sh &&
    shellcheck /tmp/install.sh
}

if [ "$WHAT" = all ] || [ "$WHAT" = go ]; then
  run "go vet" go vet ./...
  run "gofmt" gofmt_check
  run "go test" go test -race -cover ./...
fi

if [ "$WHAT" = all ] || [ "$WHAT" = shell ]; then
  # No --severity floor: every finding is either fixed or suppressed at its line
  # with a reason. A blanket floor would hide the next real one.
  run "shellcheck" shellcheck bin/*.sh e2e/*.sh deploy/*.sh deploy/feast-watch-mother-promote mother/api/uninstall.sh
  run "install template" install_template
  run "e2e: co-location" bash e2e/colocation_test.sh
  run "e2e: promote helper" bash e2e/promote_test.sh
  run "e2e: download" bash e2e/download_test.sh
  run "e2e: go toolchain" bash e2e/toolchain_test.sh
  run "e2e: smoke" bash e2e/local_smoke.sh
fi

echo
if [ ${#FAILED[@]} -eq 0 ]; then
  echo "all checks passed"
  exit 0
fi
echo "FAILED: ${FAILED[*]}" >&2
exit 1
