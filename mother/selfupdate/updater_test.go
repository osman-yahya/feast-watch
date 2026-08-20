package selfupdate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/osman-yahya/feast-watch/mother/store"
)

const running = "v1.3.0"

func testStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func digest(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// promoteHelper writes a stand-in for the root helper. Only its presence is
// read — the updater never runs it, systemd does.
func promoteHelper(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "promote")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// newFixture returns an updater whose release host serves body for every mother
// asset, and a counter of how often it asked to shut down.
func newFixture(t *testing.T, body []byte, sum, promote string) (*Updater, *store.Store, *int) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, ".sha256"):
			w.Write([]byte(sum + "\n"))
		case strings.Contains(r.URL.Path, "feast-watch-mother-"):
			w.Write(body)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	st := testStore(t)
	shutdowns := 0
	u := New(st, Config{
		ReleaseBaseURL: srv.URL,
		PromotePath:    promote,
		StageDir:       filepath.Join(t.TempDir(), "update"),
		Platform:       "linux-amd64",
		MaxAttempts:    3,
		Interval:       time.Millisecond,
	}, srv.Client(), func() time.Time { return time.Unix(1000, 0) }, func() { shutdowns++ })
	return u, st, &shutdowns
}

func TestTickStagesTheBinaryAndAsksToShutDown(t *testing.T) {
	body := []byte("new mother")
	u, st, shutdowns := newFixture(t, body, digest(body), promoteHelper(t))
	if err := st.SetMotherDesiredVersion("v1.4.0", 900); err != nil {
		t.Fatal(err)
	}

	if err := u.Tick(running); err != nil {
		t.Fatal(err)
	}

	staged, err := os.ReadFile(u.StagedPath())
	if err != nil {
		t.Fatalf("nothing staged: %v", err)
	}
	if string(staged) != string(body) {
		t.Fatalf("staged content: %q", staged)
	}
	if *shutdowns != 1 {
		t.Fatalf("shutdown requested %d times", *shutdowns)
	}
	row, err := st.MotherUpdate()
	if err != nil {
		t.Fatal(err)
	}
	if row.StagedVersion != "v1.4.0" || row.Attempts != 1 {
		t.Fatalf("row: %+v", row)
	}
}

// Nothing to do is the common case, and it must cost one row read and no
// network at all.
func TestTickDoesNothingWithoutATarget(t *testing.T) {
	u, _, shutdowns := newFixture(t, []byte("x"), digest([]byte("x")), promoteHelper(t))
	if err := u.Tick(running); err != nil {
		t.Fatal(err)
	}
	if *shutdowns != 0 {
		t.Fatal("shut down with no target set")
	}
	if _, err := os.Stat(u.StagedPath()); err == nil {
		t.Fatal("staged a binary with no target set")
	}
}

func TestTickIsANoOpWhenTheTargetIsAlreadyRunning(t *testing.T) {
	u, st, shutdowns := newFixture(t, []byte("x"), digest([]byte("x")), promoteHelper(t))
	if err := st.SetMotherDesiredVersion(running, 900); err != nil {
		t.Fatal(err)
	}
	if err := u.Tick(running); err != nil {
		t.Fatal(err)
	}
	if *shutdowns != 0 {
		t.Fatal("restarted to install the version already running")
	}
}

// A corrupt or substituted binary must never be staged, and the target stays:
// the failure may be transient, and the attempt counter bounds the retrying.
func TestTickRefusesAChecksumMismatchAndKeepsTheTarget(t *testing.T) {
	u, st, shutdowns := newFixture(t, []byte("tampered"), digest([]byte("expected")), promoteHelper(t))
	if err := st.SetMotherDesiredVersion("v1.4.0", 900); err != nil {
		t.Fatal(err)
	}

	if err := u.Tick(running); err == nil {
		t.Fatal("expected a checksum failure")
	}
	if *shutdowns != 0 {
		t.Fatal("shut down despite staging nothing")
	}
	if _, err := os.Stat(u.StagedPath()); err == nil {
		t.Fatal("a binary that failed verification was staged")
	}
	row, err := st.MotherUpdate()
	if err != nil {
		t.Fatal(err)
	}
	if row.DesiredVersion != "v1.4.0" {
		t.Fatalf("target must survive a transient failure: %+v", row)
	}
	if !strings.Contains(row.Error, "checksum mismatch") {
		t.Fatalf("error: %q", row.Error)
	}
}

// The bound is what stops a mother that restarts into the same failure from
// downloading and exiting forever.
func TestTickGivesUpAfterMaxAttempts(t *testing.T) {
	u, st, _ := newFixture(t, []byte("tampered"), digest([]byte("expected")), promoteHelper(t))
	if err := st.SetMotherDesiredVersion("v1.4.0", 900); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 3; i++ {
		if err := u.Tick(running); err == nil {
			t.Fatalf("attempt %d unexpectedly succeeded", i+1)
		}
	}
	// The fourth tick must not start a fourth attempt, and giving up is not
	// itself an error — there is nobody left to report it to.
	if err := u.Tick(running); err != nil {
		t.Fatalf("the giving-up tick must not be an error: %v", err)
	}

	row, err := st.MotherUpdate()
	if err != nil {
		t.Fatal(err)
	}
	if row.DesiredVersion != "" {
		t.Fatalf("target must be dropped once the bound is reached: %+v", row)
	}
	if row.Attempts != 3 {
		t.Fatalf("attempts: %d", row.Attempts)
	}
	if row.Error == "" {
		t.Fatal("giving up must leave a reason")
	}
}

