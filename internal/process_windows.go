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

type windowsProcessAPI struct {
	createJob     func() (windows.Handle, error)
	assignJob     func(windows.Handle, windows.Handle) error
	closeHandle   func(windows.Handle) error
	resumeProcess func(uintptr) uintptr
	terminateJob  func(windows.Handle, uint32) error
	terminateProc func(windows.Handle, uint32) error
}

var (
	processJobs   = make(map[*os.Process]*windowsProcessState)
	processJobsMu sync.Mutex
	ntdll         = windows.NewLazySystemDLL("ntdll.dll")
	ntResume      = ntdll.NewProc("NtResumeProcess")
	processAPI    = windowsProcessAPI{
		createJob:   func() (windows.Handle, error) { return windows.CreateJobObject(nil, nil) },
		assignJob:   windows.AssignProcessToJobObject,
		closeHandle: windows.CloseHandle,
		resumeProcess: func(handle uintptr) uintptr {
			status, _, _ := ntResume.Call(handle)
			return status
		},
		terminateJob:  windows.TerminateJobObject,
		terminateProc: windows.TerminateProcess,
	}
)

type windowsProcessState struct {
	mu     sync.Mutex
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
	job, err := processAPI.createJob()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		_ = processAPI.closeHandle(job)
		return err
	}
	assigned := false
	terminated := false
	stateRegistered := false
	var processErr error
	err = cmd.Process.WithHandle(func(rawHandle uintptr) {
		process := windows.Handle(rawHandle)
		assigned = processAPI.assignJob(job, process) == nil
		// Restricted outer jobs can reject nested assignment. Keep leader
		// cancellation by process handle when no child job can be installed.
		state := &windowsProcessState{}
		if assigned {
			state.job = job
		} else {
			_ = processAPI.closeHandle(job)
		}
		processJobsMu.Lock()
		processJobs[cmd.Process] = state
		processJobsMu.Unlock()
		stateRegistered = true
		if !suspended {
			return
		}
		status := processAPI.resumeProcess(rawHandle)
		if status == 0 {
			return
		}
		resumeErr := fmt.Errorf("NtResumeProcess failed with status %#x", status)
		var terminateErr error
		if assigned {
			terminateErr = processAPI.terminateJob(job, 1)
		} else {
			terminateErr = processAPI.terminateProc(process, 1)
		}
		processErr = errors.Join(resumeErr, terminateErr)
		terminated = terminateErr == nil
	})
	err = errors.Join(err, processErr)
	if err != nil {
		if stateRegistered {
			releaseProcessTree(cmd.Process)
		} else {
			_ = processAPI.closeHandle(job)
		}
		var cleanupErr error
		if terminated {
			cleanupErr = waitStartedProcessCleanup(cmd)
		} else {
			cleanupErr = stopStartedProcess(cmd)
		}
		return errors.Join(err, cleanupErr)
	}
	return nil
}

func stopStartedProcess(cmd *exec.Cmd) error {
	killErr := cmd.Process.Kill()
	if errors.Is(killErr, os.ErrProcessDone) {
		killErr = nil
	}
	return errors.Join(killErr, waitStartedProcessCleanup(cmd))
}

func waitStartedProcessCleanup(cmd *exec.Cmd) error {
	done := make(chan error, 1)
	go func() {
		err := cmd.Wait()
		var exitError *exec.ExitError
		if err == nil || errors.As(err, &exitError) {
			err = nil
		}
		done <- err
	}()
	select {
	case err := <-done:
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
	if state == nil {
		return
	}
	state.mu.Lock()
	job := state.job
	state.job = 0
	state.mu.Unlock()
	if job != 0 {
		_ = processAPI.closeHandle(job)
	}
}

func markProcessExited(process *os.Process) {
	if process == nil {
		return
	}
	processJobsMu.Lock()
	state := processJobs[process]
	processJobsMu.Unlock()
	if state != nil {
		state.mu.Lock()
		state.exited = true
		state.mu.Unlock()
	}
}

func cleanupExitedProcessTree(process *os.Process) error {
	if process == nil {
		return os.ErrProcessDone
	}
	processJobsMu.Lock()
	state := processJobs[process]
	processJobsMu.Unlock()
	if state == nil {
		return os.ErrProcessDone
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.job == 0 {
		return os.ErrProcessDone
	}
	return processAPI.terminateJob(state.job, 1)
}

func terminateProcessTree(process *os.Process, _ context.Context) error {
	if process == nil {
		return os.ErrProcessDone
	}
	processJobsMu.Lock()
	state := processJobs[process]
	processJobsMu.Unlock()
	if state == nil {
		return process.Kill()
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.job != 0 {
		return processAPI.terminateJob(state.job, 1)
	}
	if state.exited {
		return os.ErrProcessDone
	}
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
