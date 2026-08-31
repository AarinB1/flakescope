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
| `--runs N` | 20 | number of configurations to run |
| `--json` | off | emit the machine-readable report instead of text |
| `--timeout D` | 10m | per-configuration timeout |
| `--verbose` | off | include failure output for each reported test |

```
$ flakescope ./internal/queue
flakescope ./internal/queue
20 configurations: 20 completed, 0 timed out, 0 errored

FLAKY (2)
  TestDrainOrder
      failed 6/20 configurations (30%), order-dependent
      minimal repro: GOMAXPROCS=8 go test -shuffle=1 -count=1 ./internal/queue
  TestWorkerPool
      failed 11/20 configurations (55%), load-dependent
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

**Minimal reproducing configuration.** Among the configurations that reproduced
the failure, flakescope reports the smallest, ordered by:

1. fewest knobs changed from the default (shuffle off, `GOMAXPROCS` at
   `runtime.NumCPU()`, race off, count 1);
2. then lowest `GOMAXPROCS`;
3. then race off before race on;
4. then lowest shuffle seed.

Rule 4 is a pure tie-break with no meaning of its own. It exists so that the
same matrix always names the same configuration.

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

## Development

```
make check    # gofmt, go vet, go test
```

flakescope's own tests replay recorded `go test -json` streams from `testdata/`
rather than invoking `go test`, so the test suite of a flaky-test detector is
not itself flaky. The single exception is
`TestIntegrationRunsTheRealGoTool`, which is named as such and skipped under
`-short`.