// Docker has no systemd and no promote hook: a staged binary is discarded and
// the old version comes back. Offering the update there would be a button
// whose only effect is a restart.
func TestUnsupportedWithoutThePromoteHelper(t *testing.T) {
	u, st, shutdowns := newFixture(t, []byte("x"), digest([]byte("x")), "/nonexistent/promote")
	if u.Supported() {
		t.Fatal("Supported() must be false with no promote helper")
	}
	if err := st.SetMotherDesiredVersion("v1.4.0", 900); err != nil {
		t.Fatal(err)
	}
	if err := u.Tick(running); err != nil {
		t.Fatal(err)
	}
	if *shutdowns != 0 {
		t.Fatal("staged an update a deployment cannot promote")
	}
}

// The boot half: the new binary is the only proof the update landed.
func TestReconcileClearsTheRowWhenTheTargetIsRunning(t *testing.T) {
	u, st, _ := newFixture(t, []byte("x"), digest([]byte("x")), promoteHelper(t))
	if err := st.SetMotherDesiredVersion("v1.4.0", 900); err != nil {
		t.Fatal(err)
	}
	if _, err := st.BeginMotherAttempt(); err != nil {
		t.Fatal(err)
	}

	if err := u.Reconcile("v1.4.0"); err != nil {
		t.Fatal(err)
	}
	row, err := st.MotherUpdate()
	if err != nil {
		t.Fatal(err)
	}
	if row.DesiredVersion != "" || row.Attempts != 0 || row.Error != "" {
		t.Fatalf("row must be idle after a landed update: %+v", row)
	}
	if row.AppliedAt != 1000 {
		t.Fatalf("applied_at: %d", row.AppliedAt)
	}
}

// A staged binary that did not become the running version means the promote
// step did not happen — a missing helper, or one that could not write. Saying
// so is the difference between a diagnosable failure and a silent loop.
func TestReconcileReportsAStagedUpdateThatDidNotTake(t *testing.T) {
	u, st, _ := newFixture(t, []byte("x"), digest([]byte("x")), promoteHelper(t))
	if err := st.SetMotherDesiredVersion("v1.4.0", 900); err != nil {
		t.Fatal(err)
	}
	if err := st.StageMotherUpdate("v1.4.0"); err != nil {
		t.Fatal(err)
	}

	if err := u.Reconcile(running); err != nil {
		t.Fatal(err)
	}
	row, err := st.MotherUpdate()
	if err != nil {
		t.Fatal(err)
	}
	if row.Error == "" {
		t.Fatal("a staged update that did not take must say so")
	}
	if row.DesiredVersion != "v1.4.0" {
		t.Fatalf("the target survives until the attempt bound: %+v", row)
	}
}

// Run is the loop main() starts. It reconciles first, before any tick: the
// mother is offline while it updates, so the moment it comes back is the only
// one in which it can report what the last boot did.
func TestRunReconcilesBeforeItTicks(t *testing.T) {
	u, st, shutdowns := newFixture(t, []byte("x"), digest([]byte("x")), promoteHelper(t))
	if err := st.SetMotherDesiredVersion("v1.4.0", 900); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	// The target is the version now running, so reconcile clears it and the
	// ticks that follow have nothing to do — if reconcile ran second, the
	// first tick would have staged an update of the running version.
	go func() { u.Run(ctx, "v1.4.0"); close(done) }()

	deadline := time.After(2 * time.Second)
	for {
		row, err := st.MotherUpdate()
		if err != nil {
			t.Fatal(err)
		}
		if row.AppliedAt != 0 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("Run never reconciled the landed update")
		default:
			time.Sleep(time.Millisecond)
		}
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return when its context was cancelled")
	}
	if *shutdowns != 0 {
		t.Fatal("Run restarted the mother to install the version already running")
	}
}

// A tick that fails must not stop the loop: the failure is bounded by the
// attempt counter, and a loop that exited on the first error would leave the
// mother unable to take any later target without a restart.
func TestRunKeepsGoingAfterAFailedTick(t *testing.T) {
	u, st, _ := newFixture(t, []byte("tampered"), digest([]byte("expected")), promoteHelper(t))
	if err := st.SetMotherDesiredVersion("v1.4.0", 900); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { u.Run(ctx, running); close(done) }()

	// Three failing attempts, then the bound clears the target and leaves the
	// reason standing. Reaching that state at all proves the loop survived the
	// failures on the way.
	deadline := time.After(3 * time.Second)
	for {
		row, err := st.MotherUpdate()
		if err != nil {
			t.Fatal(err)
		}
		if row.DesiredVersion == "" && row.Attempts == 3 && row.Error != "" {
			break
		}
		select {
		case <-deadline:
			row, _ := st.MotherUpdate()
			t.Fatalf("the loop never reached the attempt bound: %+v", row)
		default:
			time.Sleep(time.Millisecond)
		}
	}
	cancel()
	<-done
}
