// Package report turns a matrix of runner results into the answer flakescope
// exists to give: which tests failed nondeterministically, how often, what
// configuration knob they depend on, and the smallest configuration that
// reproduces each one.
package report

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
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
	// DependenceOrder: the failure rate under shuffle is materially higher
	// than the rate without it. The test depends on what ran before it.
	DependenceOrder
	// DependenceLoad: the failure rate rises with GOMAXPROCS, or with the race
	// detector.
	DependenceLoad
	// DependenceBoth: both axes rise. The axes are evaluated independently and
	// there is no reason a test cannot depend on both, so there is a label that
	// says so rather than a priority order that hides one behind the other.
	DependenceBoth
	// DependenceUndetermined: the evidence does not support a claim on either
	// axis - too few observations in an arm, or rates too close to separate
	// from noise. A test that failed once in sixty configurations lands here,
	// and that is the honest answer rather than the likeliest-looking label.
	DependenceUndetermined
)

func (d Dependence) String() string {
	switch d {
	case DependenceOrder:
		return "order-dependent"
	case DependenceLoad:
		return "load-dependent"
	case DependenceBoth:
		return "order-and-load-dependent"
	case DependenceUndetermined:
		return "undetermined"
	default:
		return ""
	}
}

// Rate is one arm of one axis: how many of that arm's observations failed.
// Everything the classifier decides is decided by comparing two of these.
type Rate struct {
	// Label is how the arm is named in the report, e.g. "shuffled" or
	// "GOMAXPROCS=4".
	Label string
	Fail  int
	Obs   int
}

// Value is the failure rate, or zero for an arm nothing was observed in.
func (r Rate) Value() float64 {
	if r.Obs == 0 {
		return 0
	}
	return float64(r.Fail) / float64(r.Obs)
}

// String is the form the report prints: the counts first, because 3/28 and
// 300/2800 are the same rate and are not the same evidence.
func (r Rate) String() string {
	return fmt.Sprintf("%d/%d (%.0f%%)", r.Fail, r.Obs, r.Value()*100)
}

// ProcsRate is one GOMAXPROCS value's rate. The value is carried alongside the
// rate rather than only inside its label, because the threshold rule compares
// processor counts as numbers.
type ProcsRate struct {
	GOMAXPROCS int
	Rate       Rate
}

// The comparison rule, in three numbers.
//
// These are the whole classifier. They are constants rather than options
// because a threshold a user can turn down is a label a user can manufacture.
const (
	// minArmObservations is the fewest observations an arm may have and still
	// take part in a comparison. Four is not a power calculation; it is the
	// floor below which the arithmetic is meaningless whatever the rates are.
	// The real limit on what a run can resolve is the resolution() figure the
	// report prints, which is usually far above this.
	minArmObservations = 4

	// zThreshold is the pooled two-proportion z the difference between two arms
	// must clear: two standard errors, or about a one-in-forty chance of
	// arising from sampling alone.
	//
	// A normal approximation is coarse at these counts and it is the coarseness
	// that recommends it here: it is conservative for small samples, it is one
	// expression a reader can check, and it needs nothing outside the standard
	// library (CLAUDE.md rule 1).
	zThreshold = 2.0

	// rateRatio is the materiality bar. The higher arm must fail at least twice
	// as often as the lower one, so that a difference which is statistically
	// visible but practically nothing - 51% against 49% across ten thousand
	// runs - is not reported as a dependence. An arm with no failures at all
	// clears this trivially, which is the intended reading: 2% against 0% is a
	// dependence, and 2% against 1.8% is not.
	rateRatio = 2.0
)

// higherThan reports whether hi's failure rate is materially higher than lo's:
// enough observations in both arms, a doubling of the rate, and a difference
// that clears zThreshold.
//
// This is the function that replaces "every failure had shuffle on". That test
// asked whether a configuration EVER produced a failure, which any lopsided
// matrix answers yes to by chance; this one asks whether the arm fails more
// OFTEN, which no sampling accident supplies.
func higherThan(hi, lo Rate) bool {
	if hi.Obs < minArmObservations || lo.Obs < minArmObservations {
		return false
	}
	ph, pl := hi.Value(), lo.Value()
	if ph <= pl || ph < pl*rateRatio {
		return false
	}
	pooled := float64(hi.Fail+lo.Fail) / float64(hi.Obs+lo.Obs)
	se := math.Sqrt(pooled * (1 - pooled) * (1/float64(hi.Obs) + 1/float64(lo.Obs)))
	if se <= 0 {
		return false
	}
	return (ph-pl)/se >= zThreshold
}

