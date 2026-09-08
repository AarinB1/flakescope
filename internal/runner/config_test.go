package runner

import (
	"fmt"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func TestDefault(t *testing.T) {
	got := Default()
	want := Config{ShuffleSeed: 0, GOMAXPROCS: runtime.NumCPU(), Race: false, Count: 1}
	if got != want {
		t.Errorf("Default() = %+v, want %+v", got, want)
	}
	if got.Shuffled() {
		t.Error("Default() has shuffle on; the whole minimality ordering assumes it is off")
	}
}

func TestConfigArgs(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
		want []string
	}{
		{
			name: "default emits count explicitly to defeat the test cache",
			cfg:  Config{GOMAXPROCS: 8, Count: 1},
			want: []string{"test", "-json", "-count=1", "./pkg"},
		},
		{
			name: "seed zero means shuffle off, so no flag at all",
			cfg:  Config{ShuffleSeed: 0, GOMAXPROCS: 1, Count: 1},
			want: []string{"test", "-json", "-count=1", "./pkg"},
		},
		{
			name: "shuffle seed",
			cfg:  Config{ShuffleSeed: 7, GOMAXPROCS: 1, Count: 1},
			want: []string{"test", "-json", "-count=1", "-shuffle=7", "./pkg"},
		},
		{
			name: "race",
			cfg:  Config{Race: true, GOMAXPROCS: 2, Count: 1},
			want: []string{"test", "-json", "-count=1", "-race", "./pkg"},
		},
		{
			name: "every knob at once",
			cfg:  Config{ShuffleSeed: 3, GOMAXPROCS: 4, Race: true, Count: 5},
			want: []string{"test", "-json", "-count=5", "-race", "-shuffle=3", "./pkg"},
		},
		{
			name: "count below one is clamped, never omitted",
			cfg:  Config{GOMAXPROCS: 1, Count: 0},
			want: []string{"test", "-json", "-count=1", "./pkg"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.cfg.Args("./pkg")
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("Args() = %v, want %v", got, tc.want)
			}
			// GOMAXPROCS travels in the environment. If it ever leaks into the
			// flags, `go test` rejects it and every run becomes an error.
			for _, a := range got {
				if strings.Contains(a, "GOMAXPROCS") {
					t.Errorf("GOMAXPROCS leaked into the arguments: %v", got)
				}
			}
		})
	}
}

func TestConfigEnv(t *testing.T) {
	tests := []struct {
		name    string
		cfg     Config
		base    []string
		want    string
		wantLen int
	}{
		{
			name:    "appended when absent",
			cfg:     Config{GOMAXPROCS: 3},
			base:    []string{"PATH=/bin", "HOME=/root"},
			want:    "GOMAXPROCS=3",
			wantLen: 3,
		},
		{
			name:    "replaced, not shadowed, when already present",
			cfg:     Config{GOMAXPROCS: 2},
			base:    []string{"GOMAXPROCS=99", "PATH=/bin"},
			want:    "GOMAXPROCS=2",
			wantLen: 2,
		},
		{
			name:    "zero is clamped to one",
			cfg:     Config{GOMAXPROCS: 0},
			base:    nil,
			want:    "GOMAXPROCS=1",
			wantLen: 1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.cfg.Env(tc.base)
			if len(got) != tc.wantLen {
				t.Fatalf("Env() = %v, want %d entries", got, tc.wantLen)
			}
			n := 0
			for _, kv := range got {
				if strings.HasPrefix(kv, "GOMAXPROCS=") {
					n++
					if kv != tc.want {
						t.Errorf("Env() has %q, want %q", kv, tc.want)
					}
				}
			}
			if n != 1 {
				t.Errorf("Env() has %d GOMAXPROCS entries, want exactly 1: %v", n, got)
			}
		})
	}
}

