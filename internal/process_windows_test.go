//go:build windows

package internal

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func requireNestedWindowsJob(t *testing.T) {
	t.Helper()
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		t.Fatalf("create nested Job probe failed: %v", err)
	}
	defer windows.CloseHandle(job)
	cmd := processWindowsHelperCommand(nil, "sleep")
	cmd.SysProcAttr = &windows.SysProcAttr{CreationFlags: windows.CREATE_SUSPENDED}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start nested Job probe failed: %v", err)
	}
	defer func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}()
	var assignErr error
	if err := cmd.Process.WithHandle(func(rawHandle uintptr) {
		assignErr = windows.AssignProcessToJobObject(job, windows.Handle(rawHandle))
	}); err != nil {
		t.Fatalf("access nested Job probe process failed: %v", err)
	}
	if assignErr != nil {
		t.Skipf("outer Job rejects nested assignment: %v", assignErr)
	}
	if err := windows.TerminateJobObject(job, 1); err != nil {
		t.Fatalf("terminate nested Job probe failed: %v", err)
	}
	if err := cmd.Wait(); err == nil {
		t.Fatal("terminated nested Job probe returned nil error")
	}
}

func TestProcessWindowsCancelsJobTree(t *testing.T) {
	requireNestedWindowsJob(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	childPIDFile := filepath.Join(t.TempDir(), "child.pid")
	cmd := processWindowsHelperCommand(ctx, "parent", "COMPILEDB_TEST_WINDOWS_CHILD_PID="+childPIDFile)
	configureProcessCommand(cmd, ctx)
	if err := startProcessCommand(cmd); err != nil {
		t.Fatalf("start process tree failed: %v", err)
	}
	commandWaited := false
	t.Cleanup(func() {
		if !commandWaited && cmd.Process != nil {
			_ = terminateProcessTree(cmd.Process, context.Background())
			if err := waitStartedProcessCleanup(cmd); err != nil {
				t.Errorf("wait for process tree cleanup failed: %v", err)
			}
		}
		releaseProcessTree(cmd.Process)
	})
	waitForTestFile(t, childPIDFile)
	childPID := readWindowsTestPID(t, childPIDFile)
	childCleaned := false
	t.Cleanup(func() {
		if !childCleaned {
			terminateWindowsTestProcess(t, uint32(childPID))
		}
	})
	processJobsMu.Lock()
	state := processJobs[cmd.Process]
	processJobsMu.Unlock()
	if state == nil {
		t.Fatal("started process did not retain state")
	}
	state.mu.Lock()
	jobAssigned := state.job != 0
	state.mu.Unlock()

	cancel()
	err := waitStartedProcessCommand(cmd, ctx)
	commandWaited = true
	if err == nil {
		t.Fatal("canceled process tree returned nil error")
	}
	assertWindowsProcessExited(t, uint32(cmd.Process.Pid))
	if !jobAssigned {
		terminateWindowsTestProcess(t, uint32(childPID))
		childCleaned = true
		t.Fatal("process was not assigned to the supported nested Job")
	}
	assertWindowsProcessExited(t, uint32(childPID))
	childCleaned = true
	releaseProcessTree(cmd.Process)
	processJobsMu.Lock()
	_, retained := processJobs[cmd.Process]
	processJobsMu.Unlock()
	if retained {
		t.Fatal("released process job state was retained")
	}
}

func TestProcessWindowsStartFailureDoesNotRetainJob(t *testing.T) {
	originalAPI := processAPI
	createdJob := windows.Handle(8675309)
	closed := 0
	processAPI = originalAPI
	processAPI.createJob = func() (windows.Handle, error) { return createdJob, nil }
	processAPI.closeHandle = func(handle windows.Handle) error {
		if handle != createdJob {
			t.Fatalf("closed wrong Job handle: %v", handle)
		}
		closed++
		return nil
	}
	t.Cleanup(func() { processAPI = originalAPI })

	cmd := exec.Command(filepath.Join(t.TempDir(), "missing.exe"))
	configureProcessCommandWithoutContext(cmd)
	if err := startProcessCommand(cmd); err == nil {
		t.Fatal("missing executable started successfully")
	}
	if closed != 1 {
		t.Fatalf("start failure closed Job handle %d times", closed)
	}
}

func TestProcessWindowsStopStartedProcessWaitsAfterKillError(t *testing.T) {
	cmd := processWindowsHelperCommand(nil, "exit")
	configureProcessCommandWithoutContext(cmd)
	if err := startProcessCommand(cmd); err != nil {
		t.Fatalf("start short-lived process failed: %v", err)
	}
	t.Cleanup(func() { releaseProcessTree(cmd.Process) })
	assertWindowsProcessExited(t, uint32(cmd.Process.Pid))
	err := stopStartedProcess(cmd)
	if err != nil && !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		t.Fatalf("wait after exited-process kill failed: %v", err)
	}
	if cmd.ProcessState == nil {
		t.Fatal("kill error returned before Cmd.Wait completed")
	}
}

