package report

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AarinB1/flakescope/internal/gotest"
	"github.com/AarinB1/flakescope/internal/runner"
	"github.com/AarinB1/flakescope/internal/signature"
)

const fixturePkg = "github.com/AarinB1/flakescope/testdata/flakypkg"

// result builds one runner.Result by replaying a recorded stream under a stated
// configuration. Every test in this file gets its data this way; none invokes
// `go test`.
func result(t *testing.T, cfg runner.Config, stream string) runner.Result {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", "streams", stream))
	if err != nil {
		t.Fatalf("reading recorded stream: %v", err)
	}
	run, err := gotest.ParseBytes(b)
	if err != nil {
		t.Fatalf("parsing %s: %v", stream, err)
	}
	return runner.Result{Config: cfg, Outcome: runner.OutcomeCompleted, Run: run}
}

// The three configurations the recordings were actually made under. Using the
// real pairing is what makes the classifications below mean anything.
var (
	cfgSingleP  = runner.Config{GOMAXPROCS: 1, Count: 1}
	cfgShuffled = runner.Config{GOMAXPROCS: 1, ShuffleSeed: 1, Count: 1}
	cfgFourP    = runner.Config{GOMAXPROCS: 4, Count: 1}
)

func fixtureReport(t *testing.T) Report {
	t.Helper()
	base := runner.Config{GOMAXPROCS: 4, Count: 1}
	return Build(fixturePkg, base, []runner.Result{
		result(t, cfgFourP, "loadfail.json"),
		result(t, cfgShuffled, "orderfail.json"),
		result(t, cfgSingleP, "allpass.json"),
	})
}

func testByName(t *testing.T, rep Report, name string) Test {
	t.Helper()
	for _, e := range rep.Tests {
		if e.Name == name {
			return e
		}
	}
	t.Fatalf("test %q not in report", name)
	return Test{}
}

// TestClassificationFromRecordedStreams is the central claim of the tool, made
// against the real fixture. Each row names the fixture test that demonstrates
// that classification path.
func TestClassificationFromRecordedStreams(t *testing.T) {
	rep := fixtureReport(t)

	tests := []struct {
		name           string
		test           string
		wantClass      Classification
		wantDependence Dependence
		wantFail       int
		wantPass       int
		wantMinimal    *runner.Config
	}{
		{
			name:           "never fails",
			test:           "TestAlwaysPasses",
			wantClass:      ClassNeverFails,
			wantDependence: DependenceNone,
			wantPass:       3,
		},
		{
			name:           "always fails is deterministic, not flaky",
			test:           "TestAlwaysFails",
			wantClass:      ClassAlwaysFails,
			wantDependence: DependenceNone,
			wantFail:       2,
		},
		{
			// Flaky on one failure and two passes, and UNDETERMINED on why.
			// Three configurations cannot carry a rate: this test failed in the
			// one shuffled configuration there was, which is the observation
			// the old classifier read as proof of order dependence and which is
			// equally consistent with a coin landing once. The four fixtures in
			// TestClassifiesRecordedStreamsByRate supply the runs this cannot.
			name:           "order dependent, but not on three configurations",
			test:           "TestOrderDependent",
			wantClass:      ClassFlaky,
			wantDependence: DependenceUndetermined,
			wantFail:       1,
			wantPass:       2,
			wantMinimal:    &cfgShuffled,
		},
		{
			name:           "load dependent, but not on three configurations",
			test:           "TestLoadDependent",
			wantClass:      ClassFlaky,
			wantDependence: DependenceUndetermined,
			wantFail:       1,
			wantPass:       1,
			wantMinimal:    &cfgFourP,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := testByName(t, rep, tc.test)
			if got.Class != tc.wantClass {
				t.Errorf("class = %v, want %v", got.Class, tc.wantClass)
			}
			if got.Dependence != tc.wantDependence {
				t.Errorf("dependence = %v, want %v", got.Dependence, tc.wantDependence)
			}
			if got.Fail != tc.wantFail || got.Pass != tc.wantPass {
				t.Errorf("pass/fail = %d/%d, want %d/%d", got.Pass, got.Fail, tc.wantPass, tc.wantFail)
			}
			if tc.wantMinimal == nil {
				if got.Minimal != nil {
					t.Errorf("Minimal = %v, want none for a %v test", got.Minimal, tc.wantClass)
				}
				return
			}
			if got.Minimal == nil {
				t.Fatalf("Minimal is nil, want %v", *tc.wantMinimal)
			}
			if *got.Minimal != *tc.wantMinimal {
				t.Errorf("Minimal = %v, want %v", *got.Minimal, *tc.wantMinimal)
			}
		})
	}
}

// TestAlwaysFailingTestIsNotReportedAsFlaky is the assertion that CLAUDE.md rule
// 4 demands be demonstrable: TestAlwaysFails is the fixture that breaks a
// classifier which calls anything with a failure "flaky".
func TestAlwaysFailingTestIsNotReportedAsFlaky(t *testing.T) {
	rep := fixtureReport(t)

	for _, e := range rep.Flaky() {
		if e.Name == "TestAlwaysFails" {
			t.Fatalf("a test that failed in every configuration (%d/%d) was reported as flaky",
				e.Fail, e.Observations())
		}
	}
	broken := rep.AlwaysFails()
	if len(broken) != 1 || broken[0].Name != "TestAlwaysFails" {
		t.Fatalf("AlwaysFails() = %v, want exactly TestAlwaysFails", names(broken))
	}
	// And it does not raise the exit code: a consistently broken test is not a
	// flakiness finding, but the two flaky ones are.
	if got := rep.ExitCode(); got != ExitFlaky {
		t.Errorf("ExitCode() = %d, want %d", got, ExitFlaky)
	}
}

func names(ts []Test) []string {
	out := make([]string, len(ts))
	for i, t := range ts {
		out[i] = t.Name
	}
	return out
}

func TestClassify(t *testing.T) {
	tests := []struct {
		name string
		test Test
		want Classification
	}{
		{"no observations at all", Test{}, ClassNeverFails},
		{"all passes", Test{Pass: 20}, ClassNeverFails},
		{"all failures", Test{Fail: 20}, ClassAlwaysFails},
		{"one failure among passes", Test{Pass: 19, Fail: 1}, ClassFlaky},
		{"one pass among failures", Test{Pass: 1, Fail: 19}, ClassFlaky},
		{"skips and incompletes are not failures", Test{Pass: 5, Skip: 3, Incomplete: 2}, ClassNeverFails},
		{"incomplete alone is not a failure", Test{Incomplete: 20}, ClassNeverFails},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := classify(tc.test); got != tc.want {
				t.Errorf("classify(%+v) = %v, want %v", tc.test, got, tc.want)
			}
		})
	}
}

