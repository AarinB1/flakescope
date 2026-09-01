//go:build unix

package runner

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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
	// `sh -c "sleep 600"` is two processes, not one: dash forks the sleeper
	// and waits for it. The shape is kept deliberately, because it is the
	// shape that leaked, and keeping it is what makes startFixture's group
	// cleanup load bearing here rather than decorative.
	live := startFixture(t, "/bin/sh", "-c", "sleep 600")
	livePID := live.Process.Pid

	reaped := startFixture(t, "/bin/sh", "-c", "exit 0")
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

// statFieldsAfterComm returns the fields of /proc/<pid>/stat that follow the
// comm field, and whether they could be read at all.
//
// Field 2 is the executable name in parentheses and may itself contain spaces
// and parentheses, so the fields after it begin after the LAST ')', not at the
// third whitespace-separated token.
func statFieldsAfterComm(pid int) ([]string, bool) {
	b, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return nil, false
	}
	i := strings.LastIndexByte(string(b), ')')
	if i < 0 {
		return nil, false
	}
	fields := strings.Fields(string(b)[i+1:])
	if len(fields) == 0 {
		return nil, false
	}
	return fields, true
}

// procState returns the single-letter state field from /proc/<pid>/stat, and
// whether it could be read at all.
func procState(pid int) (string, bool) {
	fields, ok := statFieldsAfterComm(pid)
	if !ok {
		return "", false
	}
	return fields[0], true
}

// procPGID returns the process group id from /proc/<pid>/stat, and whether it
// could be read at all. The fields after comm run state, ppid, pgrp.
func procPGID(pid int) (int, bool) {
	fields, ok := statFieldsAfterComm(pid)
	if !ok || len(fields) < 3 {
		return 0, false
	}
	pgid, err := strconv.Atoi(fields[2])
	if err != nil {
		return 0, false
	}
	return pgid, true
}

