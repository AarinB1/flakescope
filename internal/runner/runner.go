package runner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"sync"
	"time"

	"github.com/AarinB1/flakescope/internal/gotest"
)

// Outcome is what happened to one configuration's process, as distinct from
// what happened to the tests inside it.
type Outcome int

const (
	// OutcomeCompleted means the process ran to completion and its stream was
	// parsed. The tests inside it may have passed or failed.
	OutcomeCompleted Outcome = iota
	// OutcomeTimedOut means the deadline fired first. This is neither a pass
	// nor a failure: it is an absence of evidence, and the report treats it as
	// one. Counting a timeout as a failure would inflate every failure rate
	// with the harness's own impatience.
	OutcomeTimedOut
	// OutcomeError means flakescope could not get a usable stream at all -
	// the go tool was missing, the package did not resolve, output was
	// corrupt. This is a problem with the tool or its arguments, not a
	// finding about flakiness.
	OutcomeError
)

func (o Outcome) String() string {
	switch o {
	case OutcomeTimedOut:
		return "timeout"
	case OutcomeError:
		return "error"
	default:
		return "completed"
	}
}

// Result is one configuration's outcome.
type Result struct {
	Config   Config
	Outcome  Outcome
	Duration time.Duration
	// Run is the parsed stream. It is nil when Outcome is OutcomeError, and may
	// be partial (Run.Truncated) when Outcome is OutcomeTimedOut.
	Run *gotest.Run
	// Err carries the reason for OutcomeError.
	Err error
	// Raw is the unparsed stream, kept for --verbose.
	Raw []byte
}

// Runner runs one package's tests under many configurations.
type Runner struct {
	// Package is the package pattern passed to `go test`.
	Package string
	// Workers bounds how many configurations run at once. Zero means the
	// default.
	Workers int
	// Timeout bounds each configuration individually: when it fires, the whole
	// process group is SIGKILLed. Zero means no timeout.
	//
	// It is a backstop, not the way a hung test is meant to be caught. A
	// SIGKILL leaves a stream that stops wherever the kill landed, with nothing
	// in it to say which test was stuck, so the configuration is reported as
	// producing no evidence at all. TestTimeout is what catches a hang.
	Timeout time.Duration
	// TestTimeout is passed to `go test` as -timeout. When it fires, the
	// testing package panics with a banner naming every test still running,
	// and internal/gotest reads those tests as timed out. Zero passes no
	// -timeout, which leaves go test's own default of ten minutes in force.
	//
	// It has to fire before Timeout does, or the banner never gets written.
	// Timeout also covers building the test binary and go test's own grace
	// period after the panic, and TestTimeout covers neither, so a caller that
	// sets both should leave Timeout well above it.
	TestTimeout time.Duration
	// Dir is the working directory for `go test`. Zero means the caller's.
	Dir string

	// exec runs one configuration and returns its raw -json stream. Tests
	// replace it with a function that returns a recording. It is a seam for
	// injecting fixtures, not an abstraction over os/exec: there is exactly one
	// real implementation and it is fifteen lines below.
	exec func(ctx context.Context, cfg Config) ([]byte, error)
}

// New returns a Runner that shells out to the real go tool.
func New(pkg string) *Runner {
	r := &Runner{Package: pkg}
	r.exec = r.goTest
	return r
}

// defaultWorkers leaves headroom rather than saturating the machine. Every
// configuration is itself a test binary that may spin up GOMAXPROCS threads, so
// running one per core means the timeout starts measuring contention between
// flakescope's own workers instead of the package under test.
func defaultWorkers() int {
	if n := runtime.NumCPU() / 2; n > 0 {
		return n
	}
	return 1
}

// Run executes every configuration and returns one Result per configuration, in
// the order the configurations were given. Completion order is not result
// order: a matrix is only reproducible if its report is.
func (r *Runner) Run(ctx context.Context, configs []Config) []Result {
	results := make([]Result, len(configs))
	if len(configs) == 0 {
		return results
	}

	workers := r.Workers
	if workers < 1 {
		workers = defaultWorkers()
	}
	if workers > len(configs) {
		workers = len(configs)
	}

	indices := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range indices {
				results[i] = r.runOne(ctx, configs[i])
			}
		}()
	}
	for i := range configs {
		select {
		case indices <- i:
		case <-ctx.Done():
			// The caller gave up. Stop handing out work; the results for
			// configurations never dispatched keep their zero value, which
			// carries no evidence either way.
			close(indices)
			wg.Wait()
			return results
		}
	}
	close(indices)
	wg.Wait()
	return results
}

func (r *Runner) runOne(ctx context.Context, cfg Config) Result {
	res := Result{Config: cfg}

	runCtx := ctx
	var cancel context.CancelFunc
	if r.Timeout > 0 {
		runCtx, cancel = context.WithTimeout(ctx, r.Timeout)
		defer cancel()
	}

	start := time.Now()
	raw, err := r.exec(runCtx, cfg)
	res.Duration = time.Since(start)
	res.Raw = raw

	// The deadline is checked before the error, because a killed process
	// reports a generic "signal: killed" that says nothing about why.
	if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		res.Outcome = OutcomeTimedOut
		// Parse whatever arrived before the kill. It is partial by
		// construction, so its in-flight tests come back incomplete.
		if run, perr := gotest.ParseBytes(raw); perr == nil {
			res.Run = run
		}
		return res
	}
	if err != nil {
		res.Outcome = OutcomeError
		res.Err = err
		return res
	}

	run, perr := gotest.ParseBytes(raw)
	if perr != nil {
		res.Outcome = OutcomeError
		res.Err = fmt.Errorf("parsing go test output: %w", perr)
		return res
	}
	res.Outcome = OutcomeCompleted
	res.Run = run
	return res
}

// args is the full `go` argument list for one configuration: the
// configuration's own arguments, plus -timeout when TestTimeout is set.
//
// -timeout is the runner's and not the configuration's because it is not a
// knob. It does not vary across the matrix, and a test that only passes under a
// longer one is slow, not flaky.
func (r *Runner) args(cfg Config) []string {
	args := cfg.Args(r.Package)
	if r.TestTimeout <= 0 {
		return args
	}
	return append([]string{args[0], "-timeout=" + r.TestTimeout.String()}, args[1:]...)
}

// goTest is the one real implementation of the exec seam.
func (r *Runner) goTest(ctx context.Context, cfg Config) ([]byte, error) {
	args := r.args(cfg)
	cmd := exec.CommandContext(ctx, "go", args...)
	cmd.Dir = r.Dir
	cmd.Env = cfg.Env(os.Environ())
	// CommandContext only SIGKILLs the `go` process. The test binary it
	// started is a child and survives that kill unless the whole group dies.
	configureKillProcessGroup(cmd)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	out := stdout.Bytes()

	// `go test` exits non-zero whenever a test fails, which is the case
	// flakescope exists to observe. A non-zero exit accompanied by a stream is
	// not an error. A non-zero exit with nothing on stdout is: the go tool
	// could not get as far as running anything.
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && len(bytes.TrimSpace(out)) > 0 {
		return out, nil
	}
	if err != nil {
		msg := bytes.TrimSpace(stderr.Bytes())
		if len(msg) > 0 {
			return out, fmt.Errorf("go %v: %w: %s", args, err, msg)
		}
		return out, fmt.Errorf("go %v: %w", args, err)
	}
	return out, nil
}