func TestProcessWindowsAssignmentFailureFallsBackToLeader(t *testing.T) {
	originalAPI := processAPI
	processAPI = originalAPI
	processAPI.assignJob = func(windows.Handle, windows.Handle) error { return windows.ERROR_ACCESS_DENIED }
	t.Cleanup(func() { processAPI = originalAPI })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := processWindowsHelperCommand(ctx, "sleep")
	configureProcessCommand(cmd, ctx)
	cmd.WaitDelay = 10 * time.Second
	if err := startProcessCommand(cmd); err != nil {
		t.Fatalf("start fallback process failed: %v", err)
	}
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		releaseProcessTree(cmd.Process)
	})
	processJobsMu.Lock()
	state := processJobs[cmd.Process]
	processJobsMu.Unlock()
	if state == nil || state.job != 0 {
		t.Fatalf("assignment failure did not select leader fallback: %#v", state)
	}
	done := make(chan error, 1)
	go func() { done <- waitStartedProcessCommand(cmd, ctx) }()
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("canceled fallback process returned nil error")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("leader fallback did not terminate process promptly")
	}
	releaseProcessTree(cmd.Process)
}

func TestProcessWindowsResumeFailureCleansState(t *testing.T) {

	originalAPI := processAPI
	terminatedJob := false
	terminatedProcess := false
	processAPI = originalAPI
	processAPI.resumeProcess = func(uintptr) uintptr { return 1 }
	processAPI.terminateJob = func(job windows.Handle, status uint32) error {
		terminatedJob = true
		return originalAPI.terminateJob(job, status)
	}
	processAPI.terminateProc = func(process windows.Handle, status uint32) error {
		terminatedProcess = true
		return originalAPI.terminateProc(process, status)
	}
	t.Cleanup(func() { processAPI = originalAPI })

	cmd := processWindowsHelperCommand(nil, "sleep")
	configureProcessCommandWithoutContext(cmd)
	err := startProcessCommand(cmd)
	trackWindowsTestCommand(t, cmd)
	if err == nil || !strings.Contains(err.Error(), "NtResumeProcess failed") {
		t.Fatalf("resume failure was not returned: %v", err)
	}
	if !terminatedJob && !terminatedProcess {
		t.Fatal("resume failure did not terminate the job or fallback leader")
	}
	if cmd.Process == nil {
		t.Fatal("resume failure did not start a process")
	}
	processJobsMu.Lock()
	_, retained := processJobs[cmd.Process]
	processJobsMu.Unlock()
	if retained {
		t.Fatal("resume failure retained process job state")
	}
	if cmd.ProcessState == nil {
		t.Fatal("resume failure returned before Cmd.Wait completed")
	}
}

func TestProcessWindowsResumeTerminationFailureFallsBackToLeader(t *testing.T) {
	originalAPI := processAPI
	terminateErr := errors.New("simulated termination failure")
	processAPI = originalAPI
	processAPI.resumeProcess = func(uintptr) uintptr { return 1 }
	processAPI.terminateJob = func(windows.Handle, uint32) error { return terminateErr }
	processAPI.terminateProc = func(windows.Handle, uint32) error { return terminateErr }
	t.Cleanup(func() { processAPI = originalAPI })

	cmd := processWindowsHelperCommand(nil, "sleep")
	configureProcessCommandWithoutContext(cmd)
	err := startProcessCommand(cmd)
	trackWindowsTestCommand(t, cmd)
	if !errors.Is(err, terminateErr) || !strings.Contains(err.Error(), "NtResumeProcess failed") {
		t.Fatalf("resume termination failure was not returned: %v", err)
	}
	if cmd.Process == nil {
		t.Fatal("resume termination failure did not start a process")
	}
	assertWindowsProcessExited(t, uint32(cmd.Process.Pid))
	processJobsMu.Lock()
	_, retained := processJobs[cmd.Process]
	processJobsMu.Unlock()
	if retained {
		t.Fatal("resume termination failure retained process job state")
	}
	if cmd.ProcessState == nil {
		t.Fatal("resume termination failure returned before Cmd.Wait completed")
	}
}

