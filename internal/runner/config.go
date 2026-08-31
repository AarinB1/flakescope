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

// raceEvery is how often the tail of the matrix switches the race detector on:
// one configuration in eight.
//
// RACE IS SAMPLED, NOT ALTERNATED. It is the knob that dominates wall-clock -
// a race build is commonly several times slower to build and to run than a
// plain one - and what the matrix needs from it is the answer to a yes/no
// question: does this failure need the detector? A sample answers that as well
// as a census does. Alternating it, so that half the matrix races, spends most
// of the run time on the axis with the least to say.
//
// The arithmetic, for a race build costing r times a plain one: alternating
// makes the matrix 0.5 + 0.5r times a race-free run, which at r=10 is 5.5x.
// One in eight makes it 0.875 + 0.125r, which at r=10 is 2.1x. Measured on
// this repository's own fixture r is only about 1.5, but that fixture is
// dominated by the go tool's own startup; a real package is where the number
// bites.
const raceEvery = 8

// Matrix returns n configurations derived from base.
//
// It is a pure function: the same (base, n) always yields the same slice, in the
// same order, with base itself at index 0. Nothing in here reads the clock or a
// random source, because a matrix that differed between two invocations would
// make every result flakescope reports unreproducible.
//
// # The coverage prefix
//
// The matrix opens with base, then each axis varied ALONE: shuffle seed, then
// race, then GOMAXPROCS. A short --runs has to be able to tell an
// order-dependent failure from a load-dependent one, and it cannot if the
// matrix combines knobs before it has tried them singly. That is three
// configurations plus one per remaining GOMAXPROCS candidate - six in the usual
// case, and never more than seven.
//
//	out[0]     base
//	out[1]     base + a shuffle seed
//	out[2]     base + the race detector flipped
//	out[3...]  base + each other GOMAXPROCS candidate, one per configuration
//
// Every GOMAXPROCS candidate gets an unshuffled, unraced run of its own rather
// than just the first candidate. Without that, the only unshuffled runs at
// GOMAXPROCS values other than base's would be in the tail, where every
// configuration carries a seed - and flakescope would report `-shuffle=3` as
// part of the minimal way to reproduce a load-dependent failure that has
// nothing to do with test order.
//
// # The tail
//
// Everything after that scales:
//
//	seed        a value no other configuration in the matrix uses
//	GOMAXPROCS  cycled through the candidates, one per configuration
//	race        on for one configuration in raceEvery
//
// EVERY TAIL CONFIGURATION CARRIES A DISTINCT SEED, and that is what makes the
// whole matrix duplicate-free at any n rather than only at small n. A repeated
// configuration is a run that buys no information: it costs a full `go test`
// and cannot change any count, rate or classification. A matrix that silently
// repeated itself would let flakescope advertise a thousand runs and deliver
// forty, and the report would look exactly the same either way.
//
// Count does not vary. It is a user knob, not a hypothesis about why a test
// fails, and varying it would multiply the matrix without changing which knob a
// failure depends on.
func Matrix(base Config, n int) []Config {
	if n <= 0 {
		return nil
	}

	procsAxis := axisInts(base.GOMAXPROCS, []int{1, 2, 4})
	seeds := seedsFor(base.ShuffleSeed, n+1)

	out := make([]Config, 0, n)
	add := func(c Config) bool {
		out = append(out, c)
		return len(out) < n
	}

	// The coverage prefix. Each of these differs from base in exactly one knob,
	// and each differs from the others, so none of them can collide.
	if !add(Config{ShuffleSeed: base.ShuffleSeed, GOMAXPROCS: procsAxis[0], Race: base.Race, Count: base.Count}) {
		return out
	}
	if !add(Config{ShuffleSeed: seeds[0], GOMAXPROCS: procsAxis[0], Race: base.Race, Count: base.Count}) {
		return out
	}
	if !add(Config{ShuffleSeed: base.ShuffleSeed, GOMAXPROCS: procsAxis[0], Race: !base.Race, Count: base.Count}) {
		return out
	}
	for _, procs := range procsAxis[1:] {
		if !add(Config{ShuffleSeed: base.ShuffleSeed, GOMAXPROCS: procs, Race: base.Race, Count: base.Count}) {
			return out
		}
	}

	// The tail. seeds[0] is spent on the prefix, so the tail starts at seeds[1]
	// and never revisits a seed the prefix used or one it used itself.
	seed := 1
	for i := len(out); ; i++ {
		cfg := Config{
			ShuffleSeed: seeds[seed],
			GOMAXPROCS:  procsAxis[i%len(procsAxis)],
			// Not relative to base.Race: the reason to ration this knob is what
			// running the detector costs, not how far it is from the default.
			Race:  i%raceEvery == 0,
			Count: base.Count,
		}
		seed++
		if !add(cfg) {
			return out
		}
	}
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

// seedsFor returns n shuffle seeds, none of them base's own and none repeated:
// 1, 2, 3, ... with base's value skipped.
//
// Sequential rather than spread out. Adjacent seeds produce unrelated
// permutations, so nothing is gained by scattering them, and a seed a user can
// read off the report and type back is worth more than one that looks random.
func seedsFor(base int64, n int) []int64 {
	seeds := make([]int64, 0, n)
	for seed := int64(1); len(seeds) < n; seed++ {
		if seed != base {
			seeds = append(seeds, seed)
		}
	}
	return seeds
}
