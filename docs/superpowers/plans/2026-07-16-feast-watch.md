# feast-watch Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build feast-watch: a push-based server-monitoring system — a Go agent on every server pushing metrics every 10s to a central Go+SQLite mother that rolls up, retains, and serves summarized chart data to the feast backend.

**Architecture:** Agents push `POST /v1/ingest`; the mother's response carries each agent's config (enabled collectors, interval, desired version) — no inbound ports on agents, no separate control channel. Mother stores raw 10s samples, continuously rolls them into per-server 1-minute and 1-hour tables, and serves only bounded rollup summaries through an API-key-gated `/api/*` surface.

**Tech Stack:** Go 1.22+ (agent + mother, single module), `modernc.org/sqlite` (pure-Go SQLite, keeps mother a static binary), `github.com/shirou/gopsutil/v4` (host metrics), `github.com/jackc/pgx/v5/stdlib` (postgres collector only), stdlib `net/http` (no web framework).

**Spec:** `docs/superpowers/specs/2026-07-16-feast-watch-design.md` — the plan implements it verbatim.

**Out of scope for this repo/plan:** the feast backend proxy and admin-panel screens (they live in `feast-mobile-backend` / `feast-mobile-backend-control`; this plan delivers the `/api/*` surface they will consume).

## Global Constraints

- Module path: `github.com/osman-yahya/feast-watch`. Go ≥ 1.22. **No cgo anywhere** (`CGO_ENABLED=0`).
- Agent resource budget (acceptance criteria): < 1% CPU, < 30 MB RSS.
- Metric keys are flat strings: `cpu.usage`, `mem.used_pct`, `mem.swap_used_pct`, `uptime_s`, `disk.used_pct`, `centrifugo.conns`, `centrifugo.conns_max`, `dragonfly.mem_used`, `dragonfly.mem_max`, `dragonfly.clients`, `postgres.conns`, `postgres.conns_max`, `k8s.nodes_ready`, `k8s.nodes_total`, `k8s.pods_running`, `k8s.pods_failed`, `k8s.restarts`.
- Default settings (all editable via admin API): `interval=10` (s), `heartbeat_miss_threshold=3`, `retention_raw_hours=48`, `retention_1m_days=15`, `retention_1h_days=75`, `desired_version=""` (empty = no forced update).
- Default collector set for a new server: `["cpu","memory","uptime","disk"]`.
- A collector not listed in the mother's response must never run — not even collect-and-discard.
- The chart endpoint must never query the raw `samples` table.
- No hardcoded secrets; agent config from `/etc/feast-watch/agent.conf` (KEY=VALUE), mother config from env.
- Commit messages in English, conventional-commit format, no attribution footer.
- All timestamps stored as Unix seconds (INTEGER).

## File Structure

```
go.mod
shared/protocol/protocol.go        # IngestRequest/IngestResponse — the wire contract
shared/version/version.go          # Version var (ldflags-injected)
agent/collectors/collector.go      # Collector interface + Sample + Registry
agent/collectors/cpu.go            # cpu
agent/collectors/memory.go         # memory (RAM+swap together)
agent/collectors/host.go           # uptime + disk
agent/collectors/centrifugo.go     # centrifugo conns vs max
agent/collectors/dragonfly.go      # dragonfly memory + clients (raw RESP over TCP)
agent/collectors/postgres.go       # pg conns vs max_connections
agent/collectors/k8s.go            # node/pod health via API server
agent/config.go                    # agent.conf loading + validation
agent/loop.go                      # push loop: gather → POST → apply response
agent/update.go                    # self-update (download, sha256, replace, exit)
agent/main.go                      # wiring
mother/store/store.go              # Open + migrations
mother/store/servers.go            # server CRUD, token gen, status
mother/store/samples.go            # sample insert + rollup + retention + delete
mother/store/settings.go           # typed settings access
mother/api/middleware.go           # bearer-token (ingest) + API-key (admin) auth
mother/api/ingest.go               # POST /v1/ingest
mother/api/admin.go                # /api/servers, /api/settings, /api/history
mother/api/chart.go                # /api/chart — tier selection
mother/api/install.go              # /install/<token>.sh + /download/agent/*
mother/generate.go                 # `feast-watch generate --name=X` CLI
mother/main.go                     # wiring + background jobs
deploy/install.sh.tmpl             # install script template
deploy/feast-watch-agent.service   # systemd unit
deploy/k8s/daemonset.yaml          # k8s alternative
docker-compose.yml                 # local dev: mother + 2 agents
e2e/e2e_test.sh                    # compose-driven end-to-end check
```

---

### Task 1: Module scaffold + wire protocol

**Files:**
- Create: `go.mod`, `shared/protocol/protocol.go`
- Test: `shared/protocol/protocol_test.go`

**Interfaces:**
- Consumes: nothing (first task).
- Produces: `protocol.IngestRequest{Server, AgentVersion, Hostname, IP, OS string; Samples map[string]float64}`, `protocol.IngestResponse{Collectors []string; Interval int; DesiredVersion string}`. Every later task imports these.

- [ ] **Step 1: Initialize module**

```bash
cd /Users/ceydaakin/GitHub/feast-watch
go mod init github.com/osman-yahya/feast-watch
```

- [ ] **Step 2: Write the failing test**

`shared/protocol/protocol_test.go`:

```go
package protocol

import (
	"encoding/json"
	"testing"
)

func TestIngestRequestRoundTrip(t *testing.T) {
	in := IngestRequest{
		Server: "centrifugo-1", AgentVersion: "1.2.0",
		Hostname: "cf1", IP: "10.0.0.5", OS: "linux",
		Samples: map[string]float64{"cpu.usage": 34.2, "centrifugo.conns": 4812},
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out IngestRequest
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if out.Server != in.Server || out.Samples["cpu.usage"] != 34.2 {
		t.Fatalf("round trip mismatch: %+v", out)
	}
}

func TestIngestResponseJSONFields(t *testing.T) {
	b, _ := json.Marshal(IngestResponse{Collectors: []string{"cpu"}, Interval: 10, DesiredVersion: "1.3.0"})
	want := `{"collectors":["cpu"],"interval":10,"desired_version":"1.3.0"}`
	if string(b) != want {
		t.Fatalf("got %s want %s", b, want)
	}
}
```

- [ ] **Step 3: Run test to verify it fails**

Run: `go test ./shared/... -v`
Expected: FAIL — `undefined: IngestRequest`

- [ ] **Step 4: Write minimal implementation**

`shared/protocol/protocol.go`:

```go
// Package protocol defines the wire contract between agent and mother.
package protocol

type IngestRequest struct {
	Server       string             `json:"server"`
	AgentVersion string             `json:"agent_version"`
	Hostname     string             `json:"hostname,omitempty"`
	IP           string             `json:"ip,omitempty"`
	OS           string             `json:"os,omitempty"`
	Samples      map[string]float64 `json:"samples"`
}

type IngestResponse struct {
	Collectors     []string `json:"collectors"`
	Interval       int      `json:"interval"`
	DesiredVersion string   `json:"desired_version"`
}
```

- [ ] **Step 5: Run test to verify it passes**

Run: `go test ./shared/... -v`
Expected: PASS (both tests)

- [ ] **Step 6: Commit**

```bash
git add go.mod shared/
git commit -m "feat: scaffold module and agent-mother wire protocol"
```

---

### Task 2: Collector interface, registry, and version package

**Files:**
- Create: `shared/version/version.go`, `agent/collectors/collector.go`
- Test: `agent/collectors/collector_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `collectors.Sample{Key string; Value float64}`, `collectors.Collector interface { Name() string; Collect(ctx context.Context) ([]Sample, error) }`, `collectors.Registry` with `Register(c Collector)`, `CollectEnabled(ctx context.Context, enabled []string) map[string]float64`. `version.Version` (string var, default `"dev"`).

- [ ] **Step 1: Write the failing test**

`agent/collectors/collector_test.go`:

```go
package collectors

import (
	"context"
	"errors"
	"testing"
)

type fake struct {
	name    string
	samples []Sample
	err     error
	called  *bool
}

func (f *fake) Name() string { return f.name }
func (f *fake) Collect(ctx context.Context) ([]Sample, error) {
	if f.called != nil {
		*f.called = true
	}
	return f.samples, f.err
}

func TestRegistryCollectsOnlyEnabled(t *testing.T) {
	cpuCalled, k8sCalled := false, false
	r := NewRegistry()
	r.Register(&fake{name: "cpu", samples: []Sample{{Key: "cpu.usage", Value: 12.5}}, called: &cpuCalled})
	r.Register(&fake{name: "k8s", samples: []Sample{{Key: "k8s.nodes_ready", Value: 3}}, called: &k8sCalled})

	got := r.CollectEnabled(context.Background(), []string{"cpu"})

	if got["cpu.usage"] != 12.5 {
		t.Fatalf("missing cpu sample: %v", got)
	}
	if _, ok := got["k8s.nodes_ready"]; ok {
		t.Fatal("disabled collector produced samples")
	}
	if k8sCalled {
		t.Fatal("disabled collector must never run")
	}
	if !cpuCalled {
		t.Fatal("enabled collector did not run")
	}
}

