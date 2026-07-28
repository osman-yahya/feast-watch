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

// The mother cannot know which service collectors a host is configured for —
// only the agent does, since service collectors register conditionally on
// agent.conf. Names() is how that capability set reaches the wire.
func TestRegistryNamesReportsRegisteredCollectors(t *testing.T) {
	r := NewRegistry()
	r.Register(&fake{name: "memory"})
	r.Register(&fake{name: "cpu"})
	r.Register(&fake{name: "dragonfly"})

	got := r.Names()
	want := []string{"cpu", "dragonfly", "memory"}
	if len(got) != len(want) {
		t.Fatalf("Names() = %v, want %v", got, want)
	}
	// Sorted: the list is persisted and compared downstream, so map iteration
	// order must not make an unchanged agent look like it changed.
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Names() = %v, want %v (sorted)", got, want)
		}
	}
}

func TestRegistryNamesEmptyWhenNothingRegistered(t *testing.T) {
	if got := NewRegistry().Names(); len(got) != 0 {
		t.Fatalf("Names() on empty registry = %v, want empty", got)
	}
}
