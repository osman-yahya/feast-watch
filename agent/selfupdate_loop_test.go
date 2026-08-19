package agent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/osman-yahya/feast-watch/agent/collectors"
	"github.com/osman-yahya/feast-watch/shared/protocol"
	"github.com/osman-yahya/feast-watch/shared/version"
)

// versionServer answers every push with the same desired version and records
// what the agent reported back about its own update attempts.
func versionServer(t *testing.T, desired string, seen *[]protocol.IngestRequest) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req protocol.IngestRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
		}
		*seen = append(*seen, req)
		json.NewEncoder(w).Encode(protocol.IngestResponse{Interval: 10, DesiredVersion: desired})
	}))
}

// The mother cannot tell which binary a host can run without the architecture,
// so it cannot reject an impossible rollout target before the agent tries it.
func TestFirstPushReportsArch(t *testing.T) {
	var seen []protocol.IngestRequest
	srv := versionServer(t, "", &seen)
	defer srv.Close()

	l := NewLoop(Config{MotherURL: srv.URL, Token: "t", ServerName: "s1"}, collectors.NewRegistry())
	if _, err := l.PushOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if seen[0].Arch != runtime.GOARCH {
		t.Fatalf("arch = %q, want %q", seen[0].Arch, runtime.GOARCH)
	}
}

// A failed update is otherwise invisible to the operator: the panel would show
// a target the agent silently never reaches, with the reason only in this
// host's journal.
func TestFailedUpdateIsReportedOnNextPush(t *testing.T) {
	var seen []protocol.IngestRequest
	srv := versionServer(t, "v9.9.9", &seen)
	defer srv.Close()

	l := NewLoop(Config{MotherURL: srv.URL, Token: "t", ServerName: "s1"}, collectors.NewRegistry())
	l.tryUpdate("v9.9.9", func(string) error { return errors.New("404 not staged") })

	if _, err := l.PushOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if seen[0].UpdateError != "404 not staged" {
		t.Fatalf("update error not reported: %q", seen[0].UpdateError)
	}
}

// A recovered agent must clear the error without operator action, otherwise
// the panel keeps showing a failure that is no longer true.
func TestSuccessfulUpdateClearsReportedError(t *testing.T) {
	var seen []protocol.IngestRequest
	srv := versionServer(t, "v9.9.9", &seen)
	defer srv.Close()

	l := NewLoop(Config{MotherURL: srv.URL, Token: "t", ServerName: "s1"}, collectors.NewRegistry())
	l.tryUpdate("v9.9.9", func(string) error { return errors.New("boom") })
	// A real success replaces the binary and exits; the loop only reaches the
	// clearing path when the update returns without having exited.
	l.now = func() time.Time { return time.Now().Add(updateRetryGap) }
	l.tryUpdate("v9.9.9", func(string) error { return nil })

	if _, err := l.PushOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if seen[0].UpdateError != "" {
		t.Fatalf("error must clear after a successful update, got %q", seen[0].UpdateError)
	}
}

// Without the gap, an unreachable target is re-downloaded on every push — a
// whole binary every 10 seconds, indefinitely.
func TestUpdateRetryIsThrottledPerTarget(t *testing.T) {
	attempts := 0
	fail := func(string) error { attempts++; return errors.New("nope") }

	now := time.Unix(1700000000, 0)
	l := NewLoop(Config{}, collectors.NewRegistry())
	l.now = func() time.Time { return now }

	l.tryUpdate("v9.9.9", fail)
	l.tryUpdate("v9.9.9", fail)
	now = now.Add(updateRetryGap - time.Second)
	l.tryUpdate("v9.9.9", fail)
	if attempts != 1 {
		t.Fatalf("attempts within the gap = %d, want 1", attempts)
	}

	now = now.Add(2 * time.Second) // gap elapsed
	l.tryUpdate("v9.9.9", fail)
	if attempts != 2 {
		t.Fatalf("attempts after the gap = %d, want 2", attempts)
	}
}

// An operator correcting a bad target should be acted on at the next push,
// not after the previous target's backoff expires.
func TestNewTargetResetsRetryGap(t *testing.T) {
	attempts := 0
	fail := func(string) error { attempts++; return errors.New("nope") }

	now := time.Unix(1700000000, 0)
	l := NewLoop(Config{}, collectors.NewRegistry())
	l.now = func() time.Time { return now }

	l.tryUpdate("v9.9.9", fail)
	l.tryUpdate("v1.3.0", fail) // corrected target, same instant
	if attempts != 2 {
		t.Fatalf("a new target must not inherit the previous backoff, attempts = %d", attempts)
	}
}

// scriptedMother is a fake mother whose answer to each push is scripted: push
// N is answered with the Nth desired version (the last entry repeats). It
// records every request, which is the only thing the panel ever sees, so the
// tests below assert on the wire rather than on the loop's internals.
type scriptedMother struct {
	mu      sync.Mutex
	seen    []protocol.IngestRequest
	decErr  error
	desired []string
	served  chan struct{}
}

