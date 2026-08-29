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

Flags:
  --runs N          number of configurations to run (default 20)
  --json            emit the machine-readable report instead of text
  --timeout D       per-configuration timeout (default 10m)
  --verbose         include failure output for each reported test

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

func goTest(ctx context.Context, opts options, configs []runner.Config) []runner.Result {
	r := runner.New(opts.pkg)
	r.Timeout = opts.timeout
	return r.Run(ctx, configs)
}

func run(args []string, stdout, stderr io.Writer, exec executor) int {
	opts, err := parseFlags(args, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "flakescope: %v\n", err)
		return report.ExitToolFailure
	}

	base := runner.Default()
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
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, goTest))
}