func TestConfigString(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
		want string
	}{
		{"default", Config{GOMAXPROCS: 8, Count: 1}, "GOMAXPROCS=8 go test -count=1"},
		{"shuffled", Config{GOMAXPROCS: 1, ShuffleSeed: 4, Count: 1}, "GOMAXPROCS=1 go test -shuffle=4 -count=1"},
		{"race", Config{GOMAXPROCS: 2, Race: true, Count: 1}, "GOMAXPROCS=2 go test -race -count=1"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cfg.String(); got != tc.want {
				t.Errorf("String() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestMatrixShape(t *testing.T) {
	base := Config{ShuffleSeed: 0, GOMAXPROCS: 8, Race: false, Count: 1}

	tests := []struct {
		name string
		base Config
		n    int
		want int
	}{
		{"zero runs", base, 0, 0},
		{"negative runs", base, -3, 0},
		{"one run is the base alone", base, 1, 1},
		{"twenty", base, 20, 20},
		{"more configurations than the small axes can supply", base, 200, 200},
		{"base already shuffled", Config{ShuffleSeed: 5, GOMAXPROCS: 2, Count: 1}, 20, 20},
		{"base already racing", Config{GOMAXPROCS: 1, Race: true, Count: 1}, 20, 20},
		{"base GOMAXPROCS coincides with a candidate", Config{GOMAXPROCS: 2, Count: 1}, 20, 20},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Matrix(tc.base, tc.n)
			if len(got) != tc.want {
				t.Fatalf("Matrix(%+v, %d) returned %d configurations, want %d", tc.base, tc.n, len(got), tc.want)
			}
			if tc.want == 0 {
				return
			}
			if got[0] != tc.base {
				t.Errorf("Matrix()[0] = %+v, want the base %+v", got[0], tc.base)
			}
			seen := make(map[Config]bool, len(got))
			for i, cfg := range got {
				if cfg.Shuffled() {
					if seen[cfg] {
						t.Errorf("Matrix()[%d] = %+v repeats a shuffled configuration; the same seed at the same knobs reruns the same test order", i, cfg)
					}
					seen[cfg] = true
				}
				if cfg.Count != tc.base.Count {
					t.Errorf("Matrix()[%d] varied Count to %d; Count is not an axis", i, cfg.Count)
				}
				if cfg.GOMAXPROCS < 1 {
					t.Errorf("Matrix()[%d] has GOMAXPROCS=%d", i, cfg.GOMAXPROCS)
				}
			}
		})
	}
}

// TestMatrixIsPure is the reproducibility claim. If the matrix ever depended on
// the clock, a random source or map iteration order, every configuration
// flakescope printed as "the minimal repro" would be a configuration the user
// could not get back.
func TestMatrixIsPure(t *testing.T) {
	bases := []Config{
		Default(),
		{ShuffleSeed: 0, GOMAXPROCS: 8, Race: false, Count: 1},
		{ShuffleSeed: 11, GOMAXPROCS: 3, Race: true, Count: 2},
	}
	for _, base := range bases {
		t.Run(base.String(), func(t *testing.T) {
			first := Matrix(base, 25)
			for i := 0; i < 5; i++ {
				again := Matrix(base, 25)
				if !reflect.DeepEqual(first, again) {
					t.Fatalf("Matrix is not a pure function of its inputs:\n%+v\n%+v", first, again)
				}
			}
		})
	}
}

// TestMatrixExploresEachAxisAlone is what makes a short --runs useful. If the
// matrix combined knobs before trying them singly, a 4-run matrix could fail to
// distinguish an order-dependent failure from a load-dependent one.
func TestMatrixExploresEachAxisAlone(t *testing.T) {
	base := Config{ShuffleSeed: 0, GOMAXPROCS: 8, Race: false, Count: 1}
	const window = 4
	got := Matrix(base, window)

	var sawProcsOnly, sawRaceOnly, sawSeedOnly bool
	for _, cfg := range got[1:] {
		switch {
		case cfg.GOMAXPROCS != base.GOMAXPROCS && cfg.Race == base.Race && cfg.ShuffleSeed == base.ShuffleSeed:
			sawProcsOnly = true
		case cfg.Race != base.Race && cfg.GOMAXPROCS == base.GOMAXPROCS && cfg.ShuffleSeed == base.ShuffleSeed:
			sawRaceOnly = true
		case cfg.ShuffleSeed != base.ShuffleSeed && cfg.GOMAXPROCS == base.GOMAXPROCS && cfg.Race == base.Race:
			sawSeedOnly = true
		}
	}
	if !sawProcsOnly || !sawRaceOnly || !sawSeedOnly {
		t.Errorf("the first %d configurations do not vary each axis alone (procs=%v race=%v seed=%v): %+v",
			window, sawProcsOnly, sawRaceOnly, sawSeedOnly, got)
	}
}

// TestMatrixReachesEveryGOMAXPROCSCandidate guards the axis definition itself:
// the load-dependent classification needs both a single-P run and a multi-P run
// to exist in the matrix before it can conclude anything.
func TestMatrixReachesEveryGOMAXPROCSCandidate(t *testing.T) {
	got := Matrix(Config{GOMAXPROCS: 8, Count: 1}, 20)
	want := map[int]bool{1: false, 2: false, 4: false, 8: false}
	for _, cfg := range got {
		if _, ok := want[cfg.GOMAXPROCS]; ok {
			want[cfg.GOMAXPROCS] = true
		}
	}
	for procs, seen := range want {
		if !seen {
			t.Errorf("a 20-run matrix never tries GOMAXPROCS=%d", procs)
		}
	}
}

// TestMatrixAtScale is the claim that "--runs 1000" means a thousand runs.
//
// The failure this exists to catch is silent: a matrix that repeats itself
// produces a report indistinguishable from one that did not, because a repeated
// configuration cannot change a count, a rate or a classification. It would just
// mean a thousand `go test` invocations bought forty configurations' worth of
// information, and nothing anywhere would say so.
func TestMatrixAtScale(t *testing.T) {
	bases := []struct {
		name string
		base Config
	}{
		{"default-shaped base", Config{GOMAXPROCS: 8, Count: 1}},
		{"base GOMAXPROCS coincides with a candidate", Config{GOMAXPROCS: 2, Count: 1}},
		{"base already shuffled", Config{ShuffleSeed: 5, GOMAXPROCS: 4, Count: 1}},
		{"base already racing", Config{GOMAXPROCS: 1, Race: true, Count: 1}},
		{"single processor base", Config{GOMAXPROCS: 1, Count: 1}},
	}
	sizes := []int{50, 200, 1000}

	for _, b := range bases {
		for _, n := range sizes {
			t.Run(fmt.Sprintf("%s/%d", b.name, n), func(t *testing.T) {
				got := Matrix(b.base, n)
				if len(got) != n {
					t.Fatalf("Matrix returned %d configurations, want %d", len(got), n)
				}

				seen := make(map[Config]int, n)
				for i, cfg := range got {
					if !cfg.Shuffled() {
						// Unshuffled configurations repeat by design; see
						// TestMatrixReplicatesOnlyWhereItMust for the rule.
						continue
					}
					if first, dup := seen[cfg]; dup {
						t.Fatalf("configuration %d repeats configuration %d: %s\n"+
							"a repeated shuffled configuration reruns the same test order and buys no information",
							i, first, cfg)
					}
					seen[cfg] = i
				}
				for i, cfg := range got {
					if cfg.Count != b.base.Count {
						t.Fatalf("configuration %d varied Count to %d; Count is not an axis", i, cfg.Count)
					}
					if cfg.GOMAXPROCS < 1 {
						t.Fatalf("configuration %d has GOMAXPROCS=%d", i, cfg.GOMAXPROCS)
					}
				}
			})
		}
	}
}

// TestMatrixRationsTheRaceDetector: -race dominates wall-clock, so it is
// sampled rather than alternated. Half the matrix racing would spend most of a
// thousand-run's time on the axis with the least to say.
func TestMatrixRationsTheRaceDetector(t *testing.T) {
	// A band, not a ceiling. Too much race and a thousand runs spend their time
	// on the axis with the least to say; too little and the axis is a token
	// rather than a sample - and the load-dependence rule that reads it needs
	// enough race runs to have something to compare.
	tests := []struct {
		name             string
		base             Config
		n                int
		wantMin, wantMax float64
	}{
		{name: "a thousand runs", base: Config{GOMAXPROCS: 8, Count: 1}, n: 1000, wantMin: 0.05, wantMax: 0.20},
		{name: "two hundred runs", base: Config{GOMAXPROCS: 8, Count: 1}, n: 200, wantMin: 0.05, wantMax: 0.20},
		{name: "fifty runs", base: Config{GOMAXPROCS: 4, Count: 1}, n: 50, wantMin: 0.04, wantMax: 0.25},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Matrix(tc.base, tc.n)
			raced := 0
			for _, cfg := range got {
				if cfg.Race {
					raced++
				}
			}
			frac := float64(raced) / float64(len(got))
			if frac > tc.wantMax {
				t.Errorf("%d/%d configurations race (%.0f%%), want at most %.0f%%; "+
					"the race detector is meant to be sampled, not alternated",
					raced, len(got), frac*100, tc.wantMax*100)
			}
			if frac < tc.wantMin {
				t.Errorf("only %d/%d configurations race (%.1f%%), want at least %.0f%%; "+
					"the race detector is meant to be sampled, not reduced to a token",
					raced, len(got), frac*100, tc.wantMin*100)
			}
		})
	}
}

