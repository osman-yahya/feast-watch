package collectors

import (
	"context"
	"testing"

	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/host"
	"github.com/shirou/gopsutil/v4/mem"
)

// Baseline benchmarks for the base collector set — the four collectors every
// host runs (cpu, memory, uptime, disk). These are the "cost per sample" the
// agent pays on every tick regardless of which services a host is configured
// for, so they are the reference point for any overhead-reduction work.
//
// PLATFORM WARNING: cpu/memory/host read Linux procfs (/proc/stat,
// /proc/meminfo, /proc/vmstat, /proc/uptime) in production. On darwin gopsutil
// takes an entirely different path (sysctl/host_statistics64 mach calls), so
// numbers produced here describe the darwin path only and are NOT evidence
// about Linux. Registry dispatch overhead (BenchmarkRegistryDispatchOnly) is
// the only figure here that is platform-independent.

// benchCollectors is the base set: exactly what an unconfigured host runs.
func benchCollectors() []Collector {
	return []Collector{NewCPU(), NewMemory(), NewUptime(), NewDisk()}
}

// BenchmarkCollector times each base collector individually, live (no fakes) —
// this is the real syscall / file-read cost per sample.
func BenchmarkCollector(b *testing.B) {
	ctx := context.Background()
	for _, c := range benchCollectors() {
		b.Run(c.Name(), func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := c.Collect(ctx); err != nil {
					b.Fatalf("%s: %v", c.Name(), err)
				}
			}
		})
	}
}

// BenchmarkMemorySources splits the memory collector into its two underlying
// reads. On Linux mem.VirtualMemory reads /proc/meminfo and mem.SwapMemory
// reads /proc/meminfo *plus* the whole of /proc/vmstat for pswpin/pswpout
// counters this collector discards — this benchmark is where that second read
// shows up.
func BenchmarkMemorySources(b *testing.B) {
	b.Run("VirtualMemory", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if _, err := mem.VirtualMemory(); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("SwapMemory", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if _, err := mem.SwapMemory(); err != nil {
				b.Fatal(err)
			}
		}
	})
}

// BenchmarkHostSources times the two host-level reads behind uptime and disk.
func BenchmarkHostSources(b *testing.B) {
	b.Run("Uptime", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if _, err := host.Uptime(); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("DiskUsageRoot", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if _, err := disk.Usage("/"); err != nil {
				b.Fatal(err)
			}
		}
	})
}

// BenchmarkCollectEnabled measures a whole tick through the real entrypoint:
// Registry.CollectEnabled over the base set, sequentially, as Run calls it.
func BenchmarkCollectEnabled(b *testing.B) {
	ctx := context.Background()
	r := NewRegistry()
	for _, c := range benchCollectors() {
		r.Register(c)
	}
	enabled := r.Names()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		out := r.CollectEnabled(ctx, enabled)
		if len(out) == 0 {
			b.Fatal("no samples collected")
		}
	}
}

// BenchmarkCollectEnabledSubsets shows how the tick cost scales as collectors
// are enabled, so a per-collector saving can be read against the whole tick.
func BenchmarkCollectEnabledSubsets(b *testing.B) {
	ctx := context.Background()
	r := NewRegistry()
	for _, c := range benchCollectors() {
		r.Register(c)
	}
	cases := []struct {
		name    string
		enabled []string
	}{
		{"cpu", []string{"cpu"}},
		{"cpu+memory", []string{"cpu", "memory"}},
		{"cpu+memory+uptime", []string{"cpu", "memory", "uptime"}},
		{"base4", []string{"cpu", "memory", "uptime", "disk"}},
	}
	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if out := r.CollectEnabled(ctx, tc.enabled); len(out) == 0 {
					b.Fatal("no samples collected")
				}
			}
		})
	}
}

// BenchmarkRegistryDispatchOnly isolates the registry's own cost (map lookup,
// result map build) from the collectors' I/O by using no-op fakes. Whatever
// this costs is the floor CollectEnabled can never go below — and unlike every
// other benchmark in this file it is platform-independent.
func BenchmarkRegistryDispatchOnly(b *testing.B) {
	ctx := context.Background()
	r := NewRegistry()
	for _, name := range []string{"cpu", "memory", "uptime", "disk"} {
		r.Register(&fake{name: name, samples: []Sample{{Key: name + ".x", Value: 1}}})
	}
	enabled := r.Names()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if out := r.CollectEnabled(ctx, enabled); len(out) != 4 {
			b.Fatalf("got %d samples", len(out))
		}
	}
}