func TestFailureRate(t *testing.T) {
	tests := []struct {
		name string
		test Test
		want float64
	}{
		{"no observations", Test{}, 0},
		{"half", Test{Pass: 10, Fail: 10}, 0.5},
		{"all failures", Test{Fail: 4}, 1},
		{"timeouts are not in the denominator", Test{Pass: 1, Fail: 1, Incomplete: 18}, 0.5},
		{"skips are not in the denominator", Test{Pass: 3, Fail: 1, Skip: 16}, 0.25},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.test.FailureRate(); got != tc.want {
				t.Errorf("FailureRate() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestMinimalConfiguration walks the documented ordering one level at a time.
// Each row is decided by exactly one rule, with everything above it tied, so a
// reordering of the rules breaks a specific row rather than the whole table.
func TestMinimalConfiguration(t *testing.T) {
	base := runner.Config{ShuffleSeed: 0, GOMAXPROCS: 8, Race: false, Count: 1}

	tests := []struct {
		name       string
		candidates []runner.Config
		want       runner.Config
	}{
		{
			name: "rule 1: fewest knobs changed wins, even against a lower GOMAXPROCS",
			candidates: []runner.Config{
				{ShuffleSeed: 3, GOMAXPROCS: 1, Race: true, Count: 1},
				{ShuffleSeed: 0, GOMAXPROCS: 2, Race: false, Count: 1},
			},
			want: runner.Config{ShuffleSeed: 0, GOMAXPROCS: 2, Race: false, Count: 1},
		},
		{
			name: "rule 1: the base itself is distance zero and always wins",
			candidates: []runner.Config{
				{ShuffleSeed: 1, GOMAXPROCS: 8, Count: 1},
				base,
				{ShuffleSeed: 0, GOMAXPROCS: 1, Count: 1},
			},
			want: base,
		},
		{
			name: "rule 2: same distance, lowest GOMAXPROCS wins",
			candidates: []runner.Config{
				{ShuffleSeed: 0, GOMAXPROCS: 4, Race: false, Count: 1},
				{ShuffleSeed: 0, GOMAXPROCS: 1, Race: false, Count: 1},
				{ShuffleSeed: 0, GOMAXPROCS: 2, Race: false, Count: 1},
			},
			want: runner.Config{ShuffleSeed: 0, GOMAXPROCS: 1, Race: false, Count: 1},
		},
		{
			name: "rule 3: same distance and GOMAXPROCS, race off wins",
			candidates: []runner.Config{
				{ShuffleSeed: 0, GOMAXPROCS: 2, Race: true, Count: 1},
				{ShuffleSeed: 0, GOMAXPROCS: 2, Race: false, Count: 1},
			},
			want: runner.Config{ShuffleSeed: 0, GOMAXPROCS: 2, Race: false, Count: 1},
		},
		{
			name: "rule 4: everything else tied, lowest seed wins",
			candidates: []runner.Config{
				{ShuffleSeed: 9, GOMAXPROCS: 8, Race: false, Count: 1},
				{ShuffleSeed: 2, GOMAXPROCS: 8, Race: false, Count: 1},
				{ShuffleSeed: 5, GOMAXPROCS: 8, Race: false, Count: 1},
			},
			want: runner.Config{ShuffleSeed: 2, GOMAXPROCS: 8, Race: false, Count: 1},
		},
		{
			// Each of the next three rows pits one rule directly against the
			// rule below it, with the two candidates disagreeing. Without them
			// the table passes under any permutation of rules 1 to 4, which
			// would make it an assertion that cannot fail.
			name: "rule 1 beats rule 4: a higher seed with fewer knobs still wins",
			candidates: []runner.Config{
				{ShuffleSeed: 0, GOMAXPROCS: 1, Race: true, Count: 1},
				{ShuffleSeed: 5, GOMAXPROCS: 8, Race: false, Count: 1},
			},
			want: runner.Config{ShuffleSeed: 5, GOMAXPROCS: 8, Race: false, Count: 1},
		},
		{
			name: "rule 2 beats rule 3: lower GOMAXPROCS wins even with -race on",
			candidates: []runner.Config{
				{ShuffleSeed: 0, GOMAXPROCS: 1, Race: true, Count: 1},
				{ShuffleSeed: 3, GOMAXPROCS: 2, Race: false, Count: 1},
			},
			want: runner.Config{ShuffleSeed: 0, GOMAXPROCS: 1, Race: true, Count: 1},
		},
		{
			name: "rule 3 beats rule 4: race off wins even with a higher seed",
			candidates: []runner.Config{
				{ShuffleSeed: 0, GOMAXPROCS: 2, Race: true, Count: 1},
				{ShuffleSeed: 5, GOMAXPROCS: 2, Race: false, Count: 1},
			},
			want: runner.Config{ShuffleSeed: 5, GOMAXPROCS: 2, Race: false, Count: 1},
		},
		{
			name:       "a single candidate is the answer",
			candidates: []runner.Config{{ShuffleSeed: 7, GOMAXPROCS: 4, Race: true, Count: 1}},
			want:       runner.Config{ShuffleSeed: 7, GOMAXPROCS: 4, Race: true, Count: 1},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := minimal(base, tc.candidates); got != tc.want {
				t.Errorf("minimal() = %v, want %v", got, tc.want)
			}
			// The result must not depend on the order candidates were observed
			// in, or the same matrix would name different repros run to run.
			reversed := make([]runner.Config, len(tc.candidates))
			for i, c := range tc.candidates {
				reversed[len(tc.candidates)-1-i] = c
			}
			if got := minimal(base, reversed); got != tc.want {
				t.Errorf("minimal() on reversed input = %v, want %v", got, tc.want)
			}
		})
	}
}

// dependenceOf runs the real path a report takes: configurations in, evidence
// tallied, label read off the evidence. Going through evidenceFor rather than
// constructing an Evidence by hand is what keeps these rows honest - a tally
// that dropped half the observations would leave every table below green.
func dependenceOf(failedIn, passedIn []runner.Config) (Dependence, Evidence) {
	failures := make([]failure, 0, len(failedIn))
	for _, cfg := range failedIn {
		failures = append(failures, failure{config: cfg})
	}
	t := Test{
		Pass: len(passedIn), Fail: len(failedIn),
		failures: failures, passedIn: passedIn,
	}
	ev := evidenceFor(t)
	return dependence(ev), ev
}

// repeat is how a rate is expressed in a table: n runs of one configuration,
// f of which failed. The unshuffled arm of any real matrix is built the same
// way, because there are only a handful of distinct unshuffled configurations
// to run (see runner.Matrix).
func repeat(cfg runner.Config, n int) []runner.Config {
	out := make([]runner.Config, n)
	for i := range out {
		out[i] = cfg
	}
	return out
}

// shuffledRuns returns n configurations at procs, each with its own seed, the
// way the matrix generates them.
func shuffledRuns(procs, n int, race bool) []runner.Config {
	out := make([]runner.Config, 0, n)
	for i := 1; i <= n; i++ {
		out = append(out, runner.Config{GOMAXPROCS: procs, ShuffleSeed: int64(i), Race: race})
	}
	return out
}

func concat(groups ...[]runner.Config) []runner.Config {
	var out []runner.Config
	for _, g := range groups {
		out = append(out, g...)
	}
	return out
}

// TestDependence is the rewritten classifier's table. Every row states counts,
// not the presence or absence of a failure, because the presence of a failure
// in an arm is exactly what the previous implementation mistook for evidence.
//
// The rows that MUST NOT produce a label are the point of the table. A rule
// that only ever fires is not a rule.
func TestDependence(t *testing.T) {
	var (
		plain1 = runner.Config{GOMAXPROCS: 1}
		plain2 = runner.Config{GOMAXPROCS: 2}
		plain4 = runner.Config{GOMAXPROCS: 4}
		raced4 = runner.Config{GOMAXPROCS: 4, Race: true}
	)

	tests := []struct {
		name     string
		failedIn []runner.Config
		passedIn []runner.Config
		want     Dependence
	}{
		{
			// The wild case, in miniature: one failure, and it happened to be
			// shuffled. The old rule labelled this order-dependent because
			// every failure was shuffled and something unshuffled passed. Both
			// of those remain true here.
			name:     "one failure in sixty supports nothing, even though it was shuffled",
			failedIn: shuffledRuns(4, 1, false),
			passedIn: concat(shuffledRuns(4, 29, false)[1:], repeat(plain4, 15), repeat(plain1, 16)),
			want:     DependenceUndetermined,
		},
		{
			// One GOMAXPROCS value throughout, so the load axis has no second
			// arm and cannot contribute. The rows that isolate one axis do this
			// deliberately: a row that moves two knobs cannot say which rule
			// decided it.
			name:     "order: 14/28 shuffled against 0/28 unshuffled",
			failedIn: shuffledRuns(4, 14, false),
			passedIn: concat(shuffledRuns(4, 28, false)[14:], repeat(plain4, 28)),
			want:     DependenceOrder,
		},
		{
			name:     "NOT order: the shuffled arm fails more, but not twice as often",
			failedIn: concat(shuffledRuns(4, 11, false), repeat(plain4, 20)),
			passedIn: concat(shuffledRuns(4, 30, false)[11:], repeat(plain4, 40)),
			want:     DependenceUndetermined,
		},
		{
			name:     "NOT order: a doubled rate on two observations is not a rate",
			failedIn: shuffledRuns(4, 2, false),
			passedIn: repeat(plain4, 20),
			want:     DependenceUndetermined,
		},
		{
			name:     "NOT order: nothing unshuffled was ever run, so shuffle has no control arm",
			failedIn: shuffledRuns(4, 10, false),
			passedIn: shuffledRuns(4, 20, false)[10:],
			want:     DependenceUndetermined,
		},
		{
			name:     "load, strong case: a clean GOMAXPROCS threshold with observations behind it",
			failedIn: concat(repeat(plain4, 10), repeat(plain2, 10)),
			passedIn: repeat(plain1, 10),
			want:     DependenceLoad,
		},
		{
			name:     "load, general case: 4/100 above the threshold against 0/100 below it",
			failedIn: repeat(plain4, 4),
			passedIn: concat(repeat(plain4, 96), repeat(plain1, 100)),
			want:     DependenceLoad,
		},
		{
			// The raced runs are split evenly across the processor counts, so
			// the GOMAXPROCS arms fail at the same rate and only the race arm
			// moves.
			name:     "load: the race detector's arm, not GOMAXPROCS",
			failedIn: concat(repeat(raced4, 4), repeat(runner.Config{GOMAXPROCS: 1, Race: true}, 4)),
			passedIn: concat(repeat(plain4, 40), repeat(plain1, 40)),
			want:     DependenceLoad,
		},
		{
			name:     "NOT load: the same rate at every GOMAXPROCS",
			failedIn: concat(repeat(plain4, 5), repeat(plain2, 5), repeat(plain1, 5)),
			passedIn: concat(repeat(plain4, 15), repeat(plain2, 15), repeat(plain1, 15)),
			want:     DependenceUndetermined,
		},
		{
			name:     "NOT load: one failure at four processors and one pass at one is not a threshold",
			failedIn: repeat(plain4, 1),
			passedIn: repeat(plain1, 1),
			want:     DependenceUndetermined,
		},
		{
			// Shuffled fails at 10/40 against 2/40 unshuffled, and four
			// processors at 12/40 against nothing at one. Both rise, and the
			// label says both rather than whichever rule is written first.
			name: "both: the shuffled arm and the loaded arm each rise",
			failedIn: concat(
				shuffledRuns(4, 10, false),
				repeat(plain4, 2),
			),
			passedIn: concat(
				shuffledRuns(4, 20, false)[10:],
				repeat(plain4, 18),
				shuffledRuns(1, 20, false),
				repeat(plain1, 20),
			),
			want: DependenceBoth,
		},
		{
			name:     "no failures at all",
			failedIn: nil,
			passedIn: repeat(plain4, 20),
			want:     DependenceUndetermined,
		},
		{
			name:     "no passes at all",
			failedIn: repeat(plain4, 20),
			passedIn: nil,
			want:     DependenceUndetermined,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ev := dependenceOf(tc.failedIn, tc.passedIn)
			if got != tc.want {
				t.Errorf("dependence() = %v, want %v\n  shuffle: %v vs %v\n  procs:   %v vs %v",
					got, tc.want, ev.Shuffled, ev.Unshuffled, ev.HigherProcs(), ev.LowestProcs())
			}
		})
	}
}

// TestDependenceIsNotDecidedByRuleOrder is the regression this rewrite exists
// for. Evaluating the order rule first made the load rule unreachable for any
// test the order rule claimed, and with a matrix that was 93% shuffled the
// order rule claimed nearly everything.
//
// The row below is a load-dependent test whose failures all happen to be
// shuffled - which is what a lopsided matrix produces - alongside an unshuffled
// pass at a lower processor count. The old rule read that as order-dependent.
// The rates say otherwise: the shuffled and unshuffled arms fail at the same
// rate at each processor count, and the processor count is what moves.
func TestDependenceIsNotDecidedByRuleOrder(t *testing.T) {
	failedIn := concat(
		shuffledRuns(4, 20, false),
		repeat(runner.Config{GOMAXPROCS: 4}, 20),
	)
	passedIn := concat(
		shuffledRuns(1, 20, false),
		repeat(runner.Config{GOMAXPROCS: 1}, 20),
	)

	got, ev := dependenceOf(failedIn, passedIn)
	if got != DependenceLoad {
		t.Errorf("dependence() = %v, want %v; the shuffled and unshuffled arms fail at %v and %v, "+
			"while GOMAXPROCS moves from %v to %v",
			got, DependenceLoad, ev.Shuffled, ev.Unshuffled, ev.LowestProcs(), ev.HigherProcs())
	}
	if ev.OrderRises() {
		t.Errorf("the order axis claims a rise from %v to %v, which is the same rate", ev.Unshuffled, ev.Shuffled)
	}
}

// TestEvidenceTallies pins the arithmetic the labels rest on. A tally that
// dropped observations, or counted a raced run in both race arms, would leave
// every classification table in this file passing for the wrong reason.
func TestEvidenceTallies(t *testing.T) {
	failedIn := []runner.Config{
		{GOMAXPROCS: 4, ShuffleSeed: 1},
		{GOMAXPROCS: 4, Race: true},
	}
	passedIn := []runner.Config{
		{GOMAXPROCS: 1},
		{GOMAXPROCS: 1},
		{GOMAXPROCS: 2, ShuffleSeed: 2},
	}
	_, ev := dependenceOf(failedIn, passedIn)

	tests := []struct {
		name string
		got  Rate
		want Rate
	}{
		{"shuffled", ev.Shuffled, Rate{Fail: 1, Obs: 2}},
		{"unshuffled", ev.Unshuffled, Rate{Fail: 1, Obs: 3}},
		{"raced", ev.Raced, Rate{Fail: 1, Obs: 1}},
		{"unraced", ev.Unraced, Rate{Fail: 1, Obs: 4}},
		{"lowest GOMAXPROCS", ev.LowestProcs(), Rate{Fail: 0, Obs: 2}},
		{"above the lowest", ev.HigherProcs(), Rate{Fail: 2, Obs: 3}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.got.Fail != tc.want.Fail || tc.got.Obs != tc.want.Obs {
				t.Errorf("%s = %d/%d, want %d/%d", tc.name, tc.got.Fail, tc.got.Obs, tc.want.Fail, tc.want.Obs)
			}
		})
	}

	want := []int{1, 2, 4}
	if len(ev.ByGOMAXPROCS) != len(want) {
		t.Fatalf("ByGOMAXPROCS = %v, want one entry per processor count %v", ev.ByGOMAXPROCS, want)
	}
	for i, procs := range want {
		if ev.ByGOMAXPROCS[i].GOMAXPROCS != procs {
			t.Errorf("ByGOMAXPROCS[%d] is GOMAXPROCS=%d, want %d ascending", i, ev.ByGOMAXPROCS[i].GOMAXPROCS, procs)
		}
	}
}

// TestHigherThan walks the three conditions one at a time. Each row is decided
// by exactly one of them, with the other two satisfied, so removing a condition
// breaks a specific row rather than the whole table.
func TestHigherThan(t *testing.T) {
	tests := []struct {
		name   string
		hi, lo Rate
		want   bool
	}{
		{
			name: "all three conditions met",
			hi:   Rate{Fail: 14, Obs: 28}, lo: Rate{Fail: 0, Obs: 28},
			want: true,
		},
		{
			name: "observations: the rate is 100% but there are three of them",
			hi:   Rate{Fail: 3, Obs: 3}, lo: Rate{Fail: 0, Obs: 30},
			want: false,
		},
		{
			name: "observations: the control arm is the thin one",
			hi:   Rate{Fail: 30, Obs: 60}, lo: Rate{Fail: 0, Obs: 3},
			want: false,
		},
		{
			name: "materiality: a real difference that is not a doubling",
			hi:   Rate{Fail: 590, Obs: 1000}, lo: Rate{Fail: 500, Obs: 1000},
			want: false,
		},
		{
			name: "noise: a doubling that four observations could have produced",
			hi:   Rate{Fail: 2, Obs: 4}, lo: Rate{Fail: 1, Obs: 4},
			want: false,
		},
		{
			name: "the same doubling, with the observations to support it",
			hi:   Rate{Fail: 200, Obs: 400}, lo: Rate{Fail: 100, Obs: 400},
			want: true,
		},
		{
			name: "2% against 0%, which is the wild case: not enough runs",
			hi:   Rate{Fail: 2, Obs: 100}, lo: Rate{Fail: 0, Obs: 100},
			want: false,
		},
		{
			name: "2% against 0%, with the runs to see it",
			hi:   Rate{Fail: 15, Obs: 750}, lo: Rate{Fail: 0, Obs: 250},
			want: true,
		},
		{
			name: "lower is not higher",
			hi:   Rate{Fail: 0, Obs: 100}, lo: Rate{Fail: 40, Obs: 100},
			want: false,
		},
		{
			name: "identical arms",
			hi:   Rate{Fail: 50, Obs: 100}, lo: Rate{Fail: 50, Obs: 100},
			want: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := higherThan(tc.hi, tc.lo); got != tc.want {
				t.Errorf("higherThan(%v, %v) = %v, want %v", tc.hi, tc.lo, got, tc.want)
			}
		})
	}
}