// cellOf names the (shuffle, GOMAXPROCS) cell a configuration falls in. The
// classifier compares failure rates between these cells, so a cell that is
// empty or starved makes the corresponding classification unreachable - which
// is the shape of the bug this matrix was rebalanced to remove.
func cellOf(c Config) string {
	shuffle := "unshuffled"
	if c.Shuffled() {
		shuffle = "shuffled"
	}
	return fmt.Sprintf("%s/P%d", shuffle, c.GOMAXPROCS)
}

func cellCounts(configs []Config) map[string]int {
	out := map[string]int{}
	for _, c := range configs {
		out[cellOf(c)]++
	}
	return out
}

// TestMatrixPopulatesEveryCell is the assertion the old matrix could not have
// made. It sampled 56 of 60 configurations shuffled, so the unshuffled cells at
// each GOMAXPROCS held one run apiece and "the failure rate without shuffle"
// was not a quantity the matrix could produce.
//
// The guarantee is stated on Matrix: every cell is populated from n >= 2k+6.
func TestMatrixPopulatesEveryCell(t *testing.T) {
	tests := []struct {
		name      string
		base      Config
		wantCells int
	}{
		{"default-shaped base", Config{GOMAXPROCS: 8, Count: 1}, 8},
		{"base GOMAXPROCS coincides with a candidate", Config{GOMAXPROCS: 4, Count: 1}, 6},
		{"single processor base", Config{GOMAXPROCS: 1, Count: 1}, 6},
		{"base already shuffled", Config{ShuffleSeed: 5, GOMAXPROCS: 8, Count: 1}, 8},
		{"base already racing", Config{GOMAXPROCS: 8, Race: true, Count: 1}, 8},
	}
	for _, tc := range tests {
		for _, n := range []int{16, 20, 60, 200, 1000} {
			t.Run(fmt.Sprintf("%s/%d", tc.name, n), func(t *testing.T) {
				cells := cellCounts(Matrix(tc.base, n))
				if len(cells) != tc.wantCells {
					t.Fatalf("--runs %d covered %d cells (%v), want %d", n, len(cells), cells, tc.wantCells)
				}
				for cell, count := range cells {
					if count == 0 {
						t.Errorf("cell %s is empty at --runs %d", cell, n)
					}
				}
			})
		}
	}
}