func TestProcessWindowsCancelsOutputProcesses(t *testing.T) {
	for name, mode := range map[string]string{
		"blocked":    "write-output",
		"continuous": "continuous-output",
	} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancelCause(context.Background())
			defer cancel(nil)
			outputReader, outputWriter, err := os.Pipe()
			if err != nil {
				t.Fatalf("create output pipe failed: %v", err)
			}
			defer outputReader.Close()
			defer outputWriter.Close()
			ready := filepath.Join(t.TempDir(), "output-ready")
			cmd := processWindowsHelperCommand(ctx, mode,
				"COMPILEDB_TEST_WINDOWS_OUTPUT_SIZE=16777216",
				"COMPILEDB_TEST_WINDOWS_READY="+ready,
			)
			configureProcessCommand(cmd, ctx)
			done := make(chan error, 1)
			go func() {
				err, stopWatching := runMakeCommand(ctx, cmd, outputWriter, outputWriter, EncodingRaw)
				stopWatching()
				done <- err
			}()
			commandDone := false
			cleanupStarted := false
			var drainStopped <-chan struct{}
			cleanup := func() {
				if cleanupStarted {
					return
				}
				cleanupStarted = true
				if !commandDone {
					cancel(SignalError{ProcessSignal: os.Interrupt})
					if cmd.Process != nil {
						_ = terminateProcessTree(cmd.Process, context.Background())
					}
				}
				_ = outputWriter.Close()
				_ = outputReader.Close()
				if !commandDone {
					select {
					case <-done:
						commandDone = true
					case <-time.After(3 * time.Second):
						t.Errorf("output process cleanup did not finish")
					}
				}
				if drainStopped != nil {
					select {
					case <-drainStopped:
					case <-time.After(3 * time.Second):
						t.Errorf("output drain cleanup did not finish")
					}
				}
			}
			t.Cleanup(cleanup)
			waitForTestFile(t, ready)
			var drainDone <-chan error
			if mode == "continuous-output" {
				cancelled := make(chan struct{})
				drainFailed := make(chan error, 1)
				drained := make(chan error, 1)
				drainDone = drained
				drainStoppedChannel := make(chan struct{})
				drainStopped = drainStoppedChannel
				go func() {
					defer close(drainStoppedChannel)
					buffer := make([]byte, 64*1024)
					remaining := 1024 * 1024
					for remaining > 0 {
						count, err := outputReader.Read(buffer)
						if err != nil {
							drainFailed <- err
							return
						}
						remaining -= count
					}
					cancel(SignalError{ProcessSignal: os.Interrupt})
					close(cancelled)
					_, err := io.Copy(io.Discard, outputReader)
					drained <- err
				}()
				select {
				case <-cancelled:
				case err := <-drainFailed:
					t.Fatalf("continuous output drain failed before cancellation: %v", err)
				case <-time.After(3 * time.Second):
					t.Fatal("continuous output did not begin draining")
				}
			} else {
				cancel(SignalError{ProcessSignal: os.Interrupt})
			}
			select {
			case err := <-done:
				commandDone = true
				if err == nil {
					t.Fatal("canceled output process returned nil error")
				}
			case <-time.After(3 * time.Second):
				cleanup()
				t.Fatal("canceled output process did not exit")
			}
			if drainDone != nil {
				_ = outputWriter.Close()
				select {
				case err := <-drainDone:
					if err != nil {
						t.Fatalf("continuous output drain failed: %v", err)
					}
				case <-time.After(3 * time.Second):
					t.Fatal("continuous output drain did not stop")
				}
			}
		})
	}
}

func writeWindowsBlockedOutput() {
	remaining, err := strconv.Atoi(os.Getenv("COMPILEDB_TEST_WINDOWS_OUTPUT_SIZE"))
	if err != nil || remaining <= 0 {
		os.Exit(2)
	}
	payload := make([]byte, remaining)
	runtime.GOMAXPROCS(1)
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		close(started)
		_, err := os.Stdout.Write(payload)
		done <- err
	}()
	<-started
	select {
	case <-done:
		os.Exit(2)
	case <-time.After(100 * time.Millisecond):
	}
	if err := os.WriteFile(os.Getenv("COMPILEDB_TEST_WINDOWS_READY"), nil, 0o600); err != nil {
		os.Exit(2)
	}
	<-done
}

