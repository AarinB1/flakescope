// Package report turns a matrix of runner results into the answer flakescope
// exists to give: which tests failed nondeterministically, how often, what
// configuration knob they depend on, and the smallest configuration that
// reproduces each one.
package report

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/AarinB1/flakescope/internal/gotest"
	"github.com/AarinB1/flakescope/internal/runner"
)

// Classification is what a test's results across the matrix add up to.
type Classification int

const (
	// ClassNeverFails: the test passed in every configuration that produced
	// evidence.
	ClassNeverFails Classification = iota
	// ClassAlwaysFails: the test failed in every configuration that produced
	// evidence. This is a deterministically broken test and is NOT flaky.
	// Reporting it as flaky is the failure mode that makes people stop
	// trusting the tool, so it is a separate class and a separate section of
	// the output.
	ClassAlwaysFails
	// ClassFlaky: the test both passed and failed across the matrix.
	ClassFlaky
)

func (c Classification) String() string {
	switch c {
	case ClassAlwaysFails:
		return "always-fails"
	case ClassFlaky:
		return "flaky"
	default:
		return "never-fails"
	}
}

// Dependence names the configuration knob a flaky test's failure tracks. This
// is the actual output value of flakescope: "flaky" alone tells you to rerun,
// whereas "order-dependent" tells you what to go and read.
type Dependence int

const (
	// DependenceNone is used for tests that are not flaky.
	DependenceNone Dependence = iota
	// DependenceOrder: every failure had shuffle on and at least one
	// unshuffled configuration passed. The test depends on what ran before it.
	DependenceOrder
	// DependenceLoad: failures track the race detector or a GOMAXPROCS
	// threshold rather than test order.
	DependenceLoad
	// DependenceUnknown: the failures do not line up with any single knob.
	// Saying so is better than picking the most likely-looking one.
	DependenceUnknown
)

func (d Dependence) String() string {
	switch d {
	case DependenceOrder:
		return "order-dependent"
	case DependenceLoad:
		return "load-dependent"
	case DependenceUnknown:
		return "undetermined"
	default:
		return ""
	}
}

// Test is one test's results across the whole matrix.
type Test struct {
	Package string
	Name    string

	// Pass and Fail count only configurations that produced evidence: runs
	// that completed. A run that timed out or errored contributes to neither,
	// because a deadline that fired says nothing about the test.
	Pass int
	Fail int
	// Skip and Incomplete are carried so the output can say why a test has
	// fewer observations than there were configurations.
	Skip       int
	Incomplete int

	Class      Classification
	Dependence Dependence

	// Minimal is the fewest-knobs-from-default configuration that reproduced
	// the failure. It is set only for flaky tests; for an always-failing test
	// the default configuration reproduces it and there is nothing to minimise.
	Minimal *runner.Config

	// FailureOutput is the output of the first observed failure, kept so
	// --verbose can show what the failure looked like.
	FailureOutput []string

	// failedIn and passedIn are the configurations behind Fail and Pass. They
	// drive both minimisation and dependence classification.
	failedIn []runner.Config
	passedIn []runner.Config
}

// Observations is the number of configurations that produced a pass or a fail.
func (t Test) Observations() int { return t.Pass + t.Fail }

// FailureRate is fails over observations. Configurations that timed out,
// errored, skipped or never finished are not in the denominator: dividing by
// them would let a slow machine talk flakescope out of a finding.
func (t Test) FailureRate() float64 {
	n := t.Observations()
	if n == 0 {
		return 0
	}
	return float64(t.Fail) / float64(n)
}

// Report is the whole run.
type Report struct {
	Package string
	// Base is the configuration the matrix was generated from and that
	// minimality is measured against.
	Base runner.Config

	Configurations int
	Completed      int
	TimedOut       int
	Errored        int

	// BuildFailed means the package did not compile. It is not a finding about
	// flakiness, and it maps to exit code 2 rather than 1.
	BuildFailed bool
	BuildOutput []string
	// Errs holds the reasons behind Errored.
	Errs []error

	// Tests, sorted by name so two runs of the same matrix print identically.
	Tests []Test
}

