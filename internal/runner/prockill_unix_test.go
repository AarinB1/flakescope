//go:build unix

package runner

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestTimeoutKillsProcessGroup is the fixture that breaks a runner which only
// SIGKILLs the `go` PID. A fake go that starts a grandchild sleeper must not
// leave that sleeper running after the per-configuration deadline.
//
// The fake is a shell script named `go`, not the go tool, so this does not
// invoke `go test` (CLAUDE.md rule 2).
func TestTimeoutKillsProcessGroup(t *testing.T) {
	dir := t.TempDir()
	pidPath := filepath.Join(dir, "child.pid")
	startedPath := filepath.Join(dir, "started")
	fakeGo := filepath.Join(dir, "go")

	script := "#!/bin/sh\n" +
		"sleep 600 &\n" +
		"echo $! > " + strconv.Quote(pidPath) + "\n" +
		"touch " + strconv.Quote(startedPath) + "\n" +
		"wait\n"
	if err := os.WriteFile(fakeGo, []byte(script), 0o755); err != nil {
		t.Fatalf("writing fake go: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	r := New("example.com/pkg")
	r.Timeout = 300 * time.Millisecond
	r.Workers = 1

	done := make(chan []Result, 1)
	go func() {
		done <- r.Run(context.Background(), []Config{{GOMAXPROCS: 1, Count: 1}})
	}()

	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(startedPath); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("fake go never started its child")
		}
		time.Sleep(10 * time.Millisecond)
	}

	raw, err := os.ReadFile(pidPath)
	if err != nil {
		t.Fatalf("reading child pid: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || pid <= 0 {
		t.Fatalf("child pid %q: %v", raw, err)
	}

	select {
	case results := <-done:
		if results[0].Outcome != OutcomeTimedOut {
			t.Errorf("outcome = %v, want timeout", results[0].Outcome)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after the configuration timeout")
	}

	if err := syscall.Kill(pid, 0); err == nil {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		t.Fatalf("orphaned child process %d still running after the timeout", pid)
	}
}
