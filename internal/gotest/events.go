// Package gotest decodes the newline-delimited JSON stream that `go test -json`
// writes, and folds it into per-package and per-test results.
//
// The stream is documented at `go doc cmd/test2json`. Two properties of it drive
// the whole design of this package:
//
// An event with an empty Test field is PACKAGE-scoped, not test-scoped. It says
// something about the package as a whole, and it very often arrives while some
// test is still in flight. Attributing it to that test is the standard bug in
// tools that read this stream, and it produces reports blaming a package-level
// failure on whichever test happened to be running. Nothing here ever writes a
// package-scoped event into a Test.
//
// The stream can stop in the middle of a line, which is what a killed or
// timed-out process leaves behind. Tests that were in flight when it stopped are
// INCOMPLETE. They are never treated as passing: a tool that reports a crashed
// run as green is worse than not running at all.
package gotest

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// Event is one decoded line of the stream.
//
// Time, Action, Package, Test, Elapsed and Output are the fields test2json
// documents. ImportPath and FailedBuild are not in that list but appear in real
// Go 1.24 output: build failures are reported as build-output/build-fail events
// that carry ImportPath and NO Package field at all, and the package-level fail
// that follows names the failed build. Without ImportPath a build failure would
// decode as a run of events about the empty package.
type Event struct {
	Time        time.Time
	Action      string
	Package     string
	Test        string
	Elapsed     float64
	Output      string
	ImportPath  string
	FailedBuild string
}

// The actions test2json emits. Listed for reference and used where the parser
// switches on them; unknown actions are ignored rather than rejected, so a
// future Go release adding one does not break flakescope.
const (
	ActionStart       = "start"
	ActionRun         = "run"
	ActionPause       = "pause"
	ActionCont        = "cont"
	ActionPass        = "pass"
	ActionBail        = "bail"
	ActionFail        = "fail"
	ActionOutput      = "output"
	ActionSkip        = "skip"
	ActionBuildOutput = "build-output"
	ActionBuildFail   = "build-fail"
)

// IsPackageScoped reports whether the event describes the package rather than a
// single test. This is exactly the empty-Test rule, named so that call sites
// read as the distinction they are making.
func (e Event) IsPackageScoped() bool { return e.Test == "" }

// Status is the outcome of a test or a package.
type Status int

const (
	// StatusIncomplete means the stream ended before a terminal event arrived.
	// It is the zero value on purpose: anything the parser did not explicitly
	// see finish is incomplete, never passing.
	StatusIncomplete Status = iota
	StatusPass
	StatusFail
	StatusSkip
)

func (s Status) String() string {
	switch s {
	case StatusPass:
		return "pass"
	case StatusFail:
		return "fail"
	case StatusSkip:
		return "skip"
	default:
		return "incomplete"
	}
}

// Test is one test's result within one package.
//
// Name is the full name as it appeared in the stream. Subtests arrive with a
// slash in the name ("TestFoo/case_one") and are recorded under that full name
// as their own Test. v0 does not roll them up into their parent.
type Test struct {
	Package string
	Name    string
	Status  Status
	Elapsed float64
	Output  []string
}

// IsSubtest reports whether the name is a subtest name.
func (t *Test) IsSubtest() bool { return strings.Contains(t.Name, "/") }

// Package is one package's result.
type Package struct {
	Name string
	// Status is the package's own outcome. It is set only from package-scoped
	// events, and is never copied into or out of any Test.
	Status Status
	// Output holds package-scoped output lines only, which is where a build
	// error or a "panic: test timed out" banner lands.
	Output []string
	// BuildFailed records that the package did not compile. This is not a
	// finding about any test; there were no tests.
	BuildFailed bool
	Elapsed     float64

	// Tests in the order they were first seen.
	Tests []*Test
	index map[string]*Test
}

// Test returns the named test, or nil.
func (p *Package) Test(name string) *Test { return p.index[name] }

// Run is everything one `go test -json` invocation reported.
type Run struct {
	// Packages in the order they were first seen.
	Packages []*Package
	// Truncated records that the stream ended part-way through a JSON object,
	// which is what a killed process produces. In-flight tests are marked
	// StatusIncomplete whether or not this is set: a stream can also stop
	// cleanly on a line boundary.
	Truncated bool

	index map[string]*Package
}

// Package returns the named package, or nil.
func (r *Run) Package(name string) *Package { return r.index[name] }

