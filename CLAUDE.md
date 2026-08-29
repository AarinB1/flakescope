# flakescope

A CLI that reruns a Go package's tests under many configurations and reports
which tests fail nondeterministically, along with the minimal configuration
that reproduces each.

Go has no seedable goroutine scheduler outside `testing/synctest`. This tool
varies CONFIGURATIONS, not interleavings. Never claim otherwise in code,
comments, README, or output text.

## Rules

1. Zero non-stdlib dependencies, ever. `go.mod` has no require block. This is
   enforced by a test, not asserted in prose, because the install story rests
   on it.
2. flakescope's own tests never invoke `go test`. They replay recorded JSON
   event streams from `testdata/`. A flaky-test detector with a flaky test
   suite is not shippable.
3. From v1.0.0, the `--json` report schema and the process exit codes are a
   compatibility surface. Additive changes only.
4. An assertion that cannot fail is worse than no assertion, because it also
   occupies the space where a real one would go. Every assertion about
   detection must be demonstrated to fail against a fixture that breaks it.

## Scope

Not being built, at any point: a config file format, a plugin system, a TUI, a
daemon, an abstraction layer over `os/exec`, or an interface introduced for a
feature that is not yet being built.

If a change adds an abstraction layer, it is out of scope. Say so instead.

## Working agreement

- Work proceeds in numbered steps. One step is one file plus its test.
  Complete a step, run `make check`, and commit before beginning the next.
- Do not modify files outside the current step's stated slice.
- If a step's stated slice turns out to be wrong, stop and report rather than
  widening it.
- When a test fails, state the root cause before proposing a fix.
- Table-driven tests.
- `make check` must pass before every commit. One commit per step.
- End each task by explaining the three subtlest decisions made and why.
