package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"runtime"
	"time"

	"github.com/osman-yahya/feast-watch/agent/collectors"
	"github.com/osman-yahya/feast-watch/shared/protocol"
	"github.com/osman-yahya/feast-watch/shared/version"
)

const defaultInterval = 10 // seconds; the mother overrides this per response

// updateRetryGap throttles repeated self-update attempts at the same target.
// A target the agent cannot install — never staged, wrong platform, checksum
// mismatch — otherwise gets retried on every push, which at the default 10s
// interval means downloading a whole binary every 10 seconds indefinitely.
const updateRetryGap = 5 * time.Minute

// uninstallRetryGap throttles repeated removal attempts, for the same reason
// updateRetryGap throttles updates: a host where the uninstaller is missing or
// cannot start would otherwise respawn it on every push, forever, at whatever
// interval the mother set. The mother keeps asking until the removal is
// confirmed, so the retry is guaranteed — this only decides how often.
const uninstallRetryGap = 5 * time.Minute

type Loop struct {
	cfg      Config
	reg      *collectors.Registry
	client   *http.Client
	enabled  []string
	interval int
	pushed   bool

	// Self-update state, reported to the mother on the next push so a failed
	// rollout is visible in the panel instead of only in this host's journal.
	updateErr    string
	updateTarget string
	lastUpdateAt time.Time

	// Removal state, reported the same way: an agent that cannot remove
	// itself has to say so, or its row sits in "kaldırılıyor" with no reason
	// attached.
	uninstallErr    string
	lastUninstallAt time.Time

	now func() time.Time
}

func NewLoop(cfg Config, reg *collectors.Registry) *Loop {
	return NewLoopWithClient(cfg, reg, &http.Client{Timeout: 5 * time.Second})
}

// NewLoopWithClient lets the caller supply the client (built via
// Config.HTTPClient), so the push loop and the self-update share one timeout
// policy instead of each inventing its own.
func NewLoopWithClient(cfg Config, reg *collectors.Registry, client *http.Client) *Loop {
	return &Loop{
		cfg: cfg, reg: reg,
		client:   client,
		enabled:  []string{"cpu", "memory", "uptime", "disk"},
		interval: defaultInterval,
		now:      time.Now,
	}
}

func (l *Loop) Interval() int { return l.interval }

func (l *Loop) PushOnce(ctx context.Context) (*protocol.IngestResponse, error) {
	req := protocol.IngestRequest{
		Server:         l.cfg.ServerName,
		AgentVersion:   version.Version,
		UpdateError:    l.updateErr,
		UninstallError: l.uninstallErr,
		Samples:        l.reg.CollectEnabled(ctx, l.enabled),
	}
	if !l.pushed {
		req.Hostname, _ = os.Hostname()
		req.OS = runtime.GOOS
		req.Arch = runtime.GOARCH
		req.IP = localIP()
		// Capabilities only change when agent.conf changes, which requires a
		// restart — and a restart replays the first push.
		req.Capabilities = l.reg.Names()
	}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, l.cfg.MotherURL+"/v1/ingest", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Authorization", "Bearer "+l.cfg.Token)
	httpReq.Header.Set("Content-Type", "application/json")

	httpResp, err := l.client.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer httpResp.Body.Close()
	if httpResp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ingest returned %d", httpResp.StatusCode)
	}

	var resp protocol.IngestResponse
	if err := json.NewDecoder(httpResp.Body).Decode(&resp); err != nil {
		return nil, err
	}

	l.pushed = true
	// Empty collectors response means "keep current" by design; mother omits the field
	// when it has no changes to push, to reduce payload size.
	if len(resp.Collectors) > 0 {
		l.enabled = resp.Collectors
	}
	if resp.Interval > 0 {
		l.interval = resp.Interval
	}
	return &resp, nil
}

// Run pushes forever at the mother-controlled interval. A failed push is
// retried on the next tick — the agent never crashes on network errors.
// update performs the self-update and, on success, does not return: it
// replaces this binary and exits for the service manager to restart.
// uninstall removes the agent from this host; it may be nil where removal is
// not something this deployment can do (a k8s DaemonSet, say).
func (l *Loop) Run(ctx context.Context, update func(string) error, uninstall func() error) {
	for {
		l.RunOnce(ctx, update, uninstall)
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Duration(l.interval) * time.Second):
		}
	}
}

