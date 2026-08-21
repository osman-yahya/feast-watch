// Package selfupdate converges the mother on the version an operator picked in
// the panel.
//
// The mother cannot replace its own binary. Its unit runs it as an unprivileged
// user under ProtectSystem=strict, so /usr/local/bin is read-only inside its
// mount namespace whatever the file permissions say. So this half does what it
// is allowed to do — download, verify, stage inside StateDirectory — and then
// asks the process to shut down. A root ExecStartPre helper installs the staged
// binary at the next start, before ExecStart runs it.
//
// Its shape is otherwise the agent's exactly: verify before replacing, replace
// atomically, exit, let the service manager bring the process back. Only the
// actor performing the replacement differs, because only the mother is
// sandboxed — and only the mother is offline while it updates, which is why the
// state it reports lives in the database rather than in a push.
package selfupdate

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/osman-yahya/feast-watch/mother/store"
	"github.com/osman-yahya/feast-watch/shared/release"
	sharedupdate "github.com/osman-yahya/feast-watch/shared/selfupdate"
)

// stagedName is what the promote helper looks for. Shared with
// deploy/feast-watch-mother-promote by convention, and pinned by
// e2e/promote_test.sh so the two cannot drift apart in silence.
const stagedName = "feast-watch.new"

type Config struct {
	// DownloadBaseURL is where the mother fetches its OWN replacement from: its
	// public URL, because the catalogue it compiled into is the only place a
	// mother binary of that version exists. Going over its own HTTP surface
	// rather than straight to the file is deliberate — the update travels the
	// exact path the fleet's does, so if serving is broken the mother finds
	// that out about its own update instead of being the one client that never
	// noticed.
	DownloadBaseURL string
	// PromotePath is the root helper that installs a staged binary at the next
	// start. Its absence is how a deployment says it cannot self-update.
	PromotePath string
	StageDir    string
	// Platform is this mother's "<goos>-<goarch>": which asset to fetch, and
	// which build a target must have to be selectable at all.
	Platform    string
	MaxAttempts int
	Interval    time.Duration
}

type Updater struct {
	st       *store.Store
	cfg      Config
	client   *http.Client
	now      func() time.Time
	shutdown func()
}

func New(st *store.Store, cfg Config, client *http.Client, now func() time.Time, shutdown func()) *Updater {
	return &Updater{st: st, cfg: cfg, client: client, now: now, shutdown: shutdown}
}

// StagedPath is where a verified binary waits for the promote helper.
func (u *Updater) StagedPath() string { return filepath.Join(u.cfg.StageDir, stagedName) }

// Platform is this mother's build platform, so the API can refuse a target that
// was never built for it and name what it wanted.
func (u *Updater) Platform() string { return u.cfg.Platform }

// Supported reports whether this deployment can actually apply an update.
//
// In a container there is no systemd and no promote hook: the process would
// stage a binary, exit, and come back from the image on the old version. That
// is a worse answer than refusing, so the refusal is structural — the image
// does not ship the helper, and its absence is read here.
func (u *Updater) Supported() bool {
	if u.cfg.PromotePath == "" {
		return false
	}
	info, err := os.Stat(u.cfg.PromotePath)
	return err == nil && !info.IsDir()
}

// Reconcile settles what the last boot's update did. It runs once at startup,
// before the loop: the mother is offline while it updates, so this is the only
// moment it can report on itself.
func (u *Updater) Reconcile(running string) error {
	row, err := u.st.MotherUpdate()
	if err != nil {
		return err
	}
	if row.DesiredVersion == "" {
		return nil
	}
	if row.DesiredVersion == running {
		slog.Info("mother self-update landed", "version", running)
		return u.st.ClearMotherUpdate(u.now().Unix())
	}
	if row.StagedVersion != "" {
		// Staged, restarted, still not it. The promote step did not happen.
		// The target is left standing: the attempt bound is what ends this,
		// and it ends it with a reason rather than with silence.
		return u.st.RecordMotherUpdateError(fmt.Sprintf(
			"staged %s but %s is still running: the promote step did not take",
			row.StagedVersion, running))
	}
	return nil
}

// Tick is one convergence step: at most one download, and at most one request
// to shut down.
func (u *Updater) Tick(running string) error {
	row, err := u.st.MotherUpdate()
	if err != nil {
		return err
	}
	if row.DesiredVersion == "" || row.DesiredVersion == running {
		return nil
	}
	if !u.Supported() {
		// The API refuses to set a target here, so reaching this means the
		// helper went away after one was set. Do nothing rather than stage a
		// binary that would be discarded on restart.
		return nil
	}
	if row.Attempts >= u.cfg.MaxAttempts {
		return u.st.FailMotherUpdate(fmt.Sprintf(
			"gave up on %s after %d attempts: %s",
			row.DesiredVersion, row.Attempts, lastReason(row.Error)))
	}

	// Committed BEFORE the download. A counter written after a step that never
	// completes counts nothing, and this counter is the only thing between a
	// bad target and download-exit-restart forever.
	attempt, err := u.st.BeginMotherAttempt()
	if err != nil {
		return err
	}

	goos, goarch, _ := strings.Cut(u.cfg.Platform, "-")
	asset := release.MotherAssetName(goos, goarch)
	slog.Info("staging mother self-update",
		"version", row.DesiredVersion, "attempt", attempt, "asset", asset)

	if err := os.MkdirAll(u.cfg.StageDir, 0o755); err != nil {
		return u.record(err)
	}
	if err := sharedupdate.Place(u.client, u.cfg.DownloadBaseURL, row.DesiredVersion, asset, u.StagedPath()); err != nil {
		return u.record(err)
	}
	if err := u.st.StageMotherUpdate(row.DesiredVersion); err != nil {
		return err
	}

	slog.Info("mother self-update staged; shutting down for the promote step",
		"version", row.DesiredVersion, "staged", u.StagedPath())
	u.shutdown()
	return nil
}

// record stores the reason and returns the original error, so the log and the
// panel see the same thing.
func (u *Updater) record(err error) error {
	if storeErr := u.st.RecordMotherUpdateError(err.Error()); storeErr != nil {
		slog.Error("recording a self-update failure failed too", "err", storeErr)
	}
	return err
}

func lastReason(msg string) string {
	if msg == "" {
		return "no reason recorded"
	}
	return msg
}

// Run reconciles once, then converges on a ticker until ctx is done.
//
// The interval is one indexed read of a single row against a database this
// process already holds open — cheaper than the hourly retention sweep — and it
// bounds "I pressed update" to well under a minute.
func (u *Updater) Run(ctx context.Context, running string) {
	if err := u.Reconcile(running); err != nil {
		slog.Error("reconciling the mother's update state", "err", err)
	}
	ticker := time.NewTicker(u.cfg.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := u.Tick(running); err != nil {
				slog.Error("mother self-update", "err", err)
			}
		}
	}
}
