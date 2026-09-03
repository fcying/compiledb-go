//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package internal

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestStrictSourceFileAcceptsOnlyRegularFiles(t *testing.T) {
	tmpDir := t.TempDir()
	regular := filepath.Join(tmpDir, "regular.c")
	if err := os.WriteFile(regular, nil, 0o644); err != nil {
		t.Fatalf("create regular file failed: %v", err)
	}
	link := filepath.Join(tmpDir, "link.c")
	if err := os.Symlink(regular, link); err != nil {
		t.Fatalf("create regular-file symlink failed: %v", err)
	}
	fifo := filepath.Join(tmpDir, "source.c")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatalf("create FIFO failed: %v", err)
	}
	broken := filepath.Join(tmpDir, "broken.c")
	if err := os.Symlink(filepath.Join(tmpDir, "missing.c"), broken); err != nil {
		t.Fatalf("create broken symlink failed: %v", err)
	}

	if err := strictSourceFile(regular); err != nil {
		t.Fatalf("regular file was rejected: %v", err)
	}
	if err := strictSourceFile(link); err != nil {
		t.Fatalf("symlink to regular file was rejected: %v", err)
	}
	for _, filename := range []string{tmpDir, fifo, broken} {
		if err := strictSourceFile(filename); err == nil {
			t.Fatalf("non-regular source was accepted: %q", filename)
		}
	}
}

func TestWriteFileAtomicallyRejectsNonRegularTarget(t *testing.T) {
	tmpDir := t.TempDir()
	outputFile := filepath.Join(tmpDir, "compile_commands.json")
	if err := syscall.Mkfifo(outputFile, 0o600); err != nil {
		t.Fatalf("create output FIFO failed: %v", err)
	}

	err := writeFileAtomically(outputFile, []byte("[]\n"))
	if err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("non-regular output was not rejected: %v", err)
	}
	info, err := os.Lstat(outputFile)
	if err != nil {
		t.Fatalf("stat output FIFO failed: %v", err)
	}
	if info.Mode()&os.ModeNamedPipe == 0 {
		t.Fatalf("output FIFO was replaced with mode %v", info.Mode())
	}
	entries, err := os.ReadDir(tmpDir)
	if err != nil {
		t.Fatalf("read output directory failed: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != filepath.Base(outputFile) {
		t.Fatalf("rejecting output FIFO left unexpected files: %#v", entries)
	}
}

func TestWriteJSONRejectsFIFOWithoutBlocking(t *testing.T) {
	const (
		helperEnvironment = "COMPILEDB_TEST_WRITE_JSON_FIFO_HELPER"
		outputEnvironment = "COMPILEDB_TEST_WRITE_JSON_FIFO_PATH"
	)
	if parentPID, err := strconv.Atoi(os.Getenv(helperEnvironment)); err == nil && parentPID == os.Getppid() {
		outputFile := os.Getenv(outputEnvironment)
		info, statErr := os.Lstat(outputFile)
		if statErr != nil || info.Mode()&os.ModeNamedPipe == 0 {
			t.Fatalf("invalid FIFO helper output %q: %v", outputFile, statErr)
		}
		tool := newTestTool(t, Config{OutputFile: outputFile, NoStrict: true})
		commands := []Command{{Directory: "/project", Arguments: []string{"cc", "-c", "main.c"}, File: "main.c"}}
		tool.WriteJSON(outputFile, len(commands), &commands)
		return
	}

	tmpDir := t.TempDir()
	outputFile := filepath.Join(tmpDir, "compile_commands.json")
	if err := syscall.Mkfifo(outputFile, 0o600); err != nil {
		t.Fatalf("create output FIFO failed: %v", err)
	}
	unrelatedFile := filepath.Join(tmpDir, "unrelated.json")
	if err := os.WriteFile(unrelatedFile, []byte("unchanged\n"), 0o600); err != nil {
		t.Fatalf("create unrelated output failed: %v", err)
	}
	t.Setenv(helperEnvironment, "ambient")
	t.Setenv(outputEnvironment, unrelatedFile)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestWriteJSONRejectsFIFOWithoutBlocking$")
	environment := os.Environ()
	cmd.Env = make([]string, 0, len(environment)+2)
	for _, value := range environment {
		name, _, _ := strings.Cut(value, "=")
		if name == helperEnvironment || name == outputEnvironment {
			continue
		}
		cmd.Env = append(cmd.Env, value)
	}
	cmd.Env = append(cmd.Env,
		helperEnvironment+"="+strconv.Itoa(os.Getpid()),
		outputEnvironment+"="+outputFile,
	)
	err := cmd.Run()
	if ctx.Err() != nil {
		t.Fatal("WriteJSON blocked while loading the output FIFO")
	}
	exitErr, ok := err.(*exec.ExitError)
	if !ok || exitErr.ExitCode() != 1 {
		t.Fatalf("FIFO helper did not fail through WriteJSON: %v", err)
	}
	info, err := os.Lstat(outputFile)
	if err != nil {
		t.Fatalf("stat output FIFO failed: %v", err)
	}
	if info.Mode()&os.ModeNamedPipe == 0 {
		t.Fatalf("WriteJSON replaced output FIFO with mode %v", info.Mode())
	}
	unrelatedData, err := os.ReadFile(unrelatedFile)
	if err != nil {
		t.Fatalf("read unrelated output failed: %v", err)
	}
	if string(unrelatedData) != "unchanged\n" {
		t.Fatalf("FIFO helper changed unrelated output: %q", unrelatedData)
	}
}

