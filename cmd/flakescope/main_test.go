package main

import (
	"context"
	"encoding/json"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AarinB1/flakescope/internal/gotest"
	"github.com/AarinB1/flakescope/internal/report"
	"github.com/AarinB1/flakescope/internal/runner"
)

const fixturePkg = "github.com/AarinB1/flakescope/testdata/flakypkg"

// replay builds an executor that answers each configuration with a recorded
// stream. The CLI is exercised end to end this way: flags, matrix, report,
// rendering and exit code, without invoking `go test`.
func replay(t *testing.T, pick func(runner.Config) string) executor {
	t.Helper()
	return func(_ context.Context, _ options, configs []runner.Config) []runner.Result {
		results := make([]runner.Result, len(configs))
		for i, cfg := range configs {
			name := pick(cfg)
			if name == "" {
				results[i] = runner.Result{Config: cfg, Outcome: runner.OutcomeTimedOut}
				continue
			}
			b, err := os.ReadFile(filepath.Join("..", "..", "testdata", "streams", name))
			if err != nil {
				t.Fatalf("reading recorded stream: %v", err)
			}
			run, err := gotest.ParseBytes(b)
			if err != nil {
				t.Fatalf("parsing %s: %v", name, err)
			}
			results[i] = runner.Result{Config: cfg, Outcome: runner.OutcomeCompleted, Run: run}
		}
		return results
	}
}

// fixtureBase is the base configuration the CLI tests generate their matrix
// from. It is stated rather than taken from runner.Default(), which reads
// runtime.NumCPU(): a matrix that depended on the machine would ask the fake
// for configurations no recording was made under.
var fixtureBase = runner.Config{GOMAXPROCS: 4, Count: 1}

// poisoningSeeds are the shuffle seeds that actually permute
// TestPoisonsGlobalState ahead of TestOrderDependent. They were measured
// against the fixture, not guessed: seeds 3, 5 and 8 leave the order intact.
var poisoningSeeds = map[int64]bool{1: true, 2: true, 4: true, 6: true, 7: true}

// fixtureStream maps a configuration onto the recording made under THAT
// configuration, never a nearby one (CLAUDE.md rule 5). The fixture has two
// independent discriminators - a poisoning shuffle seed, and GOMAXPROCS - and
// the processor count is not a boolean: the load-dependent failure's message
// names the count it saw, so GOMAXPROCS=2 and GOMAXPROCS=4 produce textually
// different failures and each needs its own recording.
//
// Two ways this goes wrong, both of which have happened:
//
// Answering every shuffled configuration with the single-processor recording
// reports the load-dependent test as PASSING at high GOMAXPROCS, collapses the
// threshold the classifier looks for, and makes a correct classifier look
// broken.
//
// Answering GOMAXPROCS=2 with the GOMAXPROCS=4 recording prints a repro line
// saying GOMAXPROCS=2 next to failure output saying GOMAXPROCS=4. Nothing in
// the report would say which one to believe.
//
// A configuration with no recording is answered with no stream at all, which
// the CLI reports as a timeout - an absence of evidence. Inventing one would be
// worse than measuring nothing. Nothing here varies with -race because nothing
// in the untagged fixture behaves differently under it; the racing fixture is
// behind a build tag and is not in this matrix.
func fixtureStream(cfg runner.Config) string {
	order := cfg.Shuffled() && poisoningSeeds[cfg.ShuffleSeed]
	switch cfg.GOMAXPROCS {
	case 1:
		if order {
			return "orderfail.json"
		}
		return "singleproc.json"
	case 2:
		if order {
			return "orderload2.json"
		}
		return "loadfail2.json"
	case 4:
		if order {
			return "orderload.json"
		}
		return "loadfail.json"
	default:
		return ""
	}
}

