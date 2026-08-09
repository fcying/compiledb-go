//go:build aix || darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd || solaris

package internal

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestCommandExitCodeMapsUnixSignal(t *testing.T) {
	cmd := exec.Command("sh", "-c", "kill -TERM $$")
	err := cmd.Run()
	if code := commandExitCode(err); code != 128+int(syscall.SIGTERM) {
		t.Fatalf("unexpected signal exit code: %d (%v)", code, err)
	}
}

func TestConfigureMakeCommandCancelsProcessGroup(t *testing.T) {
	tmpDir := t.TempDir()
	pidFile := filepath.Join(tmpDir, "child.pid")
	doneFile := filepath.Join(tmpDir, "child.done")
	script := "sh -c 'trap \"echo terminated > " + doneFile + "; exit 0\" TERM; while :; do sleep 1; done' & " +
		"echo $! > " + pidFile + "; wait"
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, "sh", "-c", script)
	configureMakeCommand(cmd, ctx)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start process group failed: %v", err)
	}
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
	})

	var childPID int
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(pidFile)
		if err == nil {
			childPID, err = strconv.Atoi(strings.TrimSpace(string(data)))
			if err == nil {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if childPID == 0 {
		t.Fatal("child process did not start")
	}

	cancel()
	waitDone := make(chan error, 1)
	go func() { waitDone <- cmd.Wait() }()
	select {
	case err := <-waitDone:
		if err == nil || (!errors.Is(err, context.Canceled) && commandExitCode(err) != 128+int(syscall.SIGTERM)) {
			t.Fatalf("unexpected canceled command result: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("canceled make process did not exit")
	}

	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(doneFile); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("child process %d did not receive the group signal", childPID)
}

func TestRunMakeCommandCancelsDescendantsAfterLeaderExit(t *testing.T) {
	tmpDir := t.TempDir()
	pidFile := filepath.Join(tmpDir, "background.pid")
	outputFile, err := os.Create(filepath.Join(tmpDir, "output"))
	if err != nil {
		t.Fatalf("create output failed: %v", err)
	}
	defer outputFile.Close()

	ctx, cancel := context.WithCancelCause(context.Background())
	cmd := exec.CommandContext(ctx, "sh", "-c", "sleep 30 & echo $! > "+pidFile)
	configureMakeCommand(cmd, ctx)
	done := make(chan error, 1)
	go func() {
		err, stopWatching := runMakeCommand(ctx, cmd, outputFile, outputFile, EncodingRaw)
		defer stopWatching()
		done <- err
	}()
	waitForTestFile(t, pidFile)
	data, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("read background pid failed: %v", err)
	}
	backgroundPID, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatalf("parse background pid failed: %v", err)
	}
	t.Cleanup(func() { _ = syscall.Kill(backgroundPID, syscall.SIGKILL) })

	cancel(SignalError{ProcessSignal: syscall.SIGTERM})
	select {
	case err := <-done:
		if err == nil || contextExitCode(ctx) != 128+int(syscall.SIGTERM) {
			t.Fatalf("unexpected canceled Make result: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Make output wait did not stop after cancellation")
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		err := syscall.Kill(backgroundPID, 0)
		if errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("background process %d survived cancellation", backgroundPID)
}

func TestMakeWrapCancelsRealDescendantsAfterOutputWaitEnds(t *testing.T) {
	tmpDir := t.TempDir()
	pidFile := filepath.Join(tmpDir, "real-background.pid")
	makeScript := filepath.Join(tmpDir, "fake-make.sh")
	contents := `#!/bin/sh
case " $* " in
  *" -Bnkw "*) exec sleep 30 ;;
  *) sleep 30 & echo $! > "` + pidFile + `" ;;
esac
`
	if err := os.WriteFile(makeScript, []byte(contents), 0o755); err != nil {
		t.Fatalf("write fake make failed: %v", err)
	}
	oldMakePath := makePath
	makePath = makeScript
	defer func() { makePath = oldMakePath }()

	ctx, cancel := context.WithCancelCause(context.Background())
	tool := newTestTool(t, Config{OutputFile: filepath.Join(tmpDir, "compile_commands.json"), NoStrict: true, Encoding: EncodingRaw})
	tool.Context = ctx
	done := make(chan struct{})
	go func() {
		tool.MakeWrap(nil)
		close(done)
	}()
	waitForTestFile(t, pidFile)
	data, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("read background pid failed: %v", err)
	}
	backgroundPID, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatalf("parse background pid failed: %v", err)
	}
	t.Cleanup(func() { _ = syscall.Kill(backgroundPID, syscall.SIGKILL) })
	time.Sleep(makePipeWaitDelay + 200*time.Millisecond)
	cancel(SignalError{ProcessSignal: syscall.SIGTERM})

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("MakeWrap did not return after delayed cancellation")
	}
	if tool.StatusCode != 128+int(syscall.SIGTERM) {
		t.Fatalf("unexpected cancellation status: %d", tool.StatusCode)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(backgroundPID, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("real Make background process %d survived delayed cancellation", backgroundPID)
}

func TestRunMakeCommandCancelsBlockedOutput(t *testing.T) {
	outputReader, outputWriter, err := os.Pipe()
	if err != nil {
		t.Fatalf("create output pipe failed: %v", err)
	}
	defer outputReader.Close()
	defer outputWriter.Close()

	ctx, cancel := context.WithCancelCause(context.Background())
	cmd := exec.CommandContext(ctx, "sh", "-c", "exec dd if=/dev/zero bs=1048576 count=16 2>/dev/null")
	configureMakeCommand(cmd, ctx)
	done := make(chan error, 1)
	go func() {
		err, stopWatching := runMakeCommand(ctx, cmd, outputWriter, outputWriter, EncodingRaw)
		defer stopWatching()
		done <- err
	}()
	time.Sleep(200 * time.Millisecond)
	cancel(SignalError{ProcessSignal: syscall.SIGINT})
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("canceled blocked output returned nil error")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("blocked Make output did not stop after cancellation")
	}
}

func TestRunMakeCommandDoesNotTimeOutActiveOutput(t *testing.T) {
	outputReader, outputWriter, err := os.Pipe()
	if err != nil {
		t.Fatalf("create output pipe failed: %v", err)
	}
	defer outputReader.Close()
	defer outputWriter.Close()
	flagsBefore, err := unix.FcntlInt(outputWriter.Fd(), unix.F_GETFL, 0)
	if err != nil {
		t.Fatalf("read output flags failed: %v", err)
	}

	ctx := context.Background()
	cmd := exec.CommandContext(ctx, "sh", "-c", "exec dd if=/dev/zero bs=1048576 count=4 2>/dev/null")
	configureMakeCommand(cmd, ctx)
	done := make(chan error, 1)
	go func() {
		err, stopWatching := runMakeCommand(ctx, cmd, outputWriter, outputWriter, EncodingRaw)
		defer stopWatching()
		done <- err
	}()
	time.Sleep(2 * makePipeWaitDelay)
	flagsDuring, err := unix.FcntlInt(outputWriter.Fd(), unix.F_GETFL, 0)
	if err != nil {
		t.Fatalf("read active output flags failed: %v", err)
	}
	if flagsDuring != flagsBefore {
		t.Fatalf("Make output changed caller fd flags: before=%#x during=%#x", flagsBefore, flagsDuring)
	}
	data := make([]byte, 4*1024*1024)
	_, err = io.ReadFull(outputReader, data)
	if err != nil {
		t.Fatalf("read Make output failed: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("active Make output was interrupted: %v", err)
	}
	if len(data) != 4*1024*1024 {
		t.Fatalf("Make output was truncated: got %d bytes", len(data))
	}
}

func TestRunMakeCommandBoundsContinuousOutputAfterLeaderExit(t *testing.T) {
	output, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open null output failed: %v", err)
	}
	defer output.Close()
	tmpDir := t.TempDir()
	pidFile := filepath.Join(tmpDir, "continuous-output.pid")
	ctx := context.Background()
	cmd := exec.CommandContext(ctx, "sh", "-c", "(trap '' PIPE; while :; do printf x; sleep 0.02; done) & echo $! > "+pidFile)
	configureMakeCommand(cmd, ctx)

	start := time.Now()
	err, stopWatching := runMakeCommand(ctx, cmd, output, output, EncodingRaw)
	stopWatching()
	if !errors.Is(err, errProcessOutputIncomplete) {
		t.Fatalf("continuous inherited output returned %v", err)
	}
	if elapsed := time.Since(start); elapsed > 4*makePipeWaitDelay {
		t.Fatalf("continuous inherited output exceeded absolute drain limit: %v", elapsed)
	}
	assertTestProcessExited(t, pidFile)
}

func TestBacktickWaitDelayTerminatesBackgroundProcess(t *testing.T) {
	tmpDir := t.TempDir()
	pidFile := filepath.Join(tmpDir, "backtick-background.pid")
	tool := newTestTool(t, Config{OutputFile: filepath.Join(tmpDir, "compile_commands.json"), BuildDir: tmpDir, NoStrict: true, RegexCompile: RegexCompile, RegexFile: RegexFile})
	tool.Parse([]string{"gcc -I`sleep 30 & echo $! > " + pidFile + "; echo include` -c main.c"})
	assertTestProcessExited(t, pidFile)
}

func TestMacroProbeWaitDelayTerminatesBackgroundProcess(t *testing.T) {
	tmpDir := t.TempDir()
	pidFile := filepath.Join(tmpDir, "probe-background.pid")
	compiler := filepath.Join(tmpDir, "fake-gcc")
	contents := "#!/bin/sh\nsleep 30 & echo $! > " + pidFile + "\necho '#define TEST 1'\n"
	if err := os.WriteFile(compiler, []byte(contents), 0o755); err != nil {
		t.Fatalf("write fake compiler failed: %v", err)
	}
	tool := newTestTool(t, Config{})
	if macros := tool.getPredefinedMacros([]string{compiler, "-c", "main.c"}, "main.c", tmpDir); macros != nil {
		t.Fatalf("incomplete probe returned macros: %v", macros)
	}
	assertTestProcessExited(t, pidFile)
}

func TestOutputProcessCommandTerminatesBackgroundProcesses(t *testing.T) {
	for name, script := range map[string]string{
		"redirected output": "sleep 30 </dev/null >/dev/null 2>&1 & echo $! > %s; echo complete",
		"nonzero leader":    "sleep 30 & echo $! > %s; exit 7",
	} {
		t.Run(name, func(t *testing.T) {
			pidFile := filepath.Join(t.TempDir(), "background.pid")
			ctx := context.Background()
			cmd := exec.CommandContext(ctx, "sh", "-c", fmt.Sprintf(script, pidFile))
			configureProcessCommand(cmd, ctx)

			_, err := outputProcessCommand(cmd, ctx)
			if name == "redirected output" && err != nil {
				t.Fatalf("successful helper failed: %v", err)
			}
			if name == "nonzero leader" {
				var exitError *exec.ExitError
				if !errors.As(err, &exitError) || exitError.ExitCode() != 7 {
					t.Fatalf("nonzero helper returned %v", err)
				}
			}
			assertTestProcessExited(t, pidFile)
		})
	}
}

func TestInjectedCompilerProbeReturnsWhenCanceled(t *testing.T) {
	workingDir := t.TempDir()
	startedFile := filepath.Join(workingDir, "probe.started")
	ctx, cancel := context.WithCancel(context.Background())
	tool := newTestTool(t, Config{})
	tool.Context = ctx
	tool.compilerCommand = func(_ string, _ ...string) *exec.Cmd {
		return exec.Command("sh", "-c", "printf started > "+startedFile+"; exec sleep 30")
	}
	done := make(chan []string, 1)
	go func() {
		done <- tool.getPredefinedMacros([]string{"fake-gcc", "-c", "main.c"}, "main.c", workingDir)
	}()

	waitForTestFile(t, startedFile)
	cancel()
	select {
	case macros := <-done:
		if macros != nil {
			t.Fatalf("canceled injected probe returned macros: %v", macros)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("injected compiler probe did not return after cancellation")
	}
}

func TestMakeWrapNoBuildTerminatesDryRunBackgroundProcesses(t *testing.T) {
	for name, test := range map[string]struct {
		background string
		status     int
	}{
		"successful leader with redirected output": {
			background: "sleep 30 </dev/null >/dev/null 2>&1 &",
		},
		"nonzero leader with inherited output": {
			background: "sleep 30 &",
			status:     7,
		},
	} {
		t.Run(name, func(t *testing.T) {
			tmpDir := t.TempDir()
			pidFile := filepath.Join(tmpDir, "background.pid")
			makeScript := filepath.Join(tmpDir, "fake-make")
			script := "#!/bin/sh\n" + test.background + "\necho $! > " + pidFile + "\nexit " + strconv.Itoa(test.status) + "\n"
			if err := os.WriteFile(makeScript, []byte(script), 0o755); err != nil {
				t.Fatalf("write fake Make failed: %v", err)
			}
			oldMakePath := makePath
			makePath = makeScript
			defer func() { makePath = oldMakePath }()

			tool := newTestTool(t, Config{
				OutputFile: filepath.Join(tmpDir, "compile_commands.json"),
				NoBuild:    true,
				NoStrict:   true,
			})
			tool.MakeWrap(nil)
			if tool.StatusCode != test.status {
				t.Fatalf("unexpected dry-run status: got %d want %d", tool.StatusCode, test.status)
			}
			assertTestProcessExited(t, pidFile)
		})
	}
}

func assertTestProcessExited(t *testing.T, pidFile string) {
	t.Helper()
	waitForTestFile(t, pidFile)
	data, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("read process pid failed: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatalf("parse process pid failed: %v", err)
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("background process %d survived output cleanup", pid)
}