func TestWriteJSONPreservesOutputSymlink(t *testing.T) {
	tmpDir := t.TempDir()
	realDir := filepath.Join(tmpDir, "real")
	physicalLinkDir := filepath.Join(realDir, "subdir")
	if err := os.MkdirAll(physicalLinkDir, 0o755); err != nil {
		t.Fatalf("create symlink test directories failed: %v", err)
	}
	target := filepath.Join(realDir, "database.json")
	writeTestJSON(t, target, []Command{{Directory: "/project", Command: "cc -DOLD -c main.c", File: "main.c"}})
	aliasDir := filepath.Join(tmpDir, "alias")
	if err := os.Symlink(filepath.Join("real", "subdir"), aliasDir); err != nil {
		t.Fatalf("create parent directory symlink failed: %v", err)
	}
	outputFile := filepath.Join(aliasDir, "compile_commands.json")
	if err := os.Symlink(filepath.Join("..", filepath.Base(target)), outputFile); err != nil {
		t.Fatalf("create output symlink failed: %v", err)
	}

	tool := newTestTool(t, Config{OutputFile: outputFile, NoStrict: true})
	commands := []Command{{Directory: "/project", Arguments: []string{"cc", "-DNEW", "-c", "main.c"}, File: "main.c"}}
	tool.WriteJSON(outputFile, len(commands), &commands)

	aliasInfo, err := os.Lstat(aliasDir)
	if err != nil {
		t.Fatalf("stat parent directory symlink failed: %v", err)
	}
	if aliasInfo.Mode()&os.ModeSymlink == 0 {
		t.Fatal("atomic replacement replaced the parent directory symlink")
	}
	outputInfo, err := os.Lstat(outputFile)
	if err != nil {
		t.Fatalf("stat output symlink failed: %v", err)
	}
	if outputInfo.Mode()&os.ModeSymlink == 0 {
		t.Fatal("atomic replacement replaced the output symlink")
	}
	entries := readTestDatabase(t, target)
	if len(entries) != 1 {
		t.Fatalf("unexpected target entries: %#v", entries)
	}
	assertTestArgument(t, entries[0], 1, "-DNEW")
}

func TestResolveOutputFilenameRejectsDanglingSymlink(t *testing.T) {
	outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
	if err := os.Symlink("missing.json", outputFile); err != nil {
		t.Fatalf("create dangling output symlink failed: %v", err)
	}

	if resolved, err := resolveOutputFilename(outputFile); err == nil {
		t.Fatalf("resolved dangling output symlink as %q", resolved)
	}
}

func TestResolveOutputFilenameResolvesMissingParentBeforeDotDot(t *testing.T) {
	tmpDir := t.TempDir()
	realDir := filepath.Join(tmpDir, "real")
	if err := os.MkdirAll(filepath.Join(realDir, "subdir"), 0o755); err != nil {
		t.Fatalf("create physical output directory failed: %v", err)
	}
	aliasDir := filepath.Join(tmpDir, "alias")
	if err := os.Symlink(filepath.Join("real", "subdir"), aliasDir); err != nil {
		t.Fatalf("create parent directory symlink failed: %v", err)
	}
	separator := string(filepath.Separator)
	outputFile := aliasDir + separator + ".." + separator + "compile_commands.json"

	resolved, err := resolveOutputFilename(outputFile)
	if err != nil {
		t.Fatalf("resolve missing output failed: %v", err)
	}
	physicalRealDir, err := filepath.EvalSymlinks(realDir)
	if err != nil {
		t.Fatalf("resolve physical output directory failed: %v", err)
	}
	want := filepath.Join(physicalRealDir, "compile_commands.json")
	if resolved != want {
		t.Fatalf("unexpected missing output target: want %q, got %q", want, resolved)
	}
}