func TestParseFlags(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		want    options
		wantErr bool
	}{
		{
			name: "defaults",
			args: []string{"./..."},
			want: options{pkg: "./...", runs: 20, timeout: 10 * time.Minute},
		},
		{
			name: "every flag",
			args: []string{"--runs", "5", "--json", "--timeout", "30s", "--verbose", "./pkg"},
			want: options{pkg: "./pkg", runs: 5, jsonOut: true, timeout: 30 * time.Second, verbose: true},
		},
		{
			name: "flags after the package are still parsed",
			args: []string{"--runs=3", "./pkg"},
			want: options{pkg: "./pkg", runs: 3, timeout: 10 * time.Minute},
		},
		{name: "no package", args: nil, wantErr: true},
		{name: "two packages", args: []string{"./a", "./b"}, wantErr: true},
		{name: "unknown flag", args: []string{"--nope", "./pkg"}, wantErr: true},
		{name: "runs below one", args: []string{"--runs", "0", "./pkg"}, wantErr: true},
		{name: "negative runs", args: []string{"--runs", "-4", "./pkg"}, wantErr: true},
		{name: "negative timeout", args: []string{"--timeout", "-1s", "./pkg"}, wantErr: true},
		{name: "unparseable timeout", args: []string{"--timeout", "soon", "./pkg"}, wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var stderr strings.Builder
			got, err := parseFlags(tc.args, &stderr)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseFlags(%v) succeeded, want an error", tc.args)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseFlags(%v): %v", tc.args, err)
			}
			if got != tc.want {
				t.Errorf("parseFlags(%v) = %+v, want %+v", tc.args, got, tc.want)
			}
		})
	}
}

func TestRunExitCodes(t *testing.T) {
	tests := []struct {
		name        string
		args        []string
		pick        func(runner.Config) string
		want        int
		wantContain []string
	}{
		{
			name: "flaky tests found",
			args: []string{"--runs", "8", fixturePkg},
			pick: fixtureStream,
			want: report.ExitFlaky,
			wantContain: []string{"FLAKY (2)", "TestOrderDependent", "order-dependent",
				"TestLoadDependent", "load-dependent", "minimal repro:", "-shuffle=1 -count=1"},
		},
		{
			name:        "nothing flaky",
			args:        []string{"--runs", "8", fixturePkg},
			pick:        func(runner.Config) string { return "allpass.json" },
			want:        report.ExitClean,
			wantContain: []string{"No flaky tests found."},
		},
		{
			name:        "a consistently broken test is not a flakiness finding",
			args:        []string{"--runs", "8", fixturePkg},
			pick:        func(runner.Config) string { return "loadfail.json" },
			want:        report.ExitClean,
			wantContain: []string{"ALWAYS FAILS (2)", "TestAlwaysFails", "TestLoadDependent"},
		},
		{
			name:        "a build failure is a tool failure, not a finding",
			args:        []string{"--runs", "4", fixturePkg},
			pick:        func(runner.Config) string { return "buildfail.json" },
			want:        report.ExitToolFailure,
			wantContain: []string{"BUILD FAILED"},
		},
		{
			name:        "every configuration timing out is a tool failure",
			args:        []string{"--runs", "4", fixturePkg},
			pick:        func(runner.Config) string { return "" },
			want:        report.ExitToolFailure,
			wantContain: []string{"4 timed out"},
		},
		{
			name: "bad arguments",
			args: []string{},
			pick: fixtureStream,
			want: report.ExitToolFailure,
		},
		{
			name: "unknown flag",
			args: []string{"--interleavings", fixturePkg},
			pick: fixtureStream,
			want: report.ExitToolFailure,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr strings.Builder
			got := run(tc.args, &stdout, &stderr, fixtureBase, replay(t, tc.pick))
			if got != tc.want {
				t.Errorf("run(%v) = %d, want %d\nstdout:\n%s\nstderr:\n%s",
					tc.args, got, tc.want, stdout.String(), stderr.String())
			}
			for _, want := range tc.wantContain {
				if !strings.Contains(stdout.String(), want) {
					t.Errorf("stdout missing %q:\n%s", want, stdout.String())
				}
			}
		})
	}
}