// TestAlive pins the distinction alive exists to make. The zombie row is the
// one that matters: it fails against a kill-only probe, which is the bug this
// helper replaced.
func TestAlive(t *testing.T) {
	// A child that has exited but has not been waited for is a zombie whose
	// parent is this test binary, which is the same kernel state a SIGKILLed
	// orphan under a non-reaping PID 1 ends up in.
	zombie := startFixture(t, "/bin/sh", "-c", "exit 0")
	zombiePID := zombie.Process.Pid
	defer func() { _ = zombie.Wait() }()
	waitForState(t, zombiePID, "Z")

	// A process that ran and was reaped leaves a PID that names nothing.
	reaped := startFixture(t, "/bin/sh", "-c", "exit 0")
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

// startFixture starts a fixture process in a process group of its own and
// registers cleanup that kills that whole group and then FAILS the test if
// anything from it is still running.
//
// The group is the point, and it is the same lesson configureKillProcessGroup
// encodes one file over. `sh -c "sleep 600"` is two processes: dash forks the
// sleeper and waits for it, so cleanup that kills cmd.Process kills the shell
// and leaves the sleeper running, reparented to PID 1, for its full ten
// minutes. The fixtures here did exactly that, and every assertion in them
// still passed, because they only ever asked about the pid they had started.
// The leak surfaced as a line in a CI cleanup log instead of as a red test.
//
// Killing the group closes that hole, because a forked grandchild inherits the
// group. Scanning the group afterwards is what keeps it closed: a grandchild
// whose pid the test never learned is still a member, so the scan sees it.
// TestLiveGroupMembers pins that it does.
func startFixture(t *testing.T, name string, args ...string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(name, args...)
	// Setpgid with a zero Pgid makes the child the leader of a new group whose
	// id is its own pid.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting fixture %s: %v", name, err)
	}
	pgid := cmd.Process.Pid
	desc := strings.Join(append([]string{name}, args...), " ")
	t.Cleanup(func() {
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
		// Reap the leader. Its grandchildren belong to PID 1 by then and are
		// not ours to wait for, which is why the scan below, not this Wait, is
		// what decides whether the group actually died.
		_ = cmd.Wait()
		survivors, any := waitUntilGroupDead(pgid, 2*time.Second)
		if !any {
			return
		}
		for _, pid := range survivors {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
		t.Errorf("fixture %q left process group %d running after cleanup (survivors: %v); "+
			"a fixture that outlives its test is an orphan on the CI runner", desc, pgid, survivors)
	})
	return cmd
}

// waitUntilGroupDead polls until no process in group pgid is running, up to d,
// and returns whatever is still running when the window closes.
//
// It polls for the same reason waitUntilDead does. SIGKILL is asynchronous, so
// a group that is being torn down correctly can still have a member in a
// running state for a moment after the kill returns; reading once would report
// a leak against cleanup that worked.
func waitUntilGroupDead(pgid int, d time.Duration) ([]int, bool) {
	deadline := time.Now().Add(d)
	for {
		live, any := liveGroupMembers(pgid)
		if !any {
			return nil, false
		}
		if time.Now().After(deadline) {
			return live, true
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// liveGroupMembers reports whether any process in group pgid is still RUNNING,
// and, where it can enumerate them, which ones.
//
// The zombie distinction alive draws applies to a group as much as to a pid: a
// correctly killed group leaves its orphans as zombies under a non-reaping PID
// 1, and counting those as survivors would fail cleanup that worked. So where
// /proc is available this walks it and filters with alive. Where it is not,
// PID 1 reaps orphans, no zombie outlives the kill, and signal 0 to the group
// answers the same question exactly - it just cannot name the pids.
func liveGroupMembers(pgid int) ([]int, bool) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, syscall.Kill(-pgid, 0) == nil
	}
	var live []int
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		if got, ok := procPGID(pid); !ok || got != pgid {
			continue
		}
		if !alive(pid) {
			continue
		}
		live = append(live, pid)
	}
	return live, len(live) > 0
}

// TestLiveGroupMembers pins that the teardown assertion startFixture installs
// can actually fail, per CLAUDE.md rule 4.
//
// The orphan row is the one that matters. It reconstructs the exact leak this
// file shipped with - a shell killed by pid, its forked sleeper reparented to
// PID 1 and left running - and requires that the scan still names the sleeper,
// whose pid was never registered anywhere. A check that only looked at the
// pids it was handed would pass both rows here and still let that sleeper out
// onto the runner.
func TestLiveGroupMembers(t *testing.T) {
	if _, ok := procState(os.Getpid()); !ok {
		t.Skip("no /proc: liveGroupMembers falls back to a signal-0 probe of the group, which is exact where PID 1 reaps orphans")
	}

	// A group whose leader is killed by pid, leaving the child it forked
	// running. `sleep 600 & wait` is used rather than `sleep 600` because a
	// last-command exec (bash, busybox ash, some dash builds) would replace
	// the shell with sleep and leave a single process whose pid equals the
	// pgid; waitForGroupMember would then time out against a fixture that
	// never produced the leak. Backgrounding forces the fork; wait keeps the
	// shell alive as the leader. startFixture's cleanup kills the group
	// afterwards, which is what keeps this row from leaking the sleeper it
	// deliberately strands - and exercises that the cleanup reaps a
	// grandchild, not just a leader.
	orphaned := startFixture(t, "/bin/sh", "-c", "sleep 600 & wait")
	orphanedPGID := orphaned.Process.Pid
	strandedPID := waitForGroupMember(t, orphanedPGID, orphanedPGID)
	if err := orphaned.Process.Kill(); err != nil {
		t.Fatalf("killing the group leader: %v", err)
	}
	_ = orphaned.Wait()

	// A group whose only member exited and was reaped, so nothing of it is
	// left in the process table at all.
	gone := startFixture(t, "/bin/sh", "-c", "exit 0")
	gonePGID := gone.Process.Pid
	if err := gone.Wait(); err != nil {
		t.Fatalf("waiting for the reaped leader: %v", err)
	}

	tests := []struct {
		name    string
		pgid    int
		wantPID int // 0 means the group must report nothing running
		pins    string
	}{
		{
			name:    "a child orphaned by a kill aimed at the group leader",
			pgid:    orphanedPGID,
			wantPID: strandedPID,
			pins:    "a survivor whose pid the test never registered is still found, which is what makes the teardown assertion able to fail",
		},
		{
			name:    "a group whose only member exited and was reaped",
			pgid:    gonePGID,
			wantPID: 0,
			pins:    "cleanup that worked is not reported as a leak",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, any := liveGroupMembers(tc.pgid)
			if want := tc.wantPID != 0; any != want {
				t.Fatalf("liveGroupMembers(%d) reported running=%v (%v), want %v; this row pins that %s",
					tc.pgid, any, got, want, tc.pins)
			}
			if tc.wantPID != 0 && !slices.Contains(got, tc.wantPID) {
				t.Errorf("liveGroupMembers(%d) = %v, want it to include %d; this row pins that %s",
					tc.pgid, got, tc.wantPID, tc.pins)
			}
		})
	}
}

// waitForGroupMember polls until a process other than the leader appears in
// group pgid and returns its pid. It is a state predicate rather than a sleep,
// because how long a shell takes to fork its child is exactly the kind of
// timing this repo does not get to assume.
func waitForGroupMember(t *testing.T, pgid, leader int) int {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		members, _ := liveGroupMembers(pgid)
		for _, pid := range members {
			if pid != leader {
				return pid
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("group %d never gained a member besides its leader %d", pgid, leader)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
