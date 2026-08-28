//go:build aix || darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd || solaris

package internal

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestCommandExitCodeMapsUnixSignal(t *testing.T) {
	cmd := processUnixHelperCommand(nil, "terminate")
	err := cmd.Run()
	if code := commandExitCode(err); code != 128+int(syscall.SIGTERM) {
		t.Fatalf("unexpected signal exit code: %d (%v)", code, err)
	}
}

func TestConfigureMakeCommandCancelsProcessGroup(t *testing.T) {
	tmpDir := t.TempDir()
	pidFile := filepath.Join(tmpDir, "child.pid")
	doneFile := filepath.Join(tmpDir, "child.done")
	ctx, cancel := context.WithCancel(context.Background())
	cmd := processUnixHelperCommand(ctx, "group-parent",
		"COMPILEDB_TEST_PROCESS_PID_FILE="+pidFile,
		"COMPILEDB_TEST_PROCESS_DONE_FILE="+doneFile,
	)
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
	cmd := processUnixHelperCommand(ctx, "background-parent", "COMPILEDB_TEST_PROCESS_PID_FILE="+pidFile)
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
	cmd := processUnixHelperCommand(ctx, "write-output", "COMPILEDB_TEST_PROCESS_OUTPUT_SIZE=16777216")
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
	cmd := processUnixHelperCommand(ctx, "write-output", "COMPILEDB_TEST_PROCESS_OUTPUT_SIZE=4194304")
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
	if (flagsDuring^flagsBefore)&unix.O_NONBLOCK != 0 {
		t.Fatalf("Make output changed caller nonblocking mode: before=%#x during=%#x", flagsBefore, flagsDuring)
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
	// The race runtime's default one-second exit delay is unrelated to output draining.
	cmd := processUnixHelperCommand(ctx, "continuous-parent",
		"COMPILEDB_TEST_PROCESS_PID_FILE="+pidFile,
		"GORACE=atexit_sleep_ms=0",
	)
	configureMakeCommand(cmd, ctx)

	type commandResult struct {
		err          error
		stopWatching func()
	}
	done := make(chan commandResult, 1)
	go func() {
		err, stopWatching := runMakeCommand(ctx, cmd, output, output, EncodingRaw)
		done <- commandResult{err: err, stopWatching: stopWatching}
	}()
	waitForTestFile(t, pidFile)
	start := time.Now()
	completed := <-done
	completed.stopWatching()
	err = completed.err
	if !errors.Is(err, errProcessOutputIncomplete) {
		t.Fatalf("continuous inherited output returned %v", err)
	}
	if elapsed := time.Since(start); elapsed > 4*makePipeWaitDelay {
		t.Fatalf("continuous inherited output exceeded absolute drain limit: %v", elapsed)
	}
	assertTestProcessExited(t, pidFile)
}

func TestBacktickTerminatesUnwaitedBackgroundProcess(t *testing.T) {
	tmpDir := t.TempDir()
	pidFile := filepath.Join(tmpDir, "backtick-background.pid")
	t.Setenv("COMPILEDB_TEST_BACKTICK_BACKGROUND_PID", pidFile)
	helper := ShellJoinArgs([]string{os.Args[0], "-test.run=^TestBacktickBackgroundHelperProcess$"})
	tool := newTestTool(t, Config{OutputFile: filepath.Join(tmpDir, "compile_commands.json"), BuildDir: tmpDir, NoStrict: true, RegexCompile: RegexCompile, RegexFile: RegexFile})
	tool.Parse([]string{"gcc -I`" + helper + " & while [ ! -f " + ShellJoinArgs([]string{pidFile}) + " ]; do :; done; echo include` -c main.c"})
	assertTestProcessExited(t, pidFile)
}

func TestBacktickBackgroundHelperProcess(t *testing.T) {
	pidFile := os.Getenv("COMPILEDB_TEST_BACKTICK_BACKGROUND_PID")
	if pidFile == "" {
		return
	}
	if err := os.WriteFile(pidFile, []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		os.Exit(2)
	}
	time.Sleep(30 * time.Second)
	os.Exit(0)
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
	for name, test := range map[string]struct {
		status   int
		redirect bool
	}{
		"redirected output": {redirect: true},
		"nonzero leader":    {status: 7},
	} {
		t.Run(name, func(t *testing.T) {
			pidFile := filepath.Join(t.TempDir(), "background.pid")
			ctx := context.Background()
			cmd := processUnixHelperCommand(ctx, "output-parent",
				"COMPILEDB_TEST_PROCESS_PID_FILE="+pidFile,
				"COMPILEDB_TEST_PROCESS_STATUS="+strconv.Itoa(test.status),
				"COMPILEDB_TEST_PROCESS_REDIRECT="+strconv.FormatBool(test.redirect),
			)
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
		return processUnixHelperCommand(nil, "probe-sleep", "COMPILEDB_TEST_PROCESS_STARTED_FILE="+startedFile)
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

func TestMakeWrapCancelsActiveMakePhases(t *testing.T) {
	for name, test := range map[string]struct {
		noBuild       bool
		wantDiscovery bool
		wantDatabase  bool
	}{
		"real build": {noBuild: false, wantDiscovery: false, wantDatabase: false},
		"discovery":  {noBuild: true, wantDiscovery: true, wantDatabase: false},
	} {
		t.Run(name, func(t *testing.T) {
			tmpDir := t.TempDir()
			started := filepath.Join(tmpDir, "started-$'`")
			pidFile := filepath.Join(tmpDir, "child-$'`.pid")
			discovery := filepath.Join(tmpDir, "discovery-$'`")
			script := filepath.Join(tmpDir, "fake-make.sh")
			helper := "COMPILEDB_TEST_PROCESS_MODE=sleep " + ShellJoinArgs([]string{os.Args[0], "-test.run=^TestProcessUnixHelperProcess$"})
			discoveryArg := ShellJoinArgs([]string{discovery})
			pidArg := ShellJoinArgs([]string{pidFile})
			startedArg := ShellJoinArgs([]string{started})
			contents := `#!/bin/sh
case " $* " in
  *" -Bnkw "*) touch ` + discoveryArg + `; ` + helper + ` &
    echo $! > ` + pidArg + `; touch ` + startedArg + `; wait ;;
  *) ` + helper + ` &
    echo $! > ` + pidArg + `; touch ` + startedArg + `; wait ;;
esac
`
			if err := os.WriteFile(script, []byte(contents), 0o755); err != nil {
				t.Fatalf("write fake Make failed: %v", err)
			}
			oldMakePath := makePath
			makePath = script
			t.Cleanup(func() { makePath = oldMakePath })
			ctx, cancel := context.WithCancelCause(context.Background())
			tool := newTestTool(t, Config{OutputFile: filepath.Join(tmpDir, "compile_commands.json"), NoBuild: test.noBuild, NoStrict: true, Encoding: EncodingRaw})
			tool.Context = ctx
			done := make(chan struct{})
			go func() { tool.MakeWrap(nil); close(done) }()
			pid, markProcessExited := trackMakeWrapCancellationFixture(t, pidFile, done, cancel)
			waitForTestFile(t, started)
			cancel(SignalError{ProcessSignal: syscall.SIGTERM})
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("MakeWrap did not return after cancellation")
			}
			if tool.StatusCode != 128+int(syscall.SIGTERM) {
				t.Fatalf("unexpected cancellation status: %d", tool.StatusCode)
			}
			if _, err := os.Stat(discovery); (err == nil) != test.wantDiscovery {
				t.Fatalf("unexpected discovery phase state: %v", err)
			}
			if _, err := os.Stat(tool.Config.OutputFile); (err == nil) != test.wantDatabase {
				t.Fatalf("unexpected database state: %v", err)
			}
			assertTestProcessPIDExited(t, pid)
			markProcessExited()
		})
	}
}

func TestMakeWrapCancelsRecursiveDiscoveryProxy(t *testing.T) {
	makeExecutable := requireGNUmake(t)
	for _, name := range []string{"MAKE", "MAKE_COMMAND", "MAKEFLAGS", "GNUMAKEFLAGS", "MAKEFILES", "MAKELEVEL", "MFLAGS", "MAKEOVERRIDES"} {
		value, exists := os.LookupEnv(name)
		if err := os.Unsetenv(name); err != nil {
			t.Fatalf("unset %s failed: %v", name, err)
		}
		t.Cleanup(func() {
			if exists {
				_ = os.Setenv(name, value)
			} else {
				_ = os.Unsetenv(name)
			}
		})
	}
	tmpDir := t.TempDir()
	projectDir := filepath.Join(tmpDir, "project")
	childDir := filepath.Join(projectDir, "child")
	if err := os.MkdirAll(childDir, 0o755); err != nil {
		t.Fatalf("create recursive Make project failed: %v", err)
	}
	started := filepath.Join(tmpDir, "started-$")
	pidFile := filepath.Join(tmpDir, "child-$.pid")
	proxyRecord := filepath.Join(tmpDir, "proxy-path-$")
	if err := os.WriteFile(filepath.Join(projectDir, "Makefile"), []byte("all:\n\t$(MAKE) --no-print-directory -C child\n"), 0o644); err != nil {
		t.Fatalf("write root Makefile failed: %v", err)
	}
	startedArg := strings.ReplaceAll(ShellJoinArgs([]string{started}), "$", "$$")
	pidArg := strings.ReplaceAll(ShellJoinArgs([]string{pidFile}), "$", "$$")
	proxyArg := strings.ReplaceAll(ShellJoinArgs([]string{proxyRecord}), "$", "$$")
	childMakefile := "all:\n\tprintf '%s\\n' \"$(MAKE)\" > " + proxyArg + "; $(MAKE) --version >/dev/null; sleep 30 & echo $$! > " + pidArg + "; touch " + startedArg + "; wait\n"
	if err := os.WriteFile(filepath.Join(childDir, "Makefile"), []byte(childMakefile), 0o644); err != nil {
		t.Fatalf("write child Makefile failed: %v", err)
	}
	t.Setenv("TMPDIR", tmpDir)
	ctx, cancel := context.WithCancelCause(context.Background())
	tool := newTestTool(t, Config{
		BuildDir:     projectDir,
		OutputFile:   filepath.Join(tmpDir, "compile_commands.json"),
		MakeCommand:  makeExecutable,
		NoBuild:      true,
		NoStrict:     true,
		Encoding:     EncodingRaw,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
	})
	tool.Context = ctx
	done := make(chan struct{})
	go func() { tool.MakeWrap(nil); close(done) }()
	pid, markProcessExited := trackMakeWrapCancellationFixture(t, pidFile, done, cancel)
	waitForTestFile(t, started)
	proxyData, err := os.ReadFile(proxyRecord)
	if err != nil {
		t.Fatalf("read recursive Make proxy path failed: %v", err)
	}
	proxyPath := strings.TrimSpace(string(proxyData))
	if proxyPath == makeExecutable || filepath.Base(proxyPath) != "make" || !strings.HasPrefix(filepath.Base(filepath.Dir(proxyPath)), makeProxyDirectoryPrefix) {
		t.Fatalf("recursive Make bypassed discovery proxy: %q", proxyPath)
	}
	cancel(SignalError{ProcessSignal: syscall.SIGTERM})
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("recursive discovery did not return after cancellation")
	}
	if tool.StatusCode != 128+int(syscall.SIGTERM) {
		t.Fatalf("unexpected recursive discovery cancellation status: %d", tool.StatusCode)
	}
	if _, err := os.Stat(tool.Config.OutputFile); !os.IsNotExist(err) {
		t.Fatalf("canceled recursive discovery wrote a database: %v", err)
	}
	assertTestProcessPIDExited(t, pid)
	markProcessExited()
	entries, err := os.ReadDir(tmpDir)
	if err != nil {
		t.Fatalf("read proxy temporary directory failed: %v", err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), makeProxyDirectoryPrefix) {
			t.Fatalf("recursive discovery left proxy directory %q", entry.Name())
		}
	}
}

func TestWatchExitedProcessTreeStopsBeforeLaterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cmd := processUnixHelperCommand(ctx, "sleep")
	configureMakeCommand(cmd, ctx)
	if err := startProcessCommand(cmd); err != nil {
		t.Fatalf("start process failed: %v", err)
	}
	stopWatching := watchExitedProcessTree(ctx, cmd.Process)
	watcherCleaned := false
	t.Cleanup(func() {
		if watcherCleaned {
			return
		}
		cancel()
		stopWatching()
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("stop process failed: %v", err)
	}
	if err := cmd.Wait(); err == nil {
		t.Fatal("stopped process returned nil wait error")
	}
	stopWatching()
	watcherCleaned = true

	originalSignal := signalProcessGroup
	signals := make(chan syscall.Signal, 1)
	signalProcessGroup = func(_ int, signal syscall.Signal) error {
		select {
		case signals <- signal:
		default:
		}
		return syscall.ESRCH
	}
	t.Cleanup(func() { signalProcessGroup = originalSignal })
	cancel()
	select {
	case signal := <-signals:
		t.Fatalf("stopped watcher signaled a stale process group: %v", signal)
	case <-time.After(processKillDelay):
	}
}

func TestTerminateProcessTreeSignalsRecordedProcessGroup(t *testing.T) {
	originalSignal := signalProcessGroup
	var received struct {
		pgid   int
		signal syscall.Signal
	}
	signalProcessGroup = func(pgid int, signal syscall.Signal) error {
		received.pgid, received.signal = pgid, signal
		return syscall.ESRCH
	}
	t.Cleanup(func() { signalProcessGroup = originalSignal })

	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(SignalError{ProcessSignal: syscall.SIGINT})
	process := &os.Process{Pid: 8675309}
	if err := terminateProcessTree(process, ctx); !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("unexpected simulated stale group result: %v", err)
	}
	if received.pgid != process.Pid || received.signal != syscall.SIGINT {
		t.Fatalf("wrong process-group signal: %#v", received)
	}
}

func processUnixHelperCommand(ctx context.Context, mode string, environment ...string) *exec.Cmd {
	arguments := []string{"-test.run=^TestProcessUnixHelperProcess$"}
	var cmd *exec.Cmd
	if ctx == nil {
		cmd = exec.Command(os.Args[0], arguments...)
	} else {
		cmd = exec.CommandContext(ctx, os.Args[0], arguments...)
	}
	values := append([]string{"COMPILEDB_TEST_PROCESS_MODE=" + mode}, environment...)
	cmd.Env = os.Environ()
	for _, value := range values {
		name, _, _ := strings.Cut(value, "=")
		prefix := name + "="
		cmd.Env = append([]string(nil), cmd.Env...)
		for index := 0; index < len(cmd.Env); {
			if strings.HasPrefix(cmd.Env[index], prefix) {
				cmd.Env = append(cmd.Env[:index], cmd.Env[index+1:]...)
				continue
			}
			index++
		}
		cmd.Env = append(cmd.Env, value)
	}
	return cmd
}

func TestProcessUnixHelperProcess(t *testing.T) {
	mode := os.Getenv("COMPILEDB_TEST_PROCESS_MODE")
	if mode == "" {
		return
	}

	switch mode {
	case "terminate":
		signal.Reset(syscall.SIGTERM)
		if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
			os.Exit(2)
		}
		time.Sleep(time.Second)
		os.Exit(2)
	case "group-parent":
		cmd := inheritedProcessUnixHelperCommand("group-child",
			"COMPILEDB_TEST_PROCESS_PID_FILE="+os.Getenv("COMPILEDB_TEST_PROCESS_PID_FILE"),
			"COMPILEDB_TEST_PROCESS_DONE_FILE="+os.Getenv("COMPILEDB_TEST_PROCESS_DONE_FILE"),
		)
		if err := cmd.Run(); err != nil {
			os.Exit(commandExitCode(err))
		}
		os.Exit(0)
	case "group-child":
		terminated := make(chan os.Signal, 1)
		signal.Notify(terminated, syscall.SIGTERM)
		if !writeProcessUnixHelperFile("COMPILEDB_TEST_PROCESS_PID_FILE", strconv.Itoa(os.Getpid())) {
			os.Exit(2)
		}
		<-terminated
		if !writeProcessUnixHelperFile("COMPILEDB_TEST_PROCESS_DONE_FILE", "terminated") {
			os.Exit(2)
		}
		os.Exit(0)
	case "background-parent":
		startProcessUnixHelperChild("sleep", false)
	case "continuous-parent":
		startProcessUnixHelperChild("continuous-output", false)
	case "output-parent":
		redirect, err := strconv.ParseBool(os.Getenv("COMPILEDB_TEST_PROCESS_REDIRECT"))
		if err != nil {
			os.Exit(2)
		}
		startProcessUnixHelperChild("sleep", redirect)
		if _, err := os.Stdout.Write([]byte("complete\n")); err != nil {
			os.Exit(2)
		}
		status, err := strconv.Atoi(os.Getenv("COMPILEDB_TEST_PROCESS_STATUS"))
		if err != nil || status < 0 || status > 255 {
			os.Exit(2)
		}
		os.Exit(status)
	case "sleep":
		time.Sleep(30 * time.Second)
		os.Exit(0)
	case "write-output":
		remaining, err := strconv.Atoi(os.Getenv("COMPILEDB_TEST_PROCESS_OUTPUT_SIZE"))
		if err != nil || remaining < 0 {
			os.Exit(2)
		}
		block := make([]byte, 64*1024)
		for remaining > 0 {
			size := min(remaining, len(block))
			written, err := os.Stdout.Write(block[:size])
			remaining -= written
			if err != nil || written == 0 {
				os.Exit(2)
			}
		}
		os.Exit(0)
	case "continuous-output":
		signal.Ignore(syscall.SIGPIPE)
		for {
			if _, err := os.Stdout.Write([]byte("x")); err != nil {
				os.Exit(0)
			}
			time.Sleep(20 * time.Millisecond)
		}
	case "probe-sleep":
		if !writeProcessUnixHelperFile("COMPILEDB_TEST_PROCESS_STARTED_FILE", "started") {
			os.Exit(2)
		}
		time.Sleep(30 * time.Second)
		os.Exit(0)
	default:
		os.Exit(2)
	}
}