func TestRunJSONOutput(t *testing.T) {
	var stdout, stderr strings.Builder
	code := run([]string{"--runs", "8", "--json", fixturePkg}, &stdout, &stderr, fixtureBase, replay(t, fixtureStream))

	var doc struct {
		Package  string `json:"package"`
		ExitCode int    `json:"exit_code"`
		Tests    []struct {
			Name           string `json:"name"`
			Classification string `json:"classification"`
			Dependence     string `json:"dependence"`
			Minimal        *struct {
				CommandLine string `json:"command_line"`
			} `json:"minimal_config"`
		} `json:"tests"`
	}
	if err := json.Unmarshal([]byte(stdout.String()), &doc); err != nil {
		t.Fatalf("--json did not emit valid JSON: %v\n%s", err, stdout.String())
	}
	if doc.Package != fixturePkg {
		t.Errorf("package = %q, want %q", doc.Package, fixturePkg)
	}
	if doc.ExitCode != code {
		t.Errorf("exit_code in the report = %d, but the process would exit %d", doc.ExitCode, code)
	}

	want := map[string]string{
		"TestOrderDependent": "order-dependent",
		"TestLoadDependent":  "load-dependent",
	}
	seen := map[string]bool{}
	for _, e := range doc.Tests {
		if e.Name == "TestAlwaysFails" && e.Classification == "flaky" {
			t.Error("a consistently broken test was reported as flaky in the JSON output")
		}
		wantDep, ok := want[e.Name]
		if !ok {
			continue
		}
		seen[e.Name] = true
		if e.Classification != "flaky" {
			t.Errorf("%s classification = %q, want flaky", e.Name, e.Classification)
		}
		if e.Dependence != wantDep {
			t.Errorf("%s dependence = %q, want %q", e.Name, e.Dependence, wantDep)
		}
		if e.Minimal == nil || e.Minimal.CommandLine == "" {
			t.Errorf("%s has no minimal configuration", e.Name)
		}
	}
	for name := range want {
		if !seen[name] {
			t.Errorf("%s missing from the JSON report", name)
		}
	}
}

// TestRunCancelsOnInterrupt is the fixture that breaks a CLI which still
// runs against context.Background() after Setpgid isolated the children.
// Without NotifyContext, SIGINT never reaches the executor; the test
// binaries would stay alive after flakescope itself died.
func TestRunCancelsOnInterrupt(t *testing.T) {
	// Own SIGINT first so a missing notify fails this test instead of
	// killing the process under test.
	ignored := make(chan os.Signal, 1)
	signal.Notify(ignored, os.Interrupt)
	t.Cleanup(func() { signal.Stop(ignored) })

	started := make(chan struct{})
	cancelled := make(chan struct{})
	exec := func(ctx context.Context, _ options, configs []runner.Config) []runner.Result {
		close(started)
		<-ctx.Done()
		close(cancelled)
		return make([]runner.Result, len(configs))
	}

	done := make(chan int, 1)
	go func() {
		var stdout, stderr strings.Builder
		done <- run([]string{"--runs", "1", fixturePkg}, &stdout, &stderr, fixtureBase, exec)
	}()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("run never reached the executor")
	}

	proc, err := os.FindProcess(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if err := proc.Signal(os.Interrupt); err != nil {
		t.Skipf("cannot send interrupt: %v", err)
	}

	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("interrupt did not cancel the executor context")
	}

	select {
	case code := <-done:
		if code != report.ExitToolFailure {
			t.Errorf("run after interrupt = %d, want %d; a cancelled matrix that learned nothing must not exit 0",
				code, report.ExitToolFailure)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("run did not return after the executor finished")
	}
}

// TestUsageDocumentsExitCodes: the exit codes are a compatibility surface, and
// the help text is where a user finds them. This keeps the two from drifting.
func TestUsageDocumentsExitCodes(t *testing.T) {
	tests := []string{
		"0", "no flaky tests found",
		"1", "flaky tests found",
		"2", "would not build",
		"--runs", "--json", "--timeout", "--verbose",
	}
	for _, want := range tests {
		if !strings.Contains(usage, want) {
			t.Errorf("usage text does not mention %q", want)
		}
	}
	// The scope claim from CLAUDE.md: flakescope varies configurations, not
	// interleavings, and must never say otherwise in its output.
	if !strings.Contains(usage, "not goroutine interleavings") {
		t.Error("usage text does not state that flakescope varies configurations, not interleavings")
	}
	if strings.Contains(strings.ToLower(usage), "reproduces interleavings") {
		t.Error("usage text claims to reproduce interleavings")
	}
}

