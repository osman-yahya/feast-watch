//go:build linux

package collectors

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/shirou/gopsutil/v4/mem"
)

// A representative /proc/meminfo. MemAvailable is present (kernel 3.14+), which
// is what makes gopsutil's VirtualMemory a single-file read: without it there
// is a /proc/zoneinfo fallback, and this fixture deliberately stays on the
// modern path that production hosts are on.
//
//	mem.used_pct      = (MemTotal-MemAvailable)/MemTotal = 8192000/16384000 = 50%
//	mem.swap_used_pct = (SwapTotal-SwapFree)/SwapTotal   = 1048576/4194304 = 25%
const fakeMeminfo = `MemTotal:       16384000 kB
MemFree:         2000000 kB
MemAvailable:    8192000 kB
Buffers:          100000 kB
Cached:          6000000 kB
SwapCached:        12345 kB
Active:          7000000 kB
Inactive:        4000000 kB
SwapTotal:       4194304 kB
SwapFree:        3145728 kB
Dirty:               512 kB
Writeback:             0 kB
Shmem:            300000 kB
Slab:             400000 kB
SReclaimable:     250000 kB
SUnreclaim:       150000 kB
PageTables:        30000 kB
CommitLimit:    12386304 kB
Committed_AS:    5000000 kB
VmallocTotal:   34359738367 kB
VmallocUsed:       40000 kB
VmallocChunk:          0 kB
HugePages_Total:       0
HugePages_Free:        0
Hugepagesize:       2048 kB
`

// fakeProc builds a $HOST_PROC holding meminfo and NOTHING else. The absence of
// every other file is the assertion: gopsutil returns the os.Open error to its
// caller, so any code path that still reaches for /proc/vmstat fails the sample
// loudly. (Absence, not a poisoned file — gopsutil's ReadLines propagates open
// errors but silently swallows read errors, so an unreadable file or a
// directory in vmstat's place would go undetected, and a 0000-mode file is not
// poison at all when the tests run as root, which they do in a container.)
func fakeProc(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "meminfo"), []byte(fakeMeminfo), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// The memory collector must satisfy a whole sample from one /proc/meminfo read.
// mem.SwapMemory additionally slurps the whole of /proc/vmstat for pswpin /
// pswpout / pgfault counters this collector throws away — a second seq_file the
// kernel materialises on every sample, for nothing.
func TestMemoryCollectReadsOnlyMeminfoNeverVmstat(t *testing.T) {
	t.Setenv("HOST_PROC", fakeProc(t))

	got, err := NewMemory().Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect reached for a proc file other than meminfo: %v", err)
	}
	byKey := map[string]float64{}
	for _, s := range got {
		byKey[s.Key] = s.Value
	}
	if byKey["mem.used_pct"] != 50 {
		t.Errorf("mem.used_pct = %v, want 50", byKey["mem.used_pct"])
	}
	if byKey["mem.swap_used_pct"] != 25 {
		t.Errorf("mem.swap_used_pct = %v, want 25", byKey["mem.swap_used_pct"])
	}
}

// The other half of "exactly one file, and it is meminfo": take meminfo away and
// the sample must fail, i.e. the numbers really do come from that file.
func TestMemoryCollectRequiresMeminfo(t *testing.T) {
	dir := fakeProc(t)
	if err := os.Remove(filepath.Join(dir, "meminfo")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOST_PROC", dir)

	if _, err := NewMemory().Collect(context.Background()); err == nil {
		t.Fatal("Collect succeeded with no meminfo — the sample is not coming from the file it claims to")
	}
}

// The equivalence argument for change C, checked against a real kernel rather
// than asserted: whatever this host's swap state is, the value derived from
// /proc/meminfo must be the value mem.SwapMemory would have reported. They come
// from one si_swapinfo() inside the kernel, so an exact match is expected — but
// the two are sampled a moment apart, so on a host that is actively paging they
// can legitimately disagree by that moment's worth of drift. Hence: several
// attempts, at least one of which must agree exactly, and the underlying
// Total/Free bytes are compared too so a real semantic divergence cannot hide
// behind a percentage that happens to round the same.
func TestSwapPctFromMeminfoMatchesSwapMemoryOnThisHost(t *testing.T) {
	var lastErr string
	for attempt := 0; attempt < 5; attempt++ {
		v, err := mem.VirtualMemory()
		if err != nil {
			t.Fatal(err)
		}
		s, err := mem.SwapMemory()
		if err != nil {
			t.Fatal(err)
		}
		got, err := swapPctFromMeminfo(v)
		if err != nil {
			t.Fatal(err)
		}
		if v.SwapTotal == s.Total && v.SwapFree == s.Free && got == s.UsedPercent {
			t.Logf("agreed exactly: SwapTotal=%d SwapFree=%d pct=%v", v.SwapTotal, v.SwapFree, got)
			return
		}
		lastErr = fmt.Sprintf(
			"meminfo{Total:%d Free:%d pct:%v} vs SwapMemory{Total:%d Free:%d pct:%v}",
			v.SwapTotal, v.SwapFree, got, s.Total, s.Free, s.UsedPercent)
	}
	t.Fatalf("meminfo-derived swap never matched mem.SwapMemory — the metric would change meaning: %s", lastErr)
}
