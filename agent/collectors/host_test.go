package collectors

import (
	"context"
	"testing"

	"github.com/shirou/gopsutil/v4/disk"
)

func TestUptimeCollect(t *testing.T) {
	u := NewUptime()
	u.uptime = func() (uint64, error) { return 864211, nil }
	got, err := u.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Key != "uptime_s" || got[0].Value != 864211 {
		t.Fatalf("got %+v", got)
	}
}

func TestDiskCollectsUsagePercentOnly(t *testing.T) {
	d := NewDisk()
	d.usage = func(path string) (*disk.UsageStat, error) {
		return &disk.UsageStat{UsedPercent: 71.0}, nil
	}
	got, err := d.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Key != "disk.used_pct" || got[0].Value != 71.0 {
		t.Fatalf("disk must report space %% only, got %+v", got)
	}
}
