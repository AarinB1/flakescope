//go:build unix

package runner

import (
	"context"
	"os"
	"os/exec"
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
//
// The assertion is about alive, not `syscall.Kill(pid, 0) != nil`. See alive
// for why those are different questions and why only the first one is the one
// this test is asking. It is polled rather than read once; see waitUntilDead.
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

	if !waitUntilDead(pid, 2*time.Second) {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		t.Fatalf("orphaned child process %d still running after the timeout", pid)
	}
}

// waitUntilDead polls alive until pid stops running, up to d, and reports
// whether it got there.
//
// The check this replaced read alive once, immediately after Run returned.
// Run returns when the `go` process has been waited for, and nothing orders
// the group SIGKILL's effect on the GRANDCHILD against that: the kill is
// asynchronous, so on a loaded or slow machine the sleeper can still be in a
// running state for a moment after Run comes back. A single read there fails
// the test for a runner that killed the group correctly - the same shape of
// false failure, in the same direction, as the zombie misread this file's
// alive helper exists to prevent.
//
// The window does not weaken the assertion. A runner that only SIGKILLs the
// `go` PID leaves a `sleep 600` orphan, which is still running when the window
// closes and still fails the test.
func waitUntilDead(pid int, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for {
		if !alive(pid) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestWaitUntilDead pins that the settle window added for slow machines did
// not turn the orphan assertion into one that cannot fail. The live row is the
// one that matters: a process that is still running when the window closes
// must still be reported as such, because that report is what fails
// TestTimeoutKillsProcessGroup for a runner that leaked the process group.
func TestWaitUntilDead(t *testing.T) {
	live := exec.Command("/bin/sh", "-c", "sleep 600")
	if err := live.Start(); err != nil {
		t.Fatalf("starting the live process: %v", err)
	}
	livePID := live.Process.Pid
	defer func() {
		_ = live.Process.Kill()
		_ = live.Wait()
	}()

	reaped := exec.Command("/bin/sh", "-c", "exit 0")
	if err := reaped.Start(); err != nil {
		t.Fatalf("starting the reaped process: %v", err)
	}
	reapedPID := reaped.Process.Pid
	if err := reaped.Wait(); err != nil {
		t.Fatalf("waiting for the reaped process: %v", err)
	}

	tests := []struct {
		name string
		pid  int
		want bool
		pins string
	}{
		{
			name: "a process still running when the window closes",
			pid:  livePID,
			want: false,
			pins: "the window does not swallow a genuinely leaked process",
		},
		{
			name: "a process that is already gone",
			pid:  reapedPID,
			want: true,
			pins: "a dead process is reported dead without burning the window",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := waitUntilDead(tc.pid, 50*time.Millisecond); got != tc.want {
				t.Errorf("waitUntilDead(%d) = %v, want %v; this row pins that %s", tc.pid, got, tc.want, tc.pins)
			}
		})
	}
}

// alive reports whether pid names a process that is still RUNNING.
//
// syscall.Kill(pid, 0) cannot answer that question on its own. A SIGKILLed
// orphan is reparented to PID 1, and where PID 1 is not a reaping init - which
// is the normal case inside a container - it stays a zombie indefinitely. A
// zombie still accepts signal 0, so a kill-only probe reports a process the
// group kill did destroy as having survived it. That is a false failure, and it
// is a false failure in the direction that hides nothing: it fires when the
// runner is correct.
//
// Where /proc is available the state field separates the two: "Z" is
// killed-but-unreaped, anything else is a process still holding resources.
// Where it is not (macOS, the BSDs), PID 1 reaps orphans, so no zombie outlives
// the kill and signal 0 is already exact.
func alive(pid int) bool {
	if err := syscall.Kill(pid, 0); err != nil {
		return false
	}
	state, ok := procState(pid)
	if !ok {
		return true
	}
	return state != "Z"
}

// procState returns the single-letter state field from /proc/<pid>/stat, and
// whether it could be read at all.
func procState(pid int) (string, bool) {
	b, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return "", false
	}
	// Field 2 is the executable name in parentheses and may itself contain
	// spaces and parentheses, so the state field is the first one after the
	// LAST ')', not the third whitespace-separated token.
	i := strings.LastIndexByte(string(b), ')')
	if i < 0 {
		return "", false
	}
	fields := strings.Fields(string(b)[i+1:])
	if len(fields) == 0 {
		return "", false
	}
	return fields[0], true
}

// TestAlive pins the distinction alive exists to make. The zombie row is the
// one that matters: it fails against a kill-only probe, which is the bug this
// helper replaced.
func TestAlive(t *testing.T) {
	// A child that has exited but has not been waited for is a zombie whose
	// parent is this test binary, which is the same kernel state a SIGKILLed
	// orphan under a non-reaping PID 1 ends up in.
	zombie := exec.Command("/bin/sh", "-c", "exit 0")
	if err := zombie.Start(); err != nil {
		t.Fatalf("starting the zombie's process: %v", err)
	}
	zombiePID := zombie.Process.Pid
	defer func() { _ = zombie.Wait() }()
	waitForState(t, zombiePID, "Z")

	// A process that ran and was reaped leaves a PID that names nothing.
	reaped := exec.Command("/bin/sh", "-c", "exit 0")
	if err := reaped.Start(); err != nil {
		t.Fatalf("starting the reaped process: %v", err)
	}
	reapedPID := reaped.Process.Pid
	if err := reaped.Wait(); err != nil {
		t.Fatalf("waiting for the reaped process: %v", err)
	}

	tests := []struct {
		name string
		pid  int
		want bool
		pins string
	}{
		{
			name: "this process is running",
			pid:  os.Getpid(),
			want: true,
			pins: "alive does not report every process as dead",
		},
		{
			name: "an exited but unreaped child is not running",
			pid:  zombiePID,
			want: false,
			pins: "a zombie still accepts signal 0; alive must not be fooled by that",
		},
		{
			name: "a reaped child's pid names nothing",
			pid:  reapedPID,
			want: false,
			pins: "the ordinary dead case still reads as dead",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.pid == zombiePID {
				if _, ok := procState(tc.pid); !ok {
					t.Skip("no /proc: on this platform PID 1 reaps orphans, so no zombie outlives a group kill")
				}
			}
			if got := alive(tc.pid); got != tc.want {
				t.Errorf("alive(%d) = %v, want %v; this row pins that %s", tc.pid, got, tc.want, tc.pins)
			}
		})
	}
}

// waitForState blocks until pid reaches the given /proc state, so the zombie
// row does not race the child's exit. It is a no-op where /proc is absent.
func waitForState(t *testing.T, pid int, want string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		state, ok := procState(pid)
		if !ok || state == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("process %d never reached state %q; it is %q", pid, want, state)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