// Build folds the matrix results into a report. base is the configuration the
// matrix was generated from; minimality is measured from it.
func Build(pkg string, base runner.Config, results []runner.Result) Report {
	rep := Report{Package: pkg, Base: base, Configurations: len(results)}

	byName := make(map[string]*Test)
	order := make([]string, 0)
	get := func(t *gotest.Test) *Test {
		e, ok := byName[t.Name]
		if !ok {
			e = &Test{Package: t.Package, Name: t.Name}
			byName[t.Name] = e
			order = append(order, t.Name)
		}
		return e
	}

	for _, res := range results {
		switch res.Outcome {
		case runner.OutcomeTimedOut:
			rep.TimedOut++
			continue
		case runner.OutcomeError:
			rep.Errored++
			if res.Err != nil {
				rep.Errs = append(rep.Errs, res.Err)
			}
			continue
		}
		rep.Completed++
		if res.Run == nil {
			continue
		}
		if res.Run.BuildFailed() {
			// The diagnostic is kept from the first failing configuration only.
			// Every configuration fails to build identically, and printing the
			// same compiler error twenty times buries it.
			if !rep.BuildFailed {
				for _, p := range res.Run.Packages {
					if p.BuildFailed {
						rep.BuildOutput = append(rep.BuildOutput, p.Output...)
					}
				}
			}
			rep.BuildFailed = true
			// A package that did not compile ran no tests. There is nothing to
			// attribute to any test name.
			continue
		}
		for _, t := range res.Run.Tests() {
			e := get(t)
			switch t.Status {
			case gotest.StatusPass:
				e.Pass++
				e.passedIn = append(e.passedIn, res.Config)
			case gotest.StatusFail:
				e.Fail++
				e.failedIn = append(e.failedIn, res.Config)
				if e.FailureOutput == nil {
					e.FailureOutput = t.Output
				}
			case gotest.StatusSkip:
				e.Skip++
			default:
				e.Incomplete++
			}
		}
	}

	sort.Strings(order)
	for _, name := range order {
		e := byName[name]
		e.Class = classify(*e)
		if e.Class == ClassFlaky {
			e.Dependence = dependence(*e)
			min := minimal(base, e.failedIn)
			e.Minimal = &min
		}
		rep.Tests = append(rep.Tests, *e)
	}
	return rep
}

func classify(t Test) Classification {
	switch {
	case t.Fail == 0:
		return ClassNeverFails
	case t.Pass == 0:
		return ClassAlwaysFails
	default:
		return ClassFlaky
	}
}

// Flaky returns the flaky tests.
func (r Report) Flaky() []Test { return r.byClass(ClassFlaky) }

// AlwaysFails returns the deterministically broken tests, which are reported
// separately from the flaky ones and do not affect the exit code.
func (r Report) AlwaysFails() []Test { return r.byClass(ClassAlwaysFails) }

func (r Report) byClass(c Classification) []Test {
	var out []Test
	for _, t := range r.Tests {
		if t.Class == c {
			out = append(out, t)
		}
	}
	return out
}

// Exit codes. These are a compatibility surface from v1.0.0 (CLAUDE.md rule 3).
const (
	// ExitClean: no flaky tests found.
	ExitClean = 0
	// ExitFlaky: at least one flaky test found.
	ExitFlaky = 1
	// ExitToolFailure: flakescope itself could not do its job - bad arguments,
	// or the package would not build. A build failure is not a finding about
	// flakiness, so it must not share an exit code with one.
	ExitToolFailure = 2
)

// ExitCode maps the report to a process exit code.
func (r Report) ExitCode() int {
	if r.BuildFailed {
		return ExitToolFailure
	}
	// Nothing completed means nothing was learned. Reporting "no flaky tests"
	// after twenty timeouts would be a lie told with a zero.
	if r.Configurations > 0 && r.Completed == 0 {
		return ExitToolFailure
	}
	if len(r.Flaky()) > 0 {
		return ExitFlaky
	}
	return ExitClean
}

// knobsChanged counts how many of the three varying knobs differ from base.
// Count is not counted: it does not vary across a matrix.
func knobsChanged(base, cfg runner.Config) int {
	n := 0
	if cfg.ShuffleSeed != base.ShuffleSeed {
		n++
	}
	if cfg.GOMAXPROCS != base.GOMAXPROCS {
		n++
	}
	if cfg.Race != base.Race {
		n++
	}
	return n
}

