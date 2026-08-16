package e2e_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	e2eBuildTimeout     = 2 * time.Minute
	e2eCommandTimeout   = 30 * time.Second
	e2eCommandWaitDelay = 2 * time.Second
	e2eHelperModeEnv    = "COMPILEDB_E2E_HELPER_MODE"
	e2eHelperPIDFileEnv = "COMPILEDB_E2E_HELPER_PID_FILE"
)

type e2eHarness struct {
	executable string
}

type e2eCommandOutput struct {
	stdout []byte
	stderr []byte
}

type e2eCommandResult struct {
	output e2eCommandOutput
	err    error
}

type e2eCompilationEntry struct {
	Directory string   `json:"directory"`
	Command   string   `json:"command"`
	Arguments []string `json:"arguments"`
	File      string   `json:"file"`
}

func TestE2ECommandWaitDelay(t *testing.T) {
	switch os.Getenv(e2eHelperModeEnv) {
	case "parent":
		child := exec.Command(os.Args[0], "-test.run=^TestE2ECommandWaitDelay$")
		child.Env = append(e2eEnvironment(),
			e2eHelperModeEnv+"=child",
			e2eHelperPIDFileEnv+"="+os.Getenv(e2eHelperPIDFileEnv),
		)
		child.Stdout = os.Stdout
		child.Stderr = os.Stderr
		if err := child.Start(); err != nil {
			t.Fatalf("start pipe-holding child failed: %v", err)
		}
		time.Sleep(10 * time.Second)
		return
	case "child":
		pidFile := os.Getenv(e2eHelperPIDFileEnv)
		temporaryPIDFile := pidFile + ".tmp"
		if err := os.WriteFile(temporaryPIDFile, []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
			t.Fatalf("write pipe-holding child PID failed: %v", err)
		}
		if err := os.Rename(temporaryPIDFile, pidFile); err != nil {
			t.Fatalf("publish pipe-holding child PID failed: %v", err)
		}
		time.Sleep(10 * time.Second)
		return
	}

	pidFile := filepath.Join(t.TempDir(), "child.pid")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan e2eCommandResult, 1)
	go func() {
		output, err := executeE2ECommand(ctx, 100*time.Millisecond, "",
			append(e2eEnvironment(),
				e2eHelperModeEnv+"=parent",
				e2eHelperPIDFileEnv+"="+pidFile,
			), nil, os.Args[0], "-test.run=^TestE2ECommandWaitDelay$")
		result <- e2eCommandResult{output: output, err: err}
	}()

	childPID := waitForE2EHelperPID(t, pidFile, result)
	child, err := os.FindProcess(childPID)
	if err != nil {
		cancel()
		t.Fatalf("find pipe-holding child failed: %v", err)
	}
	defer func() {
		_ = child.Kill()
		_ = child.Release()
	}()

	start := time.Now()
	cancel()
	command := <-result
	if !errors.Is(ctx.Err(), context.Canceled) || command.err == nil {
		t.Fatalf("expected command cancellation, context error=%v command error=%v", ctx.Err(), command.err)
	}
	if elapsed := time.Since(start); elapsed >= 3*time.Second {
		t.Fatalf("command waited for a descendant-held output pipe: %s", elapsed)
	}
}

func waitForE2EHelperPID(t *testing.T, pidFile string, result <-chan e2eCommandResult) int {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case command := <-result:
			t.Fatalf("helper command exited before starting child: %v\nstdout:\n%s\nstderr:\n%s", command.err, command.output.stdout, command.output.stderr)
		case <-deadline.C:
			t.Fatal("timed out waiting for pipe-holding child")
		case <-ticker.C:
			data, err := os.ReadFile(pidFile)
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				t.Fatalf("read pipe-holding child PID failed: %v", err)
			}
			pid, err := strconv.Atoi(string(data))
			if err != nil {
				t.Fatalf("parse pipe-holding child PID failed: %v", err)
			}
			return pid
		}
	}
}

