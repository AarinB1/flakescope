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

// poisoningSeeds are the shuffle seeds that actually permute
// TestPoisonsGlobalState ahead of TestOrderDependent. They were measured
// against the fixture, not guessed: seeds 3, 5 and 8 leave the order intact.
var poisoningSeeds = map[int64]bool{1: true, 2: true, 4: true, 6: true, 7: true}

// fixtureStream maps a configuration onto the recording whose outcomes the
// fixture actually produces under it. The fixture has two independent
// discriminators - a poisoning shuffle seed and GOMAXPROCS above 1 - so there
// are four outcome sets and four recordings.
//
// Mapping on outcomes rather than on one axis at a time matters. A fake that
// answered every shuffled configuration with the single-processor recording
// would report the load-dependent test as PASSING at high GOMAXPROCS, which
// collapses the threshold the classifier is looking for and makes the CLI look
// broken when it is not.
func fixtureStream(cfg runner.Config) string {
	order := cfg.Shuffled() && poisoningSeeds[cfg.ShuffleSeed]
	load := cfg.GOMAXPROCS > 1
	switch {
	case order && load:
		return "orderload.json"
	case order:
		return "orderfail.json"
	case load:
		return "loadfail.json"
	default:
		return "singleproc.json"
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
			got := run(tc.args, &stdout, &stderr, replay(t, tc.pick))
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
	code := run([]string{"--runs", "8", "--json", fixturePkg}, &stdout, &stderr, replay(t, fixtureStream))

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
		done <- run([]string{"--runs", "1", fixturePkg}, &stdout, &stderr, exec)
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