func TestWriteJSONAcceptsLongOutputBasename(t *testing.T) {
	outputFile := filepath.Join(t.TempDir(), strings.Repeat("x", 240))
	if err := os.WriteFile(outputFile, nil, 0o600); err != nil {
		if errors.Is(err, syscall.ENAMETOOLONG) {
			t.Skipf("filesystem rejects the long output basename: %v", err)
		}
		t.Fatalf("create long-name output failed: %v", err)
	}
	if err := os.Remove(outputFile); err != nil {
		t.Fatalf("remove long-name output preflight file failed: %v", err)
	}
	tool := newTestTool(t, Config{OutputFile: outputFile, NoStrict: true})
	commands := []Command{{Directory: "/project", Arguments: []string{"cc", "-c", "main.c"}, File: "main.c"}}

	tool.WriteJSON(outputFile, len(commands), &commands)

	entries := readTestDatabase(t, outputFile)
	if len(entries) != 1 || entries[0]["file"] != "main.c" {
		t.Fatalf("unexpected long-name output entries: %#v", entries)
	}
}

func TestWriteJSONPreservesOutputMode(t *testing.T) {
	outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
	writeTestJSON(t, outputFile, []Command{{Directory: "/project", Command: "cc -DOLD -c main.c", File: "main.c"}})
	if err := os.Chmod(outputFile, 0o640); err != nil {
		t.Fatalf("set original output mode failed: %v", err)
	}
	oldUmask := syscall.Umask(0o077)
	defer syscall.Umask(oldUmask)

	tool := newTestTool(t, Config{OutputFile: outputFile, NoStrict: true})
	commands := []Command{{Directory: "/project", Arguments: []string{"cc", "-DNEW", "-c", "main.c"}, File: "main.c"}}
	tool.WriteJSON(outputFile, len(commands), &commands)

	info, err := os.Stat(outputFile)
	if err != nil {
		t.Fatalf("stat replaced output failed: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o640 {
		t.Fatalf("output mode changed: want 0640, got %04o", got)
	}
}

func TestWriteJSONNewOutputRespectsUmask(t *testing.T) {
	outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
	oldUmask := syscall.Umask(0o077)
	defer syscall.Umask(oldUmask)

	tool := newTestTool(t, Config{OutputFile: outputFile, NoStrict: true})
	commands := []Command{{Directory: "/project", Arguments: []string{"cc", "-c", "main.c"}, File: "main.c"}}
	tool.WriteJSON(outputFile, len(commands), &commands)

	info, err := os.Stat(outputFile)
	if err != nil {
		t.Fatalf("stat new output failed: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("new output ignored umask: want 0600, got %04o", got)
	}
}

func TestWriteJSONPreservesOpenReaderSnapshot(t *testing.T) {
	outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
	writeTestJSON(t, outputFile, []Command{{Directory: "/project", Command: "cc -DOLD -c main.c", File: "main.c"}})
	reader, err := os.Open(outputFile)
	if err != nil {
		t.Fatalf("open original database failed: %v", err)
	}
	defer reader.Close()

	tool := newTestTool(t, Config{OutputFile: outputFile, NoStrict: true})
	commands := []Command{{Directory: "/project", Arguments: []string{"cc", "-DNEW", "-c", "main.c"}, File: "main.c"}}
	tool.WriteJSON(outputFile, len(commands), &commands)

	var oldEntries []Command
	if err := json.NewDecoder(reader).Decode(&oldEntries); err != nil {
		t.Fatalf("decode original reader failed: %v", err)
	}
	if len(oldEntries) != 1 || oldEntries[0].File != "main.c" || !strings.Contains(oldEntries[0].Command, "-DOLD") {
		t.Fatalf("open reader did not retain the old database: %#v", oldEntries)
	}
	newEntries := readTestDatabase(t, outputFile)
	if len(newEntries) != 1 {
		t.Fatalf("unexpected replacement entries: %#v", newEntries)
	}
	assertTestArgument(t, newEntries[0], 1, "-DNEW")
}

func TestExpandCompilerResponseFilesRejectsFIFO(t *testing.T) {
	workingDir := t.TempDir()
	if err := syscall.Mkfifo(filepath.Join(workingDir, "arguments.rsp"), 0o600); err != nil {
		t.Fatalf("create response FIFO failed: %v", err)
	}
	tool := newTestTool(t, Config{})
	arguments := []string{"gcc", "@arguments.rsp"}
	result, err := tool.expandCompilerResponseFiles(arguments, parseCompilerInvocation(arguments), workingDir)
	if err == nil || !strings.Contains(err.Error(), "not a regular file") || result != nil {
		t.Fatalf("response FIFO was accepted: arguments=%#v error=%v", result, err)
	}
}

func TestExpandCompilerResponseFilesAcceptsDoubleSlashAbsolutePath(t *testing.T) {
	workingDir := t.TempDir()
	filename := filepath.Join(workingDir, "arguments.rsp")
	if err := os.WriteFile(filename, []byte("-c main.c"), 0o644); err != nil {
		t.Fatalf("write response file failed: %v", err)
	}
	tool := newTestTool(t, Config{})
	arguments := []string{"gcc", "@/" + filepath.ToSlash(filename)}
	result, err := tool.expandCompilerResponseFiles(arguments, parseCompilerInvocation(arguments), workingDir)
	if err != nil {
		t.Fatalf("expand double-slash response path failed: %v", err)
	}
	want := []string{"gcc", "-c", "main.c"}
	if !slices.Equal(result, want) {
		t.Fatalf("unexpected response arguments: want %#v, got %#v", want, result)
	}
}

func TestShellSafeMakeProxyPathAcceptsNonUTF8Path(t *testing.T) {
	proxyPath := string([]byte{'/', 't', 'm', 'p', '/', 0xff, '/', 'm', 'a', 'k', 'e'})
	if !shellSafeMakeProxyPath(proxyPath) {
		t.Fatalf("shell-safe proxy path was rejected: %q", proxyPath)
	}
}

func TestInstallMakeProxyExecutableResolvesRelativeSymlink(t *testing.T) {
	sourceDir := t.TempDir()
	target := filepath.Join(sourceDir, "compiledb-real")
	if err := os.WriteFile(target, []byte("proxy executable"), 0o700); err != nil {
		t.Fatalf("write proxy executable failed: %v", err)
	}
	link := filepath.Join(sourceDir, "compiledb")
	if err := os.Symlink(filepath.Base(target), link); err != nil {
		t.Skipf("create executable symlink failed: %v", err)
	}
	oldWorkingDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("get working directory failed: %v", err)
	}
	if err := os.Chdir(sourceDir); err != nil {
		t.Fatalf("change working directory failed: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWorkingDir) })
	proxyPath := filepath.Join(t.TempDir(), "make")
	if err := installMakeProxyExecutable(filepath.Base(link), proxyPath); err != nil {
		t.Fatalf("install executable proxy failed: %v", err)
	}
	if _, err := filepath.EvalSymlinks(proxyPath); err != nil {
		t.Fatalf("resolve executable proxy failed: %v", err)
	}
	info, err := os.Lstat(proxyPath)
	if err != nil {
		t.Fatalf("stat executable proxy failed: %v", err)
	}
	contents, err := os.ReadFile(proxyPath)
	if err != nil || info.Mode()&os.ModeSymlink == 0 || string(contents) != "proxy executable" {
		t.Fatalf("resolved symlink was not preferred for the proxy: mode=%v contents=%q error=%v", info.Mode(), contents, err)
	}
}

func TestCreateDiscoveryMakeProxyRejectsNonExecutableSource(t *testing.T) {
	executable := filepath.Join(t.TempDir(), "compiledb")
	if err := os.WriteFile(executable, []byte("not executable"), 0o600); err != nil {
		t.Fatalf("write non-executable source failed: %v", err)
	}
	parent := t.TempDir()
	proxyPath, cleanup, err := createDiscoveryMakeProxyIn(parent, executable, "/tools/gmake")
	if err == nil {
		cleanup()
		t.Fatalf("non-executable proxy was accepted: %q", proxyPath)
	}
	entries, readErr := os.ReadDir(parent)
	if readErr != nil || len(entries) != 0 {
		t.Fatalf("failed proxy was not cleaned up: entries=%v error=%v", entries, readErr)
	}
}

func TestCreateDiscoveryMakeProxyFallsBackAfterCandidateFailure(t *testing.T) {
	unsafeParent := filepath.Join(t.TempDir(), "unsafe parent")
	safeParent := t.TempDir()
	if err := os.Mkdir(unsafeParent, 0o755); err != nil {
		t.Fatalf("create unsafe parent failed: %v", err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatalf("get executable failed: %v", err)
	}
	proxyPath, cleanup, err := createDiscoveryMakeProxyFromParents(executable, "/tools/gmake", []string{unsafeParent, safeParent})
	if err != nil {
		t.Fatalf("fall back to safe proxy parent failed: %v", err)
	}
	defer cleanup()
	if filepath.Dir(filepath.Dir(proxyPath)) != safeParent {
		t.Fatalf("unexpected proxy fallback path: %q", proxyPath)
	}
	entries, err := os.ReadDir(unsafeParent)
	if err != nil || len(entries) != 0 {
		t.Fatalf("failed proxy candidate was not cleaned up: entries=%v error=%v", entries, err)
	}
}

func TestMakeProxyDelegatesToSelectedExecutable(t *testing.T) {
	selectedMake := filepath.Join(t.TempDir(), "selected-make")
	outputFile := filepath.Join(t.TempDir(), "invocation")
	script := "#!/bin/sh\nprintf '%s\\n' \"$0\" \"$@\" > \"$COMPILEDB_TEST_MAKE_PROXY_OUTPUT\"\n"
	if err := os.WriteFile(selectedMake, []byte(script), 0o700); err != nil {
		t.Fatalf("write selected Make executable failed: %v", err)
	}
	proxyPath, cleanup, err := createDiscoveryMakeProxy(selectedMake)
	if err != nil {
		t.Fatalf("create Make proxy failed: %v", err)
	}
	defer cleanup()

	command := exec.Command(proxyPath, "--no-print-directory", "goal")
	command.Env = append(os.Environ(), "COMPILEDB_TEST_MAKE_PROXY_OUTPUT="+outputFile)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("run Make proxy failed: %v: %s", err, output)
	}
	contents, err := os.ReadFile(outputFile)
	if err != nil {
		t.Fatalf("read delegated invocation failed: %v", err)
	}
	arguments := strings.Split(strings.TrimSpace(string(contents)), "\n")
	if len(arguments) != 3 || arguments[0] != selectedMake || arguments[1] != "--print-directory" || arguments[2] != "goal" {
		t.Fatalf("proxy did not delegate to the selected Make: %#v", arguments)
	}
}

func TestMakeProxyCheckArgumentIsDelegatedAfterCreation(t *testing.T) {
	selectedMake := filepath.Join(t.TempDir(), "selected-make")
	outputFile := filepath.Join(t.TempDir(), "invocation")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$COMPILEDB_TEST_MAKE_PROXY_OUTPUT\"\n"
	if err := os.WriteFile(selectedMake, []byte(script), 0o700); err != nil {
		t.Fatalf("write selected Make executable failed: %v", err)
	}
	proxyPath, cleanup, err := createDiscoveryMakeProxy(selectedMake)
	if err != nil {
		t.Fatalf("create Make proxy failed: %v", err)
	}
	defer cleanup()

	command := exec.Command(proxyPath, makeProxyCheckArgument)
	command.Env = append(os.Environ(), "COMPILEDB_TEST_MAKE_PROXY_OUTPUT="+outputFile)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("run Make proxy failed: %v: %s", err, output)
	}
	contents, err := os.ReadFile(outputFile)
	if err != nil {
		t.Fatalf("read delegated invocation failed: %v", err)
	}
	want := "--print-directory\n" + makeProxyCheckArgument + "\n"
	if string(contents) != want {
		t.Fatalf("proxy check argument was not delegated: want %q, got %q", want, contents)
	}
}

func TestReadMakeProxyMetadataRejectsFIFO(t *testing.T) {
	metadata := filepath.Join(t.TempDir(), makeProxyMetadataName)
	if err := syscall.Mkfifo(metadata, 0o600); err != nil {
		t.Fatalf("create metadata FIFO failed: %v", err)
	}
	if value, ok := readMakeProxyMetadata(metadata); ok {
		t.Fatalf("metadata FIFO was accepted: %q", value)
	}
}

func TestGenerateFromFIFOStopsOnFirstCancellation(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "build.log")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatalf("create FIFO failed: %v", err)
	}
	ctx, cancel := context.WithCancelCause(context.Background())
	tool := newTestTool(t, Config{InputFile: fifo, OutputFile: filepath.Join(t.TempDir(), "compile_commands.json"), NoStrict: true})
	tool.Context = ctx
	done := make(chan struct{})
	go func() {
		tool.Generate()
		close(done)
	}()

	time.Sleep(100 * time.Millisecond)
	cancel(SignalError{ProcessSignal: syscall.SIGTERM})
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("FIFO input did not stop after the first cancellation")
	}
	if tool.StatusCode != 128+int(syscall.SIGTERM) {
		t.Fatalf("unexpected FIFO cancellation status: %d", tool.StatusCode)
	}
}
