package gotest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const fixturePkg = "github.com/AarinB1/flakescope/testdata/flakypkg"

// readStream loads a recorded stream. Every test in this file replays one of
// these; none of them invokes `go test` (CLAUDE.md rule 2).
func readStream(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", "streams", name))
	if err != nil {
		t.Fatalf("reading recorded stream: %v", err)
	}
	return b
}

func parseStream(t *testing.T, name string) *Run {
	t.Helper()
	run, err := ParseBytes(readStream(t, name))
	if err != nil {
		t.Fatalf("Parse(%s): %v", name, err)
	}
	return run
}

func TestIsPackageScoped(t *testing.T) {
	tests := []struct {
		name string
		ev   Event
		want bool
	}{
		{"package fail", Event{Action: ActionFail, Package: "p"}, true},
		{"package output", Event{Action: ActionOutput, Package: "p", Output: "FAIL\n"}, true},
		{"build fail has no package field at all", Event{Action: ActionBuildFail, ImportPath: "p [p.test]"}, true},
		{"test fail", Event{Action: ActionFail, Package: "p", Test: "TestX"}, false},
		{"subtest fail", Event{Action: ActionFail, Package: "p", Test: "TestX/sub"}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.ev.IsPackageScoped(); got != tc.want {
				t.Errorf("IsPackageScoped() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestParseRecordedStreams is the broad shape check: for each recording, the
// package outcome and every test outcome.
func TestParseRecordedStreams(t *testing.T) {
	tests := []struct {
		name          string
		stream        string
		wantPkgStatus Status
		wantBuildFail bool
		wantTruncated bool
		wantTests     map[string]Status
	}{
		{
			name:          "all pass",
			stream:        "allpass.json",
			wantPkgStatus: StatusPass,
			wantTests: map[string]Status{
				"TestAlwaysPasses":          StatusPass,
				"TestAlwaysPasses/add":      StatusPass,
				"TestAlwaysPasses/multiply": StatusPass,
				"TestOrderDependent":        StatusPass,
				"TestPoisonsGlobalState":    StatusPass,
			},
		},
		{
			name:          "order dependent failure under shuffle",
			stream:        "orderfail.json",
			wantPkgStatus: StatusFail,
			wantTests: map[string]Status{
				"TestAlwaysPasses":          StatusPass,
				"TestAlwaysPasses/add":      StatusPass,
				"TestAlwaysPasses/multiply": StatusPass,
				"TestOrderDependent":        StatusFail,
				"TestPoisonsGlobalState":    StatusPass,
				"TestLoadDependent":         StatusPass,
				"TestAlwaysFails":           StatusFail,
			},
		},
		{
			name:          "load dependent failure at GOMAXPROCS=4",
			stream:        "loadfail.json",
			wantPkgStatus: StatusFail,
			wantTests: map[string]Status{
				"TestAlwaysPasses":          StatusPass,
				"TestAlwaysPasses/add":      StatusPass,
				"TestAlwaysPasses/multiply": StatusPass,
				"TestOrderDependent":        StatusPass,
				"TestPoisonsGlobalState":    StatusPass,
				"TestLoadDependent":         StatusFail,
				"TestAlwaysFails":           StatusFail,
			},
		},
		{
			name:          "panic",
			stream:        "panic.json",
			wantPkgStatus: StatusFail,
			wantTests: map[string]Status{
				"TestAlwaysPasses":          StatusPass,
				"TestAlwaysPasses/add":      StatusPass,
				"TestAlwaysPasses/multiply": StatusPass,
				"TestLoadDependent":         StatusPass,
				"TestPanics":                StatusFail,
			},
		},
		{
			name:          "build failure has no tests at all",
			stream:        "buildfail.json",
			wantPkgStatus: StatusFail,
			wantBuildFail: true,
			wantTests:     map[string]Status{},
		},
		{
			name:          "truncated stream leaves tests incomplete",
			stream:        "truncated.json",
			wantPkgStatus: StatusIncomplete,
			wantTruncated: true,
			wantTests: map[string]Status{
				"TestAlwaysPasses":     StatusIncomplete,
				"TestAlwaysPasses/add": StatusPass,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			run := parseStream(t, tc.stream)

			if got, want := len(run.Packages), 1; got != want {
				t.Fatalf("packages = %d, want %d", got, want)
			}
			pkg := run.Package(fixturePkg)
			if pkg == nil {
				t.Fatalf("package %q not found; got %q", fixturePkg, run.Packages[0].Name)
			}
			if pkg.Status != tc.wantPkgStatus {
				t.Errorf("package status = %v, want %v", pkg.Status, tc.wantPkgStatus)
			}
			if pkg.BuildFailed != tc.wantBuildFail {
				t.Errorf("BuildFailed = %v, want %v", pkg.BuildFailed, tc.wantBuildFail)
			}
			if run.Truncated != tc.wantTruncated {
				t.Errorf("Truncated = %v, want %v", run.Truncated, tc.wantTruncated)
			}

			if got, want := len(pkg.Tests), len(tc.wantTests); got != want {
				var names []string
				for _, tst := range pkg.Tests {
					names = append(names, tst.Name)
				}
				t.Fatalf("test count = %d, want %d; got %v", got, want, names)
			}
			for name, want := range tc.wantTests {
				tst := pkg.Test(name)
				if tst == nil {
					t.Errorf("test %q missing", name)
					continue
				}
				if tst.Status != want {
					t.Errorf("test %q status = %v, want %v", name, tst.Status, want)
				}
			}
		})
	}
}

// TestBuildFailureIsNeverAttributedToATest is the empty-Test rule stated as an
// assertion. In buildfail.json every single event is package-scoped, so a parser
// that folded package-scoped events into "whichever test is current" would
// invent a test here. There must be none.
func TestBuildFailureIsNeverAttributedToATest(t *testing.T) {
	run := parseStream(t, "buildfail.json")

	if !run.BuildFailed() {
		t.Fatal("BuildFailed() = false, want true")
	}
	if got := run.Tests(); len(got) != 0 {
		t.Fatalf("a build failure produced %d test results, want 0: %v", len(got), got[0].Name)
	}

	pkg := run.Package(fixturePkg)
	joined := strings.Join(pkg.Output, "")
	if !strings.Contains(joined, "cannot use 42") {
		t.Errorf("compiler diagnostic did not land in package output; got:\n%s", joined)
	}
}

// TestPackageScopedFailureDoesNotFailInFlightTest is the fixture that breaks the
// naive implementation. A test starts, never finishes, and then the PACKAGE
// fails. The test is incomplete; it did not fail, and it certainly did not pass.
func TestPackageScopedFailureDoesNotFailInFlightTest(t *testing.T) {
	const stream = `{"Action":"start","Package":"p"}
{"Action":"run","Package":"p","Test":"TestInFlight"}
{"Action":"output","Package":"p","Test":"TestInFlight","Output":"=== RUN   TestInFlight\n"}
{"Action":"output","Package":"p","Output":"panic: test timed out after 10m0s\n"}
{"Action":"fail","Package":"p","Elapsed":600}
`
	run, err := ParseBytes([]byte(stream))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	pkg := run.Package("p")
	if pkg.Status != StatusFail {
		t.Errorf("package status = %v, want fail", pkg.Status)
	}
	tst := pkg.Test("TestInFlight")
	if tst == nil {
		t.Fatal("TestInFlight missing")
	}
	if tst.Status != StatusIncomplete {
		t.Errorf("in-flight test status = %v, want incomplete", tst.Status)
	}
	// The package-scoped banner belongs to the package, not to the test.
	if got := strings.Join(tst.Output, ""); strings.Contains(got, "test timed out") {
		t.Errorf("package-scoped output leaked into the test: %q", got)
	}
	if got := strings.Join(pkg.Output, ""); !strings.Contains(got, "test timed out") {
		t.Errorf("package-scoped output missing from the package: %q", got)
	}
}

// TestTruncatedStreamNeverReportsGreen covers the killed-process case.
func TestTruncatedStreamNeverReportsGreen(t *testing.T) {
	run := parseStream(t, "truncated.json")

	if !run.Truncated {
		t.Error("Truncated = false, want true")
	}
	pkg := run.Package(fixturePkg)
	if pkg.Status != StatusIncomplete {
		t.Errorf("package status = %v, want incomplete", pkg.Status)
	}
	// This test is named TestAlwaysPasses and it was mid-flight when the stream
	// stopped. It has not passed.
	tst := pkg.Test("TestAlwaysPasses")
	if tst == nil {
		t.Fatal("TestAlwaysPasses missing")
	}
	if tst.Status == StatusPass {
		t.Fatal("a test that never reported a result was recorded as passing")
	}
	if tst.Status != StatusIncomplete {
		t.Errorf("status = %v, want incomplete", tst.Status)
	}
	// The final line was cut in half, so the test it named must not exist.
	if got := pkg.Test("TestAlwaysPasses/multiply"); got != nil {
		t.Errorf("half a JSON object produced a test result: %+v", got)
	}
}

// TestSubtestsAreRecordedUnderTheirFullName pins the v0 decision not to roll
// subtests up into their parent.
func TestSubtestsAreRecordedUnderTheirFullName(t *testing.T) {
	run := parseStream(t, "allpass.json")
	pkg := run.Package(fixturePkg)

	tests := []struct {
		name        string
		wantSubtest bool
	}{
		{"TestAlwaysPasses", false},
		{"TestAlwaysPasses/add", true},
		{"TestAlwaysPasses/multiply", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tst := pkg.Test(tc.name)
			if tst == nil {
				t.Fatalf("test %q missing", tc.name)
			}
			if got := tst.IsSubtest(); got != tc.wantSubtest {
				t.Errorf("IsSubtest() = %v, want %v", got, tc.wantSubtest)
			}
		})
	}
}

// TestOutputIsAccumulatedPerTest checks that failure text is recoverable, and
// that it is attached to the test that produced it rather than to its
// neighbours.
func TestOutputIsAccumulatedPerTest(t *testing.T) {
	tests := []struct {
		name        string
		stream      string
		test        string
		wantContain string
		wantAbsent  string
	}{
		{
			name:        "panic text lands on the panicking test",
			stream:      "panic.json",
			test:        "TestPanics",
			wantContain: "panic: assignment to entry in nil map",
			wantAbsent:  "PASS: TestAlwaysPasses",
		},
		{
			name:        "order dependent failure message",
			stream:      "orderfail.json",
			test:        "TestOrderDependent",
			wantContain: "global state was poisoned",
			wantAbsent:  "this test is broken in every configuration",
		},
		{
			name:        "load dependent failure message",
			stream:      "loadfail.json",
			test:        "TestLoadDependent",
			wantContain: "parallel execution exposed the bug",
			wantAbsent:  "global state was poisoned",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			run := parseStream(t, tc.stream)
			tst := run.Package(fixturePkg).Test(tc.test)
			if tst == nil {
				t.Fatalf("test %q missing", tc.test)
			}
			got := strings.Join(tst.Output, "")
			if !strings.Contains(got, tc.wantContain) {
				t.Errorf("output missing %q; got:\n%s", tc.wantContain, got)
			}
			if strings.Contains(got, tc.wantAbsent) {
				t.Errorf("output of another test leaked in (%q):\n%s", tc.wantAbsent, got)
			}
		})
	}
}

func TestParseErrors(t *testing.T) {
	tests := []struct {
		name          string
		stream        string
		wantErr       bool
		wantTruncated bool
	}{
		{
			name:    "empty stream",
			stream:  "",
			wantErr: false,
		},
		{
			name:   "blank lines are skipped",
			stream: "\n\n{\"Action\":\"pass\",\"Package\":\"p\"}\n\n",
		},
		{
			name:    "corruption in the middle is an error, not truncation",
			stream:  "{\"Action\":\"start\",\"Package\":\"p\"}\nnot json at all\n{\"Action\":\"pass\",\"Package\":\"p\"}\n",
			wantErr: true,
		},
		{
			name:          "half an object at the end is truncation",
			stream:        "{\"Action\":\"start\",\"Package\":\"p\"}\n{\"Action\":\"run\",\"Package\":\"p\",\"Te",
			wantTruncated: true,
		},
		{
			name:   "a complete final line without a newline is not truncation",
			stream: "{\"Action\":\"start\",\"Package\":\"p\"}\n{\"Action\":\"pass\",\"Package\":\"p\"}",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			run, err := ParseBytes([]byte(tc.stream))
			if tc.wantErr {
				if err == nil {
					t.Fatal("Parse succeeded, want an error")
				}
				return
			}
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if run.Truncated != tc.wantTruncated {
				t.Errorf("Truncated = %v, want %v", run.Truncated, tc.wantTruncated)
			}
		})
	}
}

// TestUnknownActionsAreIgnored keeps a future Go release from breaking the
// parser by adding an action to the stream.
func TestUnknownActionsAreIgnored(t *testing.T) {
	const stream = `{"Action":"start","Package":"p"}
{"Action":"some-future-action","Package":"p","Test":"TestX"}
{"Action":"run","Package":"p","Test":"TestX"}
{"Action":"pass","Package":"p","Test":"TestX","Elapsed":0.5}
{"Action":"pass","Package":"p","Elapsed":0.5}
`
	run, err := ParseBytes([]byte(stream))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	tst := run.Package("p").Test("TestX")
	if tst == nil || tst.Status != StatusPass {
		t.Fatalf("TestX = %+v, want pass", tst)
	}
	if got, want := tst.Elapsed, 0.5; got != want {
		t.Errorf("Elapsed = %v, want %v", got, want)
	}
}
