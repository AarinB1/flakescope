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
			// dependence reads the configurations off Test.failures, which
			// clustering also reads the output off. These rows are about
			// configurations only, so the output is left empty.
			failures := make([]failure, 0, len(tc.failedIn))
			for _, cfg := range tc.failedIn {
				failures = append(failures, failure{config: cfg})
			}
			got := dependence(Test{
				Pass: len(tc.passedIn), Fail: len(tc.failedIn),
				failures: failures, passedIn: tc.passedIn,
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
