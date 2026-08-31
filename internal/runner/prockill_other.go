//go:build !unix

package runner

import "os/exec"

func configureKillProcessGroup(*exec.Cmd) {}
