// Package flakypkg is a deliberately broken fixture package used to exercise
// flakescope end to end. It lives under testdata/ so the go tool ignores it
// when matching ./..., which keeps its failing tests out of `make check`.
//
// Every test here is DETERMINISTIC given a configuration. Nothing in this file
// consults math/rand or the clock. A probabilistic fixture would make every
// test in this repository probabilistic too, and a flaky-test detector with a
// flaky test suite is not shippable.
//
// The configurations that matter, and what each test does under them:
//
//	TestAlwaysPasses      passes everywhere.
//	TestOrderDependent    fails iff TestPoisonsGlobalState ran before it,
//	                      which only -shuffle can arrange.
//	TestPoisonsGlobalState passes everywhere; it exists to break its neighbour.
//	TestLoadDependent     fails iff GOMAXPROCS > 1.
//	TestAlwaysFails       fails everywhere.
package flakypkg

import (
	"runtime"
	"sync"
	"testing"
)

// poisoned is package-level state that TestPoisonsGlobalState leaves behind.
// It is the whole mechanism behind the order-dependent failure: nothing resets
// it, so once that test has run, TestOrderDependent cannot pass.
var poisoned bool

// TestAlwaysPasses carries subtests so the recorded streams contain Test values
// with a slash in them. flakescope records subtests under their full name and
// does not roll them up into their parent.
func TestAlwaysPasses(t *testing.T) {
	cases := []struct {
		name string
		a, b int
		want int
	}{
		{"add", 2, 2, 4},
		{"multiply", 3, 3, 9},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.a + tc.b
			if tc.name == "multiply" {
				got = tc.a * tc.b
			}
			if got != tc.want {
				t.Fatalf("got %d, want %d", got, tc.want)
			}
		})
	}
}

// TestOrderDependent is declared BEFORE TestPoisonsGlobalState on purpose. Go
// runs tests in source order, so with -shuffle off this test always runs first
// and always passes. Only a shuffle seed that permutes the poisoner ahead of it
// makes it fail.
func TestOrderDependent(t *testing.T) {
	if poisoned {
		t.Fatalf("global state was poisoned by an earlier test in this run")
	}
}

func TestPoisonsGlobalState(t *testing.T) {
	poisoned = true
}

// TestLoadDependent fails when GOMAXPROCS > 1.
//
// The goroutines below do real concurrent work, but the failing assertion reads
// runtime.GOMAXPROCS directly rather than racing on the shared counter.
//
// DO NOT MAKE THIS REALISTIC. Determinism outranks realism here, and the
// mutex is not an oversight to be removed. Replacing the configuration read
// with a genuine data race breaks the premise the whole repository rests on: a
// real race is nondeterministic by definition, so this test would sometimes
// pass under GOMAXPROCS=8 and sometimes fail under GOMAXPROCS=1, and every
// assertion that depends on it - the classifier, the dependence rules, the
// minimal-configuration ordering - would inherit that nondeterminism and stop
// meaning anything. A flaky-test detector with a flaky test suite is not
// shippable (CLAUDE.md rule 2).
//
// What this test is, precisely: a stand-in for the class of failure that only
// appears once more than one P is available, which is not itself one.
func TestLoadDependent(t *testing.T) {
	const workers = 8

	var mu sync.Mutex
	var wg sync.WaitGroup
	total := 0
	for i := 1; i <= workers; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			mu.Lock()
			total += n
			mu.Unlock()
		}(i)
	}
	wg.Wait()

	if want := workers * (workers + 1) / 2; total != want {
		t.Fatalf("counter: got %d, want %d", total, want)
	}
	if p := runtime.GOMAXPROCS(0); p > 1 {
		t.Fatalf("parallel execution exposed the bug: GOMAXPROCS=%d", p)
	}
}

func TestAlwaysFails(t *testing.T) {
	t.Fatalf("this test is broken in every configuration, which is not the same as flaky")
}
