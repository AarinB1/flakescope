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
	// Timeout bounds each configuration individually. Zero means no timeout.
	Timeout time.Duration
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

// goTest is the one real implementation of the exec seam.
func (r *Runner) goTest(ctx context.Context, cfg Config) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "go", cfg.Args(r.Package)...)
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
			return out, fmt.Errorf("go %v: %w: %s", cfg.Args(r.Package), err, msg)
		}
		return out, fmt.Errorf("go %v: %w", cfg.Args(r.Package), err)
	}
	return out, nil
}
