// Package collectors defines metric collectors. Only collectors named in the
// mother's ingest response are ever executed.
package collectors

import (
	"context"
	"log/slog"
	"sort"
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

// Names lists the registered collectors, sorted. This is the agent's
// capability set: service collectors register only when agent.conf configures
// them, so this is the only place that knows a host cannot run, say, the
// dragonfly collector. Sorted because the result is persisted and compared —
// map iteration order must not make an unchanged agent look changed.
func (r *Registry) Names() []string {
	names := make([]string, 0, len(r.byName))
	for name := range r.byName {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
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