func TestProcessWindowsHelper(t *testing.T) {
	switch os.Getenv("COMPILEDB_TEST_WINDOWS_PROCESS_MODE") {
	case "parent":
		child := processWindowsHelperCommand(nil, "sleep")
		if err := child.Start(); err != nil {
			os.Exit(2)
		}
		if err := writeWindowsTestFileAtomic(os.Getenv("COMPILEDB_TEST_WINDOWS_CHILD_PID"), strconv.Itoa(child.Process.Pid)); err != nil {
			_ = child.Process.Kill()
			os.Exit(2)
		}
		time.Sleep(30 * time.Second)
	case "sleep":
		time.Sleep(30 * time.Second)
	case "write-output":
		writeWindowsBlockedOutput()
	case "continuous-output":
		writeWindowsHelperOutput(true)
	default:
		return
	}
}

func writeWindowsHelperOutput(continuous bool) {
	if ready := os.Getenv("COMPILEDB_TEST_WINDOWS_READY"); ready != "" {
		if err := os.WriteFile(ready, nil, 0o600); err != nil {
			os.Exit(2)
		}
	}
	remaining, err := strconv.Atoi(os.Getenv("COMPILEDB_TEST_WINDOWS_OUTPUT_SIZE"))
	if err != nil || remaining < 0 {
		os.Exit(2)
	}
	block := make([]byte, 64*1024)
	for continuous || remaining > 0 {
		size := len(block)
		if !continuous {
			size = min(remaining, size)
		}
		written, err := os.Stdout.Write(block[:size])
		if err != nil || written == 0 {
			return
		}
		if !continuous {
			remaining -= written
		}
	}
}

func trackWindowsTestCommand(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	if cmd.Process == nil {
		return
	}
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
		}
		releaseProcessTree(cmd.Process)
	})
}

func writeWindowsTestFileAtomic(filename, contents string) error {
	temporary := filename + ".tmp"
	if err := os.WriteFile(temporary, []byte(contents), 0o600); err != nil {
		return err
	}
	if err := os.Rename(temporary, filename); err != nil {
		_ = os.Remove(temporary)
		return err
	}
	return nil
}

func processWindowsHelperCommand(ctx context.Context, mode string, environment ...string) *exec.Cmd {
	arguments := []string{"-test.run=^TestProcessWindowsHelper$"}
	var cmd *exec.Cmd
	if ctx == nil {
		cmd = exec.Command(os.Args[0], arguments...)
	} else {
		cmd = exec.CommandContext(ctx, os.Args[0], arguments...)
	}
	cmd.Env = append(os.Environ(), "COMPILEDB_TEST_WINDOWS_PROCESS_MODE="+mode)
	cmd.Env = append(cmd.Env, environment...)
	return cmd
}

func readWindowsTestPID(t *testing.T, filename string) int {
	t.Helper()
	data, err := os.ReadFile(filename)
	if err != nil {
		t.Fatalf("read process id failed: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatalf("parse process id failed: %v", err)
	}
	return pid
}

func assertWindowsProcessExited(t *testing.T, pid uint32) {
	t.Helper()
	handle, err := windows.OpenProcess(windows.SYNCHRONIZE, false, pid)
	if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
		return
	}
	if err != nil {
		t.Fatalf("open process %d: %v", pid, err)
	}
	defer windows.CloseHandle(handle)
	status, err := windows.WaitForSingleObject(handle, uint32((2*time.Second)/time.Millisecond))
	if err != nil || status != windows.WAIT_OBJECT_0 {
		t.Fatalf("process %d survived cancellation: status=%d error=%v", pid, status, err)
	}
}

func terminateWindowsTestProcess(t *testing.T, pid uint32) {
	t.Helper()
	handle, err := windows.OpenProcess(windows.PROCESS_TERMINATE|windows.SYNCHRONIZE, false, pid)
	if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
		return
	}
	if err != nil {
		t.Fatalf("open process %d for cleanup: %v", pid, err)
	}
	defer windows.CloseHandle(handle)
	if err := windows.TerminateProcess(handle, 1); err != nil && !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		t.Fatalf("terminate fallback child %d: %v", pid, err)
	}
	assertWindowsProcessExited(t, pid)
}
