//go:build windows

package cache

import (
	"fmt"
	"os"
	"strconv"
	"syscall"
	"testing"
	"unsafe"
)

// TestMain optionally pins the whole benchmark process to a fixed set of
// logical processors before any benchmark runs. Set INMEMCACHE_AFFINITY to a
// processor-affinity mask (hex like 0x5555 or decimal) to confine the process;
// on a hybrid CPU this is how you keep the scaling sweep on physical P-cores
// instead of letting the OS spill goroutines onto E-cores or HT siblings.
//
// Find the right mask with `go run ./cmd/cpuinfo`. On the i7-14700K used for
// the published numbers, 0x5555 selects one thread on each of the 8 physical
// P-cores. Setting affinity from inside the process (rather than via an
// external launcher) keeps the bash sweep runner — which streams reliably —
// as the single execution path.
func TestMain(m *testing.M) {
	if v := os.Getenv("INMEMCACHE_AFFINITY"); v != "" {
		if mask, err := strconv.ParseUint(v, 0, 64); err == nil && mask != 0 {
			k := syscall.NewLazyDLL("kernel32.dll")
			cur, _, _ := k.NewProc("GetCurrentProcess").Call()
			r, _, _ := k.NewProc("SetProcessAffinityMask").Call(cur, uintptr(mask))
			// Read it back so the run log proves the pin actually took.
			var got, sys uintptr
			k.NewProc("GetProcessAffinityMask").Call(cur,
				uintptr(unsafe.Pointer(&got)), uintptr(unsafe.Pointer(&sys)))
			fmt.Fprintf(os.Stderr, "[affinity] requested=0x%X set_ok=%v effective=0x%X\n", mask, r != 0, got)
		}
	}
	os.Exit(m.Run())
}