// TestMatrixBalancesCells is the other half of the same claim. A populated cell
// that holds one configuration while its neighbour holds fifty supports no rate
// comparison either, and the old matrix was populated in exactly that sense.
//
// The bound is the design: the tail deals one configuration per cell in
// rotation, so cells differ by at most one from the tail alone, plus the
// coverage prefix, which lands twice in the base's unshuffled cell and once in
// each other cell it touches.
func TestMatrixBalancesCells(t *testing.T) {
	const slack = 3

	for _, base := range []Config{{GOMAXPROCS: 8, Count: 1}, {GOMAXPROCS: 4, Count: 1}} {
		for _, n := range []int{60, 200, 1000} {
			t.Run(fmt.Sprintf("P%d/%d", base.GOMAXPROCS, n), func(t *testing.T) {
				cells := cellCounts(Matrix(base, n))
				lo, hi := n, 0
				for _, count := range cells {
					if count < lo {
						lo = count
					}
					if count > hi {
						hi = count
					}
				}
				if hi-lo > slack {
					t.Errorf("cell counts range from %d to %d at --runs %d (%v); "+
						"a rate compared across cells this uneven is not a comparison",
						lo, hi, n, cells)
				}
				want := n / len(cells)
				if lo < want-slack {
					t.Errorf("the smallest cell holds %d configurations at --runs %d, want about %d (%v)",
						lo, n, want, cells)
				}
			})
		}
	}
}