// TestResolution is the number the report prints beside an undetermined
// verdict. It has to be the truth about the run rather than a comfortable
// figure: a run of twenty configurations could not have resolved one in ten,
// and saying it could would be worse than saying nothing.
func TestResolution(t *testing.T) {
	tests := []struct {
		name   string
		hi, lo Rate
		wantOK bool
		want   float64
	}{
		{"the default run count", Rate{Obs: 8}, Rate{Obs: 12}, true, 3.0 / 8.0},
		{"sixty configurations", Rate{Obs: 28}, Rate{Obs: 32}, true, 4.0 / 28.0},
		{"a thousand configurations", Rate{Obs: 498}, Rate{Obs: 502}, true, 4.0 / 498.0},
		{"the load axis at a thousand", Rate{Obs: 751}, Rate{Obs: 249}, true, 12.0 / 751.0},
		{"too few observations to resolve anything", Rate{Obs: 3}, Rate{Obs: 3}, false, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := resolution(tc.hi, tc.lo)
			if ok != tc.wantOK {
				t.Fatalf("resolution(%v, %v) ok = %v, want %v", tc.hi, tc.lo, ok, tc.wantOK)
			}
			if ok && got != tc.want {
				t.Errorf("resolution(%v, %v) = %.4f, want %.4f", tc.hi, tc.lo, got, tc.want)
			}
		})
	}
}

// TestResolutionIsAchievable is the assertion that makes the printed number
// mean something: a run really would have reported a failure rate at the
// resolution it claims, and really would not have reported one just below it.
func TestResolutionIsAchievable(t *testing.T) {
	for _, arms := range [][2]int{{8, 12}, {28, 32}, {98, 102}, {751, 249}} {
		hi, lo := Rate{Obs: arms[0]}, Rate{Obs: arms[1]}
		res, ok := resolution(hi, lo)
		if !ok {
			t.Fatalf("resolution(%v, %v) reported nothing", hi, lo)
		}
		at := int(res*float64(hi.Obs) + 0.5)
		if !higherThan(Rate{Fail: at, Obs: hi.Obs}, lo) {
			t.Errorf("%d/%d against 0/%d is the claimed resolution but is not reported",
				at, hi.Obs, lo.Obs)
		}
		if at > 1 && higherThan(Rate{Fail: at - 1, Obs: hi.Obs}, lo) {
			t.Errorf("%d/%d against 0/%d is below the claimed resolution and is still reported",
				at-1, hi.Obs, lo.Obs)
		}
	}
}

// ---------------------------------------------------------------------------
// The four cases the previous classifier got wrong
// ---------------------------------------------------------------------------

// replayer answers a configuration with a recorded stream, parsing each stream
// once and handing the same parsed run back for every replicate of it.
//
// The replicates are the point. A rate needs repeated observations under one
// configuration, so a fixture that expresses "fails 2% of the time at four
// processors" answers 4 of 200 four-processor configurations with the recording
// of a run that failed and the other 196 with the recording of a run that
// passed - both of them made under that exact configuration (CLAUDE.md rule 5,
// and see PROVENANCE.md for why two recordings of one configuration is what
// rule 5 looks like for a nondeterministic fixture).
type replayer struct {
	t     *testing.T
	cache map[string]*gotest.Run
}

func newReplayer(t *testing.T) *replayer {
	t.Helper()
	return &replayer{t: t, cache: make(map[string]*gotest.Run)}
}

func (r *replayer) parse(stream string) *gotest.Run {
	r.t.Helper()
	if run, ok := r.cache[stream]; ok {
		return run
	}
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", "streams", stream))
	if err != nil {
		r.t.Fatalf("reading recorded stream: %v", err)
	}
	run, err := gotest.ParseBytes(b)
	if err != nil {
		r.t.Fatalf("parsing %s: %v", stream, err)
	}
	r.cache[stream] = run
	return run
}

// times returns n results for cfg, all replaying stream.
func (r *replayer) times(cfg runner.Config, stream string, n int) []runner.Result {
	r.t.Helper()
	run := r.parse(stream)
	out := make([]runner.Result, n)
	for i := range out {
		out[i] = runner.Result{Config: cfg, Outcome: runner.OutcomeCompleted, Run: run}
	}
	return out
}

func results(groups ...[]runner.Result) []runner.Result {
	var out []runner.Result
	for _, g := range groups {
		out = append(out, g...)
	}
	return out
}