// Tests returns every test across every package, in stream order.
func (r *Run) Tests() []*Test {
	var all []*Test
	for _, p := range r.Packages {
		all = append(all, p.Tests...)
	}
	return all
}

// BuildFailed reports whether any package failed to compile.
func (r *Run) BuildFailed() bool {
	for _, p := range r.Packages {
		if p.BuildFailed {
			return true
		}
	}
	return false
}

// Failed reports whether any package reported failure. A package can fail with
// no failing test, which is the case this whole package exists to keep straight.
func (r *Run) Failed() bool {
	for _, p := range r.Packages {
		if p.Status == StatusFail {
			return true
		}
	}
	return false
}

// Parse decodes a `go test -json` stream.
//
// A malformed line in the middle of the stream is an error: that is corruption,
// not truncation. A partial line at the very end is truncation, and is recorded
// on Run.Truncated rather than returned as an error, because the events that did
// arrive are still worth reporting.
func Parse(r io.Reader) (*Run, error) {
	run := &Run{index: make(map[string]*Package)}
	br := bufio.NewReader(r)

	for lineNo := 1; ; lineNo++ {
		line, err := br.ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("reading line %d: %w", lineNo, err)
		}
		complete := err == nil

		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			if !complete {
				break
			}
			continue
		}

		var ev Event
		if jsonErr := json.Unmarshal([]byte(trimmed), &ev); jsonErr != nil {
			if !complete {
				// The process died part-way through writing this object.
				run.Truncated = true
				break
			}
			return nil, fmt.Errorf("line %d: %w", lineNo, jsonErr)
		}
		run.apply(ev)

		if !complete {
			break
		}
	}

	return run, nil
}

// ParseBytes is Parse over an in-memory stream.
func ParseBytes(b []byte) (*Run, error) { return Parse(strings.NewReader(string(b))) }

func (r *Run) apply(ev Event) {
	name := packageName(ev)
	if name == "" {
		return
	}
	pkg := r.pkg(name)

	if ev.IsPackageScoped() {
		r.applyPackageScoped(pkg, ev)
		return
	}
	applyTestScoped(pkg, ev)
}

// applyPackageScoped handles events with no Test field. Nothing in here may
// touch a Test.
func (r *Run) applyPackageScoped(pkg *Package, ev Event) {
	switch ev.Action {
	case ActionOutput, ActionBuildOutput:
		pkg.Output = append(pkg.Output, ev.Output)
	case ActionBuildFail:
		pkg.BuildFailed = true
		pkg.Status = StatusFail
	case ActionPass:
		pkg.Status = StatusPass
		pkg.Elapsed = ev.Elapsed
	case ActionFail, ActionBail:
		pkg.Status = StatusFail
		pkg.Elapsed = ev.Elapsed
		if ev.FailedBuild != "" {
			pkg.BuildFailed = true
		}
	case ActionSkip:
		pkg.Status = StatusSkip
		pkg.Elapsed = ev.Elapsed
	}
}

func applyTestScoped(pkg *Package, ev Event) {
	test := pkg.test(ev.Test)
	switch ev.Action {
	case ActionOutput:
		test.Output = append(test.Output, ev.Output)
	case ActionPass:
		test.Status = StatusPass
		test.Elapsed = ev.Elapsed
	case ActionFail:
		test.Status = StatusFail
		test.Elapsed = ev.Elapsed
	case ActionSkip:
		test.Status = StatusSkip
		test.Elapsed = ev.Elapsed
	}
	// run, pause and cont only establish that the test exists, which the
	// pkg.test call above already did. A test that never gets past them keeps
	// the zero value, StatusIncomplete.
}

// packageName resolves the package an event is about. Build events identify the
// package by ImportPath instead of Package, and decorate it with the build
// target: "example.com/p [example.com/p.test]".
func packageName(ev Event) string {
	if ev.Package != "" {
		return ev.Package
	}
	path := ev.ImportPath
	if i := strings.Index(path, " ["); i >= 0 {
		path = path[:i]
	}
	return path
}

func (r *Run) pkg(name string) *Package {
	if p, ok := r.index[name]; ok {
		return p
	}
	p := &Package{Name: name, index: make(map[string]*Test)}
	r.index[name] = p
	r.Packages = append(r.Packages, p)
	return p
}

func (p *Package) test(name string) *Test {
	if t, ok := p.index[name]; ok {
		return t
	}
	t := &Test{Package: p.Name, Name: name}
	p.index[name] = t
	p.Tests = append(p.Tests, t)
	return t
}