// minimal picks the smallest configuration that reproduced a failure.
//
// THE ORDERING, in full, most significant first:
//
//  1. Fewest knobs changed from base. A repro that needs one knob is a better
//     bug report than one that needs three, whatever the knobs are.
//  2. Lowest GOMAXPROCS. Fewer processors is both cheaper to rerun and a
//     stronger statement about the bug.
//  3. Race off before race on. A failure that does not need the race detector
//     is reproducible with a plain `go test`.
//  4. Lowest shuffle seed. This is a pure tie-break with no meaning of its own;
//     it exists so that the same matrix always names the same configuration.
//
// The rule is written out here, rather than left implicit in the comparison
// function, because it is a rule users have to be able to rely on: it decides
// which single command line flakescope tells them to run.
func minimal(base runner.Config, candidates []runner.Config) runner.Config {
	best := candidates[0]
	for _, c := range candidates[1:] {
		if lessMinimal(base, c, best) {
			best = c
		}
	}
	return best
}

func lessMinimal(base, a, b runner.Config) bool {
	if ka, kb := knobsChanged(base, a), knobsChanged(base, b); ka != kb {
		return ka < kb
	}
	if a.GOMAXPROCS != b.GOMAXPROCS {
		return a.GOMAXPROCS < b.GOMAXPROCS
	}
	if a.Race != b.Race {
		return !a.Race
	}
	return a.ShuffleSeed < b.ShuffleSeed
}

// dependence works out which knob a flaky test's failures track.
//
// Order is tested before load. A test whose failures ALL require shuffle, while
// something unshuffled passed, depends on what ran before it; that is a
// complete explanation, and GOMAXPROCS values among those shuffled runs are
// then just noise. A load-dependent test fails under the unshuffled base too,
// so it never reaches the order branch.
func dependence(t Test) Dependence {
	if len(t.failedIn) == 0 || len(t.passedIn) == 0 {
		return DependenceUnknown
	}

	allFailuresShuffled := true
	for _, c := range t.failedIn {
		if !c.Shuffled() {
			allFailuresShuffled = false
			break
		}
	}
	anyUnshuffledPass := false
	for _, c := range t.passedIn {
		if !c.Shuffled() {
			anyUnshuffledPass = true
			break
		}
	}
	if allFailuresShuffled && anyUnshuffledPass {
		return DependenceOrder
	}

	allFailuresRaced := true
	for _, c := range t.failedIn {
		if !c.Race {
			allFailuresRaced = false
			break
		}
	}
	anyUnracedPass := false
	for _, c := range t.passedIn {
		if !c.Race {
			anyUnracedPass = true
			break
		}
	}
	if allFailuresRaced && anyUnracedPass {
		return DependenceLoad
	}

	// A GOMAXPROCS threshold: every failure had strictly more processors than
	// every pass.
	minFail := t.failedIn[0].GOMAXPROCS
	for _, c := range t.failedIn {
		if c.GOMAXPROCS < minFail {
			minFail = c.GOMAXPROCS
		}
	}
	maxPass := t.passedIn[0].GOMAXPROCS
	for _, c := range t.passedIn {
		if c.GOMAXPROCS > maxPass {
			maxPass = c.GOMAXPROCS
		}
	}
	if minFail > maxPass {
		return DependenceLoad
	}

	return DependenceUnknown
}

// ---------------------------------------------------------------------------
// Output
// ---------------------------------------------------------------------------

// The JSON schema is a compatibility surface from v1.0.0: additive changes
// only (CLAUDE.md rule 3). It is defined by these types rather than by tags on
// the internal structs so that renaming an internal field cannot silently
// rename a wire field.
type wireReport struct {
	Package        string     `json:"package"`
	Configurations int        `json:"configurations"`
	Completed      int        `json:"completed"`
	TimedOut       int        `json:"timed_out"`
	Errored        int        `json:"errored"`
	BuildFailed    bool       `json:"build_failed"`
	BuildOutput    []string   `json:"build_output,omitempty"`
	ExitCode       int        `json:"exit_code"`
	Base           wireConfig `json:"base"`
	Tests          []wireTest `json:"tests"`
}

type wireTest struct {
	Package     string      `json:"package"`
	Name        string      `json:"name"`
	Pass        int         `json:"pass"`
	Fail        int         `json:"fail"`
	Skip        int         `json:"skip"`
	Incomplete  int         `json:"incomplete"`
	FailureRate float64     `json:"failure_rate"`
	Class       string      `json:"classification"`
	Dependence  string      `json:"dependence,omitempty"`
	Minimal     *wireConfig `json:"minimal_config,omitempty"`
}

