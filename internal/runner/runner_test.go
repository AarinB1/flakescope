package runner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AarinB1/flakescope/internal/gotest"
)

const fixturePkg = "github.com/AarinB1/flakescope/testdata/flakypkg"

func readStream(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", "streams", name))
	if err != nil {
		t.Fatalf("reading recorded stream: %v", err)
	}
	return b
}

// fakeExec replays a recorded stream chosen by the configuration. This is how
// every test in this file except the integration test gets its data: no `go
// test` is invoked, so nothing here can be flaky (CLAUDE.md rule 2).
func fakeExec(t *testing.T, pick func(Config) string) func(context.Context, Config) ([]byte, error) {
	t.Helper()
	return func(_ context.Context, cfg Config) ([]byte, error) {
		return readStream(t, pick(cfg)), nil
	}
}

func TestRunReplaysRecordedStreams(t *testing.T) {
	tests := []struct {
		name       string
		stream     string
		wantStatus gotest.Status
		wantTest   string
	}{
		{"all pass", "allpass.json", gotest.StatusPass, "TestOrderDependent"},
		{"order dependent failure", "orderfail.json", gotest.StatusFail, "TestOrderDependent"},
		{"load dependent failure", "loadfail.json", gotest.StatusFail, "TestLoadDependent"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := &Runner{Package: fixturePkg}
			r.exec = fakeExec(t, func(Config) string { return tc.stream })

			results := r.Run(context.Background(), Matrix(Default(), 3))
			if len(results) != 3 {
				t.Fatalf("got %d results, want 3", len(results))
			}
			for i, res := range results {
				if res.Outcome != OutcomeCompleted {
					t.Fatalf("result %d outcome = %v (%v), want completed", i, res.Outcome, res.Err)
				}
				tst := res.Run.Package(fixturePkg).Test(tc.wantTest)
				if tst == nil {
					t.Fatalf("result %d has no %s", i, tc.wantTest)
				}
				if tst.Status != tc.wantStatus {
					t.Errorf("result %d: %s = %v, want %v", i, tc.wantTest, tst.Status, tc.wantStatus)
				}
			}
		})
	}
}

// TestRunPreservesConfigurationOrder pins result order to matrix order. The fake
// finishes the configurations in reverse, which is enough to catch a runner that
// appended results as they arrived.
func TestRunPreservesConfigurationOrder(t *testing.T) {
	configs := Matrix(Default(), 8)

	var mu sync.Mutex
	release := make([]chan struct{}, len(configs))
	for i := range release {
		release[i] = make(chan struct{})
	}
	seedIndex := make(map[Config]int, len(configs))
	for i, cfg := range configs {
		seedIndex[cfg] = i
	}

	r := &Runner{Package: fixturePkg, Workers: len(configs)}
	r.exec = func(_ context.Context, cfg Config) ([]byte, error) {
		mu.Lock()
		i := seedIndex[cfg]
		mu.Unlock()
		<-release[i]
		return readStream(t, "allpass.json"), nil
	}

	done := make(chan []Result, 1)
	go func() { done <- r.Run(context.Background(), configs) }()
	for i := len(configs) - 1; i >= 0; i-- {
		close(release[i])
	}
	results := <-done

	for i, res := range results {
		if res.Config != configs[i] {
			t.Errorf("results[%d].Config = %+v, want %+v", i, res.Config, configs[i])
		}
	}
}

func TestRunTimeoutIsNeitherPassNorFail(t *testing.T) {
	r := &Runner{Package: fixturePkg, Timeout: 20 * time.Millisecond, Workers: 2}
	r.exec = func(ctx context.Context, _ Config) ([]byte, error) {
		// A killed `go test` leaves a half-written stream behind.
		partial := readStream(t, "truncated.json")
		<-ctx.Done()
		return partial, errors.New("signal: killed")
	}

	results := r.Run(context.Background(), Matrix(Default(), 3))
	for i, res := range results {
		if res.Outcome != OutcomeTimedOut {
			t.Fatalf("result %d outcome = %v, want timeout", i, res.Outcome)
		}
		if res.Err != nil {
			t.Errorf("result %d carries an error %v; a timeout is not an error", i, res.Err)
		}
		// The partial stream is still parsed, and what was in flight is
		// incomplete rather than passing.
		if res.Run == nil {
			t.Fatalf("result %d discarded the partial stream", i)
		}
		tst := res.Run.Package(fixturePkg).Test("TestAlwaysPasses")
		if tst == nil || tst.Status != gotest.StatusIncomplete {
			t.Errorf("result %d: in-flight test = %v, want incomplete", i, tst)
		}
	}
}

func TestRunExecErrorIsNotAFailure(t *testing.T) {
	wantErr := errors.New("exec: \"go\": executable file not found in $PATH")
	r := &Runner{Package: fixturePkg, Workers: 2}
	r.exec = func(context.Context, Config) ([]byte, error) { return nil, wantErr }

	for i, res := range r.Run(context.Background(), Matrix(Default(), 2)) {
		if res.Outcome != OutcomeError {
			t.Errorf("result %d outcome = %v, want error", i, res.Outcome)
		}
		if !errors.Is(res.Err, wantErr) {
			t.Errorf("result %d err = %v, want %v", i, res.Err, wantErr)
		}
		if res.Run != nil {
			t.Errorf("result %d has a Run despite the exec failing", i)
		}
	}
}

func TestRunCorruptStreamIsAnError(t *testing.T) {
	r := &Runner{Package: fixturePkg}
	r.exec = func(context.Context, Config) ([]byte, error) {
		return []byte("{\"Action\":\"start\",\"Package\":\"p\"}\nthis is not json\n"), nil
	}
	res := r.Run(context.Background(), Matrix(Default(), 1))[0]
	if res.Outcome != OutcomeError {
		t.Fatalf("outcome = %v, want error", res.Outcome)
	}
}