// resolution is the smallest failure rate the higher arm could have carried and
// still been reported, given how many observations each arm actually got and a
// lower arm that never failed.
//
// It is the number that turns "undetermined" from a shrug into a statement: a
// run that could not have resolved anything below one in three has not shown
// that a test failing at one in fifty is order-independent, and the report says
// so in those words.
//
// It is found by counting rather than by inverting the z expression: the
// smallest whole number of failures that would have cleared the rule is what a
// run can actually observe, and no algebra can disagree with it.
func resolution(hi, lo Rate) (float64, bool) {
	if hi.Obs < minArmObservations || lo.Obs < minArmObservations {
		return 0, false
	}
	for k := 1; k <= hi.Obs; k++ {
		if higherThan(Rate{Fail: k, Obs: hi.Obs}, Rate{Fail: 0, Obs: lo.Obs}) {
			return float64(k) / float64(hi.Obs), true
		}
	}
	return 0, false
}

// Evidence is every rate the dependence label was read off, kept so the report
// can print the basis next to the claim. A user who can see 3/28 against 0/28
// can judge the classifier; a bare label asks them to trust it.
type Evidence struct {
	// The order axis.
	Shuffled   Rate
	Unshuffled Rate
	// The load axis: one rate per GOMAXPROCS value, ascending, and the race
	// detector's own two arms.
	ByGOMAXPROCS []ProcsRate
	Raced        Rate
	Unraced      Rate
}

// LowestProcs is the control arm of the GOMAXPROCS axis, and HigherProcs is
// every other candidate pooled.
//
// POOLED, NOT COMPARED ONE AT A TIME. Testing the lowest arm against each
// higher arm separately would be three comparisons where the report makes one
// claim, and three chances for noise to supply it. Pooling also degrades in the
// right direction: a failure that needs two processors but not four still lifts
// the pooled rate, just less.
func (e Evidence) LowestProcs() Rate {
	if len(e.ByGOMAXPROCS) == 0 {
		return Rate{}
	}
	return e.ByGOMAXPROCS[0].Rate
}

func (e Evidence) HigherProcs() Rate {
	if len(e.ByGOMAXPROCS) < 2 {
		return Rate{}
	}
	out := Rate{Label: "GOMAXPROCS above " + strconv.Itoa(e.ByGOMAXPROCS[0].GOMAXPROCS)}
	for _, p := range e.ByGOMAXPROCS[1:] {
		out.Fail += p.Rate.Fail
		out.Obs += p.Rate.Obs
	}
	return out
}

// ProcsThreshold is the strong case: every failure had strictly more processors
// than every pass, so there is a clean threshold rather than a raised rate.
//
// It survives from the previous classifier, with the observation floor it never
// had. Without that floor it is satisfied by one failure at four processors and
// one pass at one processor, which is the shape of the bug this rewrite exists
// to remove - a label handed out by construction.
func (e Evidence) ProcsThreshold() bool {
	if e.LowestProcs().Obs < minArmObservations || e.HigherProcs().Obs < minArmObservations {
		return false
	}
	minFail, maxPass := 0, 0
	for _, p := range e.ByGOMAXPROCS {
		if p.Rate.Fail > 0 && (minFail == 0 || p.GOMAXPROCS < minFail) {
			minFail = p.GOMAXPROCS
		}
		if p.Rate.Obs-p.Rate.Fail > 0 && p.GOMAXPROCS > maxPass {
			maxPass = p.GOMAXPROCS
		}
	}
	return minFail > 0 && maxPass > 0 && minFail > maxPass
}

// OrderRises: the failure rate is materially higher with shuffle on.
func (e Evidence) OrderRises() bool { return higherThan(e.Shuffled, e.Unshuffled) }

// LoadRises: the failure rate climbs with GOMAXPROCS - either as a clean
// threshold or as a raised rate - or with the race detector.
//
// The race detector is on this axis rather than an axis of its own because what
// it changes is how much of the schedule the runtime interleaves and inspects,
// which is the same question GOMAXPROCS asks. A user told "load-dependent" and
// shown 8/60 raced against 0/500 unraced has what they need.
func (e Evidence) LoadRises() bool {
	return e.ProcsThreshold() ||
		higherThan(e.HigherProcs(), e.LowestProcs()) ||
		higherThan(e.Raced, e.Unraced)
}

