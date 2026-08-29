# Recorded `go test -json` streams

Every test in this repository replays these files instead of invoking `go test`
(CLAUDE.md rule 2). They were recorded from `testdata/flakypkg` with Go 1.24.7
on linux/amd64. Each command was run from the repository root with
`P=./testdata/flakypkg/`.

| File             | Command                                                                                  | What it exercises |
| ---------------- | ---------------------------------------------------------------------------------------- | ----------------- |
| `allpass.json`   | `GOMAXPROCS=1 go test -count=1 -json -run 'TestAlwaysPasses\|TestOrderDependent\|TestPoisonsGlobalState' $P` | A clean run. Includes subtests, whose `Test` values contain a slash. |
| `orderfail.json` | `GOMAXPROCS=1 go test -count=1 -json -shuffle=1 $P`                                       | The order-dependent failure. Seed 1 permutes `TestPoisonsGlobalState` ahead of `TestOrderDependent`. |
| `loadfail.json`  | `GOMAXPROCS=4 go test -count=1 -json $P`                                                  | The load-dependent failure, plus the always-failing test. |
| `panic.json`     | `GOMAXPROCS=1 go test -count=1 -json -run 'TestAlwaysPasses\|TestPanics\|TestLoadDependent' $P` | A panic. The test binary dies mid-run, so the package-level `fail` carries no `Test` field. |
| `buildfail.json` | `GOMAXPROCS=1 go test -count=1 -json $P`                                                  | A compile error. Every event is package-scoped; the build events carry `ImportPath` and no `Package` at all. |
| `truncated.json` | `head -c 1300 loadfail.json`                                                              | What a killed process leaves behind: the last JSON object is cut in half and two tests are still in flight. |

## The two mutated recordings

`panic.json` and `buildfail.json` needed source that is not in the committed
fixture. In each case a temporary file was added to `testdata/flakypkg`, the
stream was recorded, and the file was deleted. Neither is committed: a file that
does not compile would break `gofmt -l .` in `make check`, and a panicking test
would be one more thing the fixture's documented behaviour has to account for.

`panic.json` was recorded with this file present as `panic_test.go`:

```go
package flakypkg

import "testing"

func TestPanics(t *testing.T) {
	var m map[string]int
	m["boom"] = 1
}
```

`buildfail.json` was recorded with this file present as `broken_test.go`:

```go
package flakypkg

import "testing"

func TestWillNotCompile(t *testing.T) {
	var s string = 42
	_ = s
}
```

## Re-recording

Timestamps and goroutine addresses differ on every run, and the stack frames in
`panic.json` name absolute paths from the machine that recorded it. Nothing in
this repository asserts on any of those, but re-record all six together if you
re-record at all, so the set stays internally consistent.