func TestRegistrySkipsFailingCollector(t *testing.T) {
	r := NewRegistry()
	r.Register(&fake{name: "cpu", err: errors.New("boom")})
	r.Register(&fake{name: "memory", samples: []Sample{{Key: "mem.used_pct", Value: 60}}})
	got := r.CollectEnabled(context.Background(), []string{"cpu", "memory"})
	if got["mem.used_pct"] != 60 {
		t.Fatal("one failing collector must not block others")
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 sample, got %v", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./agent/... -v`
Expected: FAIL — `undefined: NewRegistry`

- [ ] **Step 3: Write minimal implementation**

`agent/collectors/collector.go`:

```go
// Package collectors defines metric collectors. Only collectors named in the
// mother's ingest response are ever executed.
package collectors

import (
	"context"
	"log/slog"
)

type Sample struct {
	Key   string
	Value float64
}

type Collector interface {
	Name() string
	Collect(ctx context.Context) ([]Sample, error)
}

type Registry struct {
	byName map[string]Collector
}

func NewRegistry() *Registry {
	return &Registry{byName: map[string]Collector{}}
}

func (r *Registry) Register(c Collector) {
	r.byName[c.Name()] = c
}

// CollectEnabled runs exactly the enabled collectors; a failing collector is
// logged and skipped so it can never block the push cycle.
func (r *Registry) CollectEnabled(ctx context.Context, enabled []string) map[string]float64 {
	out := map[string]float64{}
	for _, name := range enabled {
		c, ok := r.byName[name]
		if !ok {
			continue
		}
		samples, err := c.Collect(ctx)
		if err != nil {
			slog.Error("collector failed", "collector", name, "err", err)
			continue
		}
		for _, s := range samples {
			out[s.Key] = s.Value
		}
	}
	return out
}
```

`shared/version/version.go`:

```go
// Package version holds the build version, injected via
// -ldflags "-X github.com/osman-yahya/feast-watch/shared/version.Version=v1.2.0".
package version

var Version = "dev"
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./agent/... -v`
Expected: PASS (2 tests)

- [ ] **Step 5: Commit**

```bash
git add agent/ shared/version/
git commit -m "feat: add collector interface, registry with enabled-only execution, version package"
```

---

### Task 3: cpu and memory collectors

**Files:**
- Create: `agent/collectors/cpu.go`, `agent/collectors/memory.go`
- Test: `agent/collectors/cpu_test.go`, `agent/collectors/memory_test.go`

**Interfaces:**
- Consumes: `Sample`, `Collector` (Task 2).
- Produces: `NewCPU() *CPU` emitting `cpu.usage`; `NewMemory() *Memory` emitting `mem.used_pct` **and** `mem.swap_used_pct` together. Both structs expose injectable read funcs for tests.

- [ ] **Step 1: Add gopsutil**

```bash
go get github.com/shirou/gopsutil/v4@latest
```

- [ ] **Step 2: Write the failing tests**

`agent/collectors/cpu_test.go`:

```go
package collectors

import (
	"context"
	"testing"
	"time"
)

func TestCPUCollect(t *testing.T) {
	c := NewCPU()
	c.percent = func(interval time.Duration, percpu bool) ([]float64, error) {
		return []float64{34.2}, nil
	}
	got, err := c.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Key != "cpu.usage" || got[0].Value != 34.2 {
		t.Fatalf("got %+v", got)
	}
}
```

`agent/collectors/memory_test.go`:

```go
package collectors

import (
	"context"
	"testing"

	"github.com/shirou/gopsutil/v4/mem"
)

func TestMemoryCollectsRAMAndSwapTogether(t *testing.T) {
	m := NewMemory()
	m.virtual = func() (*mem.VirtualMemoryStat, error) {
		return &mem.VirtualMemoryStat{UsedPercent: 61.5}, nil
	}
	m.swap = func() (*mem.SwapMemoryStat, error) {
		return &mem.SwapMemoryStat{UsedPercent: 2.1}, nil
	}
	got, err := m.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	byKey := map[string]float64{}
	for _, s := range got {
		byKey[s.Key] = s.Value
	}
	if byKey["mem.used_pct"] != 61.5 || byKey["mem.swap_used_pct"] != 2.1 {
		t.Fatalf("RAM and swap must be reported together, got %v", byKey)
	}
}
```

- [ ] **Step 3: Run tests to verify they fail**

Run: `go test ./agent/collectors/ -run 'TestCPU|TestMemory' -v`
Expected: FAIL — `undefined: NewCPU`, `undefined: NewMemory`

- [ ] **Step 4: Write minimal implementations**

`agent/collectors/cpu.go`:

```go
package collectors

import (
	"context"
	"time"

	"github.com/shirou/gopsutil/v4/cpu"
)

type CPU struct {
	percent func(time.Duration, bool) ([]float64, error)
}

func NewCPU() *CPU { return &CPU{percent: cpu.Percent} }

func (c *CPU) Name() string { return "cpu" }

func (c *CPU) Collect(ctx context.Context) ([]Sample, error) {
	// interval 0 = non-blocking delta since previous call; never sleeps the loop.
	vals, err := c.percent(0, false)
	if err != nil || len(vals) == 0 {
		return nil, err
	}
	return []Sample{{Key: "cpu.usage", Value: vals[0]}}, nil
}
```

`agent/collectors/memory.go`:

```go
package collectors

import (
	"context"

	"github.com/shirou/gopsutil/v4/mem"
)

// Memory reports RAM and swap together — headroom is only visible as a pair.
type Memory struct {
	virtual func() (*mem.VirtualMemoryStat, error)
	swap    func() (*mem.SwapMemoryStat, error)
}

func NewMemory() *Memory {
	return &Memory{virtual: mem.VirtualMemory, swap: mem.SwapMemory}
}

func (m *Memory) Name() string { return "memory" }

func (m *Memory) Collect(ctx context.Context) ([]Sample, error) {
	v, err := m.virtual()
	if err != nil {
		return nil, err
	}
	s, err := m.swap()
	if err != nil {
		return nil, err
	}
	return []Sample{
		{Key: "mem.used_pct", Value: v.UsedPercent},
		{Key: "mem.swap_used_pct", Value: s.UsedPercent},
	}, nil
}
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./agent/collectors/ -v`
Expected: PASS (all)

- [ ] **Step 6: Commit**

```bash
git add agent/collectors/ go.mod go.sum
git commit -m "feat: add cpu and combined memory+swap collectors"
```

---

### Task 4: uptime and disk collectors

**Files:**
- Create: `agent/collectors/host.go`
- Test: `agent/collectors/host_test.go`

**Interfaces:**
- Consumes: `Sample`, `Collector` (Task 2).
- Produces: `NewUptime() *Uptime` emitting `uptime_s`; `NewDisk() *Disk` emitting `disk.used_pct` (space only, never I/O rates).

- [ ] **Step 1: Write the failing test**

`agent/collectors/host_test.go`:

```go
package collectors

import (
	"context"
	"testing"

	"github.com/shirou/gopsutil/v4/disk"
)

func TestUptimeCollect(t *testing.T) {
	u := NewUptime()
	u.uptime = func() (uint64, error) { return 864211, nil }
	got, err := u.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Key != "uptime_s" || got[0].Value != 864211 {
		t.Fatalf("got %+v", got)
	}
}

func TestDiskCollectsUsagePercentOnly(t *testing.T) {
	d := NewDisk()
	d.usage = func(path string) (*disk.UsageStat, error) {
		return &disk.UsageStat{UsedPercent: 71.0}, nil
	}
	got, err := d.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Key != "disk.used_pct" || got[0].Value != 71.0 {
		t.Fatalf("disk must report space %% only, got %+v", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./agent/collectors/ -run 'TestUptime|TestDisk' -v`
Expected: FAIL — `undefined: NewUptime`, `undefined: NewDisk`

- [ ] **Step 3: Write minimal implementation**

`agent/collectors/host.go`:

```go
package collectors

import (
	"context"

	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/host"
)

type Uptime struct {
	uptime func() (uint64, error)
}

func NewUptime() *Uptime { return &Uptime{uptime: host.Uptime} }

func (u *Uptime) Name() string { return "uptime" }

func (u *Uptime) Collect(ctx context.Context) ([]Sample, error) {
	v, err := u.uptime()
	if err != nil {
		return nil, err
	}
	return []Sample{{Key: "uptime_s", Value: float64(v)}}, nil
}

// Disk reports root filesystem usage percent. Deliberately no I/O rates (spec).
type Disk struct {
	usage func(string) (*disk.UsageStat, error)
}

func NewDisk() *Disk { return &Disk{usage: disk.Usage} }

func (d *Disk) Name() string { return "disk" }

func (d *Disk) Collect(ctx context.Context) ([]Sample, error) {
	u, err := d.usage("/")
	if err != nil {
		return nil, err
	}
	return []Sample{{Key: "disk.used_pct", Value: u.UsedPercent}}, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./agent/collectors/ -v`
Expected: PASS (all)

- [ ] **Step 5: Commit**

```bash
git add agent/collectors/
git commit -m "feat: add uptime and disk-usage collectors"
```

---

### Task 5: agent config loading

**Files:**
- Create: `agent/config.go`
- Test: `agent/config_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `agent.Config{MotherURL, Token, ServerName string; CentrifugoAPIURL, CentrifugoAPIKey string; CentrifugoConnsMax float64; DragonflyAddr string; PostgresDSN string; K8sAPIURL, K8sToken string}` and `LoadConfig(path string) (Config, error)` — fails fast when `MOTHER_URL`, `TOKEN`, or `SERVER_NAME` is missing.

- [ ] **Step 1: Write the failing test**

`agent/config_test.go`:

```go
package agent

import (
	"os"
	"path/filepath"
	"testing"
)

func writeConf(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "agent.conf")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadConfigParsesKeyValues(t *testing.T) {
	p := writeConf(t, "MOTHER_URL=https://10.0.0.1:8443\nTOKEN=tk_abc\nSERVER_NAME=DB_Sunucusu\n# comment\nCENTRIFUGO_CONNS_MAX=10000\n")
	cfg, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MotherURL != "https://10.0.0.1:8443" || cfg.Token != "tk_abc" || cfg.ServerName != "DB_Sunucusu" {
		t.Fatalf("got %+v", cfg)
	}
	if cfg.CentrifugoConnsMax != 10000 {
		t.Fatalf("conns max: %v", cfg.CentrifugoConnsMax)
	}
}

func TestLoadConfigFailsFastOnMissingRequired(t *testing.T) {
	p := writeConf(t, "MOTHER_URL=https://10.0.0.1:8443\n")
	if _, err := LoadConfig(p); err == nil {
		t.Fatal("expected error for missing TOKEN and SERVER_NAME")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./agent/ -v`
Expected: FAIL — `undefined: LoadConfig`

- [ ] **Step 3: Write minimal implementation**

`agent/config.go`:

```go
// Package agent implements the feast-watch agent: config, push loop, self-update.
package agent

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
)

type Config struct {
	MotherURL  string
	Token      string
	ServerName string

	CentrifugoAPIURL   string
	CentrifugoAPIKey   string
	CentrifugoConnsMax float64
	DragonflyAddr      string
	PostgresDSN        string
	K8sAPIURL          string
	K8sToken           string
}

// LoadConfig reads KEY=VALUE lines ('#' comments allowed) and validates
// required keys at startup — fail fast, never run half-configured.
func LoadConfig(path string) (Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return Config{}, fmt.Errorf("open config: %w", err)
	}
	defer f.Close()

	kv := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			return Config{}, fmt.Errorf("malformed line %q", line)
		}
		kv[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	if err := sc.Err(); err != nil {
		return Config{}, err
	}

	cfg := Config{
		MotherURL:        kv["MOTHER_URL"],
		Token:            kv["TOKEN"],
		ServerName:       kv["SERVER_NAME"],
		CentrifugoAPIURL: kv["CENTRIFUGO_API_URL"],
		CentrifugoAPIKey: kv["CENTRIFUGO_API_KEY"],
		DragonflyAddr:    kv["DRAGONFLY_ADDR"],
		PostgresDSN:      kv["POSTGRES_DSN"],
		K8sAPIURL:        kv["K8S_API_URL"],
		K8sToken:         kv["K8S_TOKEN"],
	}
	if raw := kv["CENTRIFUGO_CONNS_MAX"]; raw != "" {
		cfg.CentrifugoConnsMax, err = strconv.ParseFloat(raw, 64)
		if err != nil {
			return Config{}, fmt.Errorf("CENTRIFUGO_CONNS_MAX: %w", err)
		}
	}

	var missing []string
	for k, v := range map[string]string{"MOTHER_URL": cfg.MotherURL, "TOKEN": cfg.Token, "SERVER_NAME": cfg.ServerName} {
		if v == "" {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		return Config{}, fmt.Errorf("missing required config: %s", strings.Join(missing, ", "))
	}
	return cfg, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./agent/ -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add agent/config.go agent/config_test.go
git commit -m "feat: add agent config loading with fail-fast validation"
```

---

### Task 6: agent push loop

**Files:**
- Create: `agent/loop.go`
- Test: `agent/loop_test.go`

**Interfaces:**
- Consumes: `Config` (Task 5), `collectors.Registry` (Task 2), `protocol` types (Task 1), `version.Version` (Task 2).
- Produces: `NewLoop(cfg Config, reg *collectors.Registry) *Loop`; `(*Loop).PushOnce(ctx context.Context) (*protocol.IngestResponse, error)` — gathers enabled samples, POSTs to `<MotherURL>/v1/ingest` with `Authorization: Bearer <token>`, applies the response (enabled set + interval); `(*Loop).Run(ctx context.Context, onDesiredVersion func(string))` — ticks at the mother-controlled interval. First push includes hostname/IP/OS.

- [ ] **Step 1: Write the failing test**

`agent/loop_test.go`:

```go
package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/osman-yahya/feast-watch/agent/collectors"
	"github.com/osman-yahya/feast-watch/shared/protocol"
)

type stub struct {
	name string
	key  string
	val  float64
}

func (s *stub) Name() string { return s.name }
func (s *stub) Collect(ctx context.Context) ([]collectors.Sample, error) {
	return []collectors.Sample{{Key: s.key, Value: s.val}}, nil
}

func TestPushOnceSendsSamplesAndAppliesResponse(t *testing.T) {
	var gotReq protocol.IngestRequest
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		if err := json.NewDecoder(r.Body).Decode(&gotReq); err != nil {
			t.Error(err)
		}
		json.NewEncoder(w).Encode(protocol.IngestResponse{
			Collectors: []string{"cpu", "memory"}, Interval: 10, DesiredVersion: "",
		})
	}))
	defer srv.Close()

	reg := collectors.NewRegistry()
	reg.Register(&stub{name: "cpu", key: "cpu.usage", val: 34.2})
	reg.Register(&stub{name: "k8s", key: "k8s.nodes_ready", val: 3})

	l := NewLoop(Config{MotherURL: srv.URL, Token: "tk_abc", ServerName: "s1"}, reg)
	resp, err := l.PushOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	if gotAuth != "Bearer tk_abc" {
		t.Fatalf("auth header: %q", gotAuth)
	}
	if gotReq.Server != "s1" || gotReq.Samples["cpu.usage"] != 34.2 {
		t.Fatalf("request: %+v", gotReq)
	}
	if _, ok := gotReq.Samples["k8s.nodes_ready"]; ok {
		t.Fatal("collector outside enabled set must not run")
	}
	if gotReq.Hostname == "" || gotReq.OS == "" {
		t.Fatal("first push must carry hostname and OS")
	}
	if len(resp.Collectors) != 2 || l.Interval() != 10 {
		t.Fatalf("response not applied: %+v interval=%d", resp, l.Interval())
	}
}

