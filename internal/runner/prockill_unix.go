//go:build unix

package runner

import (
	"os/exec"
	"syscall"
)

// configureKillProcessGroup puts the `go test` process in its own group so a
// cancelled CommandContext can SIGKILL the test binary it spawned, not just
// the go tool.
func configureKillProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