func TestBuiltCLIEndToEnd(t *testing.T) {
	harness := newE2EHarness(t)

	t.Run("parse build log", func(t *testing.T) {
		logDir := t.TempDir()
		runDir := t.TempDir()
		writeE2EFile(t, filepath.Join(logDir, "main.c"), "int main(void) { return 0; }\n")
		buildLog, err := filepath.Abs(filepath.Join(logDir, "build.log"))
		if err != nil {
			t.Fatalf("resolve absolute build log path failed: %v", err)
		}
		writeE2EFile(t, buildLog, "cc -DE2E_PARSE=1 -c main.c -o main.o\n")
		outputFile := filepath.Join(runDir, "compile_commands.json")

		harness.run(t, runDir, nil, "--parse", buildLog)

		entries := readE2EDatabase(t, outputFile)
		if len(entries) != 1 {
			t.Fatalf("expected one compilation entry, got %#v", entries)
		}
		entry := entries[0]
		if entry.File != "main.c" {
			t.Fatalf("unexpected source file: %q", entry.File)
		}
		if entry.Command != "" || !slices.Equal(entry.Arguments, []string{"cc", "-DE2E_PARSE=1", "-c", "main.c", "-o", "main.o"}) {
			t.Fatalf("unexpected compilation command: %#v", entry)
		}
		checkE2EDirectory(t, entry.Directory, logDir)
	})

	t.Run("parse stdin to stdout", func(t *testing.T) {
		runDir := t.TempDir()
		output := harness.run(t, runDir, []byte("cc -c 'broken.c\ncc -DE2E_STDIN=1 -c valid.c -o valid.o\n"),
			"--output", "-",
			"--no-strict",
		)

		entries := decodeE2EDatabase(t, output.stdout, "stdout")
		if len(entries) != 1 {
			t.Fatalf("expected one compilation entry, got %#v", entries)
		}
		entry := entries[0]
		if entry.File != "valid.c" || entry.Command != "" ||
			!slices.Equal(entry.Arguments, []string{"cc", "-DE2E_STDIN=1", "-c", "valid.c", "-o", "valid.o"}) {
			t.Fatalf("unexpected compilation entry: %#v", entry)
		}
		checkE2EDirectory(t, entry.Directory, runDir)
		diagnostic := string(output.stderr)
		if !strings.Contains(diagnostic, "skip malformed command at build log line 1") ||
			!strings.Contains(diagnostic, "unterminated quote") {
			t.Fatalf("expected recoverable parser diagnostic on stderr, got %q", output.stderr)
		}
		if _, err := os.Stat(filepath.Join(runDir, "compile_commands.json")); !os.IsNotExist(err) {
			t.Fatalf("stdout output created the default database: %v", err)
		}
	})

	t.Run("explicit build directory", func(t *testing.T) {
		logDir := t.TempDir()
		buildDir := t.TempDir()
		runDir := t.TempDir()
		writeE2EFile(t, filepath.Join(buildDir, "main.c"), "int main(void) { return 0; }\n")
		buildLog := filepath.Join(logDir, "build.log")
		writeE2EFile(t, buildLog, "cc -DE2E_BUILD_DIR=1 -c main.c -o main.o\n")

		output := harness.run(t, runDir, nil,
			"--build-dir", buildDir,
			"--parse", buildLog,
			"--output", "-",
		)
		entries := decodeE2EDatabase(t, output.stdout, "stdout")
		if len(entries) != 1 {
			t.Fatalf("expected one compilation entry, got %#v", entries)
		}
		entry := entries[0]
		if entry.File != "main.c" || entry.Command != "" ||
			!slices.Equal(entry.Arguments, []string{"cc", "-DE2E_BUILD_DIR=1", "-c", "main.c", "-o", "main.o"}) {
			t.Fatalf("unexpected compilation entry: %#v", entry)
		}
		checkE2EDirectory(t, entry.Directory, buildDir)
	})

	t.Run("GNU Make build", func(t *testing.T) {
		makeExecutable := findE2EGNUMake(t)
		compiler := findE2ECompiler(t)
		projectDir := t.TempDir()
		marker := "COMPILEDB_E2E_MAKE_MARKER"
		writeE2EFile(t, filepath.Join(projectDir, "main.c"), `#ifndef COMPILEDB_E2E
#error COMPILEDB_E2E is required
#endif
int main(void) { return 0; }
`)
		writeE2EFile(t, filepath.Join(projectDir, "Makefile"), `all: main.o

main.o: main.c
	@echo `+marker+`
	$(CC) -DCOMPILEDB_E2E=1 -c main.c -o main.o
`)

		output := harness.run(t, projectDir, nil,
			"--build-dir", projectDir,
			"--output", "-",
			"make", "--cmd", makeExecutable, "CC="+compiler,
		)
		checkE2ERegularFile(t, filepath.Join(projectDir, "main.o"))
		if bytes.Contains(output.stdout, []byte(marker)) {
			t.Fatalf("Make marker leaked to stdout: %q", output.stdout)
		}
		if count := bytes.Count(output.stderr, []byte(marker)); count != 1 {
			t.Fatalf("expected one Make marker on stderr, got %d in %q", count, output.stderr)
		}

		entries := decodeE2EDatabase(t, output.stdout, "stdout")
		if len(entries) != 1 {
			t.Fatalf("expected one compilation entry, got %#v", entries)
		}
		entry := entries[0]
		if entry.File != "main.c" {
			t.Fatalf("unexpected source file: %q", entry.File)
		}
		if entry.Command != "" || !slices.Equal(entry.Arguments, []string{compiler, "-DCOMPILEDB_E2E=1", "-c", "main.c", "-o", "main.o"}) {
			t.Fatalf("unexpected compilation command: %#v", entry)
		}
		checkE2EDirectory(t, entry.Directory, projectDir)

		if err := os.Remove(filepath.Join(projectDir, "main.o")); err != nil {
			t.Fatalf("remove real-build object failed: %v", err)
		}
		replayE2ECommand(t, entry)
		checkE2ERegularFile(t, filepath.Join(projectDir, "main.o"))
	})
}

