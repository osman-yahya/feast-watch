//go:build linux

package collectors

import "github.com/shirou/gopsutil/v4/mem"

// On Linux, mem.SwapMemory costs a sysinfo(2) syscall AND a full read of
// /proc/vmstat — a seq_file the kernel materialises line by line on every open,
// hundreds of lines long — purely to fill in pswpin/pswpout/pgfault counters
// that this collector discards. The only field it uses, UsedPercent, is
// computable from the SwapTotal/SwapFree that the /proc/meminfo read done for
// mem.used_pct has already returned. So on the hosts that actually run this
// agent, a memory sample is now one file read instead of two plus a syscall.
func swapUsedPercent(v *mem.VirtualMemoryStat) (float64, error) {
	return swapPctFromMeminfo(v)
}
