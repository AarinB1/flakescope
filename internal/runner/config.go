// Package runner turns a base configuration into a matrix of configurations and
// runs `go test` under each of them.
//
// flakescope varies CONFIGURATIONS, not interleavings. Go has no seedable
// goroutine scheduler outside testing/synctest, so nothing here can replay a
// particular ordering of goroutines. What it can do is vary the knobs that
// change how the runtime and the test binary behave, and report which of them a
// failure depends on.
package runner

import (
	"fmt"
	"runtime"
	"strconv"
	"strings"
)

// Config is one set of knobs to run the package's tests under. These four are
// the knobs that actually change behaviour; there is deliberately no fifth.
type Config struct {
	// ShuffleSeed is the seed passed to `go test -shuffle`. Zero means shuffle
	// is OFF, which is why the flag is omitted entirely rather than passed as
	// -shuffle=0. Go would accept -shuffle=0 as a literal seed; flakescope
	// reserves 0 for "off" so that the zero value of Config is the unshuffled
	// case.
	ShuffleSeed int64
	// GOMAXPROCS is passed through the environment, not as a test flag.
	GOMAXPROCS int
	// Race enables the race detector.
	Race bool
	// Count is the -count value. It is always emitted, including as -count=1,
	// because passing -count explicitly is the documented way to defeat the go
	// test result cache. Without it, two runs that differ only in GOMAXPROCS
	// can be served from cache and report the same result by construction.
	Count int
}

// Default is the configuration flakescope measures everything else against:
// shuffle off, GOMAXPROCS at runtime.NumCPU(), race off, count 1.
//
// It is a named function rather than an implicit zero value because the
// minimal-reproducing-configuration logic in internal/report is defined as
// distance from it. A default nobody can name is a default nobody can measure
// against.
func Default() Config {
	return Config{
		ShuffleSeed: 0,
		GOMAXPROCS:  runtime.NumCPU(),
		Race:        false,
		Count:       1,
	}
}

// Shuffled reports whether test order is randomised under this configuration.
func (c Config) Shuffled() bool { return c.ShuffleSeed != 0 }

// Args returns the `go` arguments for this configuration, including the leading
// "test". GOMAXPROCS is not here; it travels in the environment.
func (c Config) Args(pkg string) []string {
	count := c.Count
	if count < 1 {
		count = 1
	}
	args := []string{"test", "-json", "-count=" + strconv.Itoa(count)}
	if c.Race {
		args = append(args, "-race")
	}
	if c.Shuffled() {
		args = append(args, "-shuffle="+strconv.FormatInt(c.ShuffleSeed, 10))
	}
	return append(args, pkg)
}

// Env returns base with this configuration's GOMAXPROCS applied. Any GOMAXPROCS
// already present is dropped rather than shadowed, so the result reads the same
// way it behaves.
func (c Config) Env(base []string) []string {
	procs := c.GOMAXPROCS
	if procs < 1 {
		procs = 1
	}
	out := make([]string, 0, len(base)+1)
	for _, kv := range base {
		if strings.HasPrefix(kv, "GOMAXPROCS=") {
			continue
		}
		out = append(out, kv)
	}
	return append(out, "GOMAXPROCS="+strconv.Itoa(procs))
}

// String renders the configuration the way a person would have to type it.
func (c Config) String() string {
	parts := []string{fmt.Sprintf("GOMAXPROCS=%d", c.GOMAXPROCS), "go test"}
	if c.Race {
		parts = append(parts, "-race")
	}
	if c.Shuffled() {
		parts = append(parts, fmt.Sprintf("-shuffle=%d", c.ShuffleSeed))
	}
	parts = append(parts, fmt.Sprintf("-count=%d", c.Count))
	return strings.Join(parts, " ")
}

// Matrix returns n configurations derived from base.
//
// It is a pure function: the same (base, n) always yields the same slice, in the
// same order, with base itself at index 0. Nothing in here reads the clock or a
// random source, because a matrix that differed between two invocations would
// make every result flakescope reports unreproducible.
//
// The three varying axes, each with base's own value first:
//
//	GOMAXPROCS: base.GOMAXPROCS, then 1, 2, 4 (skipping base's own value)
//	Race:       base.Race, then its negation
//	Seed:       base.ShuffleSeed, then 1, 2, 3, ... (skipping base's own)
//
// Configurations are emitted by walking the diagonals of that space: all
// combinations whose axis indices sum to 0, then to 1, then to 2, and so on,
// and within a diagonal in GOMAXPROCS-then-race-then-seed order. That ordering
// spends the early, cheap part of the matrix on single-knob changes and only
// then starts combining them, so a short --runs still covers each axis alone.
//
// Count does not vary. It is a user knob, not a hypothesis about why a test
// fails, and varying it would multiply the matrix without changing which knob a
// failure depends on.
func Matrix(base Config, n int) []Config {
	if n <= 0 {
		return nil
	}

	procsAxis := axisInts(base.GOMAXPROCS, []int{1, 2, 4})
	raceAxis := []bool{base.Race, !base.Race}
	seedAxis := seedAxisFor(base.ShuffleSeed, n)

	maxSum := (len(procsAxis) - 1) + (len(raceAxis) - 1) + (len(seedAxis) - 1)
	out := make([]Config, 0, n)
	for sum := 0; sum <= maxSum && len(out) < n; sum++ {
		for i, procs := range procsAxis {
			for j, race := range raceAxis {
				k := sum - i - j
				if k < 0 || k >= len(seedAxis) {
					continue
				}
				out = append(out, Config{
					ShuffleSeed: seedAxis[k],
					GOMAXPROCS:  procs,
					Race:        race,
					Count:       base.Count,
				})
				if len(out) == n {
					return out
				}
			}
		}
	}
	return out
}

// axisInts puts base first, then each candidate that is not base and is usable.
func axisInts(base int, candidates []int) []int {
	if base < 1 {
		base = 1
	}
	axis := []int{base}
	for _, c := range candidates {
		if c != base {
			axis = append(axis, c)
		}
	}
	return axis
}

// seedAxisFor produces base's seed followed by 1, 2, 3, ... skipping base's own
// value. It is at least n+1 long so the diagonal walk never runs out of seeds
// before it has emitted n configurations.
func seedAxisFor(base int64, n int) []int64 {
	axis := make([]int64, 0, n+1)
	axis = append(axis, base)
	for seed := int64(1); len(axis) < n+1; seed++ {
		if seed != base {
			axis = append(axis, seed)
		}
	}
	return axis
}
