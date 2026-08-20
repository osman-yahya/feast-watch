package agent

import (
	"net/http"
	"os"
	"runtime"
	"time"

	"github.com/osman-yahya/feast-watch/shared/release"
	"github.com/osman-yahya/feast-watch/shared/selfupdate"
)

// SelfUpdate downloads desiredVersion from the release host, verifies its
// SHA-256, atomically replaces the running executable and exits 0 — the
// service manager restarts us.
func SelfUpdate(cfg Config, desiredVersion string, exit func(int)) error {
	return SelfUpdateWithClient(cfg, desiredVersion, exit, &http.Client{Timeout: 60 * time.Second})
}

// SelfUpdateWithClient is SelfUpdate with a caller-supplied client, so the
// download runs on the same transport policy as the push loop with its own
// timeout — a binary transfer must not give up as fast as a push does.
func SelfUpdateWithClient(cfg Config, desiredVersion string, exit func(int), client *http.Client) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	return selfUpdate(cfg, desiredVersion, self, exit, client)
}

// selfUpdate fetches the build for this platform from the release host.
//
// The binary comes from whatever RELEASE_BASE_URL names — GitHub Releases by
// default, and the mother where it is mirroring for a fleet with no route to
// the internet. The agent cannot tell the two apart and has no reason to: both
// serve the same URL shape and the same published checksum, and it verifies
// before replacing itself either way.
//
// Fetching straight from the release host is still the better arrangement
// wherever it works, because it keeps binary distribution off the monitoring
// path entirely: the mother then stores no builds, serves no bytes, and a
// rollout cannot be blocked by the mother's disk. Mirroring is what a fleet
// whose agents are offline by policy trades that for (mother/mirror).
//
// The transfer itself lives in shared/selfupdate, which the mother uses too.
// What stays here is the half that is the agent's alone: it runs as root and
// unsandboxed, so its destination is its own executable, and the restart is
// systemd's answer to exiting 0.
func selfUpdate(cfg Config, desiredVersion, target string, exit func(int), client *http.Client) error {
	asset := release.AssetName(runtime.GOOS, runtime.GOARCH)
	if err := selfupdate.Place(client, cfg.releaseBaseURL(), desiredVersion, asset, target); err != nil {
		return err
	}
	exit(0)
	return nil
}
