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
