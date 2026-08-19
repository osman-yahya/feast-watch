//go:build !linux

package collectors

import (
	"fmt"
	"testing"

	"github.com/shirou/gopsutil/v4/mem"
)

// The counterpart to memory_procfs_linux_test.go, and the reason
// memory_swap_other.go cannot quietly be deleted or collapsed into the Linux
// implementation.
//
// The dangerous edit is a one-liner: make swapUsedPercent return
// swapPctFromMeminfo(v) on every platform. Off Linux that compiles, reads
// nothing, returns no error, and pins mem.swap_used_pct at a constant 0 forever,
// because SwapTotal/SwapFree are Linux-only fields that mem.VirtualMemory leaves
// at zero everywhere else. Nothing in the Linux-tagged tests can see that, so
// `go test ./...` on a developer's machine would stay green while the metric
// this collector exists to report went dead.
//
// Both tests below therefore assert the same thing from two directions: the
// non-Linux implementation must ignore the *mem.VirtualMemoryStat it is handed
// and ask the OS.

// Poisoned inputs: two VirtualMemoryStats whose *derived* percentages are 25 and
// 75. The derived implementation would return those two different numbers; the
// real one must return the same value for both, because it never looks at them.
var (
	poison25 = &mem.VirtualMemoryStat{SwapTotal: 4 << 30, SwapFree: 3 << 30}
	poison75 = &mem.VirtualMemoryStat{SwapTotal: 4 << 30, SwapFree: 1 << 30}
)

// swapAttempts: the host's real swap usage can genuinely move between two
// consecutive readings, so — like TestSwapPctFromMeminfoMatchesSwapMemoryOnThisHost
// on the Linux side — agreement is required at least once rather than every
// time. The derived implementation can never agree even once: 25 is never 75,
// and a constant 25 is not what the OS reports.
const swapAttempts = 5

func TestSwapUsedPercentIgnoresTheVirtualMemoryStatOffLinux(t *testing.T) {
	var last string
	for attempt := 0; attempt < swapAttempts; attempt++ {
		a, err := swapUsedPercent(poison25)
		if err != nil {
			t.Fatal(err)
		}
		b, err := swapUsedPercent(poison75)
		if err != nil {
			t.Fatal(err)
		}
		if a == b {
			t.Logf("both readings agreed at %v, independent of the stat handed in", a)
			return
		}
		last = fmt.Sprintf("stat{25%% derived} gave %v but stat{75%% derived} gave %v", a, b)
	}
	t.Fatalf("swap_used_pct tracked the VirtualMemoryStat it was handed: %s — off Linux those fields are always zero, so deriving swap from them reports a constant 0 on every non-Linux host", last)
}

func TestSwapUsedPercentComesFromSwapMemoryOffLinux(t *testing.T) {
	var last string
	for attempt := 0; attempt < swapAttempts; attempt++ {
		s, err := mem.SwapMemory()
		if err != nil {
			t.Fatal(err)
		}
		got, err := swapUsedPercent(poison25)
		if err != nil {
			t.Fatal(err)
		}
		if got == s.UsedPercent {
			t.Logf("swap_used_pct = %v, matching mem.SwapMemory on this host", got)
			return
		}
		last = fmt.Sprintf("swapUsedPercent = %v, mem.SwapMemory = %v", got, s.UsedPercent)
	}
	t.Fatalf("swap_used_pct off Linux must be what mem.SwapMemory reports — a separate sysctl there, not a field of the VirtualMemoryStat: %s", last)
}

// The whole collector, not just the helper: Collect must still report a swap
// figure sourced from the OS off Linux.
func TestMemoryCollectReportsOSSwapOffLinux(t *testing.T) {
	got, err := NewMemory().Collect(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	byKey := map[string]float64{}
	for _, s := range got {
		byKey[s.Key] = s.Value
	}
	if _, ok := byKey["mem.swap_used_pct"]; !ok {
		t.Fatal("no mem.swap_used_pct sample")
	}
	s, err := mem.SwapMemory()
	if err != nil {
		t.Fatal(err)
	}
	if s.Total == 0 {
		t.Skipf("this host reports no swap at all (SwapMemory.Total = 0), so 0 is the correct answer and cannot distinguish the two implementations")
	}
	// With swap configured, the derived implementation would report 0 here
	// (SwapTotal is zero off Linux) while the OS reports something real.
	if s.UsedPercent != 0 && byKey["mem.swap_used_pct"] == 0 {
		t.Fatalf("mem.swap_used_pct = 0 while this host's swap is %v%% used — the metric is being derived from Linux-only fields", s.UsedPercent)
	}
}
