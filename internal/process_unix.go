//go:build aix || darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd || solaris

package internal

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"syscall"
	"time"
)

const processKillDelay = time.Second

var signalProcessGroup = func(pgid int, signal syscall.Signal) error {
	return syscall.Kill(-pgid, signal)
}

func configureProcessCommand(cmd *exec.Cmd, ctx context.Context) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return terminateProcessTree(cmd.Process, ctx)
	}
	cmd.WaitDelay = processKillDelay
}

func configureProcessCommandWithoutContext(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = processKillDelay
}

func startProcessCommand(cmd *exec.Cmd) error {
	return cmd.Start()
}

func releaseProcessTree(_ *os.Process) {}

func markProcessExited(_ *os.Process) {}

func cleanupExitedProcessTree(process *os.Process) error {
	if process == nil {
		return os.ErrProcessDone
	}
	err := signalProcessGroup(process.Pid, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) {
		return os.ErrProcessDone
	}
	return err
}

func terminateProcessTree(process *os.Process, ctx context.Context) error {
	if process == nil {
		return os.ErrProcessDone
	}
	signal := syscall.SIGTERM
	if cause, ok := context.Cause(ctx).(interface{ Signal() os.Signal }); ok {
		if received, ok := cause.Signal().(syscall.Signal); ok {
			signal = received
		}
	}
	err := signalProcessGroup(process.Pid, signal)
	if errors.Is(err, syscall.ESRCH) {
		return os.ErrProcessDone
	}
	if err != nil {
		return err
	}
	deadline := time.Now().Add(processKillDelay)
	for time.Now().Before(deadline) {
		if err := signalProcessGroup(process.Pid, 0); errors.Is(err, syscall.ESRCH) {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := signalProcessGroup(process.Pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}

func configureMakeCommand(cmd *exec.Cmd, ctx context.Context) {
	configureProcessCommand(cmd, ctx)
}

func signaledExitCode(exitError *exec.ExitError) (int, bool) {
	status, ok := exitError.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() {
		return 0, false
	}
	return 128 + int(status.Signal()), true
}

func processSignalExitCode(signal os.Signal) (int, bool) {
	unixSignal, ok := signal.(syscall.Signal)
	if !ok {
		return 0, false
	}
	return 128 + int(unixSignal), true
}
