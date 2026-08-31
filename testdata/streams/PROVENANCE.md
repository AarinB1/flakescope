# Recorded `go test -json` streams

Every test in this repository replays these files instead of invoking `go test`
(CLAUDE.md rule 2). A stream is mapped to the exact configuration it was
recorded under and never to a nearby one (rule 5), so the command column below
is part of the fixture, not a note about how it was made.

They were recorded from `testdata/flakypkg` with Go 1.24.7 on linux/amd64. Each
command was run from the repository root with `P=./testdata/flakypkg/`.

| File | Command | What it exercises |
| --- | --- | --- |
| `allpass.json` | `GOMAXPROCS=1 go test -count=1 -json -run 'TestAlwaysPasses\|TestOrderDependent\|TestPoisonsGlobalState' $P` | A clean run. Includes subtests, whose `Test` values contain a slash. |
| `singleproc.json` | `GOMAXPROCS=1 go test -count=1 -json $P` | The unshuffled single-processor baseline: only the always-broken test fails. |
| `orderfail.json` | `GOMAXPROCS=1 go test -count=1 -json -shuffle=1 $P` | The order-dependent failure. Seed 1 permutes `TestPoisonsGlobalState` ahead of `TestOrderDependent`. |
| `loadfail2.json` | `GOMAXPROCS=2 go test -count=1 -json $P` | The load-dependent failure at two processors. Its message names the processor count, so it is a DIFFERENT signature from `loadfail.json`. |
| `loadfail.json` | `GOMAXPROCS=4 go test -count=1 -json $P` | The load-dependent failure at four processors, plus the always-failing test. |
| `orderload2.json` | `GOMAXPROCS=2 go test -count=1 -json -shuffle=1 $P` | Both discriminators at two processors. |
| `orderload.json` | `GOMAXPROCS=4 go test -count=1 -json -shuffle=1 $P` | Both discriminators at four processors. |
| `panic1.json` | `GOMAXPROCS=1 go test -tags flakescope_crash -count=1 -json -run 'TestPanics$' $P` | A panic with a goroutine stack: goroutine ID, heap addresses, frame offsets. |
| `panic4.json` | `GOMAXPROCS=4 go test -tags flakescope_crash -count=1 -json -run 'TestPanics$' $P` | The SAME panic recorded again. It differs from `panic1.json` textually and must hash identically; that pair is v0.2.0's exit criterion. |
| `race.json` | `GOMAXPROCS=4 go test -tags flakescope_crash -race -count=1 -json -run 'TestRaces$' $P` | The race detector's own layout: `WARNING: DATA RACE` and two conflicting-access stacks, indented differently from a panic stack. |
| `buildfail.json` | `GOMAXPROCS=1 go test -count=1 -json $P` | A compile error. Every event is package-scoped; the build events carry `ImportPath` and no `Package` at all. |
| `truncated.json` | `head -c 1300 loadfail.json` | What a killed process leaves behind: the last JSON object is cut in half and two tests are still in flight. |
| `panic.json` | `GOMAXPROCS=1 go test -count=1 -json -run 'TestAlwaysPasses\|TestPanics\|TestLoadDependent' $P` | Superseded by `panic1.json` for signature work, retained because `internal/gotest`'s parser tests are written against this exact stream: a test binary dying mid-run, so the package-level `fail` carries no `Test` field. |

## The build tag

`panic1.json`, `panic4.json` and `race.json` were recorded with
`-tags flakescope_crash`, which is what admits `testdata/flakypkg/crash_test.go`
into the build. The tag is part of the configuration those three were recorded
under, and it is why the other recordings are unaffected by the panicking and
racing fixtures: without it the package builds exactly as it did in v0.1.0. See
the comment at the top of `crash_test.go` for why neither fixture can be part of
the untagged matrix.

## The mutated recording

`buildfail.json` needs source that is not in the committed fixture. A temporary
file was added to `testdata/flakypkg`, the stream was recorded, and the file was
deleted; it is not committed, because a file that does not compile would break
`gofmt -l .` in `make check`.

`buildfail.json` was recorded with this file present as `broken_test.go`:

```go
package flakypkg

import "testing"

func TestWillNotCompile(t *testing.T) {
	var s string = 42
	_ = s
}
```

`panic.json` was recorded the same way in v0.1.0, with a temporary
`panic_test.go` holding a nil-map write. That fixture is now committed - behind
the build tag, as `TestPanics` in `crash_test.go` - so `panic.json` is the only
remaining recording whose source is gone.

## Re-recording

Timestamps, goroutine IDs and heap addresses differ on every run, and the stack
frames name absolute paths from the machine that recorded them. Nothing asserts
on any of those. **Line numbers are different.** Every recording made from the
committed fixture carries `flaky_test.go:NN` or `crash_test.go:NN`, so editing
those files - even editing a comment - invalidates every recording taken from
them at once. Re-record the whole set together, and regenerate `truncated.json`
from the new `loadfail.json` afterwards.