func newE2EHarness(t *testing.T) e2eHarness {
	t.Helper()
	executable := filepath.Join(t.TempDir(), "compiledb")
	if runtime.GOOS == "windows" {
		executable += ".exe"
	}
	moduleRoot := e2eModuleRoot(t)
	runE2ECommand(t, e2eBuildTimeout, moduleRoot, nil, nil,
		"go", "build", "-buildvcs=false", "-o", executable, "./cmd/compiledb")
	return e2eHarness{executable: executable}
}

func e2eModuleRoot(t *testing.T) string {
	t.Helper()
	output := runE2ECommand(t, e2eCommandTimeout, "", nil, nil, "go", "env", "GOMOD")
	goMod := strings.TrimSpace(string(output.stdout))
	if goMod == "" || goMod == os.DevNull {
		t.Fatalf("E2E test is not running in a Go module: GOMOD=%q", goMod)
	}
	return filepath.Dir(goMod)
}

func (h e2eHarness) run(t *testing.T, workingDir string, stdin []byte, arguments ...string) e2eCommandOutput {
	t.Helper()
	return runE2ECommand(t, e2eCommandTimeout, workingDir, e2eEnvironment(), stdin, h.executable, arguments...)
}

func replayE2ECommand(t *testing.T, entry e2eCompilationEntry) {
	t.Helper()
	if len(entry.Arguments) == 0 {
		t.Fatal("compilation entry has no arguments")
	}
	runE2ECommand(t, e2eCommandTimeout, filepath.FromSlash(entry.Directory), e2eEnvironment(), nil, entry.Arguments[0], entry.Arguments[1:]...)
}

func runE2ECommand(t *testing.T, timeout time.Duration, workingDir string, environment []string, stdin []byte, name string, arguments ...string) e2eCommandOutput {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	output, err := executeE2ECommand(ctx, e2eCommandWaitDelay, workingDir, environment, stdin, name, arguments...)
	invocation := append([]string{name}, arguments...)
	if ctx.Err() != nil {
		t.Fatalf("command timed out after %s: %v\ncommand: %q\nworking directory: %q\nstdout:\n%s\nstderr:\n%s",
			timeout, ctx.Err(), invocation, workingDir, output.stdout, output.stderr)
	}
	if err != nil {
		t.Fatalf("command failed: %v\ncommand: %q\nworking directory: %q\nstdout:\n%s\nstderr:\n%s",
			err, invocation, workingDir, output.stdout, output.stderr)
	}
	return output
}

