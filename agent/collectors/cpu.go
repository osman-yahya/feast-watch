package collectors

import (
	"context"
	"time"

	"github.com/shirou/gopsutil/v4/cpu"
)

type CPU struct {
	percent func(time.Duration, bool) ([]float64, error)
}

func NewCPU() *CPU { return &CPU{percent: cpu.Percent} }

func (c *CPU) Name() string { return "cpu" }

func (c *CPU) Collect(ctx context.Context) ([]Sample, error) {
	// interval 0 = non-blocking delta since previous call; never sleeps the loop.
	vals, err := c.percent(0, false)
	if err != nil || len(vals) == 0 {
		return nil, err
	}
	return []Sample{{Key: "cpu.usage", Value: vals[0]}}, nil
}
