package collectors

import (
	"context"

	"github.com/shirou/gopsutil/v4/mem"
)

// Memory reports RAM and swap together — headroom is only visible as a pair.
//
// One sample is one source. mem.VirtualMemory already reads every number this
// collector needs, swap included, so swapPct derives the swap figure from that
// same snapshot rather than going back to the kernel for it (see
// swapPctFromMeminfo). Both numbers therefore describe the same instant, which
// the two-source version could not promise either.
type Memory struct {
	virtual func() (*mem.VirtualMemoryStat, error)
	swapPct func(*mem.VirtualMemoryStat) (float64, error)
}

func NewMemory() *Memory {
	return &Memory{virtual: mem.VirtualMemory, swapPct: swapUsedPercent}
}

func (m *Memory) Name() string { return "memory" }

func (m *Memory) Collect(ctx context.Context) ([]Sample, error) {
	v, err := m.virtual()
	if err != nil {
		return nil, err
	}
	swap, err := m.swapPct(v)
	if err != nil {
		return nil, err
	}
	return []Sample{
		{Key: "mem.used_pct", Value: v.UsedPercent},
		{Key: "mem.swap_used_pct", Value: swap},
	}, nil
}

// swapPctFromMeminfo computes swap utilisation from the SwapTotal / SwapFree
// fields that /proc/meminfo already supplied.
//
// This is deliberately the identical arithmetic to gopsutil's own
// SwapMemoryWithContext — (Total-Free)/Total*100, and 0 rather than NaN when
// there is no swap — because the metric must keep the exact meaning and the
// exact value it had before, and most hosts here run with swap off, so the
// zero-total branch is the common path.
//
// The two are the same quantity, not merely similar: the kernel answers both
// /proc/meminfo and sysinfo(2) out of one si_swapinfo(), meminfo printing the
// numbers in kB and sysinfo scaling the same ones by mem_unit.
func swapPctFromMeminfo(v *mem.VirtualMemoryStat) (float64, error) {
	if v == nil || v.SwapTotal == 0 {
		return 0, nil
	}
	if v.SwapFree >= v.SwapTotal {
		return 0, nil // unsigned subtraction must not wrap into a nonsense percentage
	}
	return float64(v.SwapTotal-v.SwapFree) / float64(v.SwapTotal) * 100.0, nil
}
