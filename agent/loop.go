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

type Loop struct {
	cfg      Config
	reg      *collectors.Registry
	client   *http.Client
	enabled  []string
	interval int
	pushed   bool
}

func NewLoop(cfg Config, reg *collectors.Registry) *Loop {
	return NewLoopWithClient(cfg, reg, &http.Client{Timeout: 5 * time.Second})
}

// NewLoopWithClient lets the caller supply a client with custom TLS trust
// (built via Config.HTTPClient) — required when the mother uses an internal CA.
func NewLoopWithClient(cfg Config, reg *collectors.Registry, client *http.Client) *Loop {
	return &Loop{
		cfg: cfg, reg: reg,
		client:   client,
		enabled:  []string{"cpu", "memory", "uptime", "disk"},
		interval: defaultInterval,
	}
}

func (l *Loop) Interval() int { return l.interval }

func (l *Loop) PushOnce(ctx context.Context) (*protocol.IngestResponse, error) {
	req := protocol.IngestRequest{
		Server:       l.cfg.ServerName,
		AgentVersion: version.Version,
		Samples:      l.reg.CollectEnabled(ctx, l.enabled),
	}
	if !l.pushed {
		req.Hostname, _ = os.Hostname()
		req.OS = runtime.GOOS
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
func (l *Loop) Run(ctx context.Context, onDesiredVersion func(string)) {
	for {
		resp, err := l.PushOnce(ctx)
		if err != nil {
			slog.Error("push failed", "err", err)
		} else if resp.DesiredVersion != "" && resp.DesiredVersion != version.Version {
			onDesiredVersion(resp.DesiredVersion)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Duration(l.interval) * time.Second):
		}
	}
}

func localIP() string {
	conn, err := net.Dial("udp", "8.8.8.8:80")
	if err != nil {
		return ""
	}
	defer conn.Close()
	return conn.LocalAddr().(*net.UDPAddr).IP.String()
}