func newScriptedMother(desired ...string) *scriptedMother {
	return &scriptedMother{desired: desired, served: make(chan struct{}, 64)}
}

func (m *scriptedMother) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req protocol.IngestRequest
	err := json.NewDecoder(r.Body).Decode(&req)

	m.mu.Lock()
	if err != nil && m.decErr == nil {
		m.decErr = err
	}
	m.seen = append(m.seen, req)
	n := len(m.seen)
	m.mu.Unlock()

	if n > len(m.desired) {
		n = len(m.desired)
	}
	// Interval 1 keeps the loop's own sleep from dominating the test; the
	// retry gap is driven by the injected clock, not by wall time.
	json.NewEncoder(w).Encode(protocol.IngestResponse{Interval: 1, DesiredVersion: m.desired[n-1]})

	select {
	case m.served <- struct{}{}:
	default:
	}
}

// request returns the Nth push (1-based) as the mother received it.
func (m *scriptedMother) request(t *testing.T, n int) protocol.IngestRequest {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.decErr != nil {
		t.Fatalf("decoding push: %v", m.decErr)
	}
	if len(m.seen) < n {
		t.Fatalf("only %d pushes reached the mother, wanted %d", len(m.seen), n)
	}
	return m.seen[n-1]
}

// runLoop drives Run against the fake mother until it has served pushes
// pushes, then stops it. Assertions read the recorded requests, so a push
// aborted by the shutdown cannot affect them.
func runLoop(t *testing.T, m *scriptedMother, pushes int, update func(string) error) {
	t.Helper()
	srv := httptest.NewServer(m)
	defer srv.Close()

	l := NewLoop(Config{MotherURL: srv.URL, Token: "t", ServerName: "s1"}, collectors.NewRegistry())
	// One frozen instant: whether the retry gap suppresses an attempt must be
	// decided by the code under test, not by how long the test happened to take.
	frozen := time.Unix(1700000000, 0)
	l.now = func() time.Time { return frozen }

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); l.Run(ctx, update, nil) }()

	timeout := time.After(30 * time.Second)
	for served := 0; served < pushes; {
		select {
		case <-m.served:
			served++
		case <-timeout:
			cancel()
			t.Fatal("timed out waiting for pushes")
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop")
	}
}

// Withdrawing the target is how an operator calls off a rollout that will not
// install. If the agent keeps shipping the old reason, the panel shows a
// failure for a server that has nothing outstanding, and no panel action can
// clear it — the next push just writes it back.
func TestWithdrawnTargetStopsReportingTheError(t *testing.T) {
	t.Parallel()
	m := newScriptedMother("v9.9.9", "")
	runLoop(t, m, 3, func(string) error { return errors.New("404 not staged") })

	if got := m.request(t, 2).UpdateError; got != "404 not staged" {
		t.Fatalf("push 2 must still report the live failure, got %q", got)
	}
	if got := m.request(t, 3).UpdateError; got != "" {
		t.Fatalf("push after the target was withdrawn still reports %q", got)
	}
}

// Re-targeting the version the agent already runs is the other way to call a
// rollout off, and the one the mother itself produces once a host reaches the
// target. Nothing is outstanding, so nothing is failing.
func TestTargetMatchingRunningVersionStopsReportingTheError(t *testing.T) {
	t.Parallel()
	m := newScriptedMother("v9.9.9", version.Version)
	runLoop(t, m, 3, func(string) error { return errors.New("404 not staged") })

	if got := m.request(t, 3).UpdateError; got != "" {
		t.Fatalf("target already satisfied, but push still reports %q", got)
	}
}

// The other half of the rule: while a target is still being asked for, the
// last failure at it is the current state and must keep reaching the operator,
// even though the retry gap is suppressing further attempts.
func TestThrottledTargetKeepsReportingTheError(t *testing.T) {
	t.Parallel()
	attempts := 0
	m := newScriptedMother("v9.9.9")
	runLoop(t, m, 3, func(string) error { attempts++; return errors.New("404 not staged") })

	if attempts != 1 {
		t.Fatalf("retry gap must hold: attempts = %d, want 1", attempts)
	}
	if got := m.request(t, 3).UpdateError; got != "404 not staged" {
		t.Fatalf("an unresolved failure must stay visible, got %q", got)
	}
}

// Withdrawing a target and naming it again is an operator retrying by hand.
// If the gap survived the withdrawal the agent would sit out the next five
// minutes without attempting anything and without an error to show for it —
// a rollout stuck on "pending" with no reason attached.
func TestRetargetingAfterAWithdrawalAttemptsAgain(t *testing.T) {
	t.Parallel()
	attempts := 0
	m := newScriptedMother("v9.9.9", "", "v9.9.9")
	runLoop(t, m, 4, func(string) error { attempts++; return errors.New("404 not staged") })

	if attempts != 2 {
		t.Fatalf("re-named target must be attempted again, attempts = %d, want 2", attempts)
	}
	if got := m.request(t, 4).UpdateError; got != "404 not staged" {
		t.Fatalf("the fresh failure must be reported, got %q", got)
	}
}