// TestNewRunnerCarriesParsedFlags walks the whole path from argv to the runner.
//
// Every other test in this file enters through the executor seam with a
// recorded stream, which means nothing exercises the one function that turns
// parsed flags into the object that reaches os/exec. Those lines could stop
// propagating --timeout, or hand `go test` the wrong package string, and this
// suite would stay green while the tool measured the wrong thing.
//
// The wanted values are written out literally rather than read back off opts,
// so a bug in parseFlags cannot make this test agree with it.
func TestNewRunnerCarriesParsedFlags(t *testing.T) {
	tests := []struct {
		name        string
		args        []string
		wantPkg     string
		wantTimeout time.Duration
	}{
		{
			name:        "the default timeout reaches the runner",
			args:        []string{"./..."},
			wantPkg:     "./...",
			wantTimeout: 10 * time.Minute,
		},
		{
			name:        "an explicit timeout reaches the runner",
			args:        []string{"--timeout", "45s", "./internal/queue"},
			wantPkg:     "./internal/queue",
			wantTimeout: 45 * time.Second,
		},
		{
			name:        "a sub-second timeout is not rounded away",
			args:        []string{"--timeout", "250ms", "example.com/x/y"},
			wantPkg:     "example.com/x/y",
			wantTimeout: 250 * time.Millisecond,
		},
		{
			name:        "zero means no per-configuration deadline, not the default one",
			args:        []string{"--timeout", "0", "./pkg"},
			wantPkg:     "./pkg",
			wantTimeout: 0,
		},
		{
			name:        "flags that are not the runner's do not disturb it",
			args:        []string{"--runs", "1000", "--json", "--verbose", "--timeout", "1m", "./pkg"},
			wantPkg:     "./pkg",
			wantTimeout: time.Minute,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var stderr strings.Builder
			opts, err := parseFlags(tc.args, &stderr)
			if err != nil {
				t.Fatalf("parseFlags(%v): %v", tc.args, err)
			}
			r := newRunner(opts)
			if r.Package != tc.wantPkg {
				t.Errorf("runner.Package = %q, want %q; `go test` would be pointed at the wrong package",
					r.Package, tc.wantPkg)
			}
			if r.Timeout != tc.wantTimeout {
				t.Errorf("runner.Timeout = %v, want %v; --timeout is not reaching the runner",
					r.Timeout, tc.wantTimeout)
			}
			if r.Dir != "" {
				t.Errorf("runner.Dir = %q, want empty; flakescope must resolve the package from the caller's working directory",
					r.Dir)
			}
		})
	}
}

// TestGoTestWithNoConfigurationsRunsNothing covers goTest itself, which is the
// call to Run and nothing else. An empty matrix returns before any
// configuration is dispatched, so this reaches the statement without reaching a
// process (CLAUDE.md rule 2).
func TestGoTestWithNoConfigurationsRunsNothing(t *testing.T) {
	var stderr strings.Builder
	opts, err := parseFlags([]string{"--timeout", "1s", fixturePkg}, &stderr)
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	got := goTest(context.Background(), opts, nil)
	if len(got) != 0 {
		t.Errorf("goTest with no configurations returned %d results, want 0", len(got))
	}
}

// TestEveryMatrixConfigurationHasARecording guards the fake itself. If the
// matrix grows a configuration no recording was made under, fixtureStream
// answers with nothing and the CLI reports a timeout - which is honest, but it
// silently removes evidence the tests below assert on. This says so out loud
// instead.
func TestEveryMatrixConfigurationHasARecording(t *testing.T) {
	for _, runs := range []int{8, 16, 20, 1000} {
		for _, cfg := range runner.Matrix(fixtureBase, runs) {
			if fixtureStream(cfg) == "" {
				t.Errorf("--runs %d generates %s, which no recorded stream was made under", runs, cfg)
			}
		}
	}
}

