//go:build flakescope_hang

// The two fixtures in this file never finish, so the only thing that ends them
// is `go test -timeout`. They exist to record what that timeout looks like in a
// -json stream, which is not what anyone would guess: the testing package's
// timeout panic is raised from its alarm goroutine, so the test that hung
// never reports a result of its own. The banner it prints names the tests that
// were running instead, and that list is the only statement in the stream that
// they did not finish.
//
// They are behind a build tag for the same reason crash_test.go is. A test
// that blocks forever would stop every untagged full-package recording at the
// timeout, so the tag is part of the configuration their recordings were made
// under, and PROVENANCE.md names it (CLAUDE.md rule 5).
//
// Both are DETERMINISTIC given a configuration, like everything in this
// package. Each blocks on a channel nothing will ever send on, which is a hang
// by construction rather than a deadlock that depends on which goroutine runs
// first. A channel and not a sleep: a sleep finishes eventually, and a fixture
// whose outcome depends on how its duration compares with -timeout depends on
// the machine.
package flakypkg

import (
	"runtime"
	"testing"
)

// TestHangsUnderLoad hangs iff GOMAXPROCS > 1. It is the hanging counterpart of
// TestLoadDependent: a stand-in for the class of hang that only appears once
// more than one P is available, and not itself one.
func TestHangsUnderLoad(t *testing.T) {
	if runtime.GOMAXPROCS(0) > 1 {
		<-make(chan struct{})
	}
}

// TestHangsInSubtest hangs in a subtest at every GOMAXPROCS. The timeout banner
// names BOTH the parent and the subtest, and test2json attributes the banner's
// output to only one of them, which is the case a parser that reads only the
// test that owns the banner gets wrong.
func TestHangsInSubtest(t *testing.T) {
	t.Run("inner", func(t *testing.T) {
		<-make(chan struct{})
	})
}