// RunOnce performs exactly one push and acts on the answer. Split out of Run
// so the decision table — what a response means and what it triggers — is
// testable without a ticker.
func (l *Loop) RunOnce(ctx context.Context, update func(string) error, uninstall func() error) {
	resp, err := l.PushOnce(ctx)
	if err != nil {
		// A push that never landed carries no news about the rollout, so
		// the update state is left exactly as it was and reported on the
		// first push that gets through.
		slog.Error("push failed", "err", err)
		return
	}
	// Removal outranks the rollout: a host on its way out has no use for a new
	// binary, and downloading one would be work done on a machine that is
	// being decommissioned.
	if resp.Uninstall {
		l.tryUninstall(uninstall)
		return
	}
	if resp.DesiredVersion == "" || resp.DesiredVersion == version.Version {
		// Nothing outstanding: the target was withdrawn, corrected to the
		// version already running, or reached. Whatever failed before is
		// history now, and history reported as the current state is a
		// failure the operator cannot clear from the panel — the next push
		// would only write it back.
		l.forgetUpdate()
		return
	}
	l.tryUpdate(resp.DesiredVersion, update)
}

// tryUninstall starts the removal, at most once per uninstallRetryGap.
//
// A failure is kept in uninstallErr and reported on the next push rather than
// being retried immediately: the mother repeats the command on every push
// while the request stands, so the retry is already guaranteed and the only
// question is how often a host that cannot remove itself tries again.
func (l *Loop) tryUninstall(uninstall func() error) {
	if uninstall == nil {
		l.uninstallErr = "this agent cannot remove itself (no uninstaller wired in)"
		return
	}
	now := l.now()
	if !l.lastUninstallAt.IsZero() && now.Sub(l.lastUninstallAt) < uninstallRetryGap {
		// The last attempt failed and its reason is still the current state,
		// so uninstallErr is deliberately left standing — see tryUpdate.
		return
	}
	l.lastUninstallAt = now

	slog.Info("uninstall requested by the mother")
	if err := uninstall(); err != nil {
		slog.Error("uninstall failed", "err", err)
		l.uninstallErr = err.Error()
		return
	}
	// Started. Nothing here observes the outcome — the uninstaller reports it
	// to the mother itself once the host is clean (see agent/uninstall.go).
	l.uninstallErr = ""
}

// tryUpdate attempts the self-update, at most once per updateRetryGap for a
// given target. The gap resets when the mother names a different version, so
// an operator correcting a bad target is acted on at the next push rather
// than after the backoff expires.
func (l *Loop) tryUpdate(desired string, update func(string) error) {
	now := l.now()
	if desired == l.updateTarget && now.Sub(l.lastUpdateAt) < updateRetryGap {
		// updateErr is deliberately left standing. The mother is still asking
		// for this target and the last attempt at it failed, so the failure is
		// the current state, not a past one — the gap only suppresses the
		// retry, it does not resolve anything. Clearing here would blank the
		// warning on every push in between and leave a stuck rollout looking
		// like an ordinary pending one, which is the whole condition the
		// operator asked to be told about.
		return
	}
	l.updateTarget, l.lastUpdateAt = desired, now

	slog.Info("self-update requested", "desired", desired)
	if err := update(desired); err != nil {
		slog.Error("self-update failed", "desired", desired, "err", err)
		l.updateErr = err.Error()
		return
	}
	l.updateErr = ""
}

// forgetUpdate drops the state of an update nobody is asking for any more.
// The retry gap goes with it: re-targeting a version after withdrawing it is a
// deliberate operator action, not the every-push retry the gap exists to stop,
// and it should be acted on at the next push like any other new target.
func (l *Loop) forgetUpdate() {
	l.updateErr, l.updateTarget, l.lastUpdateAt = "", "", time.Time{}
}

func localIP() string {
	conn, err := net.Dial("udp", "8.8.8.8:80")
	if err != nil {
		return ""
	}
	defer conn.Close()
	return conn.LocalAddr().(*net.UDPAddr).IP.String()
}