// The configurations the recordings were made under, named so that a mismatch
// between a configuration and its stream is visible at the call site.
var (
	cfgP1          = runner.Config{GOMAXPROCS: 1, Count: 1}
	cfgP2          = runner.Config{GOMAXPROCS: 2, Count: 1}
	cfgP4          = runner.Config{GOMAXPROCS: 4, Count: 1}
	cfgP1Shuffled1 = runner.Config{GOMAXPROCS: 1, ShuffleSeed: 1, Count: 1}
	cfgP2Shuffled1 = runner.Config{GOMAXPROCS: 2, ShuffleSeed: 1, Count: 1}
	cfgP4Shuffled1 = runner.Config{GOMAXPROCS: 4, ShuffleSeed: 1, Count: 1}
)

// orderDependentResults: TestOrderDependent fails whenever seed 1 permutes
// TestPoisonsGlobalState ahead of it, and passes whenever it does not, at every
// processor count. Ten replicates per cell, sixty configurations.
func orderDependentResults(r *replayer) []runner.Result {
	const perCell = 10
	return results(
		r.times(cfgP1Shuffled1, "orderfail.json", perCell),
		r.times(cfgP2Shuffled1, "orderload2.json", perCell),
		r.times(cfgP4Shuffled1, "orderload.json", perCell),
		r.times(cfgP1, "singleproc.json", perCell),
		r.times(cfgP2, "loadfail2.json", perCell),
		r.times(cfgP4, "loadfail.json", perCell),
	)
}

// loadDependentResults: TestLoadDependent fails at two and four processors and
// passes at one, whatever the test order. Same sixty configurations.
func loadDependentResults(r *replayer) []runner.Result { return orderDependentResults(r) }

// probabilisticLoadResults: TestProbabilisticLoad fails at roughly 2% above one
// processor and never at one processor, spread evenly across shuffled and
// unshuffled configurations.
//
// Eight hundred configurations, because that is what 2% costs. The matrix
// comment in internal/runner states the same thing from the other direction:
// at --runs 20 nothing below about a third is visible.
func probabilisticLoadResults(r *replayer) []runner.Result {
	const perCell = 200
	const failsPerLoadedCell = 4 // 4/200 = 2%
	return results(
		r.times(cfgP1, "probload-p1.json", perCell),
		r.times(cfgP1Shuffled1, "probload-p1-shuffled.json", perCell),
		r.times(cfgP4, "probload-p4-fail.json", failsPerLoadedCell),
		r.times(cfgP4, "probload-p4-pass.json", perCell-failsPerLoadedCell),
		r.times(cfgP4Shuffled1, "probload-p4-shuffled-fail.json", failsPerLoadedCell),
		r.times(cfgP4Shuffled1, "probload-p4-shuffled-pass.json", perCell-failsPerLoadedCell),
	)
}

// oneFailureInSixtyResults: the same fixture, at the run count a person
// actually types. One failure, sixty configurations, and nothing to say.
func oneFailureInSixtyResults(r *replayer) []runner.Result {
	const perCell = 15
	return results(
		r.times(cfgP1, "probload-p1.json", perCell),
		r.times(cfgP1Shuffled1, "probload-p1-shuffled.json", perCell),
		r.times(cfgP4, "probload-p4-fail.json", 1),
		r.times(cfgP4, "probload-p4-pass.json", perCell-1),
		r.times(cfgP4Shuffled1, "probload-p4-shuffled-pass.json", perCell),
	)
}

// wildShapeResults is the shape the OLD matrix produced, with the failure rate
// that was actually observed in the wild: sixty configurations of which 56 are
// shuffled and 4 are not, and one failure.
//
// This is the fixture the previous classifier misclassifies. Its order rule
// asked two questions - were all the failures shuffled, and did anything
// unshuffled pass - and on this input both are yes, so it returned
// order-dependent. Both remain yes here. What changed is that they are no
// longer taken as evidence: one failure in 56 shuffled runs against none in 4
// unshuffled ones is a difference two coin flips would supply.
func wildShapeResults(r *replayer) []runner.Result {
	return results(
		r.times(cfgP4Shuffled1, "probload-p4-shuffled-fail.json", 1),
		r.times(cfgP4Shuffled1, "probload-p4-shuffled-pass.json", 55),
		r.times(cfgP4, "probload-p4-pass.json", 2),
		r.times(cfgP1, "probload-p1.json", 2),
	)
}