func TestSecondPushOmitsIdentity(t *testing.T) {
	calls := 0
	var second protocol.IngestRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 2 {
			json.NewDecoder(r.Body).Decode(&second)
		}
		json.NewEncoder(w).Encode(protocol.IngestResponse{Collectors: []string{"cpu"}, Interval: 10})
	}))
	defer srv.Close()

	l := NewLoop(Config{MotherURL: srv.URL, Token: "t", ServerName: "s1"}, collectors.NewRegistry())
	l.PushOnce(context.Background())
	l.PushOnce(context.Background())
	if second.Hostname != "" {
		t.Fatal("identity fields belong to the first push only")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./agent/ -run TestPush -v`
Expected: FAIL — `undefined: NewLoop`

- [ ] **Step 3: Write minimal implementation**

`agent/loop.go`:

```go
package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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
	return &Loop{
		cfg: cfg, reg: reg,
		client:   &http.Client{Timeout: 5 * time.Second},
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
		if err == nil && resp.DesiredVersion != "" && resp.DesiredVersion != version.Version {
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
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./agent/ -v`
Expected: PASS (all)

- [ ] **Step 5: Commit**

```bash
git add agent/loop.go agent/loop_test.go
git commit -m "feat: add agent push loop with response-driven config"
```

---

### Task 7: mother store — open, migrate, servers

**Files:**
- Create: `mother/store/store.go`, `mother/store/servers.go`
- Test: `mother/store/servers_test.go`

**Interfaces:**
- Consumes: nothing (fresh package).
- Produces: `store.Open(path string) (*Store, error)` (runs migrations; `Open(":memory:")` works in tests); `Server{ID int64; Name, Token string; Collectors []string; Hostname, IP, OS, AgentVersion string; LastPush int64; CreatedAt int64}`; `(*Store).AddServer(name string) (Server, error)` (generates `tk_`+32 hex chars token, default collectors); `(*Store).ServerByToken(token string) (Server, error)` (`store.ErrNotFound` when missing); `(*Store).ListServers() ([]Server, error)`; `(*Store).TouchServer(id int64, agentVersion, hostname, ip, os string, now int64) error`; `(*Store).SetCollectors(id int64, collectors []string) error`; `(*Store).DeleteServer(id int64) error`.

- [ ] **Step 1: Add the sqlite driver**

```bash
go get modernc.org/sqlite@latest
```

- [ ] **Step 2: Write the failing test**

`mother/store/servers_test.go`:

```go
package store

import (
	"errors"
	"strings"
	"testing"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestAddServerGeneratesTokenAndDefaults(t *testing.T) {
	s := open(t)
	srv, err := s.AddServer("DB_Sunucusu")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(srv.Token, "tk_") || len(srv.Token) != 35 {
		t.Fatalf("token format: %q", srv.Token)
	}
	if len(srv.Collectors) != 4 || srv.Collectors[0] != "cpu" {
		t.Fatalf("default collectors: %v", srv.Collectors)
	}
	if _, err := s.AddServer("DB_Sunucusu"); err == nil {
		t.Fatal("duplicate name must fail")
	}
}

func TestServerByTokenAndTouch(t *testing.T) {
	s := open(t)
	created, _ := s.AddServer("web-1")

	got, err := s.ServerByToken(created.Token)
	if err != nil || got.Name != "web-1" {
		t.Fatalf("lookup: %v %+v", err, got)
	}

	if err := s.TouchServer(got.ID, "1.2.0", "web1-host", "10.0.0.7", "linux", 1700000000); err != nil {
		t.Fatal(err)
	}
	list, _ := s.ListServers()
	if list[0].AgentVersion != "1.2.0" || list[0].LastPush != 1700000000 || list[0].IP != "10.0.0.7" {
		t.Fatalf("touch not persisted: %+v", list[0])
	}

	if _, err := s.ServerByToken("tk_nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestDeleteServerRevokesToken(t *testing.T) {
	s := open(t)
	created, _ := s.AddServer("gone")
	if err := s.DeleteServer(created.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ServerByToken(created.Token); !errors.Is(err, ErrNotFound) {
		t.Fatal("deleted server's token must be rejected")
	}
}
```

- [ ] **Step 3: Run test to verify it fails**

Run: `go test ./mother/... -v`
Expected: FAIL — `undefined: Open`

- [ ] **Step 4: Write minimal implementation**

`mother/store/store.go`:

```go
// Package store is the mother's SQLite persistence layer.
package store

import (
	"database/sql"
	"errors"

	_ "modernc.org/sqlite"
)

var ErrNotFound = errors.New("not found")

type Store struct {
	db *sql.DB
}

const schema = `
CREATE TABLE IF NOT EXISTS servers (
  id            INTEGER PRIMARY KEY,
  name          TEXT UNIQUE NOT NULL,
  token         TEXT UNIQUE NOT NULL,
  collectors    TEXT NOT NULL,
  hostname      TEXT NOT NULL DEFAULT '',
  ip            TEXT NOT NULL DEFAULT '',
  os            TEXT NOT NULL DEFAULT '',
  agent_version TEXT NOT NULL DEFAULT '',
  last_push     INTEGER NOT NULL DEFAULT 0,
  created_at    INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS samples (
  server_id INTEGER NOT NULL,
  metric    TEXT NOT NULL,
  ts        INTEGER NOT NULL,
  value     REAL NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_samples ON samples(server_id, metric, ts);
CREATE TABLE IF NOT EXISTS rollup_1m (
  server_id    INTEGER NOT NULL,
  metric       TEXT NOT NULL,
  window_start INTEGER NOT NULL,
  min REAL NOT NULL, max REAL NOT NULL, avg REAL NOT NULL, cnt INTEGER NOT NULL,
  PRIMARY KEY (server_id, metric, window_start)
);
CREATE TABLE IF NOT EXISTS rollup_1h (
  server_id    INTEGER NOT NULL,
  metric       TEXT NOT NULL,
  window_start INTEGER NOT NULL,
  min REAL NOT NULL, max REAL NOT NULL, avg REAL NOT NULL, cnt INTEGER NOT NULL,
  PRIMARY KEY (server_id, metric, window_start)
);
CREATE TABLE IF NOT EXISTS settings (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
`

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	// modernc/sqlite: single writer; serialize access instead of returning SQLITE_BUSY.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }
```

`mother/store/servers.go`:

```go
package store

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"
)

var DefaultCollectors = []string{"cpu", "memory", "uptime", "disk"}

type Server struct {
	ID           int64
	Name         string
	Token        string
	Collectors   []string
	Hostname     string
	IP           string
	OS           string
	AgentVersion string
	LastPush     int64
	CreatedAt    int64
}

func newToken() string {
	b := make([]byte, 16)
	rand.Read(b)
	return "tk_" + hex.EncodeToString(b)
}

func (s *Store) AddServer(name string) (Server, error) {
	srv := Server{
		Name: name, Token: newToken(),
		Collectors: DefaultCollectors, CreatedAt: time.Now().Unix(),
	}
	cols, _ := json.Marshal(srv.Collectors)
	res, err := s.db.Exec(
		`INSERT INTO servers (name, token, collectors, created_at) VALUES (?,?,?,?)`,
		srv.Name, srv.Token, string(cols), srv.CreatedAt)
	if err != nil {
		return Server{}, err
	}
	srv.ID, _ = res.LastInsertId()
	return srv, nil
}

const serverCols = `id, name, token, collectors, hostname, ip, os, agent_version, last_push, created_at`

func scanServer(row interface{ Scan(...any) error }) (Server, error) {
	var srv Server
	var cols string
	err := row.Scan(&srv.ID, &srv.Name, &srv.Token, &cols, &srv.Hostname, &srv.IP,
		&srv.OS, &srv.AgentVersion, &srv.LastPush, &srv.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Server{}, ErrNotFound
	}
	if err != nil {
		return Server{}, err
	}
	json.Unmarshal([]byte(cols), &srv.Collectors)
	return srv, nil
}

func (s *Store) ServerByToken(token string) (Server, error) {
	return scanServer(s.db.QueryRow(`SELECT `+serverCols+` FROM servers WHERE token = ?`, token))
}

func (s *Store) ServerByName(name string) (Server, error) {
	return scanServer(s.db.QueryRow(`SELECT `+serverCols+` FROM servers WHERE name = ?`, name))
}

func (s *Store) ListServers() ([]Server, error) {
	rows, err := s.db.Query(`SELECT ` + serverCols + ` FROM servers ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Server
	for rows.Next() {
		srv, err := scanServer(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, srv)
	}
	return out, rows.Err()
}

func (s *Store) TouchServer(id int64, agentVersion, hostname, ip, osName string, now int64) error {
	_, err := s.db.Exec(`UPDATE servers SET agent_version = ?, last_push = ?,
		hostname = CASE WHEN ? != '' THEN ? ELSE hostname END,
		ip       = CASE WHEN ? != '' THEN ? ELSE ip END,
		os       = CASE WHEN ? != '' THEN ? ELSE os END
		WHERE id = ?`,
		agentVersion, now, hostname, hostname, ip, ip, osName, osName, id)
	return err
}

func (s *Store) SetCollectors(id int64, collectors []string) error {
	cols, _ := json.Marshal(collectors)
	_, err := s.db.Exec(`UPDATE servers SET collectors = ? WHERE id = ?`, string(cols), id)
	return err
}

func (s *Store) DeleteServer(id int64) error {
	_, err := s.db.Exec(`DELETE FROM servers WHERE id = ?`, id)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`DELETE FROM samples WHERE server_id = ?`, id)
	return err
}
```

- [ ] **Step 5: Run test to verify it passes**

Run: `go test ./mother/... -v`
Expected: PASS (3 tests)

- [ ] **Step 6: Commit**

```bash
git add mother/ go.mod go.sum
git commit -m "feat: add mother store with schema and server management"
```

---

### Task 8: settings with typed defaults

**Files:**
- Create: `mother/store/settings.go`
- Test: `mother/store/settings_test.go`

**Interfaces:**
- Consumes: `Store` (Task 7).
- Produces: `Settings{Interval, HeartbeatMissThreshold, RetentionRawHours, Retention1mDays, Retention1hDays int; DesiredVersion string}`; `(*Store).GetSettings() (Settings, error)` (returns spec defaults when unset); `(*Store).SaveSettings(Settings) error`.

- [ ] **Step 1: Write the failing test**

`mother/store/settings_test.go`:

```go
package store

import "testing"

func TestSettingsDefaults(t *testing.T) {
	s := open(t)
	got, err := s.GetSettings()
	if err != nil {
		t.Fatal(err)
	}
	want := Settings{Interval: 10, HeartbeatMissThreshold: 3, RetentionRawHours: 48,
		Retention1mDays: 15, Retention1hDays: 75, DesiredVersion: ""}
	if got != want {
		t.Fatalf("defaults: got %+v want %+v", got, want)
	}
}

func TestSettingsRoundTrip(t *testing.T) {
	s := open(t)
	in := Settings{Interval: 30, HeartbeatMissThreshold: 5, RetentionRawHours: 24,
		Retention1mDays: 7, Retention1hDays: 90, DesiredVersion: "v1.3.0"}
	if err := s.SaveSettings(in); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetSettings()
	if got != in {
		t.Fatalf("got %+v want %+v", got, in)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./mother/store/ -run TestSettings -v`
Expected: FAIL — `undefined: Settings`

- [ ] **Step 3: Write minimal implementation**

`mother/store/settings.go`:

```go
package store

import "strconv"

// Settings are the panel-configurable knobs (spec: "Configurable from the panel").
type Settings struct {
	Interval               int    `json:"interval"`
	HeartbeatMissThreshold int    `json:"heartbeat_miss_threshold"`
	RetentionRawHours      int    `json:"retention_raw_hours"`
	Retention1mDays        int    `json:"retention_1m_days"`
	Retention1hDays        int    `json:"retention_1h_days"`
	DesiredVersion         string `json:"desired_version"`
}

var defaultSettings = Settings{
	Interval: 10, HeartbeatMissThreshold: 3,
	RetentionRawHours: 48, Retention1mDays: 15, Retention1hDays: 75,
}

func (s *Store) GetSettings() (Settings, error) {
	rows, err := s.db.Query(`SELECT key, value FROM settings`)
	if err != nil {
		return Settings{}, err
	}
	defer rows.Close()

	out := defaultSettings
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return Settings{}, err
		}
		n, _ := strconv.Atoi(v)
		switch k {
		case "interval":
			out.Interval = n
		case "heartbeat_miss_threshold":
			out.HeartbeatMissThreshold = n
		case "retention_raw_hours":
			out.RetentionRawHours = n
		case "retention_1m_days":
			out.Retention1mDays = n
		case "retention_1h_days":
			out.Retention1hDays = n
		case "desired_version":
			out.DesiredVersion = v
		}
	}
	return out, rows.Err()
}

func (s *Store) SaveSettings(in Settings) error {
	pairs := map[string]string{
		"interval":                 strconv.Itoa(in.Interval),
		"heartbeat_miss_threshold": strconv.Itoa(in.HeartbeatMissThreshold),
		"retention_raw_hours":      strconv.Itoa(in.RetentionRawHours),
		"retention_1m_days":        strconv.Itoa(in.Retention1mDays),
		"retention_1h_days":        strconv.Itoa(in.Retention1hDays),
		"desired_version":          in.DesiredVersion,
	}
	for k, v := range pairs {
		if _, err := s.db.Exec(
			`INSERT INTO settings (key, value) VALUES (?,?)
			 ON CONFLICT(key) DO UPDATE SET value = excluded.value`, k, v); err != nil {
			return err
		}
	}
	return nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./mother/store/ -v`
Expected: PASS (all)

- [ ] **Step 5: Commit**

```bash
git add mother/store/settings.go mother/store/settings_test.go
git commit -m "feat: add panel-configurable settings with spec defaults"
```

---

### Task 9: samples — insert, rollup, retention, deletion

**Files:**
- Create: `mother/store/samples.go`
- Test: `mother/store/samples_test.go`

**Interfaces:**
- Consumes: `Store`, `Settings` (Tasks 7–8).
- Produces: `(*Store).InsertSamples(serverID int64, ts int64, samples map[string]float64) error`; `(*Store).RollupSince(since int64) error` (raw→1m and 1m→1h, idempotent, **per server+metric**, count-weighted averages); `(*Store).EnforceRetention(now int64, cfg Settings) error`; `(*Store).DeleteHistory(serverID int64, from, to int64) error` (`serverID==0` = all servers; deletes from raw and both rollups).

- [ ] **Step 1: Write the failing test**

`mother/store/samples_test.go`:

```go
package store

import "testing"

func seed(t *testing.T, s *Store, serverID int64, metric string, base int64, vals []float64) {
	t.Helper()
	for i, v := range vals {
		if err := s.InsertSamples(serverID, base+int64(i*10), map[string]float64{metric: v}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRollupIsPerServerNotAverage(t *testing.T) {
	s := open(t)
	a, _ := s.AddServer("a")
	b, _ := s.AddServer("b")
	base := int64(1700000000) - 1700000000%60 // minute-aligned

	seed(t, s, a.ID, "cpu.usage", base, []float64{10, 20, 30}) // avg 20
	seed(t, s, b.ID, "cpu.usage", base, []float64{90, 90, 90}) // avg 90

	if err := s.RollupSince(base); err != nil {
		t.Fatal(err)
	}

	var min, max, avg float64
	var cnt int
	row := s.db.QueryRow(`SELECT min, max, avg, cnt FROM rollup_1m WHERE server_id=? AND metric='cpu.usage'`, a.ID)
	if err := row.Scan(&min, &max, &avg, &cnt); err != nil {
		t.Fatal(err)
	}
	if min != 10 || max != 30 || avg != 20 || cnt != 3 {
		t.Fatalf("server a rollup: min=%v max=%v avg=%v cnt=%d", min, max, avg, cnt)
	}
	row = s.db.QueryRow(`SELECT avg FROM rollup_1m WHERE server_id=? AND metric='cpu.usage'`, b.ID)
	row.Scan(&avg)
	if avg != 90 {
		t.Fatalf("server b must keep its own rollup, got avg=%v", avg)
	}
}

func TestRollup1hFrom1m(t *testing.T) {
	s := open(t)
	a, _ := s.AddServer("a")
	base := int64(1700000000) - 1700000000%3600 // hour-aligned
	seed(t, s, a.ID, "cpu.usage", base, []float64{10, 20})       // minute 1
	seed(t, s, a.ID, "cpu.usage", base+60, []float64{40, 50})    // minute 2
	if err := s.RollupSince(base); err != nil {
		t.Fatal(err)
	}
	var avg float64
	var cnt int
	err := s.db.QueryRow(`SELECT avg, cnt FROM rollup_1h WHERE server_id=?`, a.ID).Scan(&avg, &cnt)
	if err != nil {
		t.Fatal(err)
	}
	if cnt != 4 || avg != 30 { // count-weighted: (10+20+40+50)/4
		t.Fatalf("1h rollup: avg=%v cnt=%d", avg, cnt)
	}
}

func TestRetentionDeletesOldTiers(t *testing.T) {
	s := open(t)
	a, _ := s.AddServer("a")
	now := int64(1700000000)
	old := now - 49*3600 // older than 48h raw retention
	s.InsertSamples(a.ID, old, map[string]float64{"cpu.usage": 5})
	s.InsertSamples(a.ID, now-10, map[string]float64{"cpu.usage": 6})

	cfg, _ := s.GetSettings()
	if err := s.EnforceRetention(now, cfg); err != nil {
		t.Fatal(err)
	}
	var n int
	s.db.QueryRow(`SELECT COUNT(*) FROM samples`).Scan(&n)
	if n != 1 {
		t.Fatalf("raw retention: %d rows left", n)
	}
}

func TestDeleteHistoryByServerAndRange(t *testing.T) {
	s := open(t)
	a, _ := s.AddServer("a")
	b, _ := s.AddServer("b")
	base := int64(1700000000) - 1700000000%60
	seed(t, s, a.ID, "cpu.usage", base, []float64{1})
	seed(t, s, b.ID, "cpu.usage", base, []float64{2})
	s.RollupSince(base)

	if err := s.DeleteHistory(a.ID, base-100, base+100); err != nil {
		t.Fatal(err)
	}
	var raw, r1m int
	s.db.QueryRow(`SELECT COUNT(*) FROM samples WHERE server_id=?`, a.ID).Scan(&raw)
	s.db.QueryRow(`SELECT COUNT(*) FROM rollup_1m WHERE server_id=?`, a.ID).Scan(&r1m)
	if raw != 0 || r1m != 0 {
		t.Fatalf("server a history must be gone: raw=%d r1m=%d", raw, r1m)
	}
	s.db.QueryRow(`SELECT COUNT(*) FROM samples WHERE server_id=?`, b.ID).Scan(&raw)
	if raw != 1 {
		t.Fatal("server b history must survive")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./mother/store/ -run 'TestRollup|TestRetention|TestDelete' -v`
Expected: FAIL — `undefined: (*Store).InsertSamples` (compile error)

- [ ] **Step 3: Write minimal implementation**

`mother/store/samples.go`:

```go
package store

// InsertSamples writes one push's samples as raw rows.
func (s *Store) InsertSamples(serverID int64, ts int64, samples map[string]float64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare(`INSERT INTO samples (server_id, metric, ts, value) VALUES (?,?,?,?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for metric, value := range samples {
		if _, err := stmt.Exec(serverID, metric, ts, value); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// RollupSince recomputes rollups for windows >= since. REPLACE makes it
// idempotent; grouping is per (server, metric) — never across servers.
func (s *Store) RollupSince(since int64) error {
	if _, err := s.db.Exec(`
		INSERT OR REPLACE INTO rollup_1m (server_id, metric, window_start, min, max, avg, cnt)
		SELECT server_id, metric, (ts/60)*60, MIN(value), MAX(value), AVG(value), COUNT(*)
		FROM samples WHERE ts >= ?
		GROUP BY server_id, metric, ts/60`, since); err != nil {
		return err
	}
	_, err := s.db.Exec(`
		INSERT OR REPLACE INTO rollup_1h (server_id, metric, window_start, min, max, avg, cnt)
		SELECT server_id, metric, (window_start/3600)*3600,
		       MIN(min), MAX(max), SUM(avg*cnt)/SUM(cnt), SUM(cnt)
		FROM rollup_1m WHERE window_start >= ?
		GROUP BY server_id, metric, window_start/3600`, since)
	return err
}

// EnforceRetention deletes expired rows per tier (raw 48h, 1m 15d, 1h 75d by default).
func (s *Store) EnforceRetention(now int64, cfg Settings) error {
	cutoffs := []struct {
		table string
		col   string
		limit int64
	}{
		{"samples", "ts", now - int64(cfg.RetentionRawHours)*3600},
		{"rollup_1m", "window_start", now - int64(cfg.Retention1mDays)*86400},
		{"rollup_1h", "window_start", now - int64(cfg.Retention1hDays)*86400},
	}
	for _, c := range cutoffs {
		if _, err := s.db.Exec(`DELETE FROM `+c.table+` WHERE `+c.col+` < ?`, c.limit); err != nil {
			return err
		}
	}
	return nil
}

// DeleteHistory removes stored metrics for a server (0 = all) in [from, to].
func (s *Store) DeleteHistory(serverID int64, from, to int64) error {
	for _, q := range []struct{ table, col string }{
		{"samples", "ts"}, {"rollup_1m", "window_start"}, {"rollup_1h", "window_start"},
	} {
		query := `DELETE FROM ` + q.table + ` WHERE ` + q.col + ` BETWEEN ? AND ?`
		args := []any{from, to}
		if serverID != 0 {
			query += ` AND server_id = ?`
			args = append(args, serverID)
		}
		if _, err := s.db.Exec(query, args...); err != nil {
			return err
		}
	}
	return nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./mother/store/ -v`
Expected: PASS (all)

- [ ] **Step 5: Commit**

```bash
git add mother/store/samples.go mother/store/samples_test.go
git commit -m "feat: add sample storage with per-server rollups, retention, history deletion"
```

---

### Task 10: ingest endpoint

**Files:**
- Create: `mother/api/ingest.go`, `mother/api/middleware.go`
- Test: `mother/api/ingest_test.go`

**Interfaces:**
- Consumes: `store.Store` methods (Tasks 7–9), `protocol` types (Task 1).
- Produces: `api.New(st *store.Store, apiKey string, downloads string) *API` and `(*API).Handler() http.Handler` (routes registered incrementally in later tasks); `POST /v1/ingest` — Bearer-token auth against `servers.token`, 401 on unknown/revoked, 400 on malformed body or >256 samples, **429 when a token pushes more than once per 2 seconds** (rate limit), inserts samples, touches server, responds `IngestResponse{Collectors: server.Collectors, Interval: settings.Interval, DesiredVersion: settings.DesiredVersion}`.

- [ ] **Step 1: Write the failing test**

`mother/api/ingest_test.go`:

```go
package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/osman-yahya/feast-watch/mother/store"
	"github.com/osman-yahya/feast-watch/shared/protocol"
)

func setup(t *testing.T) (*API, *store.Store) {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return New(st, "adminkey", t.TempDir()), st
}

func postIngest(t *testing.T, h http.Handler, token string, req protocol.IngestRequest) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(req)
	r := httptest.NewRequest(http.MethodPost, "/v1/ingest", bytes.NewReader(body))
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestIngestStoresSamplesAndReturnsConfig(t *testing.T) {
	a, st := setup(t)
	srv, _ := st.AddServer("web-1")

	w := postIngest(t, a.Handler(), srv.Token, protocol.IngestRequest{
		Server: "web-1", AgentVersion: "1.2.0", Hostname: "h", IP: "10.0.0.7", OS: "linux",
		Samples: map[string]float64{"cpu.usage": 34.2},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}

	var resp protocol.IngestResponse
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Interval != 10 || len(resp.Collectors) != 4 {
		t.Fatalf("config response: %+v", resp)
	}

	list, _ := st.ListServers()
	if list[0].AgentVersion != "1.2.0" || list[0].LastPush == 0 {
		t.Fatalf("server not touched: %+v", list[0])
	}
}

func TestIngestRejectsBadToken(t *testing.T) {
	a, _ := setup(t)
	w := postIngest(t, a.Handler(), "tk_bogus", protocol.IngestRequest{Server: "x"})
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", w.Code)
	}
	w = postIngest(t, a.Handler(), "", protocol.IngestRequest{Server: "x"})
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("missing token: want 401, got %d", w.Code)
	}
}

func TestIngestValidatesPayload(t *testing.T) {
	a, st := setup(t)
	srv, _ := st.AddServer("web-1")

	r := httptest.NewRequest(http.MethodPost, "/v1/ingest", bytes.NewReader([]byte("{not json")))
	r.Header.Set("Authorization", "Bearer "+srv.Token)
	w := httptest.NewRecorder()
	a.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("malformed body: want 400, got %d", w.Code)
	}

	big := map[string]float64{}
	for i := 0; i < 300; i++ {
		big[string(rune('a'+i%26))+string(rune('0'+i%10))+string(rune('A'+i%26))+string(rune(i))] = 1
	}
	w = postIngest(t, a.Handler(), srv.Token, protocol.IngestRequest{Server: "web-1", Samples: big})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("oversized payload: want 400, got %d", w.Code)
	}
}

func TestIngestRateLimitsPerToken(t *testing.T) {
	a, st := setup(t)
	srv, _ := st.AddServer("web-1")
	req := protocol.IngestRequest{Server: "web-1", Samples: map[string]float64{"cpu.usage": 1}}

	if w := postIngest(t, a.Handler(), srv.Token, req); w.Code != http.StatusOK {
		t.Fatalf("first push: %d", w.Code)
	}
	if w := postIngest(t, a.Handler(), srv.Token, req); w.Code != http.StatusTooManyRequests {
		t.Fatalf("immediate second push: want 429, got %d", w.Code)
	}

	other, _ := st.AddServer("web-2")
	if w := postIngest(t, a.Handler(), other.Token,
		protocol.IngestRequest{Server: "web-2", Samples: map[string]float64{"cpu.usage": 1}}); w.Code != http.StatusOK {
		t.Fatalf("rate limit must be per token, got %d", w.Code)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./mother/api/ -v`
Expected: FAIL — `undefined: New`

- [ ] **Step 3: Write minimal implementation**

`mother/api/middleware.go`:

```go
// Package api is the mother's HTTP surface: agent ingest, backend admin API,
// install script and binary distribution.
package api

import (
	"net/http"
	"strings"

	"github.com/osman-yahya/feast-watch/mother/store"
)

type API struct {
	st        *store.Store
	apiKey    string
	downloads string // directory holding agent binaries + .sha256 files

	mu       sync.Mutex
	lastPush map[int64]time.Time // per-server rate-limit state
}

func New(st *store.Store, apiKey string, downloads string) *API {
	return &API{st: st, apiKey: apiKey, downloads: downloads, lastPush: map[int64]time.Time{}}
}
```

(`middleware.go` imports grow by `"sync"` and `"time"`.)

```go

func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/ingest", a.handleIngest)
	a.registerAdmin(mux)   // Task 11
	a.registerChart(mux)   // Task 12
	a.registerInstall(mux) // Task 13
	return mux
}

// bearerServer authenticates an agent push by its per-server token.
func (a *API) bearerServer(r *http.Request) (store.Server, bool) {
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || tok == "" {
		return store.Server{}, false
	}
	srv, err := a.st.ServerByToken(tok)
	if err != nil {
		return store.Server{}, false
	}
	return srv, true
}

// requireAPIKey guards the backend-facing admin surface.
func (a *API) requireAPIKey(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != a.apiKey || a.apiKey == "" {
			http.Error(w, `{"error":"invalid api key"}`, http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

// allowPush is a per-token rate limiter: at most one push per minPushGap.
func (a *API) allowPush(serverID int64) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	if last, ok := a.lastPush[serverID]; ok && now.Sub(last) < minPushGap {
		return false
	}
	a.lastPush[serverID] = now
	return true
}
```

Until Tasks 11–13 land, add empty stubs at the bottom of `middleware.go` so the package compiles (each task replaces its stub):

```go
func (a *API) registerAdmin(mux *http.ServeMux)   {} // replaced in Task 11
func (a *API) registerChart(mux *http.ServeMux)   {} // replaced in Task 12
func (a *API) registerInstall(mux *http.ServeMux) {} // replaced in Task 13
```

`mother/api/ingest.go`:

```go
package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/osman-yahya/feast-watch/shared/protocol"
)

const (
	maxSamplesPerPush = 256
	minPushGap        = 2 * time.Second // rate limit: one push per token per 2s
)

func (a *API) handleIngest(w http.ResponseWriter, r *http.Request) {
	srv, ok := a.bearerServer(r)
	if !ok {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}
	var req protocol.IngestRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		http.Error(w, `{"error":"malformed payload"}`, http.StatusBadRequest)
		return
	}
	if len(req.Samples) > maxSamplesPerPush {
		http.Error(w, `{"error":"too many samples"}`, http.StatusBadRequest)
		return
	}
	// Rate limit sits after validation so rejected payloads never consume the
	// slot; it protects the storage write path (one push per token per 2s).
	if !a.allowPush(srv.ID) {
		http.Error(w, `{"error":"pushing too fast"}`, http.StatusTooManyRequests)
		return
	}

	now := time.Now().Unix()
	if err := a.st.InsertSamples(srv.ID, now, req.Samples); err != nil {
		slog.Error("insert samples", "server", srv.Name, "err", err)
		http.Error(w, `{"error":"storage failure"}`, http.StatusInternalServerError)
		return
	}
	if err := a.st.TouchServer(srv.ID, req.AgentVersion, req.Hostname, req.IP, req.OS, now); err != nil {
		slog.Error("touch server", "server", srv.Name, "err", err)
	}

	settings, err := a.st.GetSettings()
	if err != nil {
		http.Error(w, `{"error":"storage failure"}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(protocol.IngestResponse{
		Collectors:     srv.Collectors,
		Interval:       settings.Interval,
		DesiredVersion: settings.DesiredVersion,
	})
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./mother/api/ -v`
Expected: PASS (3 tests)

- [ ] **Step 5: Commit**

```bash
git add mother/api/
git commit -m "feat: add ingest endpoint with token auth and config response"
```

---

### Task 11: admin API — servers, settings, history

**Files:**
- Create: `mother/api/admin.go` (replace the `registerAdmin` stub in `middleware.go`)
- Test: `mother/api/admin_test.go`

**Interfaces:**
- Consumes: store methods (Tasks 7–9), `requireAPIKey` (Task 10).
- Produces (all under `X-API-Key`, all JSON envelope `{"success":bool,"data":...,"error":...}`):
  - `GET /api/servers` — list with computed `status`: `"pending"` (never pushed), `"online"`, `"down"` (last push older than `threshold × interval`). Includes `agent_version`, `ip`, `hostname`, `collectors`.
  - `POST /api/servers` `{"name":"X"}` — creates server, returns server + `install_command`.
  - `DELETE /api/servers/{id}` — removes server (token revoked ⇒ future pushes 401).
  - `PUT /api/servers/{id}/collectors` `{"collectors":[...]}`.
  - `GET /api/settings`, `PUT /api/settings` (full `store.Settings` object).
  - `DELETE /api/history?server_id=&from=&to=`.

- [ ] **Step 1: Write the failing test**

`mother/api/admin_test.go`:

```go
package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/osman-yahya/feast-watch/mother/store"
	"github.com/osman-yahya/feast-watch/shared/protocol"
)

func adminReq(t *testing.T, h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("X-API-Key", "adminkey")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

type envelope struct {
	Success bool            `json:"success"`
	Data    json.RawMessage `json:"data"`
	Error   string          `json:"error"`
}

func TestAdminRequiresAPIKey(t *testing.T) {
	a, _ := setup(t)
	r := httptest.NewRequest(http.MethodGet, "/api/servers", nil)
	w := httptest.NewRecorder()
	a.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", w.Code)
	}
}

func TestAddServerReturnsInstallCommand(t *testing.T) {
	a, _ := setup(t)
	w := adminReq(t, a.Handler(), http.MethodPost, "/api/servers", `{"name":"DB_Sunucusu"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	var env envelope
	json.Unmarshal(w.Body.Bytes(), &env)
	var data struct {
		Server         store.Server `json:"server"`
		InstallCommand string       `json:"install_command"`
	}
	json.Unmarshal(env.Data, &data)
	if !strings.Contains(data.InstallCommand, "/install/"+data.Server.Token+".sh") {
		t.Fatalf("install command: %q", data.InstallCommand)
	}
}

func TestServerStatusPendingOnlineDown(t *testing.T) {
	a, st := setup(t)
	st.AddServer("never-pushed")
	fresh, _ := st.AddServer("fresh")
	stale, _ := st.AddServer("stale")

	postIngest(t, a.Handler(), fresh.Token, protocol.IngestRequest{Server: "fresh", Samples: map[string]float64{"cpu.usage": 1}})
	// stale pushed long ago: threshold(3) × interval(10) = 30s window
	st.TouchServer(stale.ID, "1.0.0", "", "", "", 1)

	w := adminReq(t, a.Handler(), http.MethodGet, "/api/servers", "")
	var env envelope
	json.Unmarshal(w.Body.Bytes(), &env)
	var list []struct {
		Name   string `json:"name"`
		Status string `json:"status"`
	}
	json.Unmarshal(env.Data, &list)

	byName := map[string]string{}
	for _, s := range list {
		byName[s.Name] = s.Status
	}
	if byName["never-pushed"] != "pending" || byName["fresh"] != "online" || byName["stale"] != "down" {
		t.Fatalf("statuses: %v", byName)
	}
}

func TestSettingsUpdateAffectsIngestResponse(t *testing.T) {
	a, st := setup(t)
	srv, _ := st.AddServer("web-1")

	adminReq(t, a.Handler(), http.MethodPut, "/api/settings",
		`{"interval":30,"heartbeat_miss_threshold":3,"retention_raw_hours":48,"retention_1m_days":15,"retention_1h_days":75,"desired_version":"v9.9.9"}`)

	w := postIngest(t, a.Handler(), srv.Token, protocol.IngestRequest{Server: "web-1", Samples: map[string]float64{}})
	var resp protocol.IngestResponse
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Interval != 30 || resp.DesiredVersion != "v9.9.9" {
		t.Fatalf("settings not applied to ingest: %+v", resp)
	}
}

func TestUpdateCollectorsAndDeleteHistory(t *testing.T) {
	a, st := setup(t)
	srv, _ := st.AddServer("cf-1")

	w := adminReq(t, a.Handler(), http.MethodPut, fmt.Sprintf("/api/servers/%d/collectors", srv.ID),
		`{"collectors":["cpu","memory","uptime","disk","centrifugo"]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("collectors update: %d %s", w.Code, w.Body)
	}
	got, _ := st.ServerByToken(srv.Token)
	if len(got.Collectors) != 5 || got.Collectors[4] != "centrifugo" {
		t.Fatalf("collectors: %v", got.Collectors)
	}

	st.InsertSamples(srv.ID, 1700000000, map[string]float64{"cpu.usage": 1})
	w = adminReq(t, a.Handler(), http.MethodDelete,
		fmt.Sprintf("/api/history?server_id=%d&from=0&to=2000000000", srv.ID), "")
	if w.Code != http.StatusOK {
		t.Fatalf("delete history: %d", w.Code)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./mother/api/ -run TestAdmin -v` (then the rest)
Expected: FAIL — 404s from stub `registerAdmin`

- [ ] **Step 3: Write minimal implementation**

Delete the `registerAdmin` stub line from `middleware.go`, then create `mother/api/admin.go`:

```go
package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/osman-yahya/feast-watch/mother/store"
)

func writeJSON(w http.ResponseWriter, status int, data any, errMsg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{
		"success": errMsg == "", "data": data, "error": errMsg,
	})
}

func (a *API) registerAdmin(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/servers", a.requireAPIKey(a.handleListServers))
	mux.HandleFunc("POST /api/servers", a.requireAPIKey(a.handleAddServer))
	mux.HandleFunc("DELETE /api/servers/{id}", a.requireAPIKey(a.handleDeleteServer))
	mux.HandleFunc("PUT /api/servers/{id}/collectors", a.requireAPIKey(a.handleSetCollectors))
	mux.HandleFunc("GET /api/settings", a.requireAPIKey(a.handleGetSettings))
	mux.HandleFunc("PUT /api/settings", a.requireAPIKey(a.handlePutSettings))
	mux.HandleFunc("DELETE /api/history", a.requireAPIKey(a.handleDeleteHistory))
}

type serverView struct {
	ID           int64    `json:"id"`
	Name         string   `json:"name"`
	Status       string   `json:"status"`
	Collectors   []string `json:"collectors"`
	Hostname     string   `json:"hostname"`
	IP           string   `json:"ip"`
	OS           string   `json:"os"`
	AgentVersion string   `json:"agent_version"`
	LastPush     int64    `json:"last_push"`
}

func status(srv store.Server, s store.Settings, now int64) string {
	if srv.LastPush == 0 {
		return "pending"
	}
	if now-srv.LastPush > int64(s.HeartbeatMissThreshold*s.Interval) {
		return "down"
	}
	return "online"
}

func (a *API) handleListServers(w http.ResponseWriter, r *http.Request) {
	servers, err := a.st.ListServers()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, nil, "storage failure")
		return
	}
	settings, err := a.st.GetSettings()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, nil, "storage failure")
		return
	}
	now := time.Now().Unix()
	views := make([]serverView, 0, len(servers))
	for _, s := range servers {
		views = append(views, serverView{
			ID: s.ID, Name: s.Name, Status: status(s, settings, now),
			Collectors: s.Collectors, Hostname: s.Hostname, IP: s.IP, OS: s.OS,
			AgentVersion: s.AgentVersion, LastPush: s.LastPush,
		})
	}
	writeJSON(w, http.StatusOK, views, "")
}

func (a *API) handleAddServer(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Name == "" {
		writeJSON(w, http.StatusBadRequest, nil, "name is required")
		return
	}
	srv, err := a.st.AddServer(in.Name)
	if err != nil {
		writeJSON(w, http.StatusConflict, nil, "server name already exists")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"server":          srv,
		"install_command": a.installCommand(srv.Token),
	}, "")
}

func (a *API) handleDeleteServer(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, nil, "invalid id")
		return
	}
	if err := a.st.DeleteServer(id); err != nil {
		writeJSON(w, http.StatusInternalServerError, nil, "storage failure")
		return
	}
	writeJSON(w, http.StatusOK, nil, "")
}

func (a *API) handleSetCollectors(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, nil, "invalid id")
		return
	}
	var in struct {
		Collectors []string `json:"collectors"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || len(in.Collectors) == 0 {
		writeJSON(w, http.StatusBadRequest, nil, "collectors list is required")
		return
	}
	if err := a.st.SetCollectors(id, in.Collectors); err != nil {
		writeJSON(w, http.StatusInternalServerError, nil, "storage failure")
		return
	}
	writeJSON(w, http.StatusOK, nil, "")
}

func (a *API) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	s, err := a.st.GetSettings()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, nil, "storage failure")
		return
	}
	writeJSON(w, http.StatusOK, s, "")
}

func (a *API) handlePutSettings(w http.ResponseWriter, r *http.Request) {
	var in store.Settings
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, nil, "malformed settings")
		return
	}
	if in.Interval < 1 || in.HeartbeatMissThreshold < 1 {
		writeJSON(w, http.StatusBadRequest, nil, "interval and threshold must be >= 1")
		return
	}
	if err := a.st.SaveSettings(in); err != nil {
		writeJSON(w, http.StatusInternalServerError, nil, "storage failure")
		return
	}
	writeJSON(w, http.StatusOK, in, "")
}

func (a *API) handleDeleteHistory(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	serverID, _ := strconv.ParseInt(q.Get("server_id"), 10, 64)
	from, err1 := strconv.ParseInt(q.Get("from"), 10, 64)
	to, err2 := strconv.ParseInt(q.Get("to"), 10, 64)
	if err1 != nil || err2 != nil || to < from {
		writeJSON(w, http.StatusBadRequest, nil, "from and to are required unix seconds")
		return
	}
	if err := a.st.DeleteHistory(serverID, from, to); err != nil {
		writeJSON(w, http.StatusInternalServerError, nil, "storage failure")
		return
	}
	writeJSON(w, http.StatusOK, nil, "")
}

// installCommand renders the one-liner shown by the panel and the CLI.
func (a *API) installCommand(token string) string {
	return fmt.Sprintf("curl -sSLk https://%s/install/%s.sh | sudo bash", a.publicAddr, token)
}
```

Add the `publicAddr` field to `API` in `middleware.go` (`New` gains a parameter):

```go
type API struct {
	st         *store.Store
	apiKey     string
	downloads  string
	publicAddr string // host:port agents reach the mother on, e.g. "10.0.0.1:8443"
}

func New(st *store.Store, apiKey string, downloads string) *API {
	return &API{st: st, apiKey: apiKey, downloads: downloads, publicAddr: "127.0.0.1:8443"}
}

func (a *API) SetPublicAddr(addr string) { a.publicAddr = addr }
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./mother/api/ -v`
Expected: PASS (all)

- [ ] **Step 5: Commit**

```bash
git add mother/api/
git commit -m "feat: add admin API — servers with status, settings, history deletion"
```

---

### Task 12: chart API with tier selection

**Files:**
- Create: `mother/api/chart.go` (replace the `registerChart` stub)
- Test: `mother/api/chart_test.go`

**Interfaces:**
- Consumes: rollup tables (Task 9), `requireAPIKey` (Task 10).
- Produces: `GET /api/chart?server_id=&metric=&from=&to=&interval=` (interval in seconds, min 60). Tier rule: `interval < 3600` → read `rollup_1m`, else `rollup_1h`; buckets grouped to `interval`; response `[]ChartPoint{TS int64; Min, Max, Avg float64}`; **maximum 500 points** (400 if `(to-from)/interval > 500`); the handler must contain no query against `samples`.

- [ ] **Step 1: Write the failing test**

`mother/api/chart_test.go`:

```go
package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
)

type chartPoint struct {
	TS  int64   `json:"ts"`
	Min float64 `json:"min"`
	Max float64 `json:"max"`
	Avg float64 `json:"avg"`
}

func TestChartReadsRollupsAndGroups(t *testing.T) {
	a, st := setup(t)
	srv, _ := st.AddServer("web-1")
	base := int64(1700000000) - 1700000000%3600

	// two minutes of raw data → rollup
	for i := int64(0); i < 12; i++ { // 12 × 10s across 2 minutes
		st.InsertSamples(srv.ID, base+i*10, map[string]float64{"cpu.usage": float64(10 + i)})
	}
	st.RollupSince(base)

	// interval=120 → grouped from rollup_1m into one 2-minute bucket
	w := adminReq(t, a.Handler(), http.MethodGet,
		fmt.Sprintf("/api/chart?server_id=%d&metric=cpu.usage&from=%d&to=%d&interval=120", srv.ID, base, base+300), "")
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	var env envelope
	json.Unmarshal(w.Body.Bytes(), &env)
	var pts []chartPoint
	json.Unmarshal(env.Data, &pts)
	if len(pts) != 1 {
		t.Fatalf("want 1 grouped bucket, got %d: %+v", len(pts), pts)
	}
	if pts[0].Min != 10 || pts[0].Max != 21 {
		t.Fatalf("bucket bounds: %+v", pts[0])
	}
}

func TestChartRejectsUnboundedRequests(t *testing.T) {
	a, st := setup(t)
	srv, _ := st.AddServer("web-1")
	// 75 days at 60s interval would be 108k points — must be rejected, not served
	w := adminReq(t, a.Handler(), http.MethodGet,
		fmt.Sprintf("/api/chart?server_id=%d&metric=cpu.usage&from=0&to=6480000&interval=60", srv.ID), "")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400 for unbounded request, got %d", w.Code)
	}
}

func TestChartValidatesParams(t *testing.T) {
	a, _ := setup(t)
	w := adminReq(t, a.Handler(), http.MethodGet, "/api/chart?metric=cpu.usage", "")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("missing params: want 400, got %d", w.Code)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./mother/api/ -run TestChart -v`
Expected: FAIL — 404 from stub `registerChart`

- [ ] **Step 3: Write minimal implementation**

Delete the `registerChart` stub, create `mother/api/chart.go`:

```go
package api

import (
	"net/http"
	"strconv"
)

// maxChartPoints bounds every chart response — the frontend can never receive
// more, regardless of range. Raw `samples` are NEVER queried here (spec).
const maxChartPoints = 500

type ChartPoint struct {
	TS  int64   `json:"ts"`
	Min float64 `json:"min"`
	Max float64 `json:"max"`
	Avg float64 `json:"avg"`
}

func (a *API) registerChart(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/chart", a.requireAPIKey(a.handleChart))
}

func (a *API) handleChart(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	serverID, err1 := strconv.ParseInt(q.Get("server_id"), 10, 64)
	from, err2 := strconv.ParseInt(q.Get("from"), 10, 64)
	to, err3 := strconv.ParseInt(q.Get("to"), 10, 64)
	interval, err4 := strconv.ParseInt(q.Get("interval"), 10, 64)
	metric := q.Get("metric")
	if err1 != nil || err2 != nil || err3 != nil || err4 != nil || metric == "" || to <= from {
		writeJSON(w, http.StatusBadRequest, nil, "server_id, metric, from, to, interval are required")
		return
	}
	if interval < 60 {
		interval = 60
	}
	if (to-from)/interval > maxChartPoints {
		writeJSON(w, http.StatusBadRequest, nil, "range/interval exceeds max points; increase interval")
		return
	}

	// Tier selection: sub-hour resolution comes from rollup_1m, else rollup_1h.
	table := "rollup_1h"
	if interval < 3600 {
		table = "rollup_1m"
	}

	rows, err := a.st.DB().Query(`
		SELECT (window_start/?)*? AS bucket,
		       MIN(min), MAX(max), SUM(avg*cnt)/SUM(cnt)
		FROM `+table+`
		WHERE server_id = ? AND metric = ? AND window_start BETWEEN ? AND ?
		GROUP BY bucket ORDER BY bucket`,
		interval, interval, serverID, metric, from, to)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, nil, "storage failure")
		return
	}
	defer rows.Close()

	points := []ChartPoint{}
	for rows.Next() {
		var p ChartPoint
		if err := rows.Scan(&p.TS, &p.Min, &p.Max, &p.Avg); err != nil {
			writeJSON(w, http.StatusInternalServerError, nil, "storage failure")
			return
		}
		points = append(points, p)
	}
	writeJSON(w, http.StatusOK, points, "")
}
```

Add the read-only DB accessor to `mother/store/store.go`:

```go
// DB exposes the handle for read-only query composition in the api package.
func (s *Store) DB() *sql.DB { return s.db }
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./mother/... -v`
Expected: PASS (all)

- [ ] **Step 5: Commit**

```bash
git add mother/
git commit -m "feat: add chart API with tier selection and bounded point count"
```

---

### Task 13: install script + binary download + CLI generate

**Files:**
- Create: `mother/api/install.go` (replace the `registerInstall` stub), `deploy/install.sh.tmpl`, `mother/generate.go`
- Test: `mother/api/install_test.go`

**Interfaces:**
- Consumes: `ServerByToken` (Task 7), `installCommand`/`publicAddr` (Task 11).
- Produces: `GET /install/{token}.sh` — 404 for unknown token; renders `deploy/install.sh.tmpl` (embedded via `go:embed`) with `{{.MotherURL}}`, `{{.Token}}`, `{{.ServerName}}`. `GET /download/agent/{version}` and `GET /download/agent/{version}.sha256` — served from the downloads dir, path-traversal-safe. `RunGenerate(st *store.Store, publicAddr string, args []string) (string, error)` — implements `feast-watch generate --name=X` (creates server if missing, prints install command).

- [ ] **Step 1: Write the failing test**

`mother/api/install_test.go`:

```go
package api

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/osman-yahya/feast-watch/mother"
	"github.com/osman-yahya/feast-watch/mother/store"
)

func TestInstallScriptRendersTokenAndMotherURL(t *testing.T) {
	a, st := setup(t)
	a.SetPublicAddr("10.0.0.1:8443")
	srv, _ := st.AddServer("DB_Sunucusu")

	r := httptest.NewRequest(http.MethodGet, "/install/"+srv.Token+".sh", nil)
	w := httptest.NewRecorder()
	a.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{"MOTHER_URL=https://10.0.0.1:8443", "TOKEN=" + srv.Token, "SERVER_NAME=DB_Sunucusu", "systemctl"} {
		if !strings.Contains(body, want) {
			t.Fatalf("script missing %q:\n%s", want, body)
		}
	}

	r = httptest.NewRequest(http.MethodGet, "/install/tk_bogus.sh", nil)
	w = httptest.NewRecorder()
	a.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusNotFound {
		t.Fatalf("unknown token: want 404, got %d", w.Code)
	}
}

func TestDownloadServesBinaryAndChecksum(t *testing.T) {
	st, _ := store.Open(":memory:")
	t.Cleanup(func() { st.Close() })
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "feast-watch-agent-v1.3.0"), []byte("BINARY"), 0o755)
	os.WriteFile(filepath.Join(dir, "feast-watch-agent-v1.3.0.sha256"), []byte("abc123\n"), 0o644)
	a := New(st, "adminkey", dir)

	r := httptest.NewRequest(http.MethodGet, "/download/agent/v1.3.0", nil)
	w := httptest.NewRecorder()
	a.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusOK || w.Body.String() != "BINARY" {
		t.Fatalf("binary download: %d %q", w.Code, w.Body.String())
	}

	r = httptest.NewRequest(http.MethodGet, "/download/agent/..%2F..%2Fetc%2Fpasswd", nil)
	w = httptest.NewRecorder()
	a.Handler().ServeHTTP(w, r)
	if w.Code == http.StatusOK {
		t.Fatal("path traversal must be rejected")
	}
}

