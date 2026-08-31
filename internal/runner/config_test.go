package runner

import (
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
				if seen[cfg] {
					t.Errorf("Matrix()[%d] = %+v is a duplicate; a repeated configuration buys no information", i, cfg)
				}
				seen[cfg] = true
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