func inheritedProcessUnixHelperCommand(mode string, environment ...string) *exec.Cmd {
	cmd := processUnixHelperCommand(nil, mode, environment...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd
}

func startProcessUnixHelperChild(mode string, redirect bool) {
	cmd := inheritedProcessUnixHelperCommand(mode)
	if redirect {
		null, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
		if err != nil {
			os.Exit(2)
		}
		cmd.Stdin = null
		cmd.Stdout = null
		cmd.Stderr = null
		defer null.Close()
	}
	if err := cmd.Start(); err != nil {
		os.Exit(2)
	}
	if !writeProcessUnixHelperFile("COMPILEDB_TEST_PROCESS_PID_FILE", strconv.Itoa(cmd.Process.Pid)) {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		os.Exit(2)
	}
}

func writeProcessUnixHelperFile(environment, contents string) bool {
	filename := os.Getenv(environment)
	return filename != "" && os.WriteFile(filename, []byte(contents), 0o600) == nil
}

func readTestProcessPID(t *testing.T, pidFile string) int {
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
	return pid
}

func trackTestProcess(t *testing.T, pidFile string) int {
	t.Helper()
	pid := readTestProcessPID(t, pidFile)
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	return pid
}

func trackMakeWrapCancellationFixture(t *testing.T, pidFile string, done <-chan struct{}, cancel context.CancelCauseFunc) (int, func()) {
	t.Helper()
	pid := 0
	cleaned := false
	t.Cleanup(func() {
		if cleaned {
			return
		}
		cancel(SignalError{ProcessSignal: syscall.SIGKILL})
		if pid != 0 {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Errorf("process cleanup did not release MakeWrap")
		}
	})
	pid = readTestProcessPID(t, pidFile)
	return pid, func() { cleaned = true }
}

func assertTestProcessExited(t *testing.T, pidFile string) {
	t.Helper()
	assertTestProcessPIDExited(t, trackTestProcess(t, pidFile))
}

func assertTestProcessPIDExited(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("background process %d survived output cleanup", pid)
}
