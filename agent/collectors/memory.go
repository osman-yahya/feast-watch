package collectors

import (
	"context"

	"github.com/shirou/gopsutil/v4/mem"
)

// Memory reports RAM and swap together — headroom is only visible as a pair.
type Memory struct {
	virtual func() (*mem.VirtualMemoryStat, error)
	swap    func() (*mem.SwapMemoryStat, error)
}

func NewMemory() *Memory {
	return &Memory{virtual: mem.VirtualMemory, swap: mem.SwapMemory}
}

func (m *Memory) Name() string { return "memory" }

func (m *Memory) Collect(ctx context.Context) ([]Sample, error) {
	v, err := m.virtual()
	if err != nil {
		return nil, err
	}
	s, err := m.swap()
	if err != nil {
		return nil, err
	}
	return []Sample{
		{Key: "mem.used_pct", Value: v.UsedPercent},
		{Key: "mem.swap_used_pct", Value: s.UsedPercent},
	}, nil
}