// TestRunClustersFailures is the v0.2.0 exit criterion through the CLI.
//
// At --runs 20 the matrix reaches GOMAXPROCS=2 as well as 4, and the
// load-dependent failure names the processor count it saw, so that one test
// produces two textually different failures. The order-dependent test produces
// one.
func TestRunClustersFailures(t *testing.T) {
	var stdout, stderr strings.Builder
	code := run([]string{"--runs", "20", fixturePkg}, &stdout, &stderr, fixtureBase, replay(t, fixtureStream))
	if code != report.ExitFlaky {
		t.Fatalf("run = %d, want %d\nstdout:\n%s", code, report.ExitFlaky, stdout.String())
	}
	out := stdout.String()

	tests := []struct {
		name string
		pins string
		want string
	}{
		{
			name: "the split test announces two signatures",
			pins: "two textually different failures are reported as two clusters",
			want: "2 distinct failure signatures:",
		},
		{
			name: "each cluster carries its own repro",
			pins: "minimality is per cluster: the two-processor failure gets a two-processor command",
			want: "minimal repro: GOMAXPROCS=2 go test -count=1",
		},
		{
			name: "the other cluster carries the other repro",
			pins: "both are printed; collapsing them would hide one failure mode",
			want: "minimal repro: GOMAXPROCS=4 go test -count=1",
		},
		{
			name: "the representative failure appears without --verbose",
			pins: "the user sees the failure next to the command that produces it",
			want: "parallel execution exposed the bug: GOMAXPROCS=2",
		},
		{
			name: "the single-signature test says so plainly",
			pins: "the common case is not printed as a cluster of one",
			want: "all failures share one signature (",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if !strings.Contains(out, tc.want) {
				t.Errorf("stdout missing %q; this row pins that %s\n%s", tc.want, tc.pins, out)
			}
		})
	}

	if strings.Contains(out, "1 distinct failure signatures") {
		t.Errorf("a one-signature test was printed as a cluster block:\n%s", out)
	}
	// The representative failure is one per cluster, not one per configuration.
	if n := strings.Count(out, "parallel execution exposed the bug: GOMAXPROCS=4"); n != 1 {
		t.Errorf("the four-processor failure was printed %d times, want 1; "+
			"printing every failure is what --verbose is for\n%s", n, out)
	}
}

