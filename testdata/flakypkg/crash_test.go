//go:build flakescope_crash

// The two fixtures in this file produce FAILURE OUTPUT for internal/signature
// to normalize. They are behind a build tag because neither can be part of the
// deterministic matrix the rest of testdata/flakypkg provides:
//
//   - TestPanics kills the test binary, so every test after it in the run is
//     never reported at all.
//   - TestRaces contains a genuine data race, so its result under a
//     configuration without -race is not determined by that configuration.
//
// Either one would invalidate the four full-package recordings in
// testdata/streams, which were taken with no -run filter and no build tag. The
// tag is part of the configuration those two fixtures' own recordings were made
// under, and PROVENANCE.md names it (CLAUDE.md rule 5).
//
// TestRaces is where the load-dependent fixture's rule does NOT apply, and the
// difference is worth being precise about. flaky_test.go's TestLoadDependent
// must never contain a real race, because assertions about DETECTION - the
// classifier, the dependence rules, the minimal configuration - are built on
// its pass/fail behaviour, and a real race would make all of them
// probabilistic. Nothing is built on whether TestRaces passes or fails. It is
// read only for the text the race detector prints, which is fixed by the shape
// of the report and not by which goroutine won.
package flakypkg

import (
	"sync"
	"testing"
)

// TestPanics panics rather than failing an assertion, so the recorded stream
// carries a goroutine stack: a message, frame offsets, and heap addresses, all
// of which a signature has to normalize away.
func TestPanics(t *testing.T) {
	var m map[string]int
	m["boom"] = 1
}

// TestRaces is a genuine unsynchronized read-modify-write, which is what makes
// the race detector emit its two-stack report. The counter is never asserted
// on; the point is the report, not the total.
func TestRaces(t *testing.T) {
	const workers = 4

	counter := 0
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			counter++
		}()
	}
	wg.Wait()
	t.Logf("counter reached %d", counter)
}