// TestMatrixReplicatesOnlyWhereItMust states the duplicate rule in the form the
// rate comparison actually needs.
//
// There are only 2k distinct unshuffled configurations in the whole space, so
// an unshuffled arm large enough to state a rate MUST rerun the same command
// line. Those repeats are independent observations of a nondeterministic
// failure and are the point. A repeated shuffle SEED is not: it reruns the same
// test order, which is the one thing the shuffled arm exists to vary.
func TestMatrixReplicatesOnlyWhereItMust(t *testing.T) {
	for _, base := range []Config{{GOMAXPROCS: 8, Count: 1}, {GOMAXPROCS: 4, Count: 1}, {ShuffleSeed: 5, GOMAXPROCS: 2, Count: 1}} {
		for _, n := range []int{20, 200, 1000} {
			t.Run(fmt.Sprintf("%s/%d", base, n), func(t *testing.T) {
				got := Matrix(base, n)

				seen := map[Config]bool{}
				shuffled := 0
				for _, cfg := range got {
					if !cfg.Shuffled() {
						continue
					}
					shuffled++
					if seen[cfg] {
						t.Fatalf("shuffled configuration %s appears twice", cfg)
					}
					seen[cfg] = true
				}
				if shuffled != len(seen) {
					t.Fatalf("%d shuffled configurations, %d of them distinct", shuffled, len(seen))
				}
				// Stronger than exact equality: every seed the matrix invents is
				// used once and once only, so no two configurations anywhere run
				// the same test order. Base's own seed is the exception, because
				// the coverage prefix carries it to each other GOMAXPROCS and to
				// the flipped race setting - one knob from base, by design.
				seedUses := map[int64]int{}
				for _, cfg := range got {
					if cfg.Shuffled() && cfg.ShuffleSeed != base.ShuffleSeed {
						seedUses[cfg.ShuffleSeed]++
					}
				}
				for seed, uses := range seedUses {
					if uses > 1 {
						t.Fatalf("shuffle seed %d is used %d times; only base's own seed may repeat", seed, uses)
					}
				}

				// Replication is bounded by the cell size: an unshuffled
				// configuration may repeat as often as its cell is deep, and no
				// deeper. Unbounded repetition would be the old failure mode
				// wearing new clothes - a thousand runs' worth of `go test` for
				// one cell's worth of information.
				cells := len(cellCounts(got))
				limit := n/cells + 4
				repeats := map[Config]int{}
				for _, cfg := range got {
					repeats[cfg]++
					if repeats[cfg] > limit {
						t.Fatalf("configuration %s appears %d times at --runs %d, more than the %d its cell can hold",
							cfg, repeats[cfg], n, limit)
					}
				}
			})
		}
	}
}

// TestMatrixSamplesBothShuffleArms is what the order axis rests on: neither arm
// may be a token. The old matrix put 56 of 60 configurations in the shuffled
// arm, which is how "all failures were shuffled" became true by construction.
func TestMatrixSamplesBothShuffleArms(t *testing.T) {
	for _, base := range []Config{{GOMAXPROCS: 8, Count: 1}, {GOMAXPROCS: 4, Count: 1}} {
		for _, n := range []int{20, 60, 200, 1000} {
			t.Run(fmt.Sprintf("P%d/%d", base.GOMAXPROCS, n), func(t *testing.T) {
				shuffled := 0
				for _, cfg := range Matrix(base, n) {
					if cfg.Shuffled() {
						shuffled++
					}
				}
				frac := float64(shuffled) / float64(n)
				if frac < 0.35 || frac > 0.65 {
					t.Errorf("%d/%d configurations are shuffled (%.0f%%), want between 35%% and 65%%; "+
						"an arm this lopsided cannot be compared against the other",
						shuffled, n, frac*100)
				}
			})
		}
	}
}

// TestMatrixDoesNotConfoundRaceWithShuffle guards the odd race period. With an
// even one, every raced configuration would land in the same shuffle arm, and a
// failure that needed the detector would be indistinguishable from one that
// needed a particular test order.
func TestMatrixDoesNotConfoundRaceWithShuffle(t *testing.T) {
	for _, n := range []int{60, 200, 1000} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			var racedShuffled, racedUnshuffled int
			for _, cfg := range Matrix(Config{GOMAXPROCS: 8, Count: 1}, n) {
				if !cfg.Race {
					continue
				}
				if cfg.Shuffled() {
					racedShuffled++
				} else {
					racedUnshuffled++
				}
			}
			if racedShuffled == 0 || racedUnshuffled == 0 {
				t.Errorf("the %d raced configurations at --runs %d are all in one shuffle arm (%d shuffled, %d unshuffled); "+
					"the race sample is confounded with test order",
					racedShuffled+racedUnshuffled, n, racedShuffled, racedUnshuffled)
			}
		})
	}
}

// TestMatrixCyclesProcessors: without cycling, a thousand runs would all sit at
// one processor count and the GOMAXPROCS axis would have nothing to compare.
func TestMatrixCyclesProcessors(t *testing.T) {
	base := Config{GOMAXPROCS: 8, Count: 1}
	const n = 1000
	procs := map[int]int{}
	for _, cfg := range Matrix(base, n) {
		procs[cfg.GOMAXPROCS]++
	}
	for _, want := range []int{1, 2, 4, 8} {
		if procs[want] == 0 {
			t.Errorf("a %d-run matrix never tries GOMAXPROCS=%d", n, want)
		}
	}
	for value, count := range procs {
		if float64(count)/float64(n) > 0.5 {
			t.Errorf("GOMAXPROCS=%d accounts for %d/%d configurations; the axis is not being cycled",
				value, count, n)
		}
	}
}
