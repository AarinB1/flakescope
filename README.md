# flakescope

[![CI](https://github.com/AarinB1/flakescope/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/AarinB1/flakescope/actions/workflows/ci.yml?query=branch%3Amain)

A CLI that reruns a Go package's tests under many configurations and reports
which tests fail nondeterministically, along with the minimal configuration
that reproduces each.

**flakescope varies configurations, not goroutine interleavings.** Go has no
seedable goroutine scheduler outside `testing/synctest`, so no tool can replay
a particular ordering of goroutines. What flakescope does is vary the knobs that
do change behaviour — test order, available processors, the race detector — and
tell you which one a failure depends on.

## Install

```
go install github.com/AarinB1/flakescope/cmd/flakescope@latest
```

flakescope has zero non-stdlib dependencies. `go.mod` has no require block, and
a test enforces it.

## Usage

```
flakescope [flags] <package>
```

| Flag | Default | Meaning |
| --- | --- | --- |
| `--runs N` | 20 | number of configurations to run; see [Scale](#scale) |
| `--json` | off | emit the machine-readable report instead of text |
| `--timeout D` | 10m | per-configuration timeout |
| `--verbose` | off | list every configuration behind each failure group |

```
$ flakescope ./internal/queue
flakescope ./internal/queue
20 configurations: 20 completed, 0 timed out, 0 errored

FLAKY (2)
  TestDrainOrder
      failed 5/20 configurations (25%), order-dependent
      shuffle:    5/8 (62%) shuffled, 0/12 (0%) unshuffled
      GOMAXPROCS: 1: 1/6 (17%), 2: 2/5 (40%), 4: 2/9 (22%)
      race:       0/4 (0%) with -race, 5/16 (31%) without
      all failures share one signature (5c52562121918ec4)
      minimal repro: GOMAXPROCS=8 go test -shuffle=1 -count=1 ./internal/queue
  TestWorkerPool
      failed 14/20 configurations (70%), load-dependent
      shuffle:    6/8 (75%) shuffled, 8/12 (67%) unshuffled
      GOMAXPROCS: 1: 0/6 (0%), 2: 5/5 (100%), 4: 9/9 (100%)
      race:       3/4 (75%) with -race, 11/16 (69%) without
      2 distinct failure signatures:
      [b1471e97a40a01c3] 9 configurations
        |     pool_test.go:108: parallel execution exposed the bug: GOMAXPROCS=4
        minimal repro: GOMAXPROCS=4 go test -count=1 ./internal/queue
      [235f6343f3c5fd50] 5 configurations
        |     pool_test.go:108: parallel execution exposed the bug: GOMAXPROCS=2
        minimal repro: GOMAXPROCS=2 go test -count=1 ./internal/queue

ALWAYS FAILS (1) - deterministic, not flaky
  TestQuotaExceeded
      failed 20/20 configurations

31 tests observed, 28 never failed.
```

## What the report says

**Classification.** A test that failed in every configuration is *always-fails*,
not flaky: it is deterministically broken, it is reported in its own section,
and it does not affect the exit code. A test that never failed is *never-fails*.
Only a test that both passed and failed is *flaky*.

**Dependence.** For each flaky test, flakescope names the knob its failures
track, by comparing the failure RATE between the two arms of each axis:

- *order-dependent* — the failure rate with `-shuffle` on is materially higher
  than the rate with it off. The test depends on what ran before it.
- *load-dependent* — the rate rises with `GOMAXPROCS`, or under the race
  detector.
- *order-and-load-dependent* — both. The two axes are evaluated independently,
  so neither can hide the other.
- *undetermined* — neither difference is large enough to separate from noise.

The rates are printed next to the label. A reader who can see `5/8` shuffled
against `0/12` unshuffled can judge the claim; a bare label asks them to trust
the classifier.

A difference is reported only when both arms hold at least four observations,
the higher arm fails at least twice as often, and the difference clears a
pooled two-proportion z of 2. **This is what the tool can and cannot see:**

| `--runs` | shuffle axis | `GOMAXPROCS` axis |
| --- | --- | --- |
| 20 (default) | 38% | 50% |
| 60 | 14% | 20% |
| 200 | 4% | 6% |
| 1000 | 1% | 2% |

A failure that reproduces 2% of the time - an ordinary rate for a real
parallelism bug - is invisible below about a thousand runs, and flakescope says
`undetermined` rather than guessing. When it does, it prints the rate that run
could have resolved, so the next `--runs` is a number rather than a hunch:

```
  TestWorkerPool
      failed 1/60 configurations (2%), undetermined
      shuffle:    1/28 (4%) shuffled, 0/32 (0%) unshuffled
      GOMAXPROCS: 1: 0/15 (0%), 2: 0/15 (0%), 4: 1/30 (3%)
      no axis separates these failures; 60 configurations could not resolve a rate below 14%
```

Until v0.3.0 the order rule was checked first and asked only whether every
failure happened to be shuffled. With a matrix that ran 56 of 60 configurations
under a seed, that was true by chance for almost any test that failed at all,
so almost every flaky test came back order-dependent and the load rule was
unreachable behind it.

**Failure clusters.** A test's failures are grouped by a normalized signature,
so that the same bug seen twenty times is one finding rather than twenty. Each
cluster reports its signature hash, how many configurations produced it, one
representative failure, and its own minimal reproducing configuration. When
every failure shares one signature - the common case - the report says so in a
line and prints one repro, rather than a cluster of one.

The signature is built from the failure output. An assertion (`t.Errorf`,
`t.Fatalf`) is identified by its `file.go:line` and message. A panic or a race
report is identified by its message plus the top five stack frames. These are
normalized away first, because they change between runs without the bug
changing:

| Class | Example | Becomes |
| --- | --- | --- |
| goroutine IDs | `goroutine 42 [running]:` | `goroutine N [running]:` |
| hex addresses | `0x00c000192` | `0xADDR` |
| frame offsets | `+0x1c` | `+0xOFF` |
| build temp paths | `/tmp/go-build123456/` | `/tmp/go-build/` |
| durations | `1.003s`, `12ms` | `Ns` |
| module cache | `/root/go/pkg/mod/` | `MODCACHE/` |

**Nothing else is normalized, and integers in messages least of all.** The
governing rule is *prefer splitting over merging*. A cluster that splits one bug
into two is visible: you see two clusters with near-identical text and read
both. A cluster that merges two bugs is silent: you fix one cause, rerun, and
the other is still there - and the report that hid it is why you did not look.
So `got 1234, want 1000` and `got 7, want 1000` are reported separately. They
are probably the same bug. The rule that merged them would also merge two that
are not, and nothing would ever tell you.

Two known limits, both on the splitting side. `go test -json` does not
distinguish `t.Logf` output from `t.Errorf` output, so a failing test that also
logs carries its log lines into its signature. And the race detector picks which
of a racing pair to call the "Previous write", so one race can produce two
different reports and land in two clusters.

**Minimal reproducing configuration.** Among the configurations that reproduced
the failure, flakescope reports the smallest, ordered by:

1. fewest knobs changed from the default (shuffle off, `GOMAXPROCS` at
   `runtime.NumCPU()`, race off, count 1);
2. then lowest `GOMAXPROCS`;
3. then race off before race on;
4. then lowest shuffle seed.

Rule 4 is a pure tie-break with no meaning of its own. It exists so that the
same matrix always names the same configuration.

**Minimality is per cluster.** A test with two failure modes has two minimal
configurations, chosen by that same ordering within each cluster. Reporting one
command line for both would hand you something that reproduces only one of the
two bugs - and you would run it, see a failure, and never learn the other
existed. The representative failure shown next to each repro comes from that
configuration's own run, so the output you read and the command you run are the
same thing.

## The `--json` report schema

From v1.0.0 this schema is a compatibility surface: additive changes only. The
`clusters` array landed in v0.2.0, before the freeze, precisely so it would not
have to be a breaking change afterwards. Every field that existed in v0.1.0 is
still present and still populated.

```jsonc
{
  "package": "./internal/queue",   // the package pattern as given
  "configurations": 20,            // configurations generated
  "completed": 20,                 // configurations that produced a stream
  "timed_out": 0,
  "errored": 0,
  "build_failed": false,
  "build_output": ["..."],         // omitted unless build_failed
  "exit_code": 1,                  // the code the process will exit with
  "base": {                        // what minimality is measured against
    "shuffle_seed": 0,
    "gomaxprocs": 8,
    "race": false,
    "count": 1,
    "command_line": "GOMAXPROCS=8 go test -count=1"
  },
  "tests": [
    {
      "package": "./internal/queue",
      "name": "TestWorkerPool",
      "pass": 7,
      "fail": 13,
      "skip": 0,
      "incomplete": 0,            // configurations where it never finished
      "failure_rate": 0.65,       // fail / (pass + fail)
      "classification": "flaky",  // flaky | always-fails | never-fails
      "dependence": "load-dependent",  // omitted when not flaky
      "minimal_config": { },      // as "base"; omitted when not flaky
      "evidence": {               // the rates the label was read off; flaky tests only
        "shuffled":   { "fail": 6, "observations": 8,  "rate": 0.75 },
        "unshuffled": { "fail": 8, "observations": 12, "rate": 0.667 },
        "by_gomaxprocs": [        // ascending; the first entry is the control arm
          { "gomaxprocs": 1, "fail": 0, "observations": 6, "rate": 0 },
          { "gomaxprocs": 2, "fail": 5, "observations": 5, "rate": 1 },
          { "gomaxprocs": 4, "fail": 9, "observations": 9, "rate": 1 }
        ],
        "raced":   { "fail": 3,  "observations": 4,  "rate": 0.75 },
        "unraced": { "fail": 11, "observations": 16, "rate": 0.6875 },
        "smallest_resolvable_rate": 0.375   // null if no axis could resolve anything
      },
      "clusters": [               // always present; empty if the test never failed
        {
          "signature": "b1471e97a40a01c3",   // sha256 of the normalized form, 8 bytes
          "kind": "assertion",               // assertion | panic | race | unknown
          "count": 9,                        // configurations producing this signature
          "minimal_config": { },             // as "base"; this cluster's own smallest
          "representative_output": ["..."]   // from minimal_config's run
        }
      ]
    }
  ]
}
```

Notes a consumer can rely on:

- `clusters` is present on every test, as an empty array for one that never
  failed. Absence and emptiness are not two different states to handle.
- `evidence` is new in v0.3.0 and appears exactly where `dependence` does: on
  flaky tests. Its `smallest_resolvable_rate` is `null`, never absent, when no
  axis had the observations to resolve anything.
- Every field that existed before v0.3.0 still means what it meant. `evidence`
  is additive, and a consumer that ignores it reads this report as it read the
  last one.
- The `count` values across a test's clusters sum to that test's `fail`.
- Clusters are ordered by descending `count`, then by `signature`. The same
  matrix always produces the same order.
- `representative_output` comes from the run named by that cluster's
  `minimal_config`, not from an arbitrary member of the cluster.
- `minimal_config` on the test and `minimal_config` on a cluster are chosen by
  the same ordering, over different candidate sets: the whole test's failures,
  and that cluster's.

## Exit codes

| Code | Meaning |
| --- | --- |
| `0` | no flaky tests found |
| `1` | flaky tests found |
| `2` | flakescope itself failed: bad arguments, or the package would not build |

A build failure is exit 2, not exit 1. It is not a finding about flakiness.
A run in which no configuration completed is also exit 2: reporting "no flaky
tests" after twenty timeouts would be a lie told with a zero.

From v1.0.0 the exit codes and the `--json` schema are a compatibility surface;
changes to them will be additive only.

## Scale

The matrix is laid out over **cells**: one per (shuffle on or off) x
`GOMAXPROCS` candidate, so eight cells in the usual case. Runs are dealt one per
cell in rotation, which is what lets a failure rate be compared between arms
rather than inferred from a single observation.

Shuffled configurations are all distinct - every one carries a seed no other run
uses, because a repeated seed reruns the same test order and buys nothing.
Unshuffled configurations repeat, deliberately: there are only sixteen distinct
unshuffled configurations in the whole space, so an arm large enough to state a
rate must rerun the same command line. Two runs of one unshuffled configuration
are two independent observations of a nondeterministic failure, which is the
only way to learn that a test fails a fifth of the time rather than always or
never. A test asserts both halves separately.

The race detector is switched on for **one run in seven** rather than every
other one. `-race` is the knob that dominates wall-clock and the one with the
least to say - it answers a yes/no question, and a sample answers that as well
as a census. For a race build costing 10x a plain one, alternating would make
the matrix 5.5x a race-free run; one in seven makes it 2.3x. Seven is odd on
purpose: the matrix flips shuffle on every run, so an even period would put
every raced run in the same shuffle arm and confound the two axes.

### Measured

Against `testdata/flakypkg` on a 4-core linux/amd64 machine, Go 1.24.7,
flakescope's default worker count (`NumCPU/2`, so 2):

| Configurations | Cold | Warm |
| --- | --- | --- |
| 1000 | 197 s | 171 s |
| 700 | - | 116 s |
| 600 | - | 103 s |

Cold means an empty `GOCACHE`, so the 26-second difference is the plain and
race-instrumented standard library being built once and then amortised over a
thousand runs.

**A thousand configurations takes about three minutes warm, not two.** The
largest round number that finishes inside two minutes on this machine is **700,
at 116 seconds**. Both numbers are floors rather than forecasts: the fixture's
tests do almost nothing, so nearly all of that time is the `go` tool starting up
and linking. A package with real tests is dominated by its own test time, and
1000 runs of it will take 1000 times however long one run takes, divided by the
worker count.

That 1000-configuration run is also what clustering is for. It produced 2,174
individual failures across three tests, and reported them as four clusters:

```
TestLoadDependent   668 failures  ->  2 clusters (335 at GOMAXPROCS=4, 333 at GOMAXPROCS=2)
TestOrderDependent  506 failures  ->  1 cluster
TestAlwaysFails    1000 failures  ->  1 cluster
```

## Development

```
make check    # gofmt, go vet, go test
```

flakescope's own tests replay recorded `go test -json` streams from `testdata/`
rather than invoking `go test`, so the test suite of a flaky-test detector is
not itself flaky. The single exception is
`TestIntegrationRunsTheRealGoTool`, which is named as such and skipped under
`-short`.
