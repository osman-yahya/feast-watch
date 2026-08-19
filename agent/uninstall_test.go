package agent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/osman-yahya/feast-watch/agent/collectors"
	"github.com/osman-yahya/feast-watch/shared/protocol"
)

// uninstallServer stands in for a mother that has scheduled this agent's
// removal, and records what the agent reports back to it.
func uninstallServer(t *testing.T, resp protocol.IngestResponse, seen *[]protocol.IngestRequest) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req protocol.IngestRequest
		json.NewDecoder(r.Body).Decode(&req)
		*seen = append(*seen, req)
		json.NewEncoder(w).Encode(resp)
	}))
}

func uninstallLoop(t *testing.T, srv *httptest.Server) *Loop {
	t.Helper()
	reg := collectors.NewRegistry()
	reg.Register(&stub{name: "cpu", key: "cpu.usage", val: 1})
	return NewLoop(Config{MotherURL: srv.URL, Token: "tk_abc", ServerName: "s1"}, reg)
}

// The removal command travels in the answer to the agent's own push — the
// mother has no way to reach the host — so the push loop is where it is acted
// on.
func TestPushActsOnTheUninstallCommand(t *testing.T) {
	var seen []protocol.IngestRequest
	srv := uninstallServer(t, protocol.IngestResponse{Interval: 10, Uninstall: true}, &seen)
	defer srv.Close()
	l := uninstallLoop(t, srv)

	calls := 0
	l.RunOnce(context.Background(), func(string) error { return nil }, func() error {
		calls++
		return nil
	})

	if calls != 1 {
		t.Fatalf("uninstall called %d times, want once", calls)
	}
}

func TestPushDoesNotUninstallWhenNotAsked(t *testing.T) {
	var seen []protocol.IngestRequest
	srv := uninstallServer(t, protocol.IngestResponse{Interval: 10}, &seen)
	defer srv.Close()
	l := uninstallLoop(t, srv)

	called := false
	l.RunOnce(context.Background(), func(string) error { return nil }, func() error {
		called = true
		return nil
	})
	if called {
		t.Fatal("an agent nobody deleted removed itself")
	}
}

// A removal that could not even start — no uninstaller on disk, a container
// with no systemd — has to reach the panel, or the row sits in "kaldırılıyor"
// with nothing saying why.
func TestUninstallFailureIsReportedOnTheNextPush(t *testing.T) {
	var seen []protocol.IngestRequest
	srv := uninstallServer(t, protocol.IngestResponse{Interval: 10, Uninstall: true}, &seen)
	defer srv.Close()
	l := uninstallLoop(t, srv)

	fail := func() error { return errors.New("uninstaller not found") }
	l.RunOnce(context.Background(), func(string) error { return nil }, fail)
	l.RunOnce(context.Background(), func(string) error { return nil }, fail)

	if len(seen) != 2 {
		t.Fatalf("want two pushes, got %d", len(seen))
	}
	if seen[0].UninstallError != "" {
		t.Fatalf("the first push cannot know about a failure yet: %q", seen[0].UninstallError)
	}
	if !strings.Contains(seen[1].UninstallError, "uninstaller not found") {
		t.Fatalf("failure not reported: %q", seen[1].UninstallError)
	}
}

// Removing an agent means stopping its own service, so the attempt is not
// retried on every push: a host where it fails would otherwise respawn the
// uninstaller every few seconds forever.
func TestUninstallRetryIsThrottled(t *testing.T) {
	var seen []protocol.IngestRequest
	srv := uninstallServer(t, protocol.IngestResponse{Interval: 10, Uninstall: true}, &seen)
	defer srv.Close()
	l := uninstallLoop(t, srv)

	now := time.Unix(1_700_000_000, 0)
	l.now = func() time.Time { return now }

	calls := 0
	attempt := func() error {
		calls++
		return errors.New("nope")
	}
	l.RunOnce(context.Background(), func(string) error { return nil }, attempt)
	l.RunOnce(context.Background(), func(string) error { return nil }, attempt)
	if calls != 1 {
		t.Fatalf("uninstall attempted %d times inside the gap, want once", calls)
	}

	now = now.Add(uninstallRetryGap + time.Second)
	l.RunOnce(context.Background(), func(string) error { return nil }, attempt)
	if calls != 2 {
		t.Fatalf("uninstall not retried after the gap: %d attempts", calls)
	}
}

// The uninstaller stops the agent's own service, so it cannot be a child of
// the agent: whatever launches it has to survive the agent's death. On systemd
// that means a transient unit.
func TestUninstallCommandUsesATransientUnitUnderSystemd(t *testing.T) {
	name, args := uninstallCommand("/usr/bin/systemd-run", "/usr/local/sbin/feast-watch-agent-uninstall")

	if name != "/usr/bin/systemd-run" {
		t.Fatalf("command = %q, want systemd-run", name)
	}
	joined := strings.Join(args, " ")
	for _, want := range []string{"/usr/local/sbin/feast-watch-agent-uninstall", "--purge", "--report"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("args %v missing %q", args, want)
		}
	}
}

// Nothing secret may appear in the command line: /proc is world-readable, so
// an argument is visible to every user on the host for as long as it runs. The
// uninstaller reads the mother URL and token out of agent.conf itself, which
// is root-owned and 0600 — it is about to delete that file anyway.
func TestUninstallCommandCarriesNoSecrets(t *testing.T) {
	for _, systemdRun := range []string{"/usr/bin/systemd-run", ""} {
		name, args := uninstallCommand(systemdRun, "/usr/local/sbin/feast-watch-agent-uninstall")
		joined := name + " " + strings.Join(args, " ")
		for _, secret := range []string{"tk_", "Bearer", "TOKEN="} {
			if strings.Contains(joined, secret) {
				t.Fatalf("command line carries %q: %s", secret, joined)
			}
		}
	}
}

// Without systemd there is no transient unit to hide behind; the uninstaller
// is launched directly and detached instead.
func TestUninstallCommandFallsBackWithoutSystemdRun(t *testing.T) {
	name, args := uninstallCommand("", "/usr/local/sbin/feast-watch-agent-uninstall")

	if name != "/usr/local/sbin/feast-watch-agent-uninstall" {
		t.Fatalf("command = %q", name)
	}
	if strings.Join(args, " ") != "--purge --report" {
		t.Fatalf("args = %v", args)
	}
}