func TestGenerateCLI(t *testing.T) {
	st, _ := store.Open(":memory:")
	t.Cleanup(func() { st.Close() })
	out, err := mother.RunGenerate(st, "10.0.0.1:8443", []string{"--name=DB_Sunucusu"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "curl -sSLk https://10.0.0.1:8443/install/tk_") {
		t.Fatalf("generate output: %q", out)
	}
	// idempotent: same name returns the existing server's command
	out2, err := mother.RunGenerate(st, "10.0.0.1:8443", []string{"--name=DB_Sunucusu"})
	if err != nil || out2 != out {
		t.Fatalf("generate must be idempotent: %v %q vs %q", err, out, out2)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./mother/api/ -run 'TestInstall|TestDownload|TestGenerate' -v`
Expected: FAIL — 404 from stub / `undefined: mother.RunGenerate`

- [ ] **Step 3: Write minimal implementation**

`deploy/install.sh.tmpl`:

```bash
#!/usr/bin/env bash
# feast-watch agent installer — rendered by the mother per server token.
set -euo pipefail

MOTHER_URL={{.MotherURL}}
TOKEN={{.Token}}
SERVER_NAME="{{.ServerName}}"

ARCH=$(uname -m)
case "$ARCH" in
  x86_64) ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  *) echo "unsupported arch: $ARCH" >&2; exit 1 ;;
esac

echo "-> downloading agent"
curl -sSLk "$MOTHER_URL/download/agent/latest-$ARCH" -o /usr/local/bin/feast-watch-agent
chmod 0755 /usr/local/bin/feast-watch-agent

echo "-> writing config"
mkdir -p /etc/feast-watch
cat > /etc/feast-watch/agent.conf <<EOF
MOTHER_URL=$MOTHER_URL
TOKEN=$TOKEN
SERVER_NAME=$SERVER_NAME
EOF
chmod 0600 /etc/feast-watch/agent.conf

echo "-> installing systemd service"
cat > /etc/systemd/system/feast-watch-agent.service <<'EOF'
[Unit]
Description=feast-watch agent
After=network-online.target

[Service]
ExecStart=/usr/local/bin/feast-watch-agent
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
EOF

systemctl daemon-reload
systemctl enable --now feast-watch-agent
echo "feast-watch agent installed — server will appear online within seconds."
```

`mother/api/install.go`:

```go
package api

import (
	_ "embed"
	"net/http"
	"path/filepath"
	"strings"
	"text/template"
)

//go:embed install.sh.tmpl
var installTmplSrc string

var installTmpl = template.Must(template.New("install").Parse(installTmplSrc))

func (a *API) registerInstall(mux *http.ServeMux) {
	mux.HandleFunc("GET /install/{token}", a.handleInstallScript)
	mux.HandleFunc("GET /download/agent/{version}", a.handleDownload)
}

func (a *API) handleInstallScript(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimSuffix(r.PathValue("token"), ".sh")
	srv, err := a.st.ServerByToken(token)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/x-shellscript")
	installTmpl.Execute(w, map[string]string{
		"MotherURL":  "https://" + a.publicAddr,
		"Token":      srv.Token,
		"ServerName": srv.Name,
	})
}

func (a *API) handleDownload(w http.ResponseWriter, r *http.Request) {
	version := r.PathValue("version")
	// filepath.Base strips any traversal attempt; names are flat in downloads/.
	name := filepath.Base("feast-watch-agent-" + version)
	if strings.Contains(version, "/") || strings.Contains(version, "..") {
		http.Error(w, "invalid version", http.StatusBadRequest)
		return
	}
	http.ServeFile(w, r, filepath.Join(a.downloads, name))
}
```

Copy the template next to the Go file so `go:embed` finds it (single source of truth stays in `deploy/`, the api copy is the embedded one):

```bash
cp deploy/install.sh.tmpl mother/api/install.sh.tmpl
```

(Alternatively keep only `mother/api/install.sh.tmpl` and symlink from deploy — choose the copy, simplest.)

`mother/generate.go`:

```go
// Package mother wires the store, API, and background jobs; also hosts the
// `feast-watch generate` CLI used on the mother host without the panel.
package mother

import (
	"errors"
	"flag"
	"fmt"
	"strings"

	"github.com/osman-yahya/feast-watch/mother/store"
)

// RunGenerate implements `feast-watch generate --name=X`: create-or-fetch the
// server and return the one-liner install command with the mother IP embedded.
func RunGenerate(st *store.Store, publicAddr string, args []string) (string, error) {
	fs := flag.NewFlagSet("generate", flag.ContinueOnError)
	name := fs.String("name", "", "server name")
	if err := fs.Parse(args); err != nil {
		return "", err
	}
	if strings.TrimSpace(*name) == "" {
		return "", errors.New("--name is required")
	}

	srv, err := st.ServerByName(*name)
	if errors.Is(err, store.ErrNotFound) {
		srv, err = st.AddServer(*name)
	}
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("curl -sSLk https://%s/install/%s.sh | sudo bash", publicAddr, srv.Token), nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./mother/... -v`
Expected: PASS (all)

- [ ] **Step 5: Commit**

```bash
git add mother/ deploy/install.sh.tmpl
git commit -m "feat: add install script rendering, binary download, generate CLI"
```

---

### Task 14: agent self-update

**Files:**
- Create: `agent/update.go`
- Test: `agent/update_test.go`

**Interfaces:**
- Consumes: `Config` (Task 5).
- Produces: `SelfUpdate(cfg Config, desiredVersion string, exit func(int)) error` — downloads `<MotherURL>/download/agent/<version>` and its `.sha256`, verifies the checksum, atomically replaces the current executable (write temp + rename), then calls `exit(0)` so systemd (`Restart=always`) restarts the new binary. On checksum mismatch: no replacement, error returned.

- [ ] **Step 1: Write the failing test**

`agent/update_test.go`:

```go
package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func updateServer(t *testing.T, binary []byte, sum string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/download/agent/v1.3.0":
			w.Write(binary)
		case "/download/agent/v1.3.0.sha256":
			fmt.Fprintln(w, sum)
		default:
			http.NotFound(w, r)
		}
	}))
}