func TestRunRespectsWorkerLimit(t *testing.T) {
	tests := []struct {
		name    string
		workers int
		configs int
		want    int32
	}{
		{"serial", 1, 6, 1},
		{"two at a time", 2, 6, 2},
		{"limit above the work available", 10, 3, 3},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var inFlight, peak int32
			started := make(chan struct{}, tc.configs)

			r := &Runner{Package: fixturePkg, Workers: tc.workers}
			r.exec = func(context.Context, Config) ([]byte, error) {
				n := atomic.AddInt32(&inFlight, 1)
				for {
					old := atomic.LoadInt32(&peak)
					if n <= old || atomic.CompareAndSwapInt32(&peak, old, n) {
						break
					}
				}
				started <- struct{}{}
				// Hold until every worker that can start has started, so the
				// peak is a real measurement rather than a race.
				if len(started) >= min(tc.workers, tc.configs) {
					time.Sleep(time.Millisecond)
				}
				atomic.AddInt32(&inFlight, -1)
				return readStream(t, "allpass.json"), nil
			}

			r.Run(context.Background(), Matrix(Default(), tc.configs))
			if got := atomic.LoadInt32(&peak); got > tc.want {
				t.Errorf("peak concurrency = %d, want at most %d", got, tc.want)
			}
		})
	}
}

func TestRunStopsDispatchingWhenContextIsCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var calls int32

	r := &Runner{Package: fixturePkg, Workers: 1}
	r.exec = func(context.Context, Config) ([]byte, error) {
		if atomic.AddInt32(&calls, 1) == 1 {
			cancel()
		}
		return readStream(t, "allpass.json"), nil
	}

	configs := Matrix(Default(), 20)
	results := r.Run(ctx, configs)
	if len(results) != len(configs) {
		t.Fatalf("got %d results, want one per configuration (%d)", len(results), len(configs))
	}
	if got := atomic.LoadInt32(&calls); int(got) >= len(configs) {
		t.Errorf("ran %d of %d configurations after cancellation", got, len(configs))
	}
	// Configurations that never ran must not look like passes.
	last := results[len(results)-1]
	if last.Run != nil {
		t.Error("an undispatched configuration came back with a parsed run")
	}
}

// TestIntegrationRunsTheRealGoTool is the one test in this repository that
// shells out to `go test`. Everything else replays recordings. It exists to
// catch the class of bug recordings cannot: wrong flag spelling, GOMAXPROCS not
// reaching the child, `-shuffle` not doing what the fixture assumes.
//
// It is skipped under -short.
func TestIntegrationRunsTheRealGoTool(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test: shells out to the real go tool")
	}

	// GOMAXPROCS is pinned rather than taken from NumCPU so the expectations
	// below hold on a single-core machine too.
	tests := []struct {
		name string
		cfg  Config
		want map[string]gotest.Status
	}{
		{
			name: "single P, no shuffle: only the always-broken test fails",
			cfg:  Config{GOMAXPROCS: 1, Count: 1},
			want: map[string]gotest.Status{
				"TestAlwaysPasses":       gotest.StatusPass,
				"TestOrderDependent":     gotest.StatusPass,
				"TestLoadDependent":      gotest.StatusPass,
				"TestAlwaysFails":        gotest.StatusFail,
				"TestPoisonsGlobalState": gotest.StatusPass,
			},
		},
		{
			name: "four Ps: the load-dependent test fails as well",
			cfg:  Config{GOMAXPROCS: 4, Count: 1},
			want: map[string]gotest.Status{
				"TestOrderDependent": gotest.StatusPass,
				"TestLoadDependent":  gotest.StatusFail,
				"TestAlwaysFails":    gotest.StatusFail,
			},
		},
		{
			name: "shuffle seed 1 at a single P: the order-dependent test fails instead",
			cfg:  Config{GOMAXPROCS: 1, ShuffleSeed: 1, Count: 1},
			want: map[string]gotest.Status{
				"TestOrderDependent": gotest.StatusFail,
				"TestLoadDependent":  gotest.StatusPass,
				"TestAlwaysFails":    gotest.StatusFail,
			},
		},
		{
			name: "shuffle seed 3 at a single P: that permutation does not reproduce it",
			cfg:  Config{GOMAXPROCS: 1, ShuffleSeed: 3, Count: 1},
			want: map[string]gotest.Status{
				"TestOrderDependent": gotest.StatusPass,
				"TestLoadDependent":  gotest.StatusPass,
				"TestAlwaysFails":    gotest.StatusFail,
			},
		},
	}

	r := New(fixturePkg)
	r.Timeout = 2 * time.Minute
	r.Workers = 2

	configs := make([]Config, len(tests))
	for i, tc := range tests {
		configs[i] = tc.cfg
	}
	results := r.Run(context.Background(), configs)

	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res := results[i]
			if res.Outcome != OutcomeCompleted {
				t.Fatalf("outcome = %v: %v", res.Outcome, res.Err)
			}
			pkg := res.Run.Package(fixturePkg)
			if pkg == nil {
				t.Fatalf("package %s missing from the stream", fixturePkg)
			}
			for name, want := range tc.want {
				tst := pkg.Test(name)
				if tst == nil {
					t.Errorf("test %q missing", name)
					continue
				}
				if tst.Status != want {
					t.Errorf("%s under %s = %v, want %v", name, tc.cfg, tst.Status, want)
				}
			}
		})
	}
}
