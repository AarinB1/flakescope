package report

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AarinB1/flakescope/internal/gotest"
	"github.com/AarinB1/flakescope/internal/runner"
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
			name:           "order dependent: fails only under shuffle",
			test:           "TestOrderDependent",
			wantClass:      ClassFlaky,
			wantDependence: DependenceOrder,
			wantFail:       1,
			wantPass:       2,
			wantMinimal:    &cfgShuffled,
		},
		{
			name:           "load dependent: fails only above a GOMAXPROCS threshold",
			test:           "TestLoadDependent",
			wantClass:      ClassFlaky,
			wantDependence: DependenceLoad,
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

// TestDependence includes the fixtures that break each rule, not just the ones
// that satisfy it. An "order-dependent" verdict that survives a counterexample
// is not a verdict.
func TestDependence(t *testing.T) {
	var (
		plain     = runner.Config{GOMAXPROCS: 4}
		plain1    = runner.Config{GOMAXPROCS: 1}
		plain2    = runner.Config{GOMAXPROCS: 2}
		shuffled  = runner.Config{GOMAXPROCS: 4, ShuffleSeed: 1}
		shuffled1 = runner.Config{GOMAXPROCS: 1, ShuffleSeed: 2}
		raced     = runner.Config{GOMAXPROCS: 4, Race: true}
		raced1    = runner.Config{GOMAXPROCS: 1, Race: true}
	)

	tests := []struct {
		name     string
		failedIn []runner.Config
		passedIn []runner.Config
		want     Dependence
	}{
		{
			name:     "order: every failure shuffled, an unshuffled run passed",
			failedIn: []runner.Config{shuffled, shuffled1},
			passedIn: []runner.Config{plain, plain1},
			want:     DependenceOrder,
		},
		{
			name:     "NOT order: one failure happened without shuffle",
			failedIn: []runner.Config{shuffled, plain},
			passedIn: []runner.Config{plain1},
			want:     DependenceLoad, // 4 and 4 fail, 1 passes: a threshold
		},
		{
			name:     "NOT order: nothing unshuffled ever passed, so shuffle is unproven",
			failedIn: []runner.Config{shuffled, shuffled1},
			passedIn: []runner.Config{{GOMAXPROCS: 4, ShuffleSeed: 3}},
			want:     DependenceUnknown,
		},
		{
			name:     "load: every failure raced, an unraced run passed",
			failedIn: []runner.Config{raced, raced1},
			passedIn: []runner.Config{plain, plain1},
			want:     DependenceLoad,
		},
		{
			name:     "load: GOMAXPROCS threshold, every failure above every pass",
			failedIn: []runner.Config{plain2, plain},
			passedIn: []runner.Config{plain1},
			want:     DependenceLoad,
		},
		{
			name:     "NOT load: the GOMAXPROCS ranges overlap",
			failedIn: []runner.Config{plain1, plain},
			passedIn: []runner.Config{plain2, plain1},
			want:     DependenceUnknown,
		},
		{
			name:     "NOT load: a failure at the same GOMAXPROCS as a pass is not a threshold",
			failedIn: []runner.Config{plain2},
			passedIn: []runner.Config{plain2},
			want:     DependenceUnknown,
		},
		{
			name:     "order beats load when both could be claimed",
			failedIn: []runner.Config{shuffled},
			passedIn: []runner.Config{plain1},
			want:     DependenceOrder,
		},
		{
			name:     "no failures at all",
			failedIn: nil,
			passedIn: []runner.Config{plain},
			want:     DependenceUnknown,
		},
		{
			name:     "no passes at all",
			failedIn: []runner.Config{plain},
			passedIn: nil,
			want:     DependenceUnknown,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := dependence(Test{
				Pass: len(tc.passedIn), Fail: len(tc.failedIn),
				failedIn: tc.failedIn, passedIn: tc.passedIn,
			})
			if got != tc.want {
				t.Errorf("dependence() = %v, want %v", got, tc.want)
			}
		})
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
				"order-dependent",
				"TestLoadDependent",
				"load-dependent",
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
