//go:build !aix && !darwin && !dragonfly && !freebsd && !illumos && !linux && !netbsd && !openbsd && !solaris && !windows

package internal

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"time"
)

const processKillDelay = time.Second

func configureProcessCommand(cmd *exec.Cmd, _ context.Context) {
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		return cmd.Process.Kill()
	}
	cmd.WaitDelay = processKillDelay
}

func configureProcessCommandWithoutContext(cmd *exec.Cmd) {
	cmd.WaitDelay = processKillDelay
}

func startProcessCommand(cmd *exec.Cmd) error {
	return cmd.Start()
}

func releaseProcessTree(_ *os.Process) {}

func markProcessExited(_ *os.Process) {}

func cleanupExitedProcessTree(_ *os.Process) error {
	return os.ErrProcessDone
}

func terminateProcessTree(process *os.Process, _ context.Context) error {
	if process == nil {
		return os.ErrProcessDone
	}
	err := process.Kill()
	if errors.Is(err, os.ErrProcessDone) {
		return os.ErrProcessDone
	}
	return err
}

func configureMakeCommand(cmd *exec.Cmd, ctx context.Context) {
	configureProcessCommand(cmd, ctx)
}

func signaledExitCode(_ *exec.ExitError) (int, bool) {
	return 0, false
}

func processSignalExitCode(_ os.Signal) (int, bool) {
	return 0, false
}