// TestClassifiesRecordedStreamsByRate is this version's exit criterion.
//
// Every case here is one the previous implementation got wrong, and the first
// three of them are wrong in the same direction: it labelled them
// order-dependent, because it asked whether every failure had been shuffled
// rather than whether shuffling made failure more likely. The fourth is the one
// it answered with a label where the honest answer is that sixty runs cannot
// see a failure that happens 2% of the time.
func TestClassifiesRecordedStreamsByRate(t *testing.T) {
	tests := []struct {
		name   string
		build  func(*replayer) []runner.Result
		test   string
		want   Dependence
		wantEv func(t *testing.T, ev Evidence)
	}{
		{
			name:  "genuinely order-dependent, at every GOMAXPROCS",
			build: orderDependentResults,
			test:  "TestOrderDependent",
			want:  DependenceOrder,
			wantEv: func(t *testing.T, ev Evidence) {
				wantRate(t, "shuffled", ev.Shuffled, 30, 30)
				wantRate(t, "unshuffled", ev.Unshuffled, 0, 30)
				// The load axis must stay silent: every processor count fails
				// at the same rate, because what moves is the seed.
				for _, p := range ev.ByGOMAXPROCS {
					wantRate(t, p.Rate.Label, p.Rate, 10, 20)
				}
				if ev.LoadRises() {
					t.Error("the load axis claims a rise where every processor count fails at 10/20")
				}
			},
		},
		{
			name:  "genuinely load-dependent, deterministic threshold",
			build: loadDependentResults,
			test:  "TestLoadDependent",
			want:  DependenceLoad,
			wantEv: func(t *testing.T, ev Evidence) {
				wantRate(t, "GOMAXPROCS=1", ev.LowestProcs(), 0, 20)
				wantRate(t, "above GOMAXPROCS=1", ev.HigherProcs(), 40, 40)
				if !ev.ProcsThreshold() {
					t.Error("a clean threshold - nothing fails at one processor, everything fails above it - was not recognised as one")
				}
				// And the order axis must stay silent, even though half the
				// failures were shuffled.
				wantRate(t, "shuffled", ev.Shuffled, 20, 30)
				wantRate(t, "unshuffled", ev.Unshuffled, 20, 30)
				if ev.OrderRises() {
					t.Error("the order axis claims a rise between two arms that fail at 20/30 apiece")
				}
			},
		},
		{
			name:  "genuinely load-dependent, probabilistic at 2%",
			build: probabilisticLoadResults,
			test:  "TestProbabilisticLoad",
			want:  DependenceLoad,
			wantEv: func(t *testing.T, ev Evidence) {
				wantRate(t, "GOMAXPROCS=1", ev.LowestProcs(), 0, 400)
				wantRate(t, "above GOMAXPROCS=1", ev.HigherProcs(), 8, 400)
				// THERE IS NO THRESHOLD HERE. Four processors both passes and
				// fails, so the rule that survives from the old classifier
				// cannot see this case at all; the rate comparison is what
				// finds it.
				if ev.ProcsThreshold() {
					t.Error("a rate of 8/400 above the threshold was read as a clean threshold")
				}
				wantRate(t, "shuffled", ev.Shuffled, 4, 400)
				wantRate(t, "unshuffled", ev.Unshuffled, 4, 400)
				if ev.OrderRises() {
					t.Error("the order axis claims a rise in a fixture whose runs contain one test, where no test order exists")
				}
			},
		},
		{
			// The one the tool got wrong in the wild, reproduced exactly: a
			// lopsided matrix, one failure, and the failure happened to land in
			// the arm that holds 93% of the runs.
			name:  "the old matrix's shape: one failure among 56 shuffled runs",
			build: wildShapeResults,
			test:  "TestProbabilisticLoad",
			want:  DependenceUndetermined,
			wantEv: func(t *testing.T, ev Evidence) {
				wantRate(t, "shuffled", ev.Shuffled, 1, 56)
				wantRate(t, "unshuffled", ev.Unshuffled, 0, 4)
				// The old rule's premise, asserted rather than described: it
				// holds here, and it is still not evidence. If this assertion
				// ever fails, the fixture has stopped being the case that
				// produced the wrong label.
				if ev.Shuffled.Fail != ev.Shuffled.Fail+ev.Unshuffled.Fail {
					t.Error("some failure was unshuffled; this is no longer the fixture the old rule claimed")
				}
				if ev.Unshuffled.Obs == ev.Unshuffled.Fail {
					t.Error("no unshuffled run passed; this is no longer the fixture the old rule claimed")
				}
			},
		},
		{
			name:  "one failure in sixty supports no claim at all",
			build: oneFailureInSixtyResults,
			test:  "TestProbabilisticLoad",
			want:  DependenceUndetermined,
			wantEv: func(t *testing.T, ev Evidence) {
				wantRate(t, "GOMAXPROCS=1", ev.LowestProcs(), 0, 30)
				wantRate(t, "above GOMAXPROCS=1", ev.HigherProcs(), 1, 30)
				res, ok := ev.Resolution()
				if !ok {
					t.Fatal("sixty configurations resolved nothing at all, not even a bound")
				}
				// The bound has to be honest about the case above: this run
				// could not have seen 2%, which is why it declines rather than
				// concluding the test is order-independent or load-independent.
				if res <= 0.02 {
					t.Errorf("sixty configurations claim to resolve %.1f%%, which would have caught the 2%% fixture", res*100)
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rep := Build(fixturePkg, cfgP4, tc.build(newReplayer(t)))
			got := testByName(t, rep, tc.test)
			if got.Class != ClassFlaky {
				t.Fatalf("%s is %v, want flaky (%d passed, %d failed)", tc.test, got.Class, got.Pass, got.Fail)
			}
			if got.Dependence != tc.want {
				t.Errorf("%s = %v, want %v\n  shuffled   %v\n  unshuffled %v\n  by procs   %v",
					tc.test, got.Dependence, tc.want, got.Evidence.Shuffled, got.Evidence.Unshuffled, got.Evidence.ByGOMAXPROCS)
			}
			tc.wantEv(t, got.Evidence)
		})
	}
}

func wantRate(t *testing.T, name string, got Rate, fail, obs int) {
	t.Helper()
	if got.Fail != fail || got.Obs != obs {
		t.Errorf("%s = %d/%d, want %d/%d", name, got.Fail, got.Obs, fail, obs)
	}
}

// TestProbabilisticLoadIsInvisibleToAThreshold states the difference between
// the two load fixtures in one assertion, because it is the difference the
// whole rewrite turns on.
//
// The deterministic fixture has a clean threshold and the old rule could have
// caught it, had the order rule not been checked first. The probabilistic one
// has no threshold at all: four processors passes 196 times and fails 4 times,
// so "every failure had strictly more processors than every pass" is false, and
// no amount of reordering the old rules would have found it.
func TestProbabilisticLoadIsInvisibleToAThreshold(t *testing.T) {
	r := newReplayer(t)

	deterministic := testByName(t, Build(fixturePkg, cfgP4, loadDependentResults(r)), "TestLoadDependent")
	if !deterministic.Evidence.ProcsThreshold() {
		t.Error("the deterministic fixture has a clean GOMAXPROCS threshold and the threshold rule missed it")
	}

	probabilistic := testByName(t, Build(fixturePkg, cfgP4, probabilisticLoadResults(r)), "TestProbabilisticLoad")
	if probabilistic.Evidence.ProcsThreshold() {
		t.Fatal("the probabilistic fixture has no threshold; reporting one means the rule is not reading the passes")
	}
	if probabilistic.Dependence != DependenceLoad {
		t.Errorf("the probabilistic fixture = %v, want %v; it is only reachable through the rate comparison",
			probabilistic.Dependence, DependenceLoad)
	}
}

// TestSixtyRunsCannotSeeTwoPercent is the pair of fixtures read the other way
// round: the same test, the same failure mechanism, and the only difference is
// how many configurations were run.
//
// This is the assertion behind the sensitivity line the report prints. A tool
// that said "undetermined" at sixty runs and "load-dependent" at eight hundred
// without ever saying why would look like it was guessing in both.
func TestSixtyRunsCannotSeeTwoPercent(t *testing.T) {
	r := newReplayer(t)

	short := testByName(t, Build(fixturePkg, cfgP4, oneFailureInSixtyResults(r)), "TestProbabilisticLoad")
	long := testByName(t, Build(fixturePkg, cfgP4, probabilisticLoadResults(r)), "TestProbabilisticLoad")

	if short.Dependence != DependenceUndetermined {
		t.Errorf("sixty configurations produced %v; one failure is not evidence of anything", short.Dependence)
	}
	if long.Dependence != DependenceLoad {
		t.Errorf("eight hundred configurations produced %v, want %v", long.Dependence, DependenceLoad)
	}

	shortRes, ok := short.Evidence.Resolution()
	if !ok {
		t.Fatal("the short run reported no resolution")
	}
	longRes, ok := long.Evidence.Resolution()
	if !ok {
		t.Fatal("the long run reported no resolution")
	}
	if !(longRes < 0.02 && shortRes > 0.02) {
		t.Errorf("resolutions are %.1f%% at sixty runs and %.1f%% at eight hundred; "+
			"the 2%% fixture must sit between them or the printed bound is not the truth about the run",
			shortRes*100, longRes*100)
	}
}

func TestExitCode(t *testing.T) {
	tests := []struct {
		name string
		rep  Report
		want int
	}{
		{
			name: "nothing flaky",
			rep:  Report{Configurations: 20, Completed: 20, Tests: []Test{{Class: ClassNeverFails}}},
			want: ExitClean,
		},
		{
			name: "an always-failing test is not a flakiness finding",
			rep:  Report{Configurations: 20, Completed: 20, Tests: []Test{{Class: ClassAlwaysFails}}},
			want: ExitClean,
		},
		{
			name: "a flaky test",
			rep:  Report{Configurations: 20, Completed: 20, Tests: []Test{{Class: ClassFlaky}}},
			want: ExitFlaky,
		},
		{
			name: "a build failure with no tests is a tool failure, not a finding",
			rep:  Report{Configurations: 20, Completed: 20, BuildFailed: true},
			want: ExitToolFailure,
		},
		{
			name: "a build failure that still produced tests is classified by those tests",
			rep:  Report{Configurations: 20, Completed: 20, BuildFailed: true, Tests: []Test{{Class: ClassFlaky}}},
			want: ExitFlaky,
		},
		{
			name: "nothing completed means nothing was learned",
			rep:  Report{Configurations: 20, Completed: 0, TimedOut: 20},
			want: ExitToolFailure,
		},
		{
			name: "an empty report with no configurations is clean, not a failure",
			rep:  Report{},
			want: ExitClean,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.rep.ExitCode(); got != tc.want {
				t.Errorf("ExitCode() = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestBuildFailureProducesNoFindings replays the recorded build failure. Nothing
// compiled, so there are no tests, nothing is flaky, and the exit code is 2.
func TestBuildFailureProducesNoFindings(t *testing.T) {
	rep := Build(fixturePkg, runner.Default(), []runner.Result{
		result(t, cfgSingleP, "buildfail.json"),
		result(t, cfgFourP, "buildfail.json"),
	})

	if !rep.BuildFailed {
		t.Fatal("BuildFailed = false, want true")
	}
	if len(rep.Tests) != 0 {
		t.Errorf("a build failure produced %d test findings, want 0: %v", len(rep.Tests), names(rep.Tests))
	}
	if got := rep.ExitCode(); got != ExitToolFailure {
		t.Errorf("ExitCode() = %d, want %d; a build failure is not a flakiness finding", got, ExitToolFailure)
	}
	// Both configurations failed to build identically. The diagnostic is
	// reported once, not once per configuration.
	if got := strings.Count(strings.Join(rep.BuildOutput, ""), "cannot use 42"); got != 1 {
		t.Errorf("the compiler diagnostic appears %d times, want exactly 1: %v", got, rep.BuildOutput)
	}
}

func resultFromJSON(t *testing.T, cfg runner.Config, stream string) runner.Result {
	t.Helper()
	run, err := gotest.ParseBytes([]byte(stream))
	if err != nil {
		t.Fatalf("parsing stream: %v", err)
	}
	return runner.Result{Config: cfg, Outcome: runner.OutcomeCompleted, Run: run}
}

func testByPkgName(t *testing.T, rep Report, pkg, name string) Test {
	t.Helper()
	for _, e := range rep.Tests {
		if e.Package == pkg && e.Name == name {
			return e
		}
	}
	t.Fatalf("test %s.%s not in report; have %v", pkg, name, names(rep.Tests))
	return Test{}
}

// TestSameNamedTestsInDifferentPackagesAreNotMerged is the fixture that breaks
// a report that keys tests by Name alone. An always-passing TestNew next to an
// always-failing TestNew is two tests, not one flake.
func TestSameNamedTestsInDifferentPackagesAreNotMerged(t *testing.T) {
	const stream = `{"Action":"run","Package":"example.com/pass","Test":"TestNew"}
{"Action":"pass","Package":"example.com/pass","Test":"TestNew"}
{"Action":"pass","Package":"example.com/pass"}
{"Action":"run","Package":"example.com/fail","Test":"TestNew"}
{"Action":"fail","Package":"example.com/fail","Test":"TestNew"}
{"Action":"fail","Package":"example.com/fail"}
`
	rep := Build("./...", runner.Default(), []runner.Result{
		resultFromJSON(t, cfgSingleP, stream),
		resultFromJSON(t, cfgFourP, stream),
	})

	if got := len(rep.Tests); got != 2 {
		t.Fatalf("tests = %d, want 2 (one per package); got %v", got, names(rep.Tests))
	}
	pass := testByPkgName(t, rep, "example.com/pass", "TestNew")
	if pass.Class != ClassNeverFails || pass.Pass != 2 || pass.Fail != 0 {
		t.Errorf("example.com/pass.TestNew = class %v pass/fail %d/%d, want never-fails 2/0",
			pass.Class, pass.Pass, pass.Fail)
	}
	fail := testByPkgName(t, rep, "example.com/fail", "TestNew")
	if fail.Class != ClassAlwaysFails || fail.Pass != 0 || fail.Fail != 2 {
		t.Errorf("example.com/fail.TestNew = class %v pass/fail %d/%d, want always-fails 0/2",
			fail.Class, fail.Pass, fail.Fail)
	}
	if got := rep.ExitCode(); got != ExitClean {
		t.Errorf("ExitCode() = %d, want %d; merging the two TestNews would invent a flake", got, ExitClean)
	}

	var b strings.Builder
	if err := rep.WriteText(&b, false); err != nil {
		t.Fatalf("WriteText: %v", err)
	}
	got := b.String()
	if !strings.Contains(got, "example.com/fail.TestNew") {
		t.Errorf("text report does not name the failing package:\n%s", got)
	}
	if strings.Contains(got, "FLAKY") {
		t.Errorf("text report invented a flake from two same-named tests:\n%s", got)
	}
}

// TestPartialBuildFailureKeepsCompiledPackages is the fixture that breaks a
// report which treats any compile error as "this configuration produced no
// tests". go test ./... still emits results for every package that compiled.
func TestPartialBuildFailureKeepsCompiledPackages(t *testing.T) {
	const stream = `{"Action":"build-output","ImportPath":"example.com/broken","Output":"broken.go:1: cannot use 42\n"}
{"Action":"build-fail","ImportPath":"example.com/broken"}
{"Action":"fail","Package":"example.com/broken","FailedBuild":"example.com/broken"}
{"Action":"run","Package":"example.com/ok","Test":"TestOK"}
{"Action":"pass","Package":"example.com/ok","Test":"TestOK"}
{"Action":"pass","Package":"example.com/ok"}
`
	rep := Build("./...", runner.Default(), []runner.Result{
		resultFromJSON(t, cfgSingleP, stream),
		resultFromJSON(t, cfgFourP, stream),
	})

	if !rep.BuildFailed {
		t.Fatal("BuildFailed = false, want true")
	}
	ok := testByPkgName(t, rep, "example.com/ok", "TestOK")
	if ok.Class != ClassNeverFails || ok.Pass != 2 {
		t.Errorf("example.com/ok.TestOK = class %v pass %d, want never-fails 2", ok.Class, ok.Pass)
	}
	if got := rep.ExitCode(); got != ExitClean {
		t.Errorf("ExitCode() = %d, want %d; a compile error in one package must not discard the rest",
			got, ExitClean)
	}

	var b strings.Builder
	if err := rep.WriteText(&b, false); err != nil {
		t.Fatalf("WriteText: %v", err)
	}
	got := b.String()
	if !strings.Contains(got, "BUILD FAILED") {
		t.Errorf("text report dropped the compile error:\n%s", got)
	}
	if !strings.Contains(got, "cannot use 42") {
		t.Errorf("text report dropped the compiler diagnostic:\n%s", got)
	}
	if !strings.Contains(got, "No flaky tests found.") {
		t.Errorf("text report did not report the packages that compiled:\n%s", got)
	}
}

// TestBuildNeverStartedResultsAreNotCompleted is the fixture that breaks a
// Build which treats the zero Result as OutcomeCompleted. The runner leaves
// that value for configurations never dispatched after cancel; counting
// those as completed makes ExitCode claim a clean matrix for a run that
// learned nothing.
func TestBuildNeverStartedResultsAreNotCompleted(t *testing.T) {
	good := result(t, cfgFourP, "allpass.json")
	tests := []struct {
		name          string
		results       []runner.Result
		wantCompleted int
		wantCode      int
	}{
		{
			name:          "all never started",
			results:       make([]runner.Result, 4),
			wantCompleted: 0,
			wantCode:      ExitToolFailure,
		},
		{
			name: "in-flight errors and never-started slots",
			results: []runner.Result{
				{Config: cfgSingleP, Outcome: runner.OutcomeError, Err: os.ErrClosed},
				{},
				{},
			},
			wantCompleted: 0,
			wantCode:      ExitToolFailure,
		},
		{
			name: "a real completion is still counted",
			results: []runner.Result{
				good,
				{},
				{},
			},
			wantCompleted: 1,
			wantCode:      ExitClean,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rep := Build(fixturePkg, runner.Default(), tc.results)
			if rep.Completed != tc.wantCompleted {
				t.Errorf("Completed = %d, want %d; never-started slots are not completions",
					rep.Completed, tc.wantCompleted)
			}
			if got := rep.ExitCode(); got != tc.wantCode {
				t.Errorf("ExitCode() = %d, want %d", got, tc.wantCode)
			}
		})
	}
}

// TestTimeoutsAndErrorsProduceNoEvidence: a configuration that timed out is
// neither a pass nor a failure, so it cannot move a failure rate.
func TestTimeoutsAndErrorsProduceNoEvidence(t *testing.T) {
	good := result(t, cfgFourP, "loadfail.json")
	rep := Build(fixturePkg, runner.Default(), []runner.Result{
		good,
		{Config: cfgSingleP, Outcome: runner.OutcomeTimedOut},
		{Config: cfgShuffled, Outcome: runner.OutcomeError, Err: os.ErrNotExist},
	})

	if rep.Completed != 1 || rep.TimedOut != 1 || rep.Errored != 1 {
		t.Fatalf("completed/timedout/errored = %d/%d/%d, want 1/1/1", rep.Completed, rep.TimedOut, rep.Errored)
	}
	load := testByName(t, rep, "TestLoadDependent")
	if load.Observations() != 1 {
		t.Errorf("observations = %d, want 1; the timeout and the error must not count",
			load.Observations())
	}
	if load.Class != ClassAlwaysFails {
		t.Errorf("class = %v, want always-fails: one failure and no passes is not flaky", load.Class)
	}
	if got := rep.ExitCode(); got != ExitClean {
		t.Errorf("ExitCode() = %d, want %d", got, ExitClean)
	}
}

// TestJSONSchema pins the field names. From v1.0.0 this is a compatibility
// surface and changes must be additive (CLAUDE.md rule 3), so the names are
// asserted rather than left to whatever the structs happen to be called.
func TestJSONSchema(t *testing.T) {
	rep := fixtureReport(t)
	b, err := json.Marshal(rep)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{
		"package", "configurations", "completed", "timed_out", "errored",
		"build_failed", "exit_code", "base", "tests",
	} {
		if _, ok := doc[key]; !ok {
			t.Errorf("report is missing field %q", key)
		}
	}

	tests, ok := doc["tests"].([]any)
	if !ok || len(tests) == 0 {
		t.Fatalf("tests = %v, want a non-empty array", doc["tests"])
	}

	var flakyEntry map[string]any
	for _, raw := range tests {
		entry := raw.(map[string]any)
		for _, key := range []string{
			"package", "name", "pass", "fail", "skip", "incomplete",
			"failure_rate", "classification",
		} {
			if _, ok := entry[key]; !ok {
				t.Errorf("test entry %v is missing field %q", entry["name"], key)
			}
		}
		if entry["classification"] == "flaky" {
			flakyEntry = entry
		}
	}
	if flakyEntry == nil {
		t.Fatal("no flaky entry in the JSON report")
	}
	if _, ok := flakyEntry["dependence"]; !ok {
		t.Error("a flaky entry has no dependence field")
	}
	min, ok := flakyEntry["minimal_config"].(map[string]any)
	if !ok {
		t.Fatalf("a flaky entry has no minimal_config: %v", flakyEntry)
	}
	for _, key := range []string{"shuffle_seed", "gomaxprocs", "race", "count", "command_line"} {
		if _, ok := min[key]; !ok {
			t.Errorf("minimal_config is missing field %q", key)
		}
	}
}

func TestWriteText(t *testing.T) {
	tests := []struct {
		name        string
		rep         Report
		verbose     bool
		wantContain []string
		wantAbsent  []string
	}{
		{
			name:    "the fixture report",
			rep:     fixtureReport(t),
			verbose: false,
			wantContain: []string{
				"FLAKY (2)",
				"TestOrderDependent",
				"TestLoadDependent",
				// Three configurations name no knob, and the report says how
				// blind it was rather than guessing.
				"undetermined",
				"too few observations on either axis",
				"minimal repro: GOMAXPROCS=1 go test -shuffle=1 -count=1 " + fixturePkg,
				"ALWAYS FAILS (1)",
				"deterministic, not flaky",
				"TestAlwaysFails",
			},
			// The always-failing test must not appear inside the flaky section.
			wantAbsent: []string{"| "},
		},
		{
			name:        "verbose includes failure output",
			rep:         fixtureReport(t),
			verbose:     true,
			wantContain: []string{"| ", "global state was poisoned"},
		},
		{
			name: "a clean report says so",
			rep: Build(fixturePkg, runner.Default(), []runner.Result{
				result(t, cfgSingleP, "allpass.json"),
			}),
			wantContain: []string{"No flaky tests found."},
			wantAbsent:  []string{"FLAKY (", "ALWAYS FAILS ("},
		},
		{
			name: "a build failure explains that nothing was measured",
			rep: Build(fixturePkg, runner.Default(), []runner.Result{
				result(t, cfgSingleP, "buildfail.json"),
			}),
			wantContain: []string{"BUILD FAILED", "cannot use 42"},
			wantAbsent:  []string{"FLAKY (", "No flaky tests found."},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var b strings.Builder
			if err := tc.rep.WriteText(&b, tc.verbose); err != nil {
				t.Fatalf("WriteText: %v", err)
			}
			got := b.String()
			for _, want := range tc.wantContain {
				if !strings.Contains(got, want) {
					t.Errorf("output missing %q:\n%s", want, got)
				}
			}
			for _, absent := range tc.wantAbsent {
				if strings.Contains(got, absent) {
					t.Errorf("output unexpectedly contains %q:\n%s", absent, got)
				}
			}
		})
	}
}

// TestReportOrderIsStable: two builds of the same results print identically, or
// the tool cannot be diffed between runs.
func TestReportOrderIsStable(t *testing.T) {
	var first string
	for i := 0; i < 5; i++ {
		var b strings.Builder
		if err := fixtureReport(t).WriteText(&b, true); err != nil {
			t.Fatalf("WriteText: %v", err)
		}
		if i == 0 {
			first = b.String()
			continue
		}
		if b.String() != first {
			t.Fatal("two reports built from identical results differ")
		}
	}
}

// The two configurations the load-dependent failure was recorded under. Its
// message names the processor count, so these two recordings carry textually
// different failures from one cause - which is exactly the case clustering has
// to have an answer for.
var cfgTwoP = runner.Config{GOMAXPROCS: 2, Count: 1}

// clusteredReport pairs every configuration with the recording made under that
// exact configuration (CLAUDE.md rule 5). Answering the two-processor
// configuration with the four-processor recording would put a failure that says
// GOMAXPROCS=4 next to a repro line that says GOMAXPROCS=2.
func clusteredReport(t *testing.T) Report {
	t.Helper()
	base := runner.Config{GOMAXPROCS: 4, Count: 1}
	return Build(fixturePkg, base, []runner.Result{
		result(t, cfgFourP, "loadfail.json"),
		result(t, cfgTwoP, "loadfail2.json"),
		result(t, cfgShuffled, "orderfail.json"),
		result(t, cfgSingleP, "singleproc.json"),
	})
}

// TestClustering is the v0.2.0 exit criterion at the report level.
func TestClustering(t *testing.T) {
	rep := clusteredReport(t)

	tests := []struct {
		name         string
		pins         string
		test         string
		wantClusters int
		wantCounts   []int
	}{
		{
			name: "one cause reported two ways is two clusters",
			pins: "PREFER SPLITTING: the message names the processor count and integers " +
				"in messages are not normalized, so this one bug splits - visibly",
			test:         "TestLoadDependent",
			wantClusters: 2,
			wantCounts:   []int{1, 1},
		},
		{
			name:         "one failure mode is one cluster",
			pins:         "the common case does not fragment",
			test:         "TestOrderDependent",
			wantClusters: 1,
			wantCounts:   []int{1},
		},
		{
			name:         "an always-failing test is clustered too",
			pins:         "clusters are computed for every failing test, not only the flaky ones",
			test:         "TestAlwaysFails",
			wantClusters: 1,
			wantCounts:   []int{4},
		},
		{
			name:         "a test that never failed has no clusters",
			pins:         "clustering does not invent a group for a passing test",
			test:         "TestAlwaysPasses",
			wantClusters: 0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := testByName(t, rep, tc.test)
			if len(got.Clusters) != tc.wantClusters {
				t.Fatalf("clusters = %d, want %d; this row pins that %s\n%s",
					len(got.Clusters), tc.wantClusters, tc.pins, describeClusters(got))
			}
			for i, want := range tc.wantCounts {
				if got.Clusters[i].Count != want {
					t.Errorf("cluster %d count = %d, want %d\n%s",
						i, got.Clusters[i].Count, want, describeClusters(got))
				}
			}
			sum := 0
			for _, c := range got.Clusters {
				sum += c.Count
			}
			if sum != got.Fail {
				t.Errorf("cluster counts sum to %d but the test failed %d times; a failure was dropped or double-counted",
					sum, got.Fail)
			}
		})
	}
}

func describeClusters(t Test) string {
	var b strings.Builder
	for _, c := range t.Clusters {
		fmt.Fprintf(&b, "  [%s] n=%d minimal=%s\n%s\n", c.Signature.Hash, c.Count, c.Minimal, c.Signature.Normalized)
	}
	return b.String()
}

// TestClusterMinimalityIsPerCluster is the reason clustering is worth having at
// all. TestLoadDependent has two failure modes; each has its own smallest
// reproducing configuration, and reporting one of them for both would hand the
// user a command line that cannot produce the failure printed beside it.
func TestClusterMinimalityIsPerCluster(t *testing.T) {
	got := testByName(t, clusteredReport(t), "TestLoadDependent")
	if len(got.Clusters) != 2 {
		t.Fatalf("clusters = %d, want 2\n%s", len(got.Clusters), describeClusters(got))
	}

	byProcs := map[int]Cluster{}
	for _, c := range got.Clusters {
		byProcs[c.Minimal.GOMAXPROCS] = c
	}
	for _, procs := range []int{2, 4} {
		c, ok := byProcs[procs]
		if !ok {
			t.Fatalf("no cluster whose minimal configuration is GOMAXPROCS=%d\n%s", procs, describeClusters(got))
		}
		// The representative output must come from the minimal configuration's
		// own run. If it came from any other run the report would print a
		// failure and a command line that do not go together.
		want := fmt.Sprintf("GOMAXPROCS=%d", procs)
		if !strings.Contains(strings.Join(c.Output, ""), want) {
			t.Errorf("cluster minimal at GOMAXPROCS=%d shows output that does not mention %q:\n%s",
				procs, want, strings.Join(c.Output, ""))
		}
	}

	// The test-level minimum is unchanged by clustering and is still chosen by
	// the same ordering: base here is GOMAXPROCS=4, so the four-processor
	// configuration changes no knobs at all and wins on the first tie-break,
	// ahead of the two-processor one. Collapsing the clusters would have
	// reported THAT configuration for both failure modes - and it cannot
	// produce the GOMAXPROCS=2 failure at all.
	if got.Minimal == nil || got.Minimal.GOMAXPROCS != 4 {
		t.Errorf("test-level minimal = %v, want GOMAXPROCS=4 (zero knobs changed from base)", got.Minimal)
	}
	if byProcs[2].Minimal.GOMAXPROCS == got.Minimal.GOMAXPROCS {
		t.Error("the two-processor cluster inherited the test-level minimum instead of its own")
	}
}

// TestClusterOrderIsStable: Go randomises map iteration, so a clustering built
// on a map has to sort before it is reported. Two builds of the same results
// must name their clusters in the same order.
func TestClusterOrderIsStable(t *testing.T) {
	first := testByName(t, clusteredReport(t), "TestLoadDependent")
	for i := 0; i < 20; i++ {
		again := testByName(t, clusteredReport(t), "TestLoadDependent")
		for j := range first.Clusters {
			if first.Clusters[j].Signature.Hash != again.Clusters[j].Signature.Hash {
				t.Fatalf("cluster order changed between builds at position %d: %s then %s",
					j, first.Clusters[j].Signature.Hash, again.Clusters[j].Signature.Hash)
			}
		}
	}
}

// TestClusterOrderIsByCount pins the ordering rule itself: most configurations
// first, so the failure mode a user is most likely to hit is the one they read
// first.
func TestClusterOrderIsByCount(t *testing.T) {
	base := runner.Config{GOMAXPROCS: 4, Count: 1}
	rep := Build(fixturePkg, base, []runner.Result{
		result(t, cfgTwoP, "loadfail2.json"),
		result(t, cfgFourP, "loadfail.json"),
		result(t, runner.Config{GOMAXPROCS: 4, ShuffleSeed: 1, Count: 1}, "orderload.json"),
		result(t, cfgSingleP, "singleproc.json"),
	})
	got := testByName(t, rep, "TestLoadDependent")
	if len(got.Clusters) != 2 {
		t.Fatalf("clusters = %d, want 2\n%s", len(got.Clusters), describeClusters(got))
	}
	// Two four-processor recordings, one two-processor one.
	if got.Clusters[0].Count != 2 || got.Clusters[1].Count != 1 {
		t.Errorf("cluster counts = %d then %d, want 2 then 1 (descending)\n%s",
			got.Clusters[0].Count, got.Clusters[1].Count, describeClusters(got))
	}
}

// TestTextReportRendersClusters pins the human output described in the step:
// a header with the hash and the count, then the representative failure, then
// the minimal configuration - and, for the common case, no cluster block at all.
func TestTextReportRendersClusters(t *testing.T) {
	var b strings.Builder
	if err := clusteredReport(t).WriteText(&b, false); err != nil {
		t.Fatalf("WriteText: %v", err)
	}
	out := b.String()

	tests := []struct {
		name string
		pins string
		want string
	}{
		{
			name: "the split test announces its cluster count",
			pins: "a user is told there is more than one failure mode",
			want: "2 distinct failure signatures:",
		},
		{
			name: "each cluster names its own repro",
			pins: "minimality is per cluster in the output, not only in the data",
			want: "minimal repro: GOMAXPROCS=2 go test -count=1",
		},
		{
			name: "the other cluster names the other repro",
			pins: "both minimal configurations are printed, not just the smallest",
			want: "minimal repro: GOMAXPROCS=4 go test -count=1",
		},
		{
			name: "the representative failure is shown",
			pins: "the user sees what the failure looks like without --verbose",
			want: "parallel execution exposed the bug: GOMAXPROCS=2",
		},
		{
			name: "the single-signature case says so plainly",
			pins: "the common case reads simply instead of printing a cluster of one",
			want: "all failures share one signature (",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if !strings.Contains(out, tc.want) {
				t.Errorf("output missing %q; this row pins that %s\n%s", tc.want, tc.pins, out)
			}
		})
	}

	// A cluster of one must never be printed as a cluster block.
	if strings.Contains(out, "1 distinct failure signatures") {
		t.Errorf("a single-signature test was printed as a cluster block:\n%s", out)
	}
}

// TestJSONClustersAreAdditive: the schema freezes at v1.0.0 and clusters land
// now precisely so they are not a breaking change later. Every v0.1.0 field has
// to survive.
func TestJSONClustersAreAdditive(t *testing.T) {
	raw, err := json.Marshal(clusteredReport(t))
	if err != nil {
		t.Fatalf("marshalling: %v", err)
	}

	var doc struct {
		Tests []map[string]json.RawMessage `json:"tests"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshalling: %v", err)
	}

	// Every field a v0.1.0 consumer could read.
	v0Fields := []string{
		"package", "name", "pass", "fail", "skip", "incomplete",
		"failure_rate", "classification",
	}
	for _, tst := range doc.Tests {
		for _, f := range v0Fields {
			if _, ok := tst[f]; !ok {
				t.Errorf("v0.1.0 field %q missing from a test entry: %v", f, tst)
			}
		}
		if _, ok := tst["clusters"]; !ok {
			t.Errorf("clusters missing from a test entry; it is present even when empty: %v", tst)
		}
	}

	var typed struct {
		Tests []struct {
			Name     string `json:"name"`
			Fail     int    `json:"fail"`
			Clusters []struct {
				Signature string `json:"signature"`
				Kind      string `json:"kind"`
				Count     int    `json:"count"`
				Minimal   struct {
					GOMAXPROCS  int    `json:"gomaxprocs"`
					CommandLine string `json:"command_line"`
				} `json:"minimal_config"`
				Output []string `json:"representative_output"`
			} `json:"clusters"`
		} `json:"tests"`
	}
	if err := json.Unmarshal(raw, &typed); err != nil {
		t.Fatalf("unmarshalling the documented schema: %v", err)
	}

	seen := false
	for _, tst := range typed.Tests {
		total := 0
		for _, c := range tst.Clusters {
			total += c.Count
			if c.Signature == "" || c.Kind == "" || c.Minimal.CommandLine == "" {
				t.Errorf("%s: incomplete cluster %+v", tst.Name, c)
			}
		}
		if total != tst.Fail {
			t.Errorf("%s: cluster counts sum to %d, want %d", tst.Name, total, tst.Fail)
		}
		if tst.Name != "TestLoadDependent" {
			continue
		}
		seen = true
		if len(tst.Clusters) != 2 {
			t.Fatalf("TestLoadDependent has %d clusters in the JSON, want 2", len(tst.Clusters))
		}
		procs := []int{tst.Clusters[0].Minimal.GOMAXPROCS, tst.Clusters[1].Minimal.GOMAXPROCS}
		if procs[0] == procs[1] {
			t.Errorf("both clusters report the same minimal configuration (GOMAXPROCS=%d); "+
				"per-cluster minimality is the point of the field", procs[0])
		}
		if len(tst.Clusters[0].Output) == 0 {
			t.Error("cluster carries no representative output")
		}
	}
	if !seen {
		t.Error("TestLoadDependent missing from the JSON report")
	}
}

var cfgFourPShuffled = runner.Config{GOMAXPROCS: 4, ShuffleSeed: 1, Count: 1}

// TestClusterMinimalIsMinimisedWithinTheCluster: a cluster holding more than
// one configuration must MINIMISE across them, not keep whichever it saw first.
//
// The shuffled configuration is fed in first and is not the smallest. A cluster
// that kept its first failure would tell the user to run `-shuffle=1` to
// reproduce a failure that needs no shuffle at all - a repro that is both
// larger than necessary and misleading about what the bug depends on.
func TestClusterMinimalIsMinimisedWithinTheCluster(t *testing.T) {
	base := runner.Config{GOMAXPROCS: 4, Count: 1}
	rep := Build(fixturePkg, base, []runner.Result{
		result(t, cfgFourPShuffled, "orderload.json"),
		result(t, cfgFourP, "loadfail.json"),
		result(t, cfgSingleP, "singleproc.json"),
	})
	got := testByName(t, rep, "TestLoadDependent")

	if len(got.Clusters) != 1 {
		t.Fatalf("clusters = %d, want 1; both recordings report the same failure\n%s",
			len(got.Clusters), describeClusters(got))
	}
	if got.Clusters[0].Count != 2 {
		t.Fatalf("cluster count = %d, want 2", got.Clusters[0].Count)
	}
	if got.Clusters[0].Minimal.Shuffled() {
		t.Errorf("cluster minimal = %s, want the unshuffled configuration; "+
			"the cluster kept its first failure instead of minimising within itself",
			got.Clusters[0].Minimal)
	}
}

// TestClusterOutputComesFromTheMinimalRun: the report prints a failure and a
// command line next to each other, and they must be the same run.
//
// The two panic recordings hash identically - that is the exit criterion - but
// their raw text differs in goroutine ID and heap addresses. The four-processor
// one is fed in first and is not the minimal configuration, so a cluster that
// kept the first output it saw would print a stack from a run the command line
// beside it does not describe.
func TestClusterOutputComesFromTheMinimalRun(t *testing.T) {
	base := runner.Config{GOMAXPROCS: 1, Count: 1}
	rep := Build(fixturePkg, base, []runner.Result{
		result(t, cfgFourP, "panic4.json"),
		result(t, cfgSingleP, "panic1.json"),
	})
	got := testByName(t, rep, "TestPanics")

	if len(got.Clusters) != 1 {
		t.Fatalf("clusters = %d, want 1; one panic recorded twice is one cluster\n%s",
			len(got.Clusters), describeClusters(got))
	}
	c := got.Clusters[0]
	if c.Minimal.GOMAXPROCS != 1 {
		t.Fatalf("cluster minimal = %s, want GOMAXPROCS=1", c.Minimal)
	}

	// panic1.json's stack; panic4.json's says goroutine 18.
	joined := strings.Join(c.Output, "")
	if !strings.Contains(joined, "goroutine 6 [running]") {
		t.Errorf("representative output is not from the minimal run:\n%s", joined)
	}
	if strings.Contains(joined, "goroutine 18 [running]") {
		t.Errorf("representative output came from the four-processor run while the "+
			"repro line names the single-processor one:\n%s", joined)
	}
}

// TestAlwaysFailingTestIsClustered: a test that fails in every configuration is
// deterministically broken, but it can still be broken in two different ways,
// and the second one is invisible if only flaky tests get cluster output.
//
// Feeding only the two configurations where the load-dependent fixture fails
// makes it always-fail while still producing its two distinct messages.
func TestAlwaysFailingTestIsClustered(t *testing.T) {
	base := runner.Config{GOMAXPROCS: 4, Count: 1}
	rep := Build(fixturePkg, base, []runner.Result{
		result(t, cfgFourP, "loadfail.json"),
		result(t, cfgTwoP, "loadfail2.json"),
	})
	got := testByName(t, rep, "TestLoadDependent")

	if got.Class != ClassAlwaysFails {
		t.Fatalf("class = %v, want always-fails", got.Class)
	}
	if len(got.Clusters) != 2 {
		t.Fatalf("clusters = %d, want 2\n%s", len(got.Clusters), describeClusters(got))
	}

	var b strings.Builder
	if err := rep.WriteText(&b, false); err != nil {
		t.Fatalf("WriteText: %v", err)
	}
	out := b.String()
	if !strings.Contains(out, "ALWAYS FAILS") {
		t.Fatalf("no always-fails section:\n%s", out)
	}
	for _, want := range []string{
		"2 distinct failure signatures:",
		"minimal repro: GOMAXPROCS=2 go test -count=1",
		"minimal repro: GOMAXPROCS=4 go test -count=1",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("always-fails section missing %q; a test broken two ways reports only one of them\n%s", want, out)
		}
	}
}

// TestVerboseListsEveryConfigurationInACluster pins what --verbose adds. Without
// it a cluster shows one representative failure and one command line; with it,
// every configuration that produced that signature is named.
func TestVerboseListsEveryConfigurationInACluster(t *testing.T) {
	base := runner.Config{GOMAXPROCS: 4, Count: 1}
	rep := Build(fixturePkg, base, []runner.Result{
		result(t, cfgFourPShuffled, "orderload.json"),
		result(t, cfgFourP, "loadfail.json"),
		result(t, cfgTwoP, "loadfail2.json"),
		result(t, cfgSingleP, "singleproc.json"),
	})

	var quiet, loud strings.Builder
	if err := rep.WriteText(&quiet, false); err != nil {
		t.Fatalf("WriteText: %v", err)
	}
	if err := rep.WriteText(&loud, true); err != nil {
		t.Fatalf("WriteText(verbose): %v", err)
	}

	// The four-processor cluster holds two configurations: unshuffled and
	// shuffled. Only the unshuffled one is its minimum, so the shuffled one
	// appears nowhere without --verbose.
	const other = "also: GOMAXPROCS=4 go test -shuffle=1 -count=1"
	if strings.Contains(quiet.String(), other) {
		t.Errorf("every configuration was listed without --verbose; that is what --verbose is for\n%s", quiet.String())
	}
	if !strings.Contains(loud.String(), other) {
		t.Errorf("--verbose did not list the other configuration in the cluster\n%s", loud.String())
	}
}

// TestVerboseListsConfigurationsForASingleSignature is the fixture that breaks
// a --verbose that only lists members inside writeClusters. One signature is
// the common case and never enters that function.
func TestVerboseListsConfigurationsForASingleSignature(t *testing.T) {
	min := runner.Config{GOMAXPROCS: 4, Count: 1}
	other := runner.Config{GOMAXPROCS: 4, ShuffleSeed: 1, Count: 1}
	const also = "also: GOMAXPROCS=4 go test -shuffle=1 -count=1"

	tests := []struct {
		name string
		pins string
		rep  Report
	}{
		{
			name: "flaky, one signature",
			pins: "the common path lists members; writeClusters is not the only --verbose path",
			rep: Report{
				Package:        "example.com/p",
				Configurations: 3,
				Completed:      3,
				Tests: []Test{{
					Package:    "example.com/p",
					Name:       "TestOneCause",
					Pass:       1,
					Fail:       2,
					Class:      ClassFlaky,
					Dependence: DependenceLoad,
					Minimal:    &min,
					Clusters: []Cluster{{
						Signature: signature.Signature{Hash: "aaaaaaaaaaaaaaaa"},
						Count:     2,
						Minimal:   min,
						configs:   []runner.Config{other, min},
					}},
				}},
			},
		},
		{
			name: "always-fails, one signature",
			pins: "always-fails uses the same one-signature verbose path",
			rep: Report{
				Package:        "example.com/p",
				Configurations: 2,
				Completed:      2,
				Tests: []Test{{
					Package: "example.com/p",
					Name:    "TestBroken",
					Fail:    2,
					Class:   ClassAlwaysFails,
					Clusters: []Cluster{{
						Signature: signature.Signature{Hash: "bbbbbbbbbbbbbbbb"},
						Count:     2,
						Minimal:   min,
						configs:   []runner.Config{other, min},
					}},
				}},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var quiet, loud strings.Builder
			if err := tc.rep.WriteText(&quiet, false); err != nil {
				t.Fatalf("WriteText: %v", err)
			}
			if err := tc.rep.WriteText(&loud, true); err != nil {
				t.Fatalf("WriteText(verbose): %v", err)
			}
			if strings.Contains(quiet.String(), "distinct failure signatures") {
				t.Fatalf("a one-signature test was printed as a cluster block; this row pins that %s\n%s",
					tc.pins, quiet.String())
			}
			if strings.Contains(quiet.String(), also) {
				t.Errorf("every configuration was listed without --verbose; that is what --verbose is for\n%s",
					quiet.String())
			}
			if !strings.Contains(loud.String(), also) {
				t.Errorf("--verbose did not list the other configuration; this row pins that %s\n%s",
					tc.pins, loud.String())
			}
		})
	}
}