func executeE2ECommand(ctx context.Context, waitDelay time.Duration, workingDir string, environment []string, stdin []byte, name string, arguments ...string) (e2eCommandOutput, error) {
	command := exec.CommandContext(ctx, name, arguments...)
	command.Dir = workingDir
	command.WaitDelay = waitDelay
	if environment != nil {
		command.Env = environment
	}
	if stdin != nil {
		command.Stdin = bytes.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	output := e2eCommandOutput{stdout: stdout.Bytes(), stderr: stderr.Bytes()}
	return output, err
}

func findE2EGNUMake(t *testing.T) string {
	t.Helper()
	for _, name := range []string{"make", "gmake", "mingw32-make"} {
		executable, err := exec.LookPath(name)
		if err != nil {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), e2eCommandTimeout)
		command := exec.CommandContext(ctx, executable, "--version")
		command.Env = e2eEnvironment()
		output, err := command.CombinedOutput()
		cancel()
		if err == nil && bytes.Contains(output, []byte("GNU Make")) {
			return executable
		}
	}
	t.Skip("GNU Make is not available")
	return ""
}

func findE2ECompiler(t *testing.T) string {
	t.Helper()
	for _, name := range []string{"cc", "gcc", "clang"} {
		if _, err := exec.LookPath(name); err == nil {
			return name
		}
	}
	t.Skip("a GNU-compatible C compiler is not available")
	return ""
}

func e2eEnvironment() []string {
	ignored := map[string]bool{
		"COMPILEDB_ENCODING": true,
		e2eHelperModeEnv:     true,
		e2eHelperPIDFileEnv:  true,
		"GNUMAKEFLAGS":       true,
		"MAKEFLAGS":          true,
		"MAKELEVEL":          true,
		"MFLAGS":             true,
		"LC_ALL":             true,
	}
	environment := make([]string, 0, len(os.Environ())+1)
	for _, variable := range os.Environ() {
		name, _, _ := strings.Cut(variable, "=")
		if !ignored[strings.ToUpper(name)] {
			environment = append(environment, variable)
		}
	}
	return append(environment, "LC_ALL=C")
}

func writeE2EFile(t *testing.T, filename, contents string) {
	t.Helper()
	if err := os.WriteFile(filename, []byte(contents), 0o644); err != nil {
		t.Fatalf("write %s failed: %v", filename, err)
	}
}

func readE2EDatabase(t *testing.T, filename string) []e2eCompilationEntry {
	t.Helper()
	data, err := os.ReadFile(filename)
	if err != nil {
		t.Fatalf("read compilation database %s failed: %v", filename, err)
	}
	return decodeE2EDatabase(t, data, filename)
}

func decodeE2EDatabase(t *testing.T, data []byte, source string) []e2eCompilationEntry {
	t.Helper()
	if len(data) == 0 || data[len(data)-1] != '\n' {
		t.Fatalf("%s must contain JSON with exactly one trailing newline: %q", source, data)
	}
	jsonData := data[:len(data)-1]
	if !bytes.Equal(jsonData, bytes.TrimSpace(jsonData)) {
		t.Fatalf("%s contains whitespace outside the JSON and final newline: %q", source, data)
	}
	var entries []e2eCompilationEntry
	if err := json.Unmarshal(jsonData, &entries); err != nil {
		t.Fatalf("decode compilation database from %s failed: %v\n%s", source, err, data)
	}
	return entries
}

func checkE2EDirectory(t *testing.T, got, want string) {
	t.Helper()
	gotInfo, err := os.Stat(filepath.FromSlash(got))
	if err != nil {
		t.Fatalf("stat generated directory %q failed: %v", got, err)
	}
	wantInfo, err := os.Stat(want)
	if err != nil {
		t.Fatalf("stat expected directory %q failed: %v", want, err)
	}
	if !os.SameFile(gotInfo, wantInfo) {
		t.Fatalf("unexpected compilation directory: want %q, got %q", want, got)
	}
}

func checkE2ERegularFile(t *testing.T, filename string) {
	t.Helper()
	info, err := os.Stat(filename)
	if err != nil {
		t.Fatalf("stat %s failed: %v", filename, err)
	}
	if !info.Mode().IsRegular() {
		t.Fatalf("expected %s to be a regular file, mode=%s", filename, info.Mode())
	}
}