// TestRunJSONClusters: the schema freezes at v1.0.0, so clusters land now.
// Every v0.1.0 field has to survive alongside them.
func TestRunJSONClusters(t *testing.T) {
	var stdout, stderr strings.Builder
	run([]string{"--runs", "20", "--json", fixturePkg}, &stdout, &stderr, fixtureBase, replay(t, fixtureStream))

	var doc struct {
		Tests []struct {
			Name string `json:"name"`
			Fail int    `json:"fail"`
			// v0.1.0 fields, still required.
			Classification string `json:"classification"`
			Dependence     string `json:"dependence"`
			Minimal        *struct {
				CommandLine string `json:"command_line"`
			} `json:"minimal_config"`
			// v0.2.0.
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
	if err := json.Unmarshal([]byte(stdout.String()), &doc); err != nil {
		t.Fatalf("--json did not emit valid JSON: %v\n%s", err, stdout.String())
	}

	var load, order bool
	for _, e := range doc.Tests {
		total := 0
		for _, c := range e.Clusters {
			total += c.Count
		}
		if total != e.Fail {
			t.Errorf("%s: cluster counts sum to %d, want %d", e.Name, total, e.Fail)
		}
		switch e.Name {
		case "TestLoadDependent":
			load = true
			if len(e.Clusters) != 2 {
				t.Fatalf("TestLoadDependent has %d clusters, want 2", len(e.Clusters))
			}
			a, b := e.Clusters[0], e.Clusters[1]
			if a.Signature == b.Signature {
				t.Error("two clusters share a signature; they should have been one")
			}
			if a.Minimal.GOMAXPROCS == b.Minimal.GOMAXPROCS {
				t.Errorf("both clusters report GOMAXPROCS=%d as their minimum; "+
					"per-cluster minimality is the point of the field", a.Minimal.GOMAXPROCS)
			}
			for _, c := range e.Clusters {
				if c.Kind != "assertion" {
					t.Errorf("cluster kind = %q, want assertion", c.Kind)
				}
				if len(c.Output) == 0 {
					t.Error("cluster carries no representative output")
				}
			}
			// v0.1.0's fields are untouched by clustering.
			if e.Classification != "flaky" || e.Dependence != "load-dependent" || e.Minimal == nil {
				t.Errorf("a v0.1.0 consumer would read %s differently now: class=%q dependence=%q minimal=%v",
					e.Name, e.Classification, e.Dependence, e.Minimal)
			}
		case "TestOrderDependent":
			order = true
			if len(e.Clusters) != 1 {
				t.Errorf("TestOrderDependent has %d clusters, want 1", len(e.Clusters))
			}
		case "TestAlwaysPasses":
			if len(e.Clusters) != 0 {
				t.Errorf("a test that never failed has %d clusters, want 0", len(e.Clusters))
			}
		}
	}
	if !load || !order {
		t.Errorf("expected tests missing from the report: load=%v order=%v", load, order)
	}
}

// TestRunAtAThousandConfigurations is the scale claim through the whole CLI:
// flags, matrix, executor, clustering, rendering and exit code.
//
// It replays recorded streams rather than invoking `go test` (CLAUDE.md rule 2),
// so what it measures is that the pipeline handles a thousand configurations
// coherently - not how long a thousand real `go test` invocations take. That
// number is measured separately and reported in the README, because a wall-clock
// figure produced by a test that never starts a process would be a lie.
func TestRunAtAThousandConfigurations(t *testing.T) {
	const runs = 1000

	var stdout, stderr strings.Builder
	code := run([]string{"--runs", "1000", "--json", fixturePkg}, &stdout, &stderr, fixtureBase, replay(t, fixtureStream))
	if code != report.ExitFlaky {
		t.Fatalf("run = %d, want %d\nstderr:\n%s", code, report.ExitFlaky, stderr.String())
	}

	var doc struct {
		Configurations int `json:"configurations"`
		Completed      int `json:"completed"`
		TimedOut       int `json:"timed_out"`
		Errored        int `json:"errored"`
		Tests          []struct {
			Name     string `json:"name"`
			Pass     int    `json:"pass"`
			Fail     int    `json:"fail"`
			Clusters []struct {
				Signature string `json:"signature"`
				Count     int    `json:"count"`
			} `json:"clusters"`
		} `json:"tests"`
	}
	if err := json.Unmarshal([]byte(stdout.String()), &doc); err != nil {
		t.Fatalf("--json did not emit valid JSON at %d configurations: %v", runs, err)
	}

	if doc.Configurations != runs {
		t.Errorf("configurations = %d, want %d", doc.Configurations, runs)
	}
	// Every configuration was answered. A timeout here would mean the matrix
	// generated something no recording was made under, and the numbers below
	// would be measuring a smaller run than the one advertised.
	if doc.Completed != runs || doc.TimedOut != 0 || doc.Errored != 0 {
		t.Errorf("completed/timed out/errored = %d/%d/%d, want %d/0/0",
			doc.Completed, doc.TimedOut, doc.Errored, runs)
	}

	var seenLoad bool
	for _, e := range doc.Tests {
		if e.Pass+e.Fail != runs {
			t.Errorf("%s was observed in %d configurations, want %d", e.Name, e.Pass+e.Fail, runs)
		}
		total := 0
		hashes := map[string]bool{}
		for _, c := range e.Clusters {
			total += c.Count
			if hashes[c.Signature] {
				t.Errorf("%s: signature %s appears in two clusters", e.Name, c.Signature)
			}
			hashes[c.Signature] = true
		}
		if total != e.Fail {
			t.Errorf("%s: cluster counts sum to %d, want %d", e.Name, total, e.Fail)
		}
		if e.Name != "TestLoadDependent" {
			continue
		}
		seenLoad = true
		// A thousand configurations must not fragment one bug into a thousand
		// clusters. The fixture produces exactly two distinct failure texts.
		if len(e.Clusters) != 2 {
			t.Errorf("TestLoadDependent has %d clusters at %d configurations, want 2; "+
				"clustering is not collapsing repeats", len(e.Clusters), runs)
		}
	}
	if !seenLoad {
		t.Error("TestLoadDependent missing from the thousand-configuration report")
	}
}