func TestSelfUpdateReplacesBinaryAndExits(t *testing.T) {
	binary := []byte("NEW BINARY")
	h := sha256.Sum256(binary)
	srv := updateServer(t, binary, hex.EncodeToString(h[:]))
	defer srv.Close()

	target := filepath.Join(t.TempDir(), "feast-watch-agent")
	os.WriteFile(target, []byte("OLD"), 0o755)

	exitCode := -1
	err := selfUpdate(Config{MotherURL: srv.URL}, "v1.3.0", target, func(c int) { exitCode = c })
	if err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(target)
	if string(got) != "NEW BINARY" {
		t.Fatalf("binary not replaced: %q", got)
	}
	if exitCode != 0 {
		t.Fatalf("must exit 0 for systemd restart, got %d", exitCode)
	}
}

func TestSelfUpdateRejectsBadChecksum(t *testing.T) {
	srv := updateServer(t, []byte("NEW BINARY"), "deadbeef")
	defer srv.Close()

	target := filepath.Join(t.TempDir(), "feast-watch-agent")
	os.WriteFile(target, []byte("OLD"), 0o755)

	err := selfUpdate(Config{MotherURL: srv.URL}, "v1.3.0", target, func(int) { t.Fatal("must not exit") })
	if err == nil {
		t.Fatal("checksum mismatch must fail")
	}
	got, _ := os.ReadFile(target)
	if string(got) != "OLD" {
		t.Fatal("binary must be untouched on checksum failure")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./agent/ -run TestSelfUpdate -v`
Expected: FAIL — `undefined: selfUpdate`

- [ ] **Step 3: Write minimal implementation**

`agent/update.go`:

```go
package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// SelfUpdate downloads desiredVersion from the mother, verifies its SHA-256,
// atomically replaces the running executable and exits 0 — systemd restarts us.
func SelfUpdate(cfg Config, desiredVersion string, exit func(int)) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	return selfUpdate(cfg, desiredVersion, self, exit)
}

func selfUpdate(cfg Config, desiredVersion, target string, exit func(int)) error {
	client := &http.Client{Timeout: 60 * time.Second}

	fetch := func(path string) ([]byte, error) {
		resp, err := client.Get(cfg.MotherURL + path)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("GET %s: %d", path, resp.StatusCode)
		}
		return io.ReadAll(resp.Body)
	}

	binary, err := fetch("/download/agent/" + desiredVersion)
	if err != nil {
		return err
	}
	sumRaw, err := fetch("/download/agent/" + desiredVersion + ".sha256")
	if err != nil {
		return err
	}
	wantSum := strings.TrimSpace(string(sumRaw))

	h := sha256.Sum256(binary)
	if hex.EncodeToString(h[:]) != wantSum {
		return fmt.Errorf("checksum mismatch for %s: refusing update", desiredVersion)
	}

	tmp := target + ".new"
	if err := os.WriteFile(tmp, binary, 0o755); err != nil {
		return err
	}
	if err := os.Rename(tmp, target); err != nil {
		os.Remove(tmp)
		return err
	}
	exit(0)
	return nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./agent/ -v`
Expected: PASS (all)

- [ ] **Step 5: Commit**

```bash
git add agent/update.go agent/update_test.go
git commit -m "feat: add checksum-verified agent self-update"
```

---

### Task 15: centrifugo and dragonfly collectors

**Files:**
- Create: `agent/collectors/centrifugo.go`, `agent/collectors/dragonfly.go`
- Test: `agent/collectors/centrifugo_test.go`, `agent/collectors/dragonfly_test.go`

**Interfaces:**
- Consumes: `Sample`, `Collector` (Task 2).
- Produces: `NewCentrifugo(apiURL, apiKey string, connsMax float64) *Centrifugo` emitting `centrifugo.conns` (sum of `num_clients` across nodes from the `info` API) + `centrifugo.conns_max` (from config); `NewDragonfly(addr string) *Dragonfly` emitting `dragonfly.mem_used`, `dragonfly.mem_max`, `dragonfly.clients` via a raw `INFO` command over TCP (RESP inline protocol — no redis client dependency).

- [ ] **Step 1: Write the failing tests**

`agent/collectors/centrifugo_test.go`:

```go
package collectors

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCentrifugoSumsClientsAcrossNodes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != "cfkey" {
			t.Errorf("missing api key header")
		}
		w.Write([]byte(`{"result":{"nodes":[{"num_clients":3000},{"num_clients":1812}]}}`))
	}))
	defer srv.Close()

	c := NewCentrifugo(srv.URL, "cfkey", 10000)
	got, err := c.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	byKey := map[string]float64{}
	for _, s := range got {
		byKey[s.Key] = s.Value
	}
	if byKey["centrifugo.conns"] != 4812 || byKey["centrifugo.conns_max"] != 10000 {
		t.Fatalf("got %v", byKey)
	}
}
```

`agent/collectors/dragonfly_test.go`:

```go
package collectors