type wireConfig struct {
	ShuffleSeed int64  `json:"shuffle_seed"`
	GOMAXPROCS  int    `json:"gomaxprocs"`
	Race        bool   `json:"race"`
	Count       int    `json:"count"`
	CommandLine string `json:"command_line"`
}

func toWireConfig(c runner.Config) wireConfig {
	return wireConfig{
		ShuffleSeed: c.ShuffleSeed,
		GOMAXPROCS:  c.GOMAXPROCS,
		Race:        c.Race,
		Count:       c.Count,
		CommandLine: c.String(),
	}
}

// MarshalJSON emits the documented report schema.
func (r Report) MarshalJSON() ([]byte, error) {
	w := wireReport{
		Package:        r.Package,
		Configurations: r.Configurations,
		Completed:      r.Completed,
		TimedOut:       r.TimedOut,
		Errored:        r.Errored,
		BuildFailed:    r.BuildFailed,
		BuildOutput:    r.BuildOutput,
		ExitCode:       r.ExitCode(),
		Base:           toWireConfig(r.Base),
		Tests:          make([]wireTest, 0, len(r.Tests)),
	}
	for _, t := range r.Tests {
		wt := wireTest{
			Package:     t.Package,
			Name:        t.Name,
			Pass:        t.Pass,
			Fail:        t.Fail,
			Skip:        t.Skip,
			Incomplete:  t.Incomplete,
			FailureRate: t.FailureRate(),
			Class:       t.Class.String(),
			Dependence:  t.Dependence.String(),
		}
		if t.Minimal != nil {
			c := toWireConfig(*t.Minimal)
			wt.Minimal = &c
		}
		w.Tests = append(w.Tests, wt)
	}
	return json.Marshal(w)
}

// WriteText renders the human-readable report.
func (r Report) WriteText(w io.Writer, verbose bool) error {
	var b strings.Builder

	fmt.Fprintf(&b, "flakescope %s\n", r.Package)
	fmt.Fprintf(&b, "%d configurations: %d completed, %d timed out, %d errored\n",
		r.Configurations, r.Completed, r.TimedOut, r.Errored)

	if r.BuildFailed {
		b.WriteString("\nBUILD FAILED - the package does not compile, so nothing was measured.\n")
		for _, line := range r.BuildOutput {
			b.WriteString("  " + strings.TrimRight(line, "\n") + "\n")
		}
		_, err := io.WriteString(w, b.String())
		return err
	}

	for _, e := range r.Errs {
		fmt.Fprintf(&b, "  error: %v\n", e)
	}

	flaky := r.Flaky()
	b.WriteString("\n")
	if len(flaky) == 0 {
		b.WriteString("No flaky tests found.\n")
	} else {
		fmt.Fprintf(&b, "FLAKY (%d)\n", len(flaky))
		for _, t := range flaky {
			fmt.Fprintf(&b, "  %s\n", t.Name)
			fmt.Fprintf(&b, "      failed %d/%d configurations (%.0f%%)",
				t.Fail, t.Observations(), t.FailureRate()*100)
			if d := t.Dependence.String(); d != "" {
				fmt.Fprintf(&b, ", %s", d)
			}
			b.WriteString("\n")
			if t.Minimal != nil {
				fmt.Fprintf(&b, "      minimal repro: %s %s\n", *t.Minimal, r.Package)
			}
			if verbose {
				writeOutput(&b, t.FailureOutput)
			}
		}
	}

	if broken := r.AlwaysFails(); len(broken) > 0 {
		fmt.Fprintf(&b, "\nALWAYS FAILS (%d) - deterministic, not flaky\n", len(broken))
		for _, t := range broken {
			fmt.Fprintf(&b, "  %s\n      failed %d/%d configurations\n", t.Name, t.Fail, t.Observations())
			if verbose {
				writeOutput(&b, t.FailureOutput)
			}
		}
	}

	fmt.Fprintf(&b, "\n%d tests observed, %d never failed.\n",
		len(r.Tests), len(r.byClass(ClassNeverFails)))

	_, err := io.WriteString(w, b.String())
	return err
}

func writeOutput(b *strings.Builder, lines []string) {
	for _, line := range lines {
		b.WriteString("      | " + strings.TrimRight(line, "\n") + "\n")
	}
}
