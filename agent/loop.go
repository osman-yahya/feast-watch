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
	now          func() time.Time
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
		Server:       l.cfg.ServerName,
		AgentVersion: version.Version,
		UpdateError:  l.updateErr,
		Samples:      l.reg.CollectEnabled(ctx, l.enabled),
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
func (l *Loop) Run(ctx context.Context, update func(string) error) {
	for {
		resp, err := l.PushOnce(ctx)
		if err != nil {
			slog.Error("push failed", "err", err)
		} else if resp.DesiredVersion != "" && resp.DesiredVersion != version.Version {
			l.tryUpdate(resp.DesiredVersion, update)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Duration(l.interval) * time.Second):
		}
	}
}

// tryUpdate attempts the self-update, at most once per updateRetryGap for a
// given target. The gap resets when the mother names a different version, so
// an operator correcting a bad target is acted on at the next push rather
// than after the backoff expires.
func (l *Loop) tryUpdate(desired string, update func(string) error) {
	now := l.now()
	if desired == l.updateTarget && now.Sub(l.lastUpdateAt) < updateRetryGap {
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

func localIP() string {
	conn, err := net.Dial("udp", "8.8.8.8:80")
	if err != nil {
		return ""
	}
	defer conn.Close()
	return conn.LocalAddr().(*net.UDPAddr).IP.String()
}
