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

// raceEvery is how often the matrix switches the race detector on: one
// configuration in seven.
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
// One in seven makes it 0.857 + 0.143r, which at r=10 is 2.3x. Measured on
// this repository's own fixture r is only about 1.5, but that fixture is
// dominated by the go tool's own startup; a real package is where the number
// bites.
//
// SEVEN IS ODD ON PURPOSE. The tail below flips shuffle on every configuration,
// so an even period would land every raced run in the same shuffle arm: the
// race sample would be perfectly confounded with test order, and a failure that
// tracked one would be indistinguishable from a failure that tracked the other.
const raceEvery = 7

// Matrix returns n configurations derived from base.
//
// It is a pure function: the same (base, n) always yields the same slice, in the
// same order, with base itself at index 0. Nothing in here reads the clock or a
// random source, because a matrix that differed between two invocations would
// make every result flakescope reports unreproducible.
//
// # What the matrix is for
//
// internal/report classifies a flaky test by COMPARING FAILURE RATES between
// arms of an axis: shuffled against unshuffled, and high GOMAXPROCS against the
// lowest. A rate needs repeated observations under the same conditions. A
// matrix that samples one arm sixty times and the other four cannot support a
// comparison, and a classifier reading it will hand out whichever label the
// sampling made likely - which is the bug this shape exists to remove.
//
// # Cells
//
// The matrix is laid out over CELLS: one per (shuffle on/off) x (GOMAXPROCS
// candidate) pair. With the usual four candidates - base plus 1, 2 and 4 -
// that is eight cells, and with a base that already sits on one of them, six.
//
// The tail fills them round-robin: shuffle flips every configuration and the
// GOMAXPROCS candidate advances every two, so after the coverage prefix each
// cell holds n/(2k) configurations to within one, where k is the number of
// candidates. Nothing else in the tail varies with the cell index, so no cell
// is systematically cheaper or more likely to be reached.
//
// # Replication, and why the unshuffled arm repeats itself
//
// Configuration is a four-knob space and Count is not an axis, so there are
// only 2k distinct UNSHUFFLED configurations in the whole space - eight, in the
// usual case. Any matrix that samples the unshuffled arm often enough to state
// a rate must therefore run the same command line more than once. That is
// replication, not waste: two runs of the same unshuffled configuration are two
// independent observations of a nondeterministic failure, and they are the only
// way to learn that it fails a fifth of the time rather than always or never.
//
// Shuffled configurations are a different matter, and they never repeat: every
// one carries a seed no other configuration in the matrix uses. A repeated seed
// at the same GOMAXPROCS and race setting reruns the same test ORDER, which is
// the one thing the shuffled arm exists to vary. Duplicate-free where it buys
// information, replicated where it must be.
//
// # The coverage prefix
//
// The matrix opens with base, then each axis varied ALONE: shuffle seed, then
// race, then GOMAXPROCS. A short --runs has to be able to tell an
// order-dependent failure from a load-dependent one, and it cannot if the
// matrix combines knobs before it has tried them singly.
//
//	out[0]     base
//	out[1]     base + a shuffle seed
//	out[2]     base + the race detector flipped
//	out[3...]  base + each other GOMAXPROCS candidate, one per configuration
//
// # What this matrix can and cannot distinguish
//
// Under internal/report's comparison rule - a pooled two-proportion z of 2,
// plus a doubling of the rate - with n configurations and k GOMAXPROCS
// candidates, the shuffle axis compares two arms of about n/2 each and the
// GOMAXPROCS axis compares the lowest candidate's n/k against the other
// candidates' 3n/4 or so.
//
// AT THE DEFAULT --runs 20 THAT IS 8 SHUFFLED AND 12 UNSHUFFLED RUNS, AND THE
// SMALLEST FAILURE RATE IT CAN TELL FROM NOISE IS 38% ON THE SHUFFLE AXIS AND
// 50% ON THE GOMAXPROCS AXIS. Twenty configurations settle whether a test fails
// always, often, or not at all. They cannot settle anything finer, and a test
// that failed once in twenty is reported as undetermined rather than labelled.
//
// The rest of the curve, measured against this matrix rather than estimated,
// for a base at GOMAXPROCS 8 (k=4) and at 4 (k=3):
//
//	           shuffle axis   GOMAXPROCS axis
//	--runs 20     38%            50-53%
//	--runs 60     14%            20-24%
//	--runs 200     4%             6- 8%
//	--runs 1000    1%             1- 2%
//
// A failure that reproduces at 2%, which is an ordinary rate for a real
// parallelism bug, therefore needs --runs of about a thousand before
// flakescope will say anything about it at all. Saying so out loud is the
// point: the alternative is a confident label read off two observations.
//
// Every cell is populated from n >= 2k+6 - 14 configurations in the usual case.
// Below that the matrix is a coverage probe rather than a measurement, and the
// classifier declines accordingly.
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

	// The tail, one configuration per cell in rotation. seeds[0] is spent on the
	// prefix, so the shuffled cells start at seeds[1].
	//
	// The unshuffled cells use seed 0 - shuffle genuinely OFF - even when base
	// carries a seed of its own. The order axis is a comparison against a
	// control arm, and a control arm that is itself shuffled is not one.
	seed := 1
	for j := 0; ; j++ {
		cfg := Config{
			GOMAXPROCS: procsAxis[(j/2)%len(procsAxis)],
			// Not relative to base.Race: the reason to ration this knob is what
			// running the detector costs, not how far it is from the default.
			Race:  j%raceEvery == 0,
			Count: base.Count,
		}
		if j%2 == 1 {
			cfg.ShuffleSeed = seeds[seed]
			seed++
		}
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
