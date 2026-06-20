package cache

import (
	"os"
	"sync"
	"testing"
)

// TestNaiveRace demonstrates that the built-in map (and therefore the Naive
// cache) is not safe for concurrent use. It is SKIPPED by default because it
// is expected to crash the test process:
//
//   - With concurrent writes, the Go runtime detects the unsafe access and
//     deliberately throws a fatal "concurrent map writes" error. This is NOT
//     a panic and cannot be caught with recover(); it tears down the process.
//   - Under `go test -race`, the race detector reports the data race and
//     fails the test even before any fatal throw.
//
// Run it deliberately to see either behavior:
//
//	INMEMCACHE_RACE_DEMO=1 go test -run TestNaiveRace
//	INMEMCACHE_RACE_DEMO=1 go test -race -run TestNaiveRace
func TestNaiveRace(t *testing.T) {
	if os.Getenv("INMEMCACHE_RACE_DEMO") != "1" {
		t.Skip("set INMEMCACHE_RACE_DEMO=1 to run; this test is expected to crash the process")
	}

	c := NewNaive()
	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 100_000; i++ {
				c.Set("k", "v") // concurrent writes -> fatal "concurrent map writes"
			}
		}()
	}
	wg.Wait()
}
