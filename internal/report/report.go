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
	"github.com/AarinB1/flakescope/internal/signature"
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

// Cluster is one group of a test's failures that share a normalized signature.
//
// A test with two distinct failure modes has two clusters, and each carries its
// own minimal reproducing configuration. Collapsing them to one would report a
// single command line that reproduces only one of the two bugs, which is worse
// than reporting neither: the user runs it, sees the failure it does reproduce,
// and never learns the other exists.
type Cluster struct {
	// Signature is the normalized form and its hash. The hash is what appears
	// in the output and in the JSON.
	Signature signature.Signature
	// Count is how many configurations produced this signature.
	Count int
	// Minimal is the smallest configuration in THIS cluster, by the same
	// ordering minimal uses for the test as a whole.
	Minimal runner.Config
	// Output is the failure output from Minimal's own run, so the failure the
	// report shows is the one the command line it prints will produce.
	Output []string

	// configs is every configuration in the cluster, for --verbose.
	configs []runner.Config
}

// failure pairs a failing configuration with the output it produced. Clustering
// needs both; a list of configurations alone cannot be grouped by signature.
type failure struct {
	config runner.Config
	output []string
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

	// Clusters groups the failures by normalized signature, most configurations
	// first. It is set for every test that failed at all, flaky or not: a
	// deterministically broken test can still be broken in two different ways,
	// and that is worth saying.
	Clusters []Cluster

	// failures are the configurations behind Fail and the output each produced.
	// passedIn are the configurations behind Pass. Together they drive
	// minimisation, dependence classification and clustering.
	failures []failure
	passedIn []runner.Config
}

// failedConfigs is the configurations that failed, which is what minimisation
// and dependence are defined over.
func (t Test) failedConfigs() []runner.Config {
	out := make([]runner.Config, 0, len(t.failures))
	for _, f := range t.failures {
		out = append(out, f.config)
	}
	return out
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

	// BuildFailed means at least one package did not compile. When that left
	// nothing to measure, it maps to exit code 2 rather than 1: a compile
	// error is not a flakiness finding. When other packages still produced
	// tests, those tests decide the exit code.
	BuildFailed bool
	BuildOutput []string
	// Errs holds the reasons behind Errored.
	Errs []error

	// Tests, sorted by package then name so two runs of the same matrix print
	// identically, and so same-named tests in different packages stay distinct.
	Tests []Test
}

