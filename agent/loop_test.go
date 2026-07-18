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
