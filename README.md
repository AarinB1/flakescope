# flakescope

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
| `--runs N` | 20 | number of configurations to run; all distinct, see [Scale](#scale) |
| `--json` | off | emit the machine-readable report instead of text |
| `--timeout D` | 10m | per-configuration timeout |
| `--verbose` | off | list every configuration behind each failure group |

```
$ flakescope ./internal/queue
flakescope ./internal/queue
20 configurations: 20 completed, 0 timed out, 0 errored

FLAKY (2)
  TestDrainOrder
      failed 6/20 configurations (30%), order-dependent
      all failures share one signature (5c52562121918ec4)
      minimal repro: GOMAXPROCS=8 go test -shuffle=1 -count=1 ./internal/queue
  TestWorkerPool
      failed 13/20 configurations (65%), load-dependent
      2 distinct failure signatures:
      [b1471e97a40a01c3] 9 configurations
        |     pool_test.go:108: parallel execution exposed the bug: GOMAXPROCS=4
        minimal repro: GOMAXPROCS=4 go test -count=1 ./internal/queue
      [235f6343f3c5fd50] 4 configurations
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
track:

- *order-dependent* — every failure had `-shuffle` on and at least one
  unshuffled run passed. The test depends on what ran before it.
- *load-dependent* — every failure needed the race detector, or every failure
  had strictly more processors than every pass.
- *undetermined* — the failures do not line up with any single knob. Saying so
  is better than guessing.

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

flakescope generates every configuration distinctly. `--runs 1000` means a
thousand different configurations, not a thousand invocations of forty of them,
and a test enforces it: a repeated configuration costs a full `go test` and
cannot change any count, rate or classification, so a matrix that quietly
repeated itself would look identical to one that did not.

Above the first handful of configurations the matrix scales by giving every run
a shuffle seed no other run uses, cycling `GOMAXPROCS` through its candidates,
and switching the race detector on for **one run in eight** rather than every
other one. `-race` is the knob that dominates wall-clock and the one with the
least to say - it answers a yes/no question, and a sample answers that as well
as a census. For a race build costing 10x a plain one, alternating would make
the matrix 5.5x a race-free run; one in eight makes it 2.1x.

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
