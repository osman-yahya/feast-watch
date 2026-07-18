package collectors

import (
	"context"

	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/host"
)

type Uptime struct {
	uptime func() (uint64, error)
}

func NewUptime() *Uptime { return &Uptime{uptime: host.Uptime} }

func (u *Uptime) Name() string { return "uptime" }

func (u *Uptime) Collect(ctx context.Context) ([]Sample, error) {
	v, err := u.uptime()
	if err != nil {
		return nil, err
	}
	return []Sample{{Key: "uptime_s", Value: float64(v)}}, nil
}

// Disk reports root filesystem usage percent. Deliberately no I/O rates (spec).
type Disk struct {
	usage func(string) (*disk.UsageStat, error)
}

func NewDisk() *Disk { return &Disk{usage: disk.Usage} }

func (d *Disk) Name() string { return "disk" }

func (d *Disk) Collect(ctx context.Context) ([]Sample, error) {
	u, err := d.usage("/")
	if err != nil {
		return nil, err
	}
	return []Sample{{Key: "disk.used_pct", Value: u.UsedPercent}}, nil
}
