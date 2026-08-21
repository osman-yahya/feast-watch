package agent

import (
	"net/http"
	"os"
	"runtime"
	"time"

	"github.com/osman-yahya/feast-watch/shared/release"
	"github.com/osman-yahya/feast-watch/shared/selfupdate"
)

// SelfUpdate downloads desiredVersion from the mother, verifies its SHA-256,
// atomically replaces the running executable and exits 0 — the service manager
// restarts us.
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

// selfUpdate fetches the build for this platform from the mother.
//
// The mother is the only address an agent has. There is no release host key to
// set and no public default to fall back to, because these agents have no route
// off their network: the mother compiles what they run (mother/build) and serves
// it on the same URL shape a release host used, so this is one download against
// the host the agent was already talking to.
//
// That the mother is also the authority for the checksum is the trade this
// arrangement makes, and it is a real one — verification here proves the
// transfer was intact, not that somebody else agreed what the bytes should be.
// What buys it back is that the mother is on the private network and compiled
// the binary itself, so there is no third party in the path to disagree with.
//
// The transfer itself lives in shared/selfupdate, which the mother uses too.
// What stays here is the half that is the agent's alone: it runs as root and
// unsandboxed, so its destination is its own executable, and the restart is
// systemd's answer to exiting 0.
func selfUpdate(cfg Config, desiredVersion, target string, exit func(int), client *http.Client) error {
	asset := release.AssetName(runtime.GOOS, runtime.GOARCH)
	if err := selfupdate.Place(client, cfg.MotherURL, desiredVersion, asset, target); err != nil {
		return err
	}
	exit(0)
	return nil
}
