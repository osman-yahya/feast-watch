//go:build !linux

package collectors

import "github.com/shirou/gopsutil/v4/mem"

// Everywhere except Linux, mem.VirtualMemory leaves SwapTotal/SwapFree at zero:
// those fields are Linux-only, filled from /proc/meminfo. On darwin, for
// instance, VirtualMemory is host_statistics64 + sysctl and swap comes from a
// separate sysctl vm.swapusage. Deriving swap from the VirtualMemoryStat there
// would silently turn mem.swap_used_pct into a constant 0 on every developer
// machine — a change in what the metric means, dressed up as an optimisation.
//
// So the wasteful-second-read fix is scoped to the platform that has the waste.
// Nothing is lost: on darwin mem.SwapMemory is a single sysctl and reads no
// files at all, which is why there is nothing to save here in the first place.
func swapUsedPercent(*mem.VirtualMemoryStat) (float64, error) {
	s, err := mem.SwapMemory()
	if err != nil {
		return 0, err
	}
	return s.UsedPercent, nil
}