// Build folds the matrix results into a report. base is the configuration the
// matrix was generated from; minimality is measured from it.
func Build(pkg string, base runner.Config, results []runner.Result) Report {
	rep := Report{Package: pkg, Base: base, Configurations: len(results)}

	byKey := make(map[string]*Test)
	order := make([]string, 0)
	get := func(t *gotest.Test) *Test {
		key := t.Package + "\x00" + t.Name
		e, ok := byKey[key]
		if !ok {
			e = &Test{Package: t.Package, Name: t.Name}
			byKey[key] = e
			order = append(order, key)
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
		// OutcomeCompleted is iota zero, so a Result the runner never
		// filled in (parent context cancelled before dispatch) lands
		// here with a nil Run. That is not a completed configuration:
		// counting it as one makes ExitCode report a clean matrix for
		// a run that learned nothing.
		if res.Run == nil {
			continue
		}
		rep.Completed++
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
			// A package that did not compile ran no tests, but `go test ./...`
			// still emits results for every package that did compile. Those
			// stay in the report.
		}
		for _, t := range res.Run.Tests() {
			e := get(t)
			switch t.Status {
			case gotest.StatusPass:
				e.Pass++
				e.passedIn = append(e.passedIn, res.Config)
			case gotest.StatusFail:
				e.Fail++
				e.failures = append(e.failures, failure{config: res.Config, output: t.Output})
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
	for _, key := range order {
		e := byKey[key]
		e.Class = classify(*e)
		e.Clusters = clusterFailures(base, e.failures)
		if e.Class == ClassFlaky {
			e.Dependence = dependence(*e)
			min := minimal(base, e.failedConfigs())
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
	// A compile error with no tests is a tool failure. A compile error that
	// left other packages' results intact is not: those results are what
	// flakescope was asked to measure.
	if r.BuildFailed && len(r.Tests) == 0 {
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

// clusterFailures groups a test's failures by normalized signature.
//
// MINIMALITY IS PER CLUSTER. Each cluster's minimal configuration is chosen by
// lessMinimal, the same ordering documented on minimal above - fewest knobs
// changed from base, then lowest GOMAXPROCS, then race off, then lowest shuffle
// seed - applied within the cluster rather than across the test. The ordering is
// not restated here because it is a rule users rely on and two copies of it will
// eventually disagree.
//
// The representative output is taken from the minimal configuration's own run,
// not from the first failure seen. The report prints a failure and a command
// line next to each other, and they have to be the same run: a user who runs the
// command and sees different output than the report showed has no way to tell
// whether they reproduced the bug.
//
// Clusters are ordered by descending count, then by hash. The hash tie-break has
// no meaning of its own; it exists because map iteration order in Go is
// randomised, and a report that named its clusters in a different order on two
// runs of the same matrix would be useless for the comparison people run
// flakescope to make.
func clusterFailures(base runner.Config, failures []failure) []Cluster {
	if len(failures) == 0 {
		return nil
	}
	byHash := make(map[string]*Cluster)
	var order []*Cluster
	for _, f := range failures {
		sig := signature.Of(f.output)
		c, ok := byHash[sig.Hash]
		if !ok {
			c = &Cluster{Signature: sig, Minimal: f.config, Output: f.output}
			byHash[sig.Hash] = c
			order = append(order, c)
		} else if lessMinimal(base, f.config, c.Minimal) {
			c.Minimal = f.config
			c.Output = f.output
		}
		c.Count++
		c.configs = append(c.configs, f.config)
	}

	sort.SliceStable(order, func(i, j int) bool {
		if order[i].Count != order[j].Count {
			return order[i].Count > order[j].Count
		}
		return order[i].Signature.Hash < order[j].Signature.Hash
	})

	out := make([]Cluster, 0, len(order))
	for _, c := range order {
		out = append(out, *c)
	}
	return out
}

// dependence works out which knob a flaky test's failures track.
//
// Order is tested before load. A test whose failures ALL require shuffle, while
// something unshuffled passed, depends on what ran before it; that is a
// complete explanation, and GOMAXPROCS values among those shuffled runs are
// then just noise. A load-dependent test fails under the unshuffled base too,
// so it never reaches the order branch.
func dependence(t Test) Dependence {
	failedIn := t.failedConfigs()
	if len(failedIn) == 0 || len(t.passedIn) == 0 {
		return DependenceUnknown
	}

	allFailuresShuffled := true
	for _, c := range failedIn {
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
	for _, c := range failedIn {
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
	minFail := failedIn[0].GOMAXPROCS
	for _, c := range failedIn {
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
	// Clusters is new in v0.2.0 and is the only addition to the v0.1.0 schema.
	// Every field above it is unchanged and still populated, so a v0.1.0
	// consumer reads this report exactly as it read the last one. It is always
	// present, empty for a test that never failed, rather than omitted: a
	// consumer that has to distinguish "no clusters" from "field absent" is a
	// consumer this schema has failed.
	Clusters []wireCluster `json:"clusters"`
}

// wireCluster is one group of failures sharing a normalized signature.
type wireCluster struct {
	Signature string     `json:"signature"`
	Kind      string     `json:"kind"`
	Count     int        `json:"count"`
	Minimal   wireConfig `json:"minimal_config"`
	Output    []string   `json:"representative_output,omitempty"`
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
		wt.Clusters = make([]wireCluster, 0, len(t.Clusters))
		for _, c := range t.Clusters {
			wt.Clusters = append(wt.Clusters, wireCluster{
				Signature: c.Signature.Hash,
				Kind:      c.Signature.Kind.String(),
				Count:     c.Count,
				Minimal:   toWireConfig(c.Minimal),
				Output:    c.Output,
			})
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
		if len(r.Tests) == 0 {
			b.WriteString("\nBUILD FAILED - the package does not compile, so nothing was measured.\n")
		} else {
			b.WriteString("\nBUILD FAILED - some packages did not compile; results below are from those that did.\n")
		}
		for _, line := range r.BuildOutput {
			b.WriteString("  " + strings.TrimRight(line, "\n") + "\n")
		}
		if len(r.Tests) == 0 {
			_, err := io.WriteString(w, b.String())
			return err
		}
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
			fmt.Fprintf(&b, "  %s\n", testID(t))
			fmt.Fprintf(&b, "      failed %d/%d configurations (%.0f%%)",
				t.Fail, t.Observations(), t.FailureRate()*100)
			if d := t.Dependence.String(); d != "" {
				fmt.Fprintf(&b, ", %s", d)
			}
			b.WriteString("\n")
			if len(t.Clusters) > 1 {
				writeClusters(&b, t, r.Package, verbose)
				continue
			}
			// One signature is the common case, and it reads as it did before
			// clustering existed: one repro line, not a cluster of one.
			if len(t.Clusters) == 1 {
				fmt.Fprintf(&b, "      all failures share one signature (%s)\n", t.Clusters[0].Signature.Hash)
			}
			if t.Minimal != nil {
				fmt.Fprintf(&b, "      minimal repro: %s %s\n", *t.Minimal, testPkg(t, r.Package))
			}
			if verbose {
				writeOutput(&b, t.FailureOutput)
				if len(t.Clusters) == 1 {
					for _, cfg := range t.Clusters[0].configs {
						fmt.Fprintf(&b, "      also: %s\n", cfg)
					}
				}
			}
		}
	}

	if broken := r.AlwaysFails(); len(broken) > 0 {
		fmt.Fprintf(&b, "\nALWAYS FAILS (%d) - deterministic, not flaky\n", len(broken))
		for _, t := range broken {
			fmt.Fprintf(&b, "  %s\n      failed %d/%d configurations\n", testID(t), t.Fail, t.Observations())
			// A test that fails everywhere can still fail in two different
			// ways, and the second one is invisible without this.
			if len(t.Clusters) > 1 {
				writeClusters(&b, t, r.Package, verbose)
				continue
			}
			if verbose {
				writeOutput(&b, t.FailureOutput)
				if len(t.Clusters) == 1 {
					for _, cfg := range t.Clusters[0].configs {
						fmt.Fprintf(&b, "      also: %s\n", cfg)
					}
				}
			}
		}
	}

	fmt.Fprintf(&b, "\n%d tests observed, %d never failed.\n",
		len(r.Tests), len(r.byClass(ClassNeverFails)))

	_, err := io.WriteString(w, b.String())
	return err
}

// testID names a test so same-named tests in different packages stay distinct
// in the text report. The JSON schema already carries package and name as
// separate fields.
func testID(t Test) string {
	if t.Package == "" {
		return t.Name
	}
	return t.Package + "." + t.Name
}

func testPkg(t Test, fallback string) string {
	if t.Package != "" {
		return t.Package
	}
	return fallback
}

// writeClusters renders a test whose failures did not all share a signature.
//
// Each cluster prints its hash and count, then ONE representative failure, then
// the configuration that reproduces that cluster. Printing every failure in the
// cluster is what --verbose is for; without it a test that failed in 400 of
// 1000 configurations would bury the report it is part of.
func writeClusters(b *strings.Builder, t Test, fallbackPkg string, verbose bool) {
	fmt.Fprintf(b, "      %d distinct failure signatures:\n", len(t.Clusters))
	for _, c := range t.Clusters {
		fmt.Fprintf(b, "      [%s] %d configuration%s\n", c.Signature.Hash, c.Count, plural(c.Count))
		writeOutputIndent(b, c.Output, "        ")
		fmt.Fprintf(b, "        minimal repro: %s %s\n", c.Minimal, testPkg(t, fallbackPkg))
		if verbose {
			for _, cfg := range c.configs {
				fmt.Fprintf(b, "        also: %s\n", cfg)
			}
		}
	}
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func writeOutput(b *strings.Builder, lines []string) {
	writeOutputIndent(b, lines, "      ")
}

func writeOutputIndent(b *strings.Builder, lines []string, indent string) {
	for _, line := range lines {
		b.WriteString(indent + "| " + strings.TrimRight(line, "\n") + "\n")
	}
}
