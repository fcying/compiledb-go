//go:build windows

package internal

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"time"

	"golang.org/x/sys/windows"
)

const processKillDelay = time.Second

var (
	processJobs   = make(map[*os.Process]*windowsProcessState)
	processJobsMu sync.Mutex
	ntdll         = windows.NewLazySystemDLL("ntdll.dll")
	ntResume      = ntdll.NewProc("NtResumeProcess")
)

type windowsProcessState struct {
	job    windows.Handle
	exited bool
}

func configureProcessCommand(cmd *exec.Cmd, ctx context.Context) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &windows.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_SUSPENDED
	cmd.Cancel = func() error {
		return terminateProcessTree(cmd.Process, ctx)
	}
	cmd.WaitDelay = processKillDelay
}

func configureProcessCommandWithoutContext(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &windows.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_SUSPENDED
	cmd.WaitDelay = processKillDelay
}

func startProcessCommand(cmd *exec.Cmd) error {
	suspended := cmd.SysProcAttr != nil && cmd.SysProcAttr.CreationFlags&windows.CREATE_SUSPENDED != 0
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		_ = windows.CloseHandle(job)
		return err
	}
	assigned := false
	terminated := false
	stateRegistered := false
	var processErr error
	err = cmd.Process.WithHandle(func(rawHandle uintptr) {
		process := windows.Handle(rawHandle)
		assigned = windows.AssignProcessToJobObject(job, process) == nil
		// Restricted outer jobs can reject nested assignment. Keep leader
		// cancellation by process handle when no child job can be installed.
		state := &windowsProcessState{}
		if assigned {
			state.job = job
		} else {
			_ = windows.CloseHandle(job)
		}
		processJobsMu.Lock()
		processJobs[cmd.Process] = state
		processJobsMu.Unlock()
		stateRegistered = true
		if !suspended {
			return
		}
		status, _, _ := ntResume.Call(rawHandle)
		if status == 0 {
			return
		}
		resumeErr := fmt.Errorf("NtResumeProcess failed with status %#x", status)
		terminated = true
		if assigned {
			processErr = errors.Join(resumeErr, windows.TerminateJobObject(job, 1))
			return
		}
		processErr = errors.Join(resumeErr, windows.TerminateProcess(process, 1))
	})
	err = errors.Join(err, processErr)
	if err != nil {
		if stateRegistered {
			releaseProcessTree(cmd.Process)
		} else {
			_ = windows.CloseHandle(job)
		}
		if terminated {
			_ = waitStartedProcessCleanup(cmd)
		} else {
			err = errors.Join(err, stopStartedProcess(cmd))
		}
		return err
	}
	return nil
}

func stopStartedProcess(cmd *exec.Cmd) error {
	err := cmd.Process.Kill()
	if err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	return waitStartedProcessCleanup(cmd)
}

func waitStartedProcessCleanup(cmd *exec.Cmd) error {
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		var exitError *exec.ExitError
		if err == nil || errors.As(err, &exitError) {
			return nil
		}
		return err
	case <-time.After(processKillDelay):
		return errors.New("timed out waiting for process cleanup")
	}
}

func releaseProcessTree(process *os.Process) {
	if process == nil {
		return
	}
	processJobsMu.Lock()
	state := processJobs[process]
	delete(processJobs, process)
	processJobsMu.Unlock()
	if state != nil && state.job != 0 {
		_ = windows.CloseHandle(state.job)
	}
}

func markProcessExited(process *os.Process) {
	if process == nil {
		return
	}
	processJobsMu.Lock()
	if state := processJobs[process]; state != nil {
		state.exited = true
	}
	processJobsMu.Unlock()
}

func cleanupExitedProcessTree(process *os.Process) error {
	if process == nil {
		return os.ErrProcessDone
	}
	processJobsMu.Lock()
	state := processJobs[process]
	if state == nil || state.job == 0 {
		processJobsMu.Unlock()
		return os.ErrProcessDone
	}
	err := windows.TerminateJobObject(state.job, 1)
	processJobsMu.Unlock()
	return err
}

func terminateProcessTree(process *os.Process, _ context.Context) error {
	if process == nil {
		return os.ErrProcessDone
	}
	processJobsMu.Lock()
	state := processJobs[process]
	if state != nil {
		if state.job != 0 {
			err := windows.TerminateJobObject(state.job, 1)
			processJobsMu.Unlock()
			return err
		}
		if state.exited {
			processJobsMu.Unlock()
			return os.ErrProcessDone
		}
		processJobsMu.Unlock()
		return process.Kill()
	}
	processJobsMu.Unlock()
	return process.Kill()
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
