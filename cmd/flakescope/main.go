// Command flakescope reruns a Go package's tests under a matrix of
// configurations and reports which tests fail nondeterministically, along with
// the minimal configuration that reproduces each.
//
// flakescope varies CONFIGURATIONS, not interleavings. Go has no seedable
// goroutine scheduler outside testing/synctest, so nothing here replays a
// particular ordering of goroutines.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"time"

	"github.com/AarinB1/flakescope/internal/report"
	"github.com/AarinB1/flakescope/internal/runner"
)

const usage = `flakescope [flags] <package>

Reruns a package's tests under a matrix of configurations - shuffle seed,
GOMAXPROCS, and the race detector - and reports which tests fail
nondeterministically, at what rate, and under the smallest configuration that
reproduced the failure.

flakescope varies configurations, not goroutine interleavings.

A test's failures are grouped by normalized signature, and each group carries
its own minimal reproducing configuration: a test that fails two different ways
has two of them, and reporting one command line for both would hide a bug.

Flags:
  --runs N          number of configurations to run (default 20)
  --json            emit the machine-readable report instead of text
  --timeout D       per-configuration timeout (default 10m)
  --verbose         list every configuration behind each failure group

Exit codes:
  0   no flaky tests found
  1   flaky tests found
  2   flakescope itself failed: bad arguments, or the package would not build

A build failure is exit 2, not exit 1. It is not a finding about flakiness.
`

type options struct {
	pkg     string
	runs    int
	jsonOut bool
	timeout time.Duration
	verbose bool
}

// errUsage marks a bad invocation, which is a tool failure (exit 2) rather than
// a finding.
var errUsage = errors.New("usage")

func parseFlags(args []string, stderr io.Writer) (options, error) {
	fs := flag.NewFlagSet("flakescope", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(stderr, usage) }

	var opts options
	fs.IntVar(&opts.runs, "runs", 20, "number of configurations to run")
	fs.BoolVar(&opts.jsonOut, "json", false, "emit the machine-readable report")
	// The default matches `go test`'s own 10 minute timeout, so flakescope does
	// not kill runs the go tool would have let finish and then report them as
	// timeouts of its own making.
	fs.DurationVar(&opts.timeout, "timeout", 10*time.Minute, "per-configuration timeout")
	fs.BoolVar(&opts.verbose, "verbose", false, "include failure output")

	if err := fs.Parse(args); err != nil {
		return opts, fmt.Errorf("%w: %v", errUsage, err)
	}
	if opts.runs < 1 {
		fmt.Fprint(stderr, usage)
		return opts, fmt.Errorf("%w: --runs must be at least 1, got %d", errUsage, opts.runs)
	}
	if opts.timeout < 0 {
		fmt.Fprint(stderr, usage)
		return opts, fmt.Errorf("%w: --timeout must not be negative", errUsage)
	}
	switch fs.NArg() {
	case 1:
		opts.pkg = fs.Arg(0)
	case 0:
		fmt.Fprint(stderr, usage)
		return opts, fmt.Errorf("%w: no package given", errUsage)
	default:
		fmt.Fprint(stderr, usage)
		return opts, fmt.Errorf("%w: expected one package, got %d", errUsage, fs.NArg())
	}
	return opts, nil
}

// executor produces one result per configuration. It is the seam that lets the
// CLI be tested end to end against recorded streams; goTest below is the only
// implementation that reaches a process.
type executor func(ctx context.Context, opts options, configs []runner.Config) []runner.Result

// newRunner builds the runner goTest will use.
//
// It is split out from goTest so that a test can assert the parsed flags reach
// the runner without invoking `go test` (CLAUDE.md rule 2). This is the seam
// between what a user types and everything the tool does: a --timeout that
// stopped being propagated, or a package string that arrived wrong, would
// change every result flakescope produces while leaving a suite that replays
// recorded streams entirely green.
//
// Dir is deliberately left at its zero value, which means the caller's working
// directory. flakescope resolves the package the same way the user's own
// `go test` would.
func newRunner(opts options) *runner.Runner {
	r := runner.New(opts.pkg)
	r.Timeout = opts.timeout
	return r
}

func goTest(ctx context.Context, opts options, configs []runner.Config) []runner.Result {
	return newRunner(opts).Run(ctx, configs)
}

// run is the whole CLI. base is the configuration the matrix is generated from
// and that minimality is measured against; main passes runner.Default().
//
// It is a parameter rather than a call to runner.Default() in here because
// Default reads runtime.NumCPU(). A test that cannot fix it is a test whose
// matrix depends on the machine, and the fake would then be asked for
// configurations no recording was made under - which it can only answer by
// handing back a recording from a nearby one, fabricating results (CLAUDE.md
// rule 5).
func run(args []string, stdout, stderr io.Writer, base runner.Config, exec executor) int {
	opts, err := parseFlags(args, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "flakescope: %v\n", err)
		return report.ExitToolFailure
	}

	// Setpgid puts each `go test` in its own process group so a timeout can
	// SIGKILL the test binary, not just the go tool. That also isolates those
	// processes from a terminal SIGINT, so this context must cancel on
	// interrupt or the binaries keep running after flakescope itself exits.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	results := exec(ctx, opts, runner.Matrix(base, opts.runs))
	rep := report.Build(opts.pkg, base, results)

	if opts.jsonOut {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rep); err != nil {
			fmt.Fprintf(stderr, "flakescope: writing report: %v\n", err)
			return report.ExitToolFailure
		}
	} else if err := rep.WriteText(stdout, opts.verbose); err != nil {
		fmt.Fprintf(stderr, "flakescope: writing report: %v\n", err)
		return report.ExitToolFailure
	}

	return rep.ExitCode()
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, runner.Default(), goTest))
}
