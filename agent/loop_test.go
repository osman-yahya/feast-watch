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

// Capabilities travel with the identity fields: they change only when
// agent.conf changes, which requires a restart, and a restart replays the
// first push. Sending them every cycle would be pure overhead.
func TestFirstPushReportsCapabilities(t *testing.T) {
	var reqs []protocol.IngestRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req protocol.IngestRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
		}
		reqs = append(reqs, req)
		json.NewEncoder(w).Encode(protocol.IngestResponse{Interval: 10})
	}))
	defer srv.Close()

	reg := collectors.NewRegistry()
	reg.Register(&stub{name: "cpu", key: "cpu.usage", val: 1})
	reg.Register(&stub{name: "postgres", key: "postgres.conns", val: 5})

	l := NewLoop(Config{MotherURL: srv.URL, Token: "tk_abc", ServerName: "s1"}, reg)
	if _, err := l.PushOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := l.PushOnce(context.Background()); err != nil {
		t.Fatal(err)
	}

	if got := reqs[0].Capabilities; len(got) != 2 || got[0] != "cpu" || got[1] != "postgres" {
		t.Fatalf("first push capabilities = %v, want [cpu postgres]", got)
	}
	if got := reqs[1].Capabilities; len(got) != 0 {
		t.Fatalf("second push must omit capabilities, got %v", got)
	}
}
