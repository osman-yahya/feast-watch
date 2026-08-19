package collectors

import (
	"context"
	"math"
	"testing"

	"github.com/shirou/gopsutil/v4/mem"
)

func TestMemoryCollectsRAMAndSwapTogether(t *testing.T) {
	m := NewMemory()
	m.virtual = func() (*mem.VirtualMemoryStat, error) {
		return &mem.VirtualMemoryStat{UsedPercent: 61.5}, nil
	}
	m.swapPct = func(*mem.VirtualMemoryStat) (float64, error) { return 2.1, nil }
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

// mem.swap_used_pct must keep exactly the meaning it had when it came from
// mem.SwapMemory: used/total of the swap space, as a percentage. gopsutil
// computes (Total-Free)/Total*100 and pins the result to 0 when Total is 0;
// deriving the same ratio from meminfo's SwapTotal/SwapFree must not change a
// single reported value. The kernel fills both from one si_swapinfo() call —
// /proc/meminfo prints those numbers in kB and sysinfo(2) scales the same ones
// by mem_unit — so this is the same quantity, not an approximation of it.
func TestSwapPctFromMeminfoMatchesGopsutilSemantics(t *testing.T) {
	const kB = 1024
	tests := []struct {
		name             string
		swapTotal        uint64
		swapFree         uint64
		want             float64
		wantGopsutilSame bool
	}{
		{name: "quarter used", swapTotal: 4194304 * kB, swapFree: 3145728 * kB, want: 25, wantGopsutilSame: true},
		{name: "untouched swap", swapTotal: 2097152 * kB, swapFree: 2097152 * kB, want: 0, wantGopsutilSame: true},
		{name: "swap exhausted", swapTotal: 1048576 * kB, swapFree: 0, want: 100, wantGopsutilSame: true},
		{name: "typical light paging", swapTotal: 8388608 * kB, swapFree: 8283750 * kB, want: 1.2500047683715820, wantGopsutilSame: true},
		// The case that must not produce NaN or +Inf: a host with swap off at
		// all. Every Hetzner/K3S node in this fleet is in this state, so it is
		// the common path, not an edge case.
		{name: "no swap configured", swapTotal: 0, swapFree: 0, want: 0, wantGopsutilSame: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := &mem.VirtualMemoryStat{SwapTotal: tt.swapTotal, SwapFree: tt.swapFree}
			got, err := swapPctFromMeminfo(v)
			if err != nil {
				t.Fatal(err)
			}
			if math.IsNaN(got) || math.IsInf(got, 0) {
				t.Fatalf("swap_used_pct = %v — a host with no swap must not divide by zero", got)
			}
			if got != tt.want {
				t.Fatalf("swap_used_pct = %v, want %v", got, tt.want)
			}
			if tt.wantGopsutilSame {
				// The exact arithmetic gopsutil's SwapMemoryWithContext does.
				want := 0.0
				if tt.swapTotal != 0 {
					want = float64(tt.swapTotal-tt.swapFree) / float64(tt.swapTotal) * 100.0
				}
				if got != want {
					t.Fatalf("value drifted from gopsutil's own formula: got %v, gopsutil %v", got, want)
				}
			}
		})
	}
}

// SwapFree can never exceed SwapTotal, but these are unsigned and a subtraction
// underflow would report a nonsense ~1.8e19% rather than fail. Clamp instead.
func TestSwapPctFromMeminfoDoesNotUnderflow(t *testing.T) {
	got, err := swapPctFromMeminfo(&mem.VirtualMemoryStat{SwapTotal: 100, SwapFree: 200})
	if err != nil {
		t.Fatal(err)
	}
	if got != 0 {
		t.Fatalf("swap_used_pct = %v, want 0", got)
	}
}