import (
	"context"
	"fmt"
	"net"
	"testing"
)

// fakeRESP answers any command with a canned bulk-string INFO payload.
func fakeRESP(t *testing.T, info string) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				buf := make([]byte, 4096)
				c.Read(buf)
				fmt.Fprintf(c, "$%d\r\n%s\r\n", len(info), info)
			}(conn)
		}
	}()
	return ln
}

func TestDragonflyParsesInfo(t *testing.T) {
	info := "# Memory\r\nused_memory:1073741824\r\nmaxmemory:2147483648\r\n# Clients\r\nconnected_clients:42\r\n"
	ln := fakeRESP(t, info)
	defer ln.Close()

	d := NewDragonfly(ln.Addr().String())
	got, err := d.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	byKey := map[string]float64{}
	for _, s := range got {
		byKey[s.Key] = s.Value
	}
	if byKey["dragonfly.mem_used"] != 1073741824 || byKey["dragonfly.mem_max"] != 2147483648 || byKey["dragonfly.clients"] != 42 {
		t.Fatalf("got %v", byKey)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./agent/collectors/ -run 'TestCentrifugo|TestDragonfly' -v`
Expected: FAIL — `undefined: NewCentrifugo`, `undefined: NewDragonfly`

- [ ] **Step 3: Write minimal implementations**

`agent/collectors/centrifugo.go`:

```go
package collectors

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// Centrifugo reads total client connections from the server `info` API and
// reports them against the configured maximum ("conns vs max" family).
type Centrifugo struct {
	apiURL   string
	apiKey   string
	connsMax float64
	client   *http.Client
}

func NewCentrifugo(apiURL, apiKey string, connsMax float64) *Centrifugo {
	return &Centrifugo{apiURL: apiURL, apiKey: apiKey, connsMax: connsMax,
		client: &http.Client{Timeout: 3 * time.Second}}
}

func (c *Centrifugo) Name() string { return "centrifugo" }

func (c *Centrifugo) Collect(ctx context.Context) ([]Sample, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.apiURL,
		bytes.NewReader([]byte(`{"method":"info","params":{}}`)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", c.apiKey)

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("centrifugo info: %d", resp.StatusCode)
	}

	var out struct {
		Result struct {
			Nodes []struct {
				NumClients float64 `json:"num_clients"`
			} `json:"nodes"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	total := 0.0
	for _, n := range out.Result.Nodes {
		total += n.NumClients
	}
	return []Sample{
		{Key: "centrifugo.conns", Value: total},
		{Key: "centrifugo.conns_max", Value: c.connsMax},
	}, nil
}
```

`agent/collectors/dragonfly.go`:

```go
package collectors

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"
)

// Dragonfly issues a raw RESP INFO over TCP — no redis client dependency.
// "RAM runs out → it stops", so memory vs maxmemory is the headline metric.
type Dragonfly struct {
	addr string
}

func NewDragonfly(addr string) *Dragonfly { return &Dragonfly{addr: addr} }

func (d *Dragonfly) Name() string { return "dragonfly" }

func (d *Dragonfly) Collect(ctx context.Context) ([]Sample, error) {
	dialer := net.Dialer{Timeout: 3 * time.Second}
	conn, err := dialer.DialContext(ctx, "tcp", d.addr)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(3 * time.Second))

	if _, err := conn.Write([]byte("INFO\r\n")); err != nil {
		return nil, err
	}

	r := bufio.NewReader(conn)
	header, err := r.ReadString('\n')
	if err != nil || !strings.HasPrefix(header, "$") {
		return nil, fmt.Errorf("unexpected INFO reply: %q", header)
	}
	size, err := strconv.Atoi(strings.TrimSpace(header[1:]))
	if err != nil || size <= 0 {
		return nil, fmt.Errorf("bad INFO length: %q", header)
	}
	body := make([]byte, size)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, err
	}

	fields := map[string]float64{}
	for _, line := range strings.Split(string(body), "\r\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
			fields[k] = f
		}
	}
	return []Sample{
		{Key: "dragonfly.mem_used", Value: fields["used_memory"]},
		{Key: "dragonfly.mem_max", Value: fields["maxmemory"]},
		{Key: "dragonfly.clients", Value: fields["connected_clients"]},
	}, nil
}
```

(Add `"io"` to the dragonfly imports.)

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./agent/collectors/ -v`
Expected: PASS (all)

- [ ] **Step 5: Commit**

```bash
git add agent/collectors/
git commit -m "feat: add centrifugo and dragonfly connection collectors"
```

---

### Task 16: postgres and k8s collectors

**Files:**
- Create: `agent/collectors/postgres.go`, `agent/collectors/k8s.go`
- Test: `agent/collectors/postgres_test.go`, `agent/collectors/k8s_test.go`

**Interfaces:**
- Consumes: `Sample`, `Collector` (Task 2).
- Produces: `NewPostgres(dsn string) *Postgres` emitting `postgres.conns` (`SELECT count(*) FROM pg_stat_activity`) + `postgres.conns_max` (`SHOW max_connections`) — query funcs injectable for unit tests; `NewK8s(apiURL, token string) *K8s` emitting `k8s.nodes_ready`, `k8s.nodes_total`, `k8s.pods_running`, `k8s.pods_failed`, `k8s.restarts` from `/api/v1/nodes` and `/api/v1/pods`.

- [ ] **Step 1: Add pgx**

```bash
go get github.com/jackc/pgx/v5/stdlib@latest
```

- [ ] **Step 2: Write the failing tests**

`agent/collectors/postgres_test.go`:

```go
package collectors

import (
	"context"
	"testing"
)

func TestPostgresReportsConnsVsMax(t *testing.T) {
	p := NewPostgres("postgres://ignored")
	p.query = func(ctx context.Context) (conns, connsMax float64, err error) {
		return 42, 100, nil
	}
	got, err := p.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	byKey := map[string]float64{}
	for _, s := range got {
		byKey[s.Key] = s.Value
	}
	if byKey["postgres.conns"] != 42 || byKey["postgres.conns_max"] != 100 {
		t.Fatalf("got %v", byKey)
	}
}
```

`agent/collectors/k8s_test.go`:

```go
package collectors

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestK8sCollectsNodeAndPodHealth(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer satoken" {
			t.Errorf("missing bearer token")
		}
		switch r.URL.Path {
		case "/api/v1/nodes":
			w.Write([]byte(`{"items":[
				{"status":{"conditions":[{"type":"Ready","status":"True"}]}},
				{"status":{"conditions":[{"type":"Ready","status":"False"}]}}]}`))
		case "/api/v1/pods":
			w.Write([]byte(`{"items":[
				{"status":{"phase":"Running","containerStatuses":[{"restartCount":2}]}},
				{"status":{"phase":"Failed","containerStatuses":[{"restartCount":5}]}}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	k := NewK8s(srv.URL, "satoken")
	got, err := k.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	byKey := map[string]float64{}
	for _, s := range got {
		byKey[s.Key] = s.Value
	}
	want := map[string]float64{
		"k8s.nodes_ready": 1, "k8s.nodes_total": 2,
		"k8s.pods_running": 1, "k8s.pods_failed": 1, "k8s.restarts": 7,
	}
	for k2, v := range want {
		if byKey[k2] != v {
			t.Fatalf("%s: got %v want %v (all: %v)", k2, byKey[k2], v, byKey)
		}
	}
}
```

- [ ] **Step 3: Run tests to verify they fail**

Run: `go test ./agent/collectors/ -run 'TestPostgres|TestK8s' -v`
Expected: FAIL — `undefined: NewPostgres`, `undefined: NewK8s`

- [ ] **Step 4: Write minimal implementations**

`agent/collectors/postgres.go`:

```go
package collectors

import (
	"context"
	"database/sql"
	"strconv"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// Postgres reports active connections vs max_connections. Supabase is hosted
// externally, so this collector runs on the backend server's agent and
// queries remotely — deliberately minimal catalog queries (spec).
type Postgres struct {
	dsn   string
	query func(ctx context.Context) (conns, connsMax float64, err error)
}

func NewPostgres(dsn string) *Postgres {
	p := &Postgres{dsn: dsn}
	p.query = p.liveQuery
	return p
}

func (p *Postgres) Name() string { return "postgres" }

func (p *Postgres) liveQuery(ctx context.Context) (float64, float64, error) {
	db, err := sql.Open("pgx", p.dsn)
	if err != nil {
		return 0, 0, err
	}
	defer db.Close()

	var conns float64
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM pg_stat_activity`).Scan(&conns); err != nil {
		return 0, 0, err
	}
	var maxStr string
	if err := db.QueryRowContext(ctx, `SHOW max_connections`).Scan(&maxStr); err != nil {
		return 0, 0, err
	}
	maxConns, err := strconv.ParseFloat(maxStr, 64)
	if err != nil {
		return 0, 0, err
	}
	return conns, maxConns, nil
}

func (p *Postgres) Collect(ctx context.Context) ([]Sample, error) {
	conns, maxConns, err := p.query(ctx)
	if err != nil {
		return nil, err
	}
	return []Sample{
		{Key: "postgres.conns", Value: conns},
		{Key: "postgres.conns_max", Value: maxConns},
	}, nil
}
```

`agent/collectors/k8s.go`:

```go
package collectors

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// K8s reports node readiness and pod phase counts from the API server.
type K8s struct {
	apiURL string
	token  string
	client *http.Client
}

func NewK8s(apiURL, token string) *K8s {
	return &K8s{apiURL: apiURL, token: token, client: &http.Client{
		Timeout: 5 * time.Second,
		// in-cluster CA handling is deploy-time config; skip-verify inside the cluster
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
	}}
}

func (k *K8s) Name() string { return "k8s" }

func (k *K8s) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, k.apiURL+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+k.token)
	resp, err := k.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %d", path, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (k *K8s) Collect(ctx context.Context) ([]Sample, error) {
	var nodes struct {
		Items []struct {
			Status struct {
				Conditions []struct {
					Type   string `json:"type"`
					Status string `json:"status"`
				} `json:"conditions"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := k.get(ctx, "/api/v1/nodes", &nodes); err != nil {
		return nil, err
	}
	ready := 0.0
	for _, n := range nodes.Items {
		for _, c := range n.Status.Conditions {
			if c.Type == "Ready" && c.Status == "True" {
				ready++
			}
		}
	}

	var pods struct {
		Items []struct {
			Status struct {
				Phase             string `json:"phase"`
				ContainerStatuses []struct {
					RestartCount float64 `json:"restartCount"`
				} `json:"containerStatuses"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := k.get(ctx, "/api/v1/pods", &pods); err != nil {
		return nil, err
	}
	running, failed, restarts := 0.0, 0.0, 0.0
	for _, p := range pods.Items {
		switch p.Status.Phase {
		case "Running":
			running++
		case "Failed":
			failed++
		}
		for _, cs := range p.Status.ContainerStatuses {
			restarts += cs.RestartCount
		}
	}

	return []Sample{
		{Key: "k8s.nodes_ready", Value: ready},
		{Key: "k8s.nodes_total", Value: float64(len(nodes.Items))},
		{Key: "k8s.pods_running", Value: running},
		{Key: "k8s.pods_failed", Value: failed},
		{Key: "k8s.restarts", Value: restarts},
	}, nil
}
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./agent/collectors/ -v`
Expected: PASS (all)

- [ ] **Step 6: Commit**

```bash
git add agent/collectors/ go.mod go.sum
git commit -m "feat: add postgres and k8s health collectors"
```

---

### Task 17: binaries — agent main and mother main

**Files:**
- Create: `agent/cmd/feast-watch-agent/main.go`, `mother/cmd/feast-watch/main.go`
- Test: build-only verification (wiring code; behavior already unit-tested)

**Interfaces:**
- Consumes: everything above.
- Produces: `feast-watch-agent` binary (reads `/etc/feast-watch/agent.conf`, registers all collectors conditionally on config presence, runs `Loop.Run` with `SelfUpdate` as the desired-version callback); `feast-watch` mother binary (env config: `FW_DB_PATH`, `FW_LISTEN`, `FW_PUBLIC_ADDR`, `FW_API_KEY`, `FW_DOWNLOADS_DIR`, `FW_TLS_CERT`, `FW_TLS_KEY`; subcommand `generate` → `RunGenerate`; starts HTTP(S) server + rollup ticker (30s) + retention ticker (1h)).

- [ ] **Step 1: Write agent main**

`agent/cmd/feast-watch-agent/main.go`:

```go
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"

	"github.com/osman-yahya/feast-watch/agent"
	"github.com/osman-yahya/feast-watch/agent/collectors"
)

func main() {
	confPath := flag.String("config", "/etc/feast-watch/agent.conf", "config file path")
	flag.Parse()

	cfg, err := agent.LoadConfig(*confPath)
	if err != nil {
		slog.Error("config", "err", err)
		os.Exit(1)
	}

	reg := collectors.NewRegistry()
	reg.Register(collectors.NewCPU())
	reg.Register(collectors.NewMemory())
	reg.Register(collectors.NewUptime())
	reg.Register(collectors.NewDisk())
	// Service collectors register only when configured; the mother's enabled
	// list decides whether they actually run.
	if cfg.CentrifugoAPIURL != "" {
		reg.Register(collectors.NewCentrifugo(cfg.CentrifugoAPIURL, cfg.CentrifugoAPIKey, cfg.CentrifugoConnsMax))
	}
	if cfg.DragonflyAddr != "" {
		reg.Register(collectors.NewDragonfly(cfg.DragonflyAddr))
	}
	if cfg.PostgresDSN != "" {
		reg.Register(collectors.NewPostgres(cfg.PostgresDSN))
	}
	if cfg.K8sAPIURL != "" {
		reg.Register(collectors.NewK8s(cfg.K8sAPIURL, cfg.K8sToken))
	}

	loop := agent.NewLoop(cfg, reg)
	loop.Run(context.Background(), func(desired string) {
		slog.Info("self-update requested", "desired", desired)
		if err := agent.SelfUpdate(cfg, desired, os.Exit); err != nil {
			slog.Error("self-update failed", "err", err)
		}
	})
}
```

- [ ] **Step 2: Write mother main**

`mother/cmd/feast-watch/main.go`:

```go
package main

import (
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/osman-yahya/feast-watch/mother"
	"github.com/osman-yahya/feast-watch/mother/api"
	"github.com/osman-yahya/feast-watch/mother/store"
)

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func main() {
	st, err := store.Open(env("FW_DB_PATH", "/var/lib/feast-watch/mother.db"))
	if err != nil {
		slog.Error("open store", "err", err)
		os.Exit(1)
	}
	defer st.Close()

	publicAddr := env("FW_PUBLIC_ADDR", "127.0.0.1:8443")

	// `feast-watch generate --name=X` — CLI alternative to the panel's Add Server.
	if len(os.Args) > 1 && os.Args[1] == "generate" {
		out, err := mother.RunGenerate(st, publicAddr, os.Args[2:])
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println(out)
		return
	}

	apiKey := os.Getenv("FW_API_KEY")
	if apiKey == "" {
		slog.Error("FW_API_KEY is required")
		os.Exit(1)
	}
	a := api.New(st, apiKey, env("FW_DOWNLOADS_DIR", "/var/lib/feast-watch/downloads"))
	a.SetPublicAddr(publicAddr)

	go func() { // rollup every 30s over the last 10 minutes (idempotent REPLACE)
		for range time.Tick(30 * time.Second) {
			if err := st.RollupSince(time.Now().Unix() - 600); err != nil {
				slog.Error("rollup", "err", err)
			}
		}
	}()
	go func() { // retention hourly
		for range time.Tick(time.Hour) {
			cfg, err := st.GetSettings()
			if err == nil {
				err = st.EnforceRetention(time.Now().Unix(), cfg)
			}
			if err != nil {
				slog.Error("retention", "err", err)
			}
		}
	}()

	listen := env("FW_LISTEN", ":8443")
	cert, key := os.Getenv("FW_TLS_CERT"), os.Getenv("FW_TLS_KEY")
	slog.Info("mother listening", "addr", listen, "tls", cert != "")
	if cert != "" {
		err = http.ListenAndServeTLS(listen, cert, key, a.Handler())
	} else {
		err = http.ListenAndServe(listen, a.Handler())
	}
	slog.Error("server stopped", "err", err)
	os.Exit(1)
}
```

- [ ] **Step 3: Verify both binaries build statically**

Run: `CGO_ENABLED=0 go build ./... && CGO_ENABLED=0 go vet ./...`
Expected: clean build, no vet findings

- [ ] **Step 4: Run the full test suite**

Run: `go test ./...`
Expected: PASS (all packages)

- [ ] **Step 5: Commit**

```bash
git add agent/cmd/ mother/cmd/
git commit -m "feat: add agent and mother binaries with background jobs"
```

---

### Task 18: deploy assets + local compose + e2e

**Files:**
- Create: `deploy/feast-watch-agent.service`, `deploy/k8s/daemonset.yaml`, `docker-compose.yml`, `e2e/e2e_test.sh`, `Dockerfile.agent`, `Dockerfile.mother`, `.gitignore`, `README.md`, `QUICKSTART.md`, `.env.example`, `.dockerignore`
- Test: `e2e/e2e_test.sh` (compose-driven)

**Interfaces:**
- Consumes: both binaries (Task 17), install template (Task 13).
- Produces: a `docker compose up`-able local stack (mother + 2 agents) and an e2e script asserting: servers appear online, chart returns rollup points, settings change propagates.

- [ ] **Step 1: Write deploy assets**

`deploy/feast-watch-agent.service` (same unit the install script writes — kept in-repo as the canonical copy):

```ini
[Unit]
Description=feast-watch agent
After=network-online.target

[Service]
ExecStart=/usr/local/bin/feast-watch-agent
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
```

`deploy/k8s/daemonset.yaml`:

```yaml
apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: feast-watch-agent
  namespace: kube-system
spec:
  selector:
    matchLabels: { app: feast-watch-agent }
  template:
    metadata:
      labels: { app: feast-watch-agent }
    spec:
      hostPID: true
      containers:
        - name: agent
          image: feast-watch-agent:latest # updates via image tag, not self-update
          args: ["-config", "/etc/feast-watch/agent.conf"]
          volumeMounts:
            - { name: proc, mountPath: /host/proc, readOnly: true }
            - { name: conf, mountPath: /etc/feast-watch, readOnly: true }
          resources:
            limits: { memory: 64Mi, cpu: 100m }
      volumes:
        - name: proc
          hostPath: { path: /proc }
        - name: conf
          secret: { secretName: feast-watch-agent-conf }
```

`Dockerfile.mother`:

```dockerfile
FROM golang:1.22-alpine AS build
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -o /out/feast-watch ./mother/cmd/feast-watch

FROM alpine:3.20
COPY --from=build /out/feast-watch /usr/local/bin/feast-watch
ENTRYPOINT ["feast-watch"]
```

`Dockerfile.agent`:

```dockerfile
FROM golang:1.22-alpine AS build
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -o /out/feast-watch-agent ./agent/cmd/feast-watch-agent

FROM alpine:3.20
COPY --from=build /out/feast-watch-agent /usr/local/bin/feast-watch-agent
ENTRYPOINT ["feast-watch-agent", "-config", "/etc/feast-watch/agent.conf"]
```

`docker-compose.yml` (local development only — production uses systemd):

```yaml
services:
  mother:
    build: { context: ., dockerfile: Dockerfile.mother }
    environment:
      FW_DB_PATH: /data/mother.db
      FW_LISTEN: ":8443"
      FW_PUBLIC_ADDR: "mother:8443"
      FW_API_KEY: dev-api-key
      FW_DOWNLOADS_DIR: /data/downloads
    volumes: [ "mother-data:/data" ]
    ports: [ "8443:8443" ]

  agent-1:
    build: { context: ., dockerfile: Dockerfile.agent }
    depends_on: [ mother ]
    volumes: [ "./e2e/agent-1.conf:/etc/feast-watch/agent.conf:ro" ]

  agent-2:
    build: { context: ., dockerfile: Dockerfile.agent }
    depends_on: [ mother ]
    volumes: [ "./e2e/agent-2.conf:/etc/feast-watch/agent.conf:ro" ]

volumes:
  mother-data:
```

`.env.example`:

```bash
FW_DB_PATH=/var/lib/feast-watch/mother.db
FW_LISTEN=:8443
FW_PUBLIC_ADDR=10.0.0.1:8443
FW_API_KEY=change-me
FW_DOWNLOADS_DIR=/var/lib/feast-watch/downloads
FW_TLS_CERT=
FW_TLS_KEY=
```

- [ ] **Step 2: Write the e2e script**

`e2e/e2e_test.sh`:

```bash
#!/usr/bin/env bash
# End-to-end: mother + 2 agents via compose; asserts push → status → rollup → chart.
set -euo pipefail
cd "$(dirname "$0")/.."

API="http://localhost:8443"
KEY="X-API-Key: dev-api-key"

cleanup() { docker compose down -v >/dev/null 2>&1 || true; }
trap cleanup EXIT

docker compose up -d --build mother
sleep 2

echo "-> add two servers via admin API"
TOKEN1=$(curl -sf -H "$KEY" -X POST "$API/api/servers" -d '{"name":"e2e-1"}' | python3 -c 'import sys,json;print(json.load(sys.stdin)["data"]["server"]["Token"])')
TOKEN2=$(curl -sf -H "$KEY" -X POST "$API/api/servers" -d '{"name":"e2e-2"}' | python3 -c 'import sys,json;print(json.load(sys.stdin)["data"]["server"]["Token"])')

mkdir -p e2e
printf 'MOTHER_URL=http://mother:8443\nTOKEN=%s\nSERVER_NAME=e2e-1\n' "$TOKEN1" > e2e/agent-1.conf
printf 'MOTHER_URL=http://mother:8443\nTOKEN=%s\nSERVER_NAME=e2e-2\n' "$TOKEN2" > e2e/agent-2.conf

docker compose up -d --build agent-1 agent-2
echo "-> waiting for pushes"
sleep 15

echo "-> both servers must be online"
STATUSES=$(curl -sf -H "$KEY" "$API/api/servers" | python3 -c 'import sys,json;print(",".join(sorted(s["status"] for s in json.load(sys.stdin)["data"])))')
[ "$STATUSES" = "online,online" ] || { echo "FAIL: statuses=$STATUSES"; exit 1; }

echo "-> chart must return rollup points (never raw)"
SID=$(curl -sf -H "$KEY" "$API/api/servers" | python3 -c 'import sys,json;print(json.load(sys.stdin)["data"][0]["id"])')
sleep 45  # let the 30s rollup ticker fire
NOW=$(date +%s)
POINTS=$(curl -sf -H "$KEY" "$API/api/chart?server_id=$SID&metric=cpu.usage&from=$((NOW-600))&to=$NOW&interval=60" \
  | python3 -c 'import sys,json;print(len(json.load(sys.stdin)["data"]))')
[ "$POINTS" -ge 1 ] || { echo "FAIL: no chart points"; exit 1; }

echo "E2E PASS"
```

```bash
chmod +x e2e/e2e_test.sh
```

- [ ] **Step 3: Write README.md, QUICKSTART.md, .gitignore, .dockerignore**

`.gitignore`:

```
/e2e/agent-*.conf
/downloads/
*.db
```

`.dockerignore`:

```
.git
docs
e2e/agent-*.conf
```

`README.md` — brief: what feast-watch is (3 sentences from the spec's Purpose), the architecture ASCII diagram from the spec, links to `docs/superpowers/specs/2026-07-16-feast-watch-design.md` and `QUICKSTART.md`.

`QUICKSTART.md` — local dev flow: `docker compose up -d --build`, add a server via `curl -X POST .../api/servers`, run `./e2e/e2e_test.sh`; production flow: build binaries, run mother with env from `.env.example`, panel/CLI Add Server, paste the one-liner on the target.

- [ ] **Step 4: Run the e2e**

Run: `./e2e/e2e_test.sh`
Expected: `E2E PASS`

- [ ] **Step 5: Run full suite + coverage gate**

Run: `go test ./... -cover`
Expected: PASS; core packages (`shared/...`, `agent/...`, `mother/...`) each ≥ 80% coverage. If any package is below, add table-driven cases for the uncovered branches before closing the task.

- [ ] **Step 6: Commit**

```bash
git add deploy/ docker-compose.yml e2e/ Dockerfile.* .gitignore .dockerignore README.md QUICKSTART.md .env.example
git commit -m "feat: add deploy assets, local compose stack, and e2e test"
```

---

## Verification (after all tasks)

- [ ] `CGO_ENABLED=0 go build ./...` — both binaries build statically.
- [ ] `go test ./... -cover` — all green, ≥80% on core packages.
- [ ] `./e2e/e2e_test.sh` — full push → rollup → chart cycle passes.
- [ ] Resource budget check: run the agent binary on a host for 5 minutes under `time -l` / `ps -o rss,pcpu`; RSS < 30 MB, CPU < 1%. Record numbers in the PR description.
- [ ] Grep gate for the spec's hard rule: `grep -rn "FROM samples" mother/api/` must return **nothing** (chart never touches raw).

## Follow-up plans (separate repos, after this ships)

1. `feast-mobile-backend`: proxy endpoints (`/admin/monitoring/*` → mother `/api/*` with `FW_API_KEY`), RBAC gating.
2. `feast-mobile-backend-control`: Servers page (list + status + versions + Add Server dialog showing the install command), Settings page, per-server collector toggles, chart components calling the proxy with `range`/`interval` params.
