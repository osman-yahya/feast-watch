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