// Resolution is the smallest failure rate this run could have resolved on
// either axis, and whether any axis had the observations to resolve anything.
// It is what the report prints beside an undetermined verdict.
func (e Evidence) Resolution() (float64, bool) {
	best, ok := 0.0, false
	for _, pair := range [][2]Rate{
		{e.Shuffled, e.Unshuffled},
		{e.HigherProcs(), e.LowestProcs()},
	} {
		r, got := resolution(pair[0], pair[1])
		if !got {
			continue
		}
		if !ok || r < best {
			best, ok = r, true
		}
	}
	return best, ok
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
	// Evidence is the per-axis failure rates Dependence was read off. It is
	// populated for flaky tests only, and it is what the report prints beside
	// the label: a classifier that was wrong until today has to show its work.
	Evidence Evidence

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
			e.Evidence = evidenceFor(*e)
			e.Dependence = dependence(e.Evidence)
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

// evidenceFor tallies a flaky test's observations into one rate per arm of each
// axis. It is the whole input to the classifier: everything below is a
// comparison between two of these rates, and nothing else about a run reaches
// the label.
func evidenceFor(t Test) Evidence {
	ev := Evidence{
		Shuffled:   Rate{Label: "shuffled"},
		Unshuffled: Rate{Label: "unshuffled"},
		Raced:      Rate{Label: "-race"},
		Unraced:    Rate{Label: "no -race"},
	}
	byProcs := make(map[int]*Rate)

	observe := func(c runner.Config, failed bool) {
		add := func(r *Rate) {
			r.Obs++
			if failed {
				r.Fail++
			}
		}
		if c.Shuffled() {
			add(&ev.Shuffled)
		} else {
			add(&ev.Unshuffled)
		}
		if c.Race {
			add(&ev.Raced)
		} else {
			add(&ev.Unraced)
		}
		r, ok := byProcs[c.GOMAXPROCS]
		if !ok {
			r = &Rate{Label: fmt.Sprintf("GOMAXPROCS=%d", c.GOMAXPROCS)}
			byProcs[c.GOMAXPROCS] = r
		}
		add(r)
	}

	for _, f := range t.failures {
		observe(f.config, true)
	}
	for _, c := range t.passedIn {
		observe(c, false)
	}

	// Ascending, so the first entry is the control arm. Map iteration order is
	// randomised in Go, and an evidence table that printed its rows in a
	// different order on two runs of the same matrix would be undiffable.
	ev.ByGOMAXPROCS = make([]ProcsRate, 0, len(byProcs))
	for procs, r := range byProcs {
		ev.ByGOMAXPROCS = append(ev.ByGOMAXPROCS, ProcsRate{GOMAXPROCS: procs, Rate: *r})
	}
	sort.Slice(ev.ByGOMAXPROCS, func(i, j int) bool {
		return ev.ByGOMAXPROCS[i].GOMAXPROCS < ev.ByGOMAXPROCS[j].GOMAXPROCS
	})
	return ev
}

// dependence reads the label off the evidence.
//
// THE TWO AXES ARE EVALUATED INDEPENDENTLY AND NEITHER IS CHECKED FIRST. The
// previous implementation asked "were all the failures shuffled, and did
// something unshuffled pass?" before it asked anything about load, which made
// the load rule unreachable for any test the order rule claimed. With a matrix
// that ran 56 of 60 configurations shuffled, the order rule claimed nearly
// everything: for a test that failed once, "all failures shuffled" held by
// chance 56 times in 60, and an unshuffled pass was near-certain. Every flaky
// test in two separate runs came back order-dependent, and one of them was
// afterwards proved parallelism-dependent by hand.
//
// So: both axes are asked, both answers are reported, and a test that raises
// both rates is labelled as depending on both rather than on whichever rule
// happened to be written first.
func dependence(ev Evidence) Dependence {
	order, load := ev.OrderRises(), ev.LoadRises()
	switch {
	case order && load:
		return DependenceBoth
	case order:
		return DependenceOrder
	case load:
		return DependenceLoad
	default:
		return DependenceUndetermined
	}
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
	// Evidence is new in v0.3.0 and is the rate table the dependence label was
	// read off. It is present exactly where "dependence" is - on flaky tests -
	// because there is no evidence to show for a test that never failed.
	//
	// A consumer that only reads "dependence" reads this report as it read the
	// last one. A consumer that wants to check the label can now do so.
	Evidence *wireEvidence `json:"evidence,omitempty"`
}

// wireRate is one arm of one axis.
type wireRate struct {
	Fail int `json:"fail"`
	Obs  int `json:"observations"`
	// Rate is redundant with fail and observations, and is emitted anyway so
	// that a consumer plotting rates does not have to decide what 0/0 means.
	Rate float64 `json:"rate"`
}

type wireProcsRate struct {
	GOMAXPROCS int     `json:"gomaxprocs"`
	Fail       int     `json:"fail"`
	Obs        int     `json:"observations"`
	Rate       float64 `json:"rate"`
}

type wireEvidence struct {
	Shuffled     wireRate        `json:"shuffled"`
	Unshuffled   wireRate        `json:"unshuffled"`
	ByGOMAXPROCS []wireProcsRate `json:"by_gomaxprocs"`
	Raced        wireRate        `json:"raced"`
	Unraced      wireRate        `json:"unraced"`
	// SmallestResolvableRate is the lowest failure rate this run could have
	// reported on any axis, and is null when no axis had the observations to
	// resolve anything. It is not omitted when null: a consumer that has to
	// tell "could not resolve" from "field absent" is a consumer this schema
	// has failed.
	SmallestResolvableRate *float64 `json:"smallest_resolvable_rate"`
}

func toWireRate(r Rate) wireRate {
	return wireRate{Fail: r.Fail, Obs: r.Obs, Rate: r.Value()}
}

func toWireEvidence(ev Evidence) *wireEvidence {
	w := &wireEvidence{
		Shuffled:     toWireRate(ev.Shuffled),
		Unshuffled:   toWireRate(ev.Unshuffled),
		ByGOMAXPROCS: make([]wireProcsRate, 0, len(ev.ByGOMAXPROCS)),
		Raced:        toWireRate(ev.Raced),
		Unraced:      toWireRate(ev.Unraced),
	}
	for _, p := range ev.ByGOMAXPROCS {
		w.ByGOMAXPROCS = append(w.ByGOMAXPROCS, wireProcsRate{
			GOMAXPROCS: p.GOMAXPROCS,
			Fail:       p.Rate.Fail,
			Obs:        p.Rate.Obs,
			Rate:       p.Rate.Value(),
		})
	}
	if res, ok := ev.Resolution(); ok {
		w.SmallestResolvableRate = &res
	}
	return w
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
		if t.Class == ClassFlaky {
			wt.Evidence = toWireEvidence(t.Evidence)
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
			// THE EVIDENCE, NEXT TO THE LABEL. A reader who can see 3/28
			// against 0/28 can judge the claim; a bare label asks them to
			// trust a classifier that was wrong until today, and the way that
			// bug survived was that nothing in the output disagreed with it.
			writeEvidence(&b, t.Evidence)
			// An undetermined verdict is only useful if it says how blind the
			// run was. "We could not tell" and "we could not have told below
			// one in three" are different sentences, and the second one names
			// the --runs that would have answered.
			if t.Dependence == DependenceUndetermined {
				if res, ok := t.Evidence.Resolution(); ok {
					fmt.Fprintf(&b, "      no axis separates these failures; %d configurations could not resolve a rate below %.0f%%\n",
						r.Completed, res*100)
				} else {
					fmt.Fprintf(&b, "      too few observations on either axis to compare rates; %d configurations is not enough\n",
						r.Completed)
				}
			}
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

// writeEvidence prints one line per axis: the two arms of the shuffle axis, the
// rate at each GOMAXPROCS value, and the race detector's two arms.
//
// The race line is omitted when nothing raced, rather than printed as 0/0.
// An arm with no observations is not a measurement of zero.
func writeEvidence(b *strings.Builder, ev Evidence) {
	if ev.Shuffled.Obs > 0 || ev.Unshuffled.Obs > 0 {
		fmt.Fprintf(b, "      shuffle:    %v shuffled, %v unshuffled\n", ev.Shuffled, ev.Unshuffled)
	}
	if len(ev.ByGOMAXPROCS) > 0 {
		parts := make([]string, 0, len(ev.ByGOMAXPROCS))
		for _, p := range ev.ByGOMAXPROCS {
			parts = append(parts, fmt.Sprintf("%d: %v", p.GOMAXPROCS, p.Rate))
		}
		fmt.Fprintf(b, "      GOMAXPROCS: %s\n", strings.Join(parts, ", "))
	}
	if ev.Raced.Obs > 0 {
		fmt.Fprintf(b, "      race:       %v with -race, %v without\n", ev.Raced, ev.Unraced)
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
