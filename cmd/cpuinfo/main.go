//go:build windows

// Command cpuinfo reports the performance/efficiency core split of a hybrid
// Windows CPU and prints processor-affinity masks for pinning a benchmark to
// the P-cores. It reads the kernel's CPU-set information (EfficiencyClass);
// the highest efficiency class is the performance (P) cores.
//
//	go run ./cmd/cpuinfo
package main

import (
	"fmt"
	"sort"
	"syscall"
	"unsafe"
)

func main() {
	k := syscall.NewLazyDLL("kernel32.dll")
	getInfo := k.NewProc("GetSystemCpuSetInformation")
	getCur := k.NewProc("GetCurrentProcess")
	cur, _, _ := getCur.Call()

	var n uint32
	getInfo.Call(0, 0, uintptr(unsafe.Pointer(&n)), cur, 0)
	if n == 0 {
		fmt.Println("GetSystemCpuSetInformation returned no length")
		return
	}
	buf := make([]byte, n)
	r1, _, err := getInfo.Call(uintptr(unsafe.Pointer(&buf[0])), uintptr(n), uintptr(unsafe.Pointer(&n)), cur, 0)
	if r1 == 0 {
		fmt.Println("GetSystemCpuSetInformation failed:", err)
		return
	}

	type lp struct {
		logical, core int
		eff           byte
	}
	var lps []lp
	for off := 0; off < int(n); {
		size := *(*uint32)(unsafe.Pointer(&buf[off]))
		typ := *(*uint32)(unsafe.Pointer(&buf[off+4]))
		if size == 0 {
			break
		}
		if typ == 0 { // CpuSetInformation
			lps = append(lps, lp{
				logical: int(buf[off+14]),
				core:    int(buf[off+15]),
				eff:     buf[off+18],
			})
		}
		off += int(size)
	}

	var maxEff byte
	for _, p := range lps {
		if p.eff > maxEff {
			maxEff = p.eff
		}
	}
	primaryByCore := map[int]int{}
	var pLog, eLog []int
	for _, p := range lps {
		if p.eff == maxEff {
			pLog = append(pLog, p.logical)
			if cur, ok := primaryByCore[p.core]; !ok || p.logical < cur {
				primaryByCore[p.core] = p.logical
			}
		} else {
			eLog = append(eLog, p.logical)
		}
	}
	var primaries []int
	for _, v := range primaryByCore {
		primaries = append(primaries, v)
	}
	sort.Ints(pLog)
	sort.Ints(eLog)
	sort.Ints(primaries)

	mask := func(ids []int) uint64 {
		var m uint64
		for _, l := range ids {
			m |= 1 << uint(l)
		}
		return m
	}

	fmt.Printf("logical processors:        %d\n", len(lps))
	fmt.Printf("P-core logicals (%2d):       %v\n", len(pLog), pLog)
	fmt.Printf("E-core logicals (%2d):       %v\n", len(eLog), eLog)
	fmt.Printf("physical P-cores:          %d\n", len(primaries))
	fmt.Printf("one-thread-per-P-core:     %v\n", primaries)
	fmt.Printf("AFFINITY mask (1/P-core):  0x%X = %d\n", mask(primaries), mask(primaries))
	fmt.Printf("affinity mask (all P incl HT): 0x%X = %d\n", mask(pLog), mask(pLog))
}
