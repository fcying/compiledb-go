package internal

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mattn/go-shellwords"
)

func TestParseMergesTrailingContinuation(t *testing.T) {
	tmpDir := t.TempDir()
	outputFile := filepath.Join(tmpDir, "compile_commands.json")

	tool := newTestTool(t, Config{
		InputFile:    "stdin",
		OutputFile:   outputFile,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
		NoStrict:     true,
	})

	tool.Parse([]string{
		"gcc -DMODE=1 \\",
		"-c src/main.c",
	})

	data, err := os.ReadFile(outputFile)
	if err != nil {
		t.Fatalf("read output failed: %v", err)
	}
	got := string(data)
	if got == "[]" || got == "[]\n" {
		t.Fatalf("expected at least one command, got %q", got)
	}
}

func TestParseCommandStyleQuotesArguments(t *testing.T) {
	tmpDir := t.TempDir()
	outputFile := filepath.Join(tmpDir, "compile_commands.json")

	tool := newTestTool(t, Config{
		InputFile:    "stdin",
		OutputFile:   outputFile,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
		NoStrict:     true,
		CommandStyle: true,
	})

	tool.Parse([]string{`clang -DNAME="hello world" -c "src dir/test space.c" -o "obj/test space.o"`})

	data, err := os.ReadFile(outputFile)
	if err != nil {
		t.Fatalf("read output failed: %v", err)
	}

	var commands []Command
	if err := json.Unmarshal(data, &commands); err != nil {
		t.Fatalf("unmarshal output failed: %v", err)
	}

	if len(commands) != 1 {
		t.Fatalf("expected 1 command, got %d", len(commands))
	}

	want := `clang '-DNAME=hello world' -c 'src dir/test space.c' -o 'obj/test space.o'`
	if commands[0].Command != want {
		t.Fatalf("unexpected command output\nwant: %q\ngot:  %q", want, commands[0].Command)
	}
}

func TestParsePreservesQuotedShellSeparators(t *testing.T) {
	outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
	tool := newTestTool(t, Config{
		InputFile:    "stdin",
		OutputFile:   outputFile,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
		NoStrict:     true,
	})

	tool.Parse([]string{`gcc '-DNAME=a;b' '-DALT=x&&y||z' -c semicolon.c`})
	commands := readCompilerTestCommands(t, outputFile)
	if len(commands) != 1 || !slices.Contains(commands[0].Arguments, "-DNAME=a;b") ||
		!slices.Contains(commands[0].Arguments, "-DALT=x&&y||z") {
		t.Fatalf("quoted shell separator was split: %#v", commands)
	}
}

func TestParseRunsBackticksInTrackedWorkingDirectory(t *testing.T) {
	workingDir := t.TempDir()
	outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
	tool := newTestTool(t, Config{
		InputFile:    "stdin",
		OutputFile:   outputFile,
		BuildDir:     workingDir,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
		NoStrict:     true,
	})

	tool.Parse([]string{"gcc -I`pwd` -c main.c"})
	commands := readCompilerTestCommands(t, outputFile)
	want := "-I" + ConvertPath(workingDir)
	if len(commands) != 1 || !slices.Contains(commands[0].Arguments, want) {
		t.Fatalf("backtick command used the wrong working directory: want %q, got %#v", want, commands)
	}
}

func TestParseExpandsBackticksBeforeInlineCD(t *testing.T) {
	projectDir := t.TempDir()
	childDir := filepath.Join(projectDir, "sub")
	if err := os.Mkdir(childDir, 0o755); err != nil {
		t.Fatalf("create child directory failed: %v", err)
	}
	outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
	tool := newTestTool(t, Config{
		OutputFile:   outputFile,
		BuildDir:     projectDir,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
		NoStrict:     true,
	})

	tool.Parse([]string{"cd `printf sub` && gcc -c main.c"})

	commands := readCompilerTestCommands(t, outputFile)
	if len(commands) != 1 || commands[0].Directory != ConvertPath(childDir) {
		t.Fatalf("backtick-driven cd was not tracked: %#v", commands)
	}
}

func TestParseCancelsBacktickCommand(t *testing.T) {
	workingDir := t.TempDir()
	startedFile := filepath.Join(workingDir, "backtick.started")
	outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
	ctx, cancel := context.WithCancel(context.Background())
	tool := newTestTool(t, Config{
		InputFile:    "stdin",
		OutputFile:   outputFile,
		BuildDir:     workingDir,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
		NoStrict:     true,
	})
	tool.Context = ctx
	nested := "printf started > " + ShellJoinArgs([]string{startedFile}) + "; exec sleep 30"
	done := make(chan struct{})
	go func() {
		tool.Parse([]string{"gcc -I`" + nested + "` -c main.c"})
		close(done)
	}()

	waitForTestFile(t, startedFile)
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("backtick command did not return after cancellation")
	}
	if tool.StatusCode == 0 {
		t.Fatal("canceled backtick parse returned a successful status")
	}
	if _, err := os.Stat(outputFile); !os.IsNotExist(err) {
		t.Fatalf("canceled backtick parse wrote a compilation database: %v", err)
	}
}

func TestParseRespectsQuotedCommandSubstitutions(t *testing.T) {
	projectDir := t.TempDir()
	marker := filepath.Join(projectDir, "executed")
	outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
	tool := newTestTool(t, Config{
		InputFile:    "stdin",
		OutputFile:   outputFile,
		BuildDir:     projectDir,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
		NoStrict:     true,
	})

	tool.Parse([]string{
		"gcc '-DVALUE=`touch " + marker + "`' -c literal.c",
		`gcc "-I$(pwd)" -c unsupported.c`,
		`gcc '-DVALUE=$(pwd)' -c single-quoted.c`,
	})
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("single-quoted backtick was executed: %v", err)
	}
	commands := readCompilerTestCommands(t, outputFile)
	if len(commands) != 2 || commands[0].File != "literal.c" || commands[1].File != "single-quoted.c" {
		t.Fatalf("quoted substitutions were handled incorrectly: %#v", commands)
	}
}

func TestParseDoesNotReevaluateBacktickOutput(t *testing.T) {
	projectDir := t.TempDir()
	marker := filepath.Join(projectDir, "executed-twice")
	outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
	tool := newTestTool(t, Config{
		InputFile:    "stdin",
		OutputFile:   outputFile,
		BuildDir:     projectDir,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
		NoStrict:     true,
	})

	tool.Parse([]string{`gcc -DVALUE=` + "`printf '\\140touch " + marker + "\\140'`" + ` -c main.c`})
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("backtick output was evaluated as another command: %v", err)
	}
}

func TestParsePreservesBacktickOutputAsData(t *testing.T) {
	projectDir := t.TempDir()
	emitter := filepath.Join(projectDir, "emit")
	if err := os.WriteFile(emitter, []byte("#!/bin/sh\nprintf \"  a'b\\\\c  \"\n"), 0o755); err != nil {
		t.Fatalf("write emitter failed: %v", err)
	}
	outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
	tool := newTestTool(t, Config{
		InputFile:    "stdin",
		OutputFile:   outputFile,
		BuildDir:     projectDir,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
		NoStrict:     true,
	})
	nested := ShellJoinArgs([]string{emitter})

	tool.Parse([]string{
		"gcc -DVALUE=`" + nested + "` -c unquoted.c",
		"gcc \"-DVALUE=`" + nested + "`\" -c quoted.c",
	})

	commands := readCompilerTestCommands(t, outputFile)
	if len(commands) != 2 || !slices.Contains(commands[0].Arguments, `-DVALUE=a'b\c`) ||
		!slices.Contains(commands[1].Arguments, "-DVALUE=  a'b\\c  ") {
		t.Fatalf("backtick output changed shell lexical state: %#v", commands)
	}
}

func TestParsePreservesEscapedQuotes(t *testing.T) {
	outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
	tool := newTestTool(t, Config{
		InputFile:    "stdin",
		OutputFile:   outputFile,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
		NoStrict:     true,
	})
	tool.Parse([]string{`gcc -DSTR=\"quoted\" -c main.c`})

	commands := readCompilerTestCommands(t, outputFile)
	if len(commands) != 1 || !slices.Contains(commands[0].Arguments, `-DSTR="quoted"`) {
		t.Fatalf("escaped quotes were changed: %#v", commands)
	}
}

func TestParsePreservesDoubleQuotedBackslashes(t *testing.T) {
	outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
	tool := newTestTool(t, Config{
		InputFile:    "stdin",
		OutputFile:   outputFile,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
		NoStrict:     true,
	})
	tool.Parse([]string{`gcc "-DREGEX=\d+" "-DQUOTE=\"value\"" -c main.c`})

	commands := readCompilerTestCommands(t, outputFile)
	if len(commands) != 1 || !slices.Contains(commands[0].Arguments, `-DREGEX=\d+`) ||
		!slices.Contains(commands[0].Arguments, `-DQUOTE="value"`) {
		t.Fatalf("double-quoted backslashes were changed: %#v", commands)
	}
}

func TestParseHonorsKnownConditionalBranches(t *testing.T) {
	projectDir := t.TempDir()
	subDir := filepath.Join(projectDir, "sub")
	if err := os.Mkdir(subDir, 0o755); err != nil {
		t.Fatalf("create subdirectory failed: %v", err)
	}
	outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
	tool := newTestTool(t, Config{
		InputFile:    "stdin",
		OutputFile:   outputFile,
		BuildDir:     projectDir,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
		NoStrict:     true,
	})

	tool.Parse([]string{
		"false && gcc -c false-and.c",
		"true || gcc -c true-or.c",
		"cd sub || gcc -c successful-cd-fallback.c",
		"cd missing || gcc -c failed-cd-fallback.c",
		"true && gcc -c true-and.c",
	})
	commands := readCompilerTestCommands(t, outputFile)
	if len(commands) != 2 || commands[0].File != "failed-cd-fallback.c" ||
		commands[0].Directory != ConvertPath(projectDir) || commands[1].File != "true-and.c" {
		t.Fatalf("conditional branches were parsed incorrectly: %#v", commands)
	}
}

func TestParseBuildLogFixture(t *testing.T) {
	fixture := filepath.Join("..", "tests", "build.log")
	outputFile := filepath.Join(t.TempDir(), "compile_commands.json")

	tool := newTestTool(t, Config{
		InputFile:    fixture,
		OutputFile:   outputFile,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
		NoStrict:     true,
	})
	tool.Generate()

	data, err := os.ReadFile(outputFile)
	if err != nil {
		t.Fatalf("read output failed: %v", err)
	}

	var commands []Command
	if err := json.Unmarshal(data, &commands); err != nil {
		t.Fatalf("unmarshal output failed: %v", err)
	}

	if len(commands) != 9 {
		t.Fatalf("expected 9 unique commands, got %d", len(commands))
	}

	type expectedCommand struct {
		directory string
		file      string
	}

	checks := []expectedCommand{
		{directory: "/opt/compiledb_test", file: "src/test1.c"},
		{directory: "/opt/compiledb_test/src", file: "src dir/test space.c"},
		{directory: "/opt/compiledb_test/sub", file: "nested/sub_file.c"},
		{directory: "/opt/compiledb_test/build", file: "../quoted-name.c"},
		{directory: "/opt/compiledb_test/relative-build", file: "src3.cc"},
	}

	passed := 0
	failed := []string{}

	for _, check := range checks {
		found := false
		for _, cmd := range commands {
			if cmd.Directory == check.directory && cmd.File == check.file {
				found = true
				break
			}
		}

		if found {
			passed++
			continue
		}

		failed = append(failed, fmt.Sprintf("directory=%s file=%s", check.directory, check.file))
	}

	t.Logf("fixture checks: passed=%d failed=%d total=%d", passed, len(failed), len(checks))
	if len(failed) > 0 {
		t.Fatalf("fixture checks: passed=%d failed=%d total=%d\nfailed entries:\n%s", passed, len(failed), len(checks), strings.Join(failed, "\n"))
	}
}

func TestWriteJSONWritesEmptyArrayForZeroCommands(t *testing.T) {
	tmpDir := t.TempDir()
	outputFile := filepath.Join(tmpDir, "compile_commands.json")

	if err := os.WriteFile(outputFile, []byte("stale"), 0o644); err != nil {
		t.Fatalf("seed output failed: %v", err)
	}

	tool := newTestTool(t, Config{OutputFile: outputFile})
	commands := []Command{}
	tool.WriteJSON(outputFile, 0, &commands)

	data, err := os.ReadFile(outputFile)
	if err != nil {
		t.Fatalf("read output failed: %v", err)
	}
	if string(data) != "[]" {
		t.Fatalf("expected empty JSON array, got %q", string(data))
	}
}

func TestParseWritesEmptyArrayWhenNoCommandsFound(t *testing.T) {
	tmpDir := t.TempDir()
	outputFile := filepath.Join(tmpDir, "compile_commands.json")

	tool := newTestTool(t, Config{
		InputFile:    "stdin",
		OutputFile:   outputFile,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
		NoStrict:     true,
	})

	tool.Parse([]string{"echo not-a-compile-command"})

	data, err := os.ReadFile(outputFile)
	if err != nil {
		t.Fatalf("read output failed: %v", err)
	}
	if string(data) != "[]" {
		t.Fatalf("expected empty JSON array, got %q", string(data))
	}
}

func TestParseIgnoresCompilerLineWithoutSourceFile(t *testing.T) {
	tmpDir := t.TempDir()
	outputFile := filepath.Join(tmpDir, "compile_commands.json")

	tool := newTestTool(t, Config{
		InputFile:    "stdin",
		OutputFile:   outputFile,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
		NoStrict:     true,
	})

	tool.Parse([]string{"gcc -v"})

	data, err := os.ReadFile(outputFile)
	if err != nil {
		t.Fatalf("read output failed: %v", err)
	}
	if string(data) != "[]" {
		t.Fatalf("expected empty JSON array, got %q", string(data))
	}
}

func TestParseGeneratesEntriesForSourceFilesWithoutCompileOnlyFlag(t *testing.T) {
	tmpDir := t.TempDir()
	outputFile := filepath.Join(tmpDir, "compile_commands.json")

	tool := newTestTool(t, Config{
		InputFile:    "stdin",
		OutputFile:   outputFile,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
		NoStrict:     true,
	})

	tool.Parse([]string{"cc -Iinc -o app a.c b.c -lm"})

	commands := readCompilerTestCommands(t, outputFile)
	if len(commands) != 2 || commands[0].File != "a.c" || commands[1].File != "b.c" {
		t.Fatalf("expected one entry per source, got %#v", commands)
	}
}

func TestParseGeneratesEntryWhenCompileOnlyFlagIsAnOptionOperand(t *testing.T) {
	outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
	tool := newTestTool(t, Config{
		InputFile:    "stdin",
		OutputFile:   outputFile,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
		NoStrict:     true,
	})

	tool.Parse([]string{"gcc -u -c main.c"})

	commands := readCompilerTestCommands(t, outputFile)
	if len(commands) != 1 || commands[0].File != "main.c" {
		t.Fatalf("expected source command entry, got %#v", commands)
	}
}

func TestParseIgnoresLinkOnlyOrUnrelatedOutput(t *testing.T) {
	tmpDir := t.TempDir()
	outputFile := filepath.Join(tmpDir, "compile_commands.json")

	tool := newTestTool(t, Config{
		InputFile:    "stdin",
		OutputFile:   outputFile,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
		NoStrict:     true,
	})

	tool.Parse([]string{
		"cc -o app a.o b.o -lm",
		"cc -c a.c -o a.o",
		"echo cc -c fake.c",
		"echo cc -o app a.c b.c",
	})

	commands := readCompilerTestCommands(t, outputFile)
	if len(commands) != 1 || commands[0].File != "a.c" {
		t.Fatalf("unrelated output produced compilation entries: %#v", commands)
	}
}

func TestParseAppendsRepeatedAddArgs(t *testing.T) {
	tmpDir := t.TempDir()
	outputFile := filepath.Join(tmpDir, "compile_commands.json")

	tool := newTestTool(t, Config{
		InputFile:    "stdin",
		OutputFile:   outputFile,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
		NoStrict:     true,
		AddArgs:      []string{"-DMODE=1", `-DNAME="hello world"`, "-UDEBUG"},
	})

	tool.Parse([]string{"gcc -c src/main.c"})

	data, err := os.ReadFile(outputFile)
	if err != nil {
		t.Fatalf("read output failed: %v", err)
	}

	var commands []Command
	if err := json.Unmarshal(data, &commands); err != nil {
		t.Fatalf("unmarshal output failed: %v", err)
	}
	if len(commands) != 1 {
		t.Fatalf("expected 1 command, got %d", len(commands))
	}

	got := commands[0].Arguments
	wantSuffix := []string{"-DMODE=1", `-DNAME="hello world"`, "-UDEBUG"}
	if len(got) < len(wantSuffix) {
		t.Fatalf("arguments too short: %v", got)
	}

	suffix := got[len(got)-len(wantSuffix):]
	for i := range wantSuffix {
		if suffix[i] != wantSuffix[i] {
			t.Fatalf("unexpected macro suffix\nwant: %v\ngot:  %v", wantSuffix, suffix)
		}
	}
}

func TestParseCommandStylePreservesAddedArgument(t *testing.T) {
	outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
	tool := newTestTool(t, Config{
		InputFile:    "stdin",
		OutputFile:   outputFile,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
		NoStrict:     true,
		CommandStyle: true,
		AddArgs:      []string{`-DNAME="hello world"`, `-DREGEX=\d+`},
	})

	tool.Parse([]string{"gcc -c src/main.c"})
	data, err := os.ReadFile(outputFile)
	if err != nil {
		t.Fatalf("read output failed: %v", err)
	}
	var commands []Command
	if err := json.Unmarshal(data, &commands); err != nil {
		t.Fatalf("unmarshal output failed: %v", err)
	}
	if len(commands) != 1 {
		t.Fatalf("expected one command, got %#v", commands)
	}

	toolArgs, err := shellwords.Parse(commands[0].Command)
	if err != nil {
		t.Fatalf("parse command output failed: %v", err)
	}
	wantSuffix := []string{`-DNAME="hello world"`, `-DREGEX=\d+`}
	if len(toolArgs) < len(wantSuffix) || !slices.Equal(toolArgs[len(toolArgs)-len(wantSuffix):], wantSuffix) {
		t.Fatalf("command does not round-trip added arguments: %v", toolArgs)
	}
}

func TestParseAddArgDoesNotSplitComma(t *testing.T) {
	tmpDir := t.TempDir()
	outputFile := filepath.Join(tmpDir, "compile_commands.json")

	tool := newTestTool(t, Config{
		InputFile:    "stdin",
		OutputFile:   outputFile,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
		NoStrict:     true,
		AddArgs:      []string{`-DTEST_BOARD,-m32`},
	})

	tool.Parse([]string{"gcc -c src/main.c"})

	data, err := os.ReadFile(outputFile)
	if err != nil {
		t.Fatalf("read output failed: %v", err)
	}

	var commands []Command
	if err := json.Unmarshal(data, &commands); err != nil {
		t.Fatalf("unmarshal output failed: %v", err)
	}
	if len(commands) != 1 {
		t.Fatalf("expected 1 command, got %d", len(commands))
	}

	got := commands[0].Arguments
	wantSuffix := []string{"-DTEST_BOARD,-m32"}
	if len(got) < len(wantSuffix) {
		t.Fatalf("arguments too short: %v", got)
	}

	suffix := got[len(got)-len(wantSuffix):]
	for i := range wantSuffix {
		if suffix[i] != wantSuffix[i] {
			t.Fatalf("unexpected macro suffix\nwant: %v\ngot:  %v", wantSuffix, suffix)
		}
	}
}

func TestParseNormalizesTargetArgumentForClangd(t *testing.T) {
	tmpDir := t.TempDir()
	outputFile := filepath.Join(tmpDir, "compile_commands.json")

	tool := newTestTool(t, Config{
		InputFile:    "stdin",
		OutputFile:   outputFile,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
		NoStrict:     true,
	})

	tool.Parse([]string{`clang -target pi32v2 -c "test2.c"`})

	data, err := os.ReadFile(outputFile)
	if err != nil {
		t.Fatalf("read output failed: %v", err)
	}

	var commands []Command
	if err := json.Unmarshal(data, &commands); err != nil {
		t.Fatalf("unmarshal output failed: %v", err)
	}
	if len(commands) != 1 {
		t.Fatalf("expected 1 command, got %d", len(commands))
	}

	if len(commands[0].Arguments) < 4 {
		t.Fatalf("unexpected arguments: %v", commands[0].Arguments)
	}

	want := []string{"clang", "--target=pi32v2", "-c", "test2.c"}
	got := commands[0].Arguments[:4]
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("unexpected target normalization\nwant: %v\ngot:  %v", want, got)
		}
	}
}

func TestParseDoesNotNormalizeAddedTargetArguments(t *testing.T) {
	outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
	tool := newTestTool(t, Config{
		InputFile:    "stdin",
		OutputFile:   outputFile,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
		NoStrict:     true,
		AddArgs:      []string{"-target", "pi32v2"},
	})

	tool.Parse([]string{"clang -c test.c"})
	var commands []Command
	data, err := os.ReadFile(outputFile)
	if err != nil {
		t.Fatalf("read output failed: %v", err)
	}
	if err := json.Unmarshal(data, &commands); err != nil {
		t.Fatalf("unmarshal output failed: %v", err)
	}
	wantSuffix := []string{"-target", "pi32v2"}
	if len(commands) != 1 || !slices.Equal(commands[0].Arguments[len(commands[0].Arguments)-2:], wantSuffix) {
		t.Fatalf("added arguments were normalized: %#v", commands)
	}
}

func TestParseInsertsAddedArgumentsBeforeCompilerOptionTerminator(t *testing.T) {
	for name, command := range map[string]string{
		"compiler":         "clang -c -- test.c",
		"wrapped compiler": "ccache clang -c -- test.c",
	} {
		t.Run(name, func(t *testing.T) {
			outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
			tool := newTestTool(t, Config{
				InputFile:    "stdin",
				OutputFile:   outputFile,
				RegexCompile: RegexCompile,
				RegexFile:    RegexFile,
				NoStrict:     true,
				AddArgs:      []string{"-DADDED=1"},
			})
			tool.Parse([]string{command})

			var commands []Command
			data, err := os.ReadFile(outputFile)
			if err != nil {
				t.Fatalf("read output failed: %v", err)
			}
			if err := json.Unmarshal(data, &commands); err != nil {
				t.Fatalf("unmarshal output failed: %v", err)
			}
			if len(commands) != 1 {
				t.Fatalf("expected one command, got %#v", commands)
			}
			compilerIndex := compilerArgumentIndex(commands[0].Arguments)
			terminator := slices.Index(commands[0].Arguments[compilerIndex+1:], "--") + compilerIndex + 1
			added := slices.Index(commands[0].Arguments, "-DADDED=1")
			if added < compilerIndex || terminator <= added {
				t.Fatalf("added argument must precede compiler terminator: %v", commands[0].Arguments)
			}
		})
	}
}

func TestParseDoesNotTreatOptionOperandAsTerminator(t *testing.T) {
	for name, command := range map[string]string{
		"output":     "gcc -c test.c -o --",
		"dependency": "gcc -c test.c -MF --",
	} {
		t.Run(name, func(t *testing.T) {
			outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
			tool := newTestTool(t, Config{
				InputFile:    "stdin",
				OutputFile:   outputFile,
				RegexCompile: RegexCompile,
				RegexFile:    RegexFile,
				NoStrict:     true,
				AddArgs:      []string{"-DADDED=1"},
			})
			tool.Parse([]string{command})

			commands := readCompilerTestCommands(t, outputFile)
			if len(commands) != 1 || commands[0].Arguments[len(commands[0].Arguments)-2] != "--" ||
				commands[0].Arguments[len(commands[0].Arguments)-1] != "-DADDED=1" {
				t.Fatalf("option operand was treated as a terminator: %#v", commands)
			}
		})
	}
}

func TestParseSupportsImplicitDistributedCompiler(t *testing.T) {
	for _, wrapper := range []string{"distcc", "icecc"} {
		t.Run(wrapper, func(t *testing.T) {
			outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
			tool := newTestTool(t, Config{
				InputFile:    "stdin",
				OutputFile:   outputFile,
				RegexCompile: RegexCompile,
				RegexFile:    RegexFile,
				NoStrict:     true,
				Macros:       true,
				FullPath:     true,
			})
			var compilerName string
			tool.compilerCommand = func(name string, _ ...string) *exec.Cmd {
				compilerName = name
				cmd := exec.Command(os.Args[0], "-test.run=^TestCompilerMacrosHelperProcess$")
				cmd.Env = append(os.Environ(), "COMPILEDB_TEST_COMPILER_HELPER=success")
				return cmd
			}
			tool.Parse([]string{wrapper + " -c test.c"})

			commands := readCompilerTestCommands(t, outputFile)
			if wrapper == "icecc" {
				if len(commands) != 1 || compilerName != "" || !slices.Equal(commands[0].Arguments, []string{"icecc", "-c", "test.c"}) {
					t.Fatalf("icecc macro probing was not disabled safely: compiler=%q commands=%#v", compilerName, commands)
				}
				return
			}
			if len(commands) != 1 || executableBase(compilerName) != "cc" ||
				!slices.Equal(commands[0].Arguments[:2], []string{wrapper, "-DORIGINAL=0"}) {
				t.Fatalf("implicit compiler was not handled correctly: compiler=%q commands=%#v", compilerName, commands)
			}
		})
	}
}

func TestParseSelectsSourceInsteadOfOptionOperand(t *testing.T) {
	for name, command := range map[string]string{
		"include":           "gcc -x c++ -c main.c -include config.c",
		"long include":      "gcc -c main.cpp --include config.c",
		"output":            "gcc -c main.cpp -o output.c",
		"long output":       "gcc -c main.cpp --output output.c",
		"dependency":        "gcc -c main.cpp -MF deps.c",
		"clang dependency":  "clang -c main.cpp -dependency-file deps.c",
		"VFS overlay":       "clang -c main.cpp -ivfsoverlay overlay.c",
		"working directory": "clang -c main.cpp -working-directory build.c",
		"diagnostics":       "clang -c main.cpp -serialize-diagnostics diagnostics.c",
		"linker script":     "gcc -c main.cpp -T script.c",
		"framework":         "clang -c main.cpp -F frameworks.c",
		"linker option":     "gcc -z defs.c -c main.cpp",
		"assertion":         "gcc -A predicate.c -c main.cpp",
		"multilib":          "gcc -imultilib multilib.c -c main.cpp",
		"multiarch":         "gcc -imultiarch multiarch.c -c main.cpp",
		"index store":       "clang -index-store-path index.c -c main.cpp",
		"debug directory":   "clang -fdebug-compilation-dir debug.c -c main.cpp",
		"API notes modules": "clang -iapinotes-modules notes.c -c main.cpp",
	} {
		t.Run(name, func(t *testing.T) {
			outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
			tool := newTestTool(t, Config{
				InputFile:    "stdin",
				OutputFile:   outputFile,
				RegexCompile: RegexCompile,
				RegexFile:    RegexFile,
				NoStrict:     true,
			})
			tool.Parse([]string{command})

			commands := readCompilerTestCommands(t, outputFile)
			if len(commands) != 1 || commands[0].File != map[string]string{
				"include": "main.c", "long include": "main.cpp", "output": "main.cpp", "long output": "main.cpp",
				"dependency": "main.cpp", "clang dependency": "main.cpp", "VFS overlay": "main.cpp",
				"working directory": "main.cpp", "diagnostics": "main.cpp", "linker script": "main.cpp", "framework": "main.cpp",
				"linker option": "main.cpp", "assertion": "main.cpp", "multilib": "main.cpp", "multiarch": "main.cpp", "index store": "main.cpp",
				"debug directory": "main.cpp", "API notes modules": "main.cpp",
			}[name] {
				t.Fatalf("option operand was selected as source: %#v", commands)
			}
		})
	}
}

func TestParseDefaultSourceScanner(t *testing.T) {
	outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
	tool := newTestTool(t, Config{
		InputFile:    "stdin",
		OutputFile:   outputFile,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
		NoStrict:     true,
	})

	tool.Parse([]string{
		"gcc before.c -c",
		"gcc -c upper.C",
		"gcc -c startup.S",
		"gcc -c one.c two.cpp",
		"gcc -x c -c extensionless",
	})
	commands := readCompilerTestCommands(t, outputFile)
	files := make([]string, 0, len(commands))
	for _, command := range commands {
		files = append(files, command.File)
	}
	want := []string{"before.c", "upper.C", "startup.S", "one.c", "two.cpp", "extensionless"}
	if !slices.Equal(files, want) {
		t.Fatalf("unexpected source entries:\nwant: %v\ngot:  %v", want, files)
	}
}

func TestParsePreservesOriginalBackslashes(t *testing.T) {
	outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
	tool := newTestTool(t, Config{
		InputFile:    "stdin",
		OutputFile:   outputFile,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
		NoStrict:     true,
	})
	tool.Parse([]string{`gcc '-DREGEX=\d+' -c main.c`})

	commands := readCompilerTestCommands(t, outputFile)
	if len(commands) != 1 || !slices.Contains(commands[0].Arguments, `-DREGEX=\d+`) {
		t.Fatalf("non-path backslashes were changed: %#v", commands)
	}
}

func TestNormalizeCompilerArgsRespectsOptionOperands(t *testing.T) {
	for _, arguments := range [][]string{
		{"clang", "-c", "-o", "-target", "main.c"},
		{"clang", "-c", "-MF", "-target", "main.c"},
		{"clang", "-c", "--", "-target", "main.c"},
	} {
		if got := normalizeCompilerArgs(arguments); !slices.Equal(got, arguments) {
			t.Fatalf("option operand was normalized:\nwant: %v\ngot:  %v", arguments, got)
		}
	}
	want := []string{"clang", "--target=triple", "-c", "main.c"}
	if got := normalizeCompilerArgs([]string{"clang", "-target", "triple", "-c", "main.c"}); !slices.Equal(got, want) {
		t.Fatalf("real target option was not normalized: want %v, got %v", want, got)
	}
}

func TestParseStrictRejectsSourceDirectory(t *testing.T) {
	projectDir := t.TempDir()
	if err := os.Mkdir(filepath.Join(projectDir, "include.c"), 0o755); err != nil {
		t.Fatalf("create source-like directory failed: %v", err)
	}
	outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
	tool := newTestTool(t, Config{
		InputFile:    "stdin",
		OutputFile:   outputFile,
		BuildDir:     projectDir,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
	})
	tool.Parse([]string{"gcc -c include.c"})
	if commands := readCompilerTestCommands(t, outputFile); len(commands) != 0 {
		t.Fatalf("source directory was accepted: %#v", commands)
	}
}

func TestParseUsesCompilerWorkingDirectory(t *testing.T) {
	projectDir := t.TempDir()
	subDir := filepath.Join(projectDir, "sub")
	if err := os.Mkdir(subDir, 0o755); err != nil {
		t.Fatalf("create working directory failed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(subDir, "main.c"), []byte("int value;\n"), 0o644); err != nil {
		t.Fatalf("write source failed: %v", err)
	}
	outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
	tool := newTestTool(t, Config{
		InputFile:    "stdin",
		OutputFile:   outputFile,
		BuildDir:     projectDir,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
	})

	tool.Parse([]string{"clang -working-directory sub -c main.c"})
	commands := readCompilerTestCommands(t, outputFile)
	if len(commands) != 1 || commands[0].Directory != ConvertPath(subDir) ||
		!slices.Contains(commands[0].Arguments, ConvertPath(subDir)) {
		t.Fatalf("compiler working directory was not applied: %#v", commands)
	}
}

func TestParseFiltersSourcesBeforeMacroProbe(t *testing.T) {
	for name, config := range map[string]Config{
		"excluded": {NoStrict: true, Exclude: `excluded[.]c`},
		"missing":  {},
	} {
		t.Run(name, func(t *testing.T) {
			outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
			config.InputFile = "stdin"
			config.OutputFile = outputFile
			config.BuildDir = t.TempDir()
			config.RegexCompile = RegexCompile
			config.RegexFile = RegexFile
			config.Macros = true
			tool := newTestTool(t, config)
			calls := 0
			tool.compilerCommand = func(_ string, _ ...string) *exec.Cmd {
				calls++
				return exec.Command(os.Args[0])
			}
			file := "missing.c"
			if name == "excluded" {
				file = "excluded.c"
			}
			tool.Parse([]string{"gcc -c " + file})
			if calls != 0 {
				t.Fatalf("filtered source started macro probe %d times", calls)
			}
		})
	}
}

func TestParseSupportsKnownCompilerLaunchers(t *testing.T) {
	outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
	tool := newTestTool(t, Config{
		InputFile:    "stdin",
		OutputFile:   outputFile,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
		NoStrict:     true,
	})
	tool.Parse([]string{
		"env BUILD_MODE=release gcc -c env.c",
		"xcrun --sdk macosx clang -c xcrun.c",
		"xcrun --run clang -c xcrun-run.c",
		"xcrun -r clang -c xcrun-short-run.c",
		"./libtool --mode=compile gcc -c libtool.c",
		"/bin/sh ./libtool --tag=CC --mode=compile gcc -c shell-libtool.c",
		"libtool: compile: gcc -c logged-libtool.c",
		"echo env gcc -c fake.c",
	})
	commands := readCompilerTestCommands(t, outputFile)
	want := []string{"env.c", "xcrun.c", "xcrun-run.c", "xcrun-short-run.c", "libtool.c", "shell-libtool.c", "logged-libtool.c"}
	files := make([]string, 0, len(commands))
	for _, command := range commands {
		files = append(files, command.File)
	}
	if !slices.Equal(files, want) {
		t.Fatalf("known compiler launchers were not handled safely: %#v", commands)
	}
}

func TestParseFullPathBehindSafeEnvironmentLauncher(t *testing.T) {
	compilerDir := t.TempDir()
	compiler := filepath.Join(compilerDir, "fake-gcc")
	if err := os.WriteFile(compiler, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write fake compiler failed: %v", err)
	}
	t.Setenv("PATH", compilerDir)
	outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
	tool := newTestTool(t, Config{
		InputFile:    "stdin",
		OutputFile:   outputFile,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
		NoStrict:     true,
		FullPath:     true,
	})

	tool.Parse([]string{"env BUILD_MODE=release fake-gcc -c env.c"})
	commands := readCompilerTestCommands(t, outputFile)
	if len(commands) != 1 || commands[0].Arguments[0] != ConvertPath(compiler) {
		t.Fatalf("full path was not resolved behind a safe env launcher: %#v", commands)
	}
}

func TestParseAppliesEnvironmentLauncherDirectory(t *testing.T) {
	for name, option := range map[string]string{
		"short": "-C sub",
		"long":  "--chdir=sub",
	} {
		t.Run(name, func(t *testing.T) {
			projectDir := t.TempDir()
			subDir := filepath.Join(projectDir, "sub")
			if err := os.Mkdir(subDir, 0o755); err != nil {
				t.Fatalf("create subdirectory failed: %v", err)
			}
			if err := os.WriteFile(filepath.Join(subDir, "main.c"), []byte("int value;\n"), 0o644); err != nil {
				t.Fatalf("write source failed: %v", err)
			}
			compiler := filepath.Join(subDir, "fake-gcc")
			if err := os.WriteFile(compiler, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
				t.Fatalf("write compiler failed: %v", err)
			}
			outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
			tool := newTestTool(t, Config{
				InputFile:    "stdin",
				OutputFile:   outputFile,
				BuildDir:     projectDir,
				RegexCompile: RegexCompile,
				RegexFile:    RegexFile,
				FullPath:     true,
			})

			tool.Parse([]string{"env " + option + " ./fake-gcc -c main.c"})

			commands := readCompilerTestCommands(t, outputFile)
			if len(commands) != 1 || commands[0].Directory != ConvertPath(subDir) ||
				commands[0].Arguments[0] != ConvertPath(compiler) {
				t.Fatalf("env chdir was not applied to compiler command: %#v", commands)
			}
		})
	}
}

func TestParseKeepsPOSIXEscapedRelativeSource(t *testing.T) {
	projectDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(projectDir, "srcmain.c"), []byte("int value;\n"), 0o644); err != nil {
		t.Fatalf("write source failed: %v", err)
	}
	outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
	tool := newTestTool(t, Config{
		InputFile:    "stdin",
		OutputFile:   outputFile,
		BuildDir:     projectDir,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
	})
	tool.Parse([]string{`gcc -I a:b -c src\main.c`})
	commands := readCompilerTestCommands(t, outputFile)
	if len(commands) != 1 || commands[0].File != "srcmain.c" {
		t.Fatalf("POSIX escaped source was treated as a Windows path: %#v", commands)
	}
}

func TestParseStrictPreservesPOSIXColonAndBackslashPaths(t *testing.T) {
	projectDir := t.TempDir()
	colonDir := filepath.Join(projectDir, "1:a")
	if err := os.Mkdir(colonDir, 0o755); err != nil {
		t.Fatalf("create colon directory failed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(colonDir, "main.c"), nil, 0o644); err != nil {
		t.Fatalf("create colon source failed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, `src\main.c`), nil, 0o644); err != nil {
		t.Fatalf("create backslash source failed: %v", err)
	}
	outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
	tool := newTestTool(t, Config{
		InputFile:    "stdin",
		OutputFile:   outputFile,
		BuildDir:     projectDir,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
	})

	tool.Parse([]string{
		"cd 1:a && gcc -c main.c",
		`gcc -c 'src\main.c'`,
	})

	commands := readCompilerTestCommands(t, outputFile)
	if len(commands) != 2 || commands[0].Directory != ConvertPath(colonDir) || commands[0].File != "main.c" ||
		commands[1].Directory != ConvertPath(projectDir) || commands[1].File != `src\main.c` {
		t.Fatalf("POSIX colon or backslash path was changed: %#v", commands)
	}
}

func TestParseCustomFileRegexRemainsAuthoritative(t *testing.T) {
	outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
	tool := newTestTool(t, Config{
		InputFile:    "stdin",
		OutputFile:   outputFile,
		RegexCompile: `mycc`,
		RegexFile:    `--src=([^[:space:]]+[.]foo)`,
		NoStrict:     true,
	})

	tool.Parse([]string{"mycc --src=main.foo"})
	commands := readCompilerTestCommands(t, outputFile)
	if len(commands) != 1 || commands[0].File != "main.foo" ||
		!slices.Equal(commands[0].Arguments, []string{"mycc", "--src=main.foo"}) {
		t.Fatalf("custom file regex capture was overwritten: %#v", commands)
	}
}

func TestParseMultipleSourcesSkipsMacroProbe(t *testing.T) {
	outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
	tool := newTestTool(t, Config{
		InputFile:    "stdin",
		OutputFile:   outputFile,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
		NoStrict:     true,
		Macros:       true,
	})
	var logs bytes.Buffer
	tool.Logger.SetOutput(&logs)
	calls := 0
	tool.compilerCommand = func(_ string, _ ...string) *exec.Cmd {
		calls++
		return exec.Command(os.Args[0])
	}

	tool.Parse([]string{"gcc -c one.c two.cpp"})
	commands := readCompilerTestCommands(t, outputFile)
	if len(commands) != 2 || commands[0].File != "one.c" || commands[1].File != "two.cpp" {
		t.Fatalf("expected one entry per source, got %#v", commands)
	}
	if calls != 0 || !strings.Contains(logs.String(), "multiple source inputs are not supported") {
		t.Fatalf("multiple sources must skip macro probing: calls=%d logs=%q", calls, logs.String())
	}
}

func TestParsePreservesWindowsSourcePaths(t *testing.T) {
	for name, test := range map[string]struct {
		command      string
		want         string
		wantArgument string
	}{
		"drive":  {command: `gcc -c C:\work\main.c`, want: "C:/work/main.c"},
		"quoted": {command: `gcc -c "C:\work\main.c"`, want: "C:/work/main.c"},
		"quoted space": {
			command: `gcc -c "C:\work dir\main.c" --include config.c`,
			want:    "C:/work dir/main.c",
		},
		"relative": {command: `C:\toolchain\gcc.exe -c src\main.c`, want: "src/main.c"},
		"attached absolute include establishes context": {
			command: `gcc -IC:\sdk -c src\main.c`,
			want:    "src/main.c",
		},
		"attached absolute sysroot establishes context": {
			command: `gcc --sysroot=C:/sdk -c src\main.c`,
			want:    "src/main.c",
		},
		"attached backslash UNC include establishes context": {
			command:      `gcc -I\\server\share -c src\main.c`,
			want:         "src/main.c",
			wantArgument: "-I//server/share",
		},
		"attached slash UNC sysroot establishes context": {
			command:      `gcc --sysroot=//server/share -c src\main.c`,
			want:         "src/main.c",
			wantArgument: "--sysroot=//server/share",
		},
		"UNC": {command: `gcc -c \\server\share\main.c`, want: "//server/share/main.c"},
		"followed by include": {
			command: `gcc -c C:\work\main.c --include config.c`,
			want:    "C:/work/main.c",
		},
	} {
		t.Run(name, func(t *testing.T) {
			outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
			tool := newTestTool(t, Config{
				InputFile:    "stdin",
				OutputFile:   outputFile,
				RegexCompile: RegexCompile,
				RegexFile:    RegexFile,
				NoStrict:     true,
			})
			tool.Parse([]string{test.command})

			commands := readCompilerTestCommands(t, outputFile)
			if len(commands) != 1 || commands[0].File != test.want || !slices.Contains(commands[0].Arguments, test.want) {
				t.Fatalf("Windows source path was not preserved: %#v", commands)
			}
			if test.wantArgument != "" && !slices.Contains(commands[0].Arguments, test.wantArgument) {
				t.Fatalf("Windows option path was not preserved: %#v", commands)
			}
		})
	}
}

func TestParsePreservesWindowsCompilerAndPathArguments(t *testing.T) {
	outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
	tool := newTestTool(t, Config{
		InputFile:    "stdin",
		OutputFile:   outputFile,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
		NoStrict:     true,
	})

	tool.Parse([]string{`C:\toolchain\bin\gcc.exe -IC:\sdk\include -include C:\cfg\config.h -c C:\src\main.c -o C:\obj\main.o`})
	commands := readCompilerTestCommands(t, outputFile)
	want := []string{
		"C:/toolchain/bin/gcc.exe", "-IC:/sdk/include", "-include", "C:/cfg/config.h",
		"-c", "C:/src/main.c", "-o", "C:/obj/main.o",
	}
	if len(commands) != 1 || commands[0].File != "C:/src/main.c" || !slices.Equal(commands[0].Arguments, want) {
		t.Fatalf("Windows command arguments were not preserved:\nwant: %v\ngot:  %#v", want, commands)
	}
}

func TestParseRejectsOutputWithoutSourceInput(t *testing.T) {
	outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
	tool := newTestTool(t, Config{
		InputFile:    "stdin",
		OutputFile:   outputFile,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
		NoStrict:     true,
	})
	tool.Parse([]string{"gcc -c -o output.c"})

	if commands := readCompilerTestCommands(t, outputFile); len(commands) != 0 {
		t.Fatalf("output operand was treated as a source: %#v", commands)
	}
}

func TestMakeCommandDirectory(t *testing.T) {
	for name, test := range map[string]struct {
		line string
		base string
		want string
	}{
		"quoted":            {line: `make -C "sub dir"`, base: "/project", want: "/project/sub dir"},
		"attached":          {line: "gmake -Csub", base: "/project", want: "/project/sub"},
		"long":              {line: "mingw32-make --directory=sub", base: "/project", want: "/project/sub"},
		"multiple":          {line: "make -C one --directory two", base: "/project", want: "/project/one/two"},
		"UNC":               {line: "make -C sub", base: "//server/share/project", want: "//server/share/project/sub"},
		"absolute resets":   {line: "make -C one -C /other", base: "/project", want: "/other"},
		"option terminator": {line: "make -C one -- -C two", base: "/project", want: "/project/one"},
		"Windows drive":     {line: `mingw32-make -C "C:\Program Files\build"`, base: "/project", want: "C:/Program Files/build"},
		"Windows UNC":       {line: `make -C "\\server\share\build"`, base: "/project", want: "//server/share/build"},
		"unquoted UNC":      {line: `make -C \\server\share\build`, base: "/project", want: "//server/share/build"},
		"relative Windows":  {line: `mingw32-make -C sub\dir`, base: "/project", want: "/project/sub/dir"},
		"drive relative":    {line: `mingw32-make -C C:sub\dir`, base: "C:/project", want: "C:/project/sub/dir"},
		"POSIX colon":       {line: `make -C 1:a`, base: "/project", want: "/project/1:a"},
	} {
		t.Run(name, func(t *testing.T) {
			got, ok := makeCommandDirectory(test.line, test.base)
			if !ok || got != test.want {
				t.Fatalf("unexpected make directory: want %q, got %q, ok=%v", test.want, got, ok)
			}
		})
	}
}

func TestParseDoesNotApplyMakeDirectoryToSiblingCommand(t *testing.T) {
	for _, separator := range []string{" && ", "; ", " || "} {
		t.Run(strings.TrimSpace(separator), func(t *testing.T) {
			projectDir := t.TempDir()
			outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
			tool := newTestTool(t, Config{
				InputFile:    "stdin",
				OutputFile:   outputFile,
				BuildDir:     projectDir,
				RegexCompile: RegexCompile,
				RegexFile:    RegexFile,
				NoStrict:     true,
			})
			tool.Parse([]string{
				"make -C sub" + separator + "gcc -c parent.c",
				"gcc -c child.c",
			})

			commands := readCompilerTestCommands(t, outputFile)
			if separator == "; " {
				if len(commands) != 2 || commands[0].Directory != ConvertPath(projectDir) ||
					commands[1].Directory != ConvertPath(filepath.Join(projectDir, "sub")) {
					t.Fatalf("make directory leaked into sibling command: %#v", commands)
				}
			} else if len(commands) != 1 || commands[0].Directory != ConvertPath(projectDir) || commands[0].File != "child.c" {
				t.Fatalf("unknown conditional branch produced an entry or directory frame: %#v", commands)
			}
		})
	}
}

func TestParseTracksMakeDirectoryAfterInlineCD(t *testing.T) {
	projectDir := t.TempDir()
	if err := os.Mkdir(filepath.Join(projectDir, "sub"), 0o755); err != nil {
		t.Fatalf("create inline cd directory failed: %v", err)
	}
	outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
	tool := newTestTool(t, Config{
		InputFile:    "stdin",
		OutputFile:   outputFile,
		BuildDir:     projectDir,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
		NoStrict:     true,
	})

	tool.Parse([]string{"cd sub && make -C child", "gcc -c nested.c"})
	commands := readCompilerTestCommands(t, outputFile)
	wantDir := ConvertPath(filepath.Join(projectDir, "sub", "child"))
	if len(commands) != 1 || commands[0].Directory != wantDir {
		t.Fatalf("make directory did not use the preceding cd: %#v", commands)
	}
}

func TestParseDoesNotTrackConditionallySkippedMake(t *testing.T) {
	projectDir := t.TempDir()
	outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
	tool := newTestTool(t, Config{
		InputFile:    "stdin",
		OutputFile:   outputFile,
		BuildDir:     projectDir,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
		NoStrict:     true,
	})

	tool.Parse([]string{"false && make -C sub", "gcc -c parent.c"})
	commands := readCompilerTestCommands(t, outputFile)
	if len(commands) != 1 || commands[0].Directory != ConvertPath(projectDir) {
		t.Fatalf("conditionally skipped make changed directory: %#v", commands)
	}
}

func TestParseMatchesMakeLeaveQuotesAndProtectsBaseFrame(t *testing.T) {
	projectDir := t.TempDir()
	childDir := filepath.Join(projectDir, "sub")
	outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
	tool := newTestTool(t, Config{
		InputFile:    "stdin",
		OutputFile:   outputFile,
		BuildDir:     projectDir,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
		NoStrict:     true,
	})

	tool.Parse([]string{
		`make: Leaving directory "` + projectDir + `"`,
		`make: Entering directory "` + childDir + `"`,
		"gcc -c child.c",
		`make: Leaving directory "` + childDir + `"`,
		"gcc -c parent.c",
	})
	commands := readCompilerTestCommands(t, outputFile)
	if len(commands) != 2 || commands[0].Directory != ConvertPath(childDir) ||
		commands[1].Directory != ConvertPath(projectDir) {
		t.Fatalf("make leave handling corrupted the directory stack: %#v", commands)
	}
}

func TestParseSupportsLegacyMakeDirectoryQuotes(t *testing.T) {
	projectDir := t.TempDir()
	childDir := filepath.Join(projectDir, "sub")
	outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
	tool := newTestTool(t, Config{
		InputFile:    "stdin",
		OutputFile:   outputFile,
		BuildDir:     projectDir,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
		NoStrict:     true,
	})
	tool.Parse([]string{
		"make: Entering directory `" + childDir + "'",
		"gcc -c child.c",
		"make: Leaving directory `" + childDir + "'",
		"gcc -c parent.c",
	})
	commands := readCompilerTestCommands(t, outputFile)
	if len(commands) != 2 || commands[0].Directory != ConvertPath(childDir) ||
		commands[1].Directory != ConvertPath(projectDir) {
		t.Fatalf("legacy Make directory markers were not tracked: %#v", commands)
	}
}

func TestParsePreservesQuotesInsideMakeDirectory(t *testing.T) {
	projectDir := t.TempDir()
	childDir := filepath.Join(projectDir, `child'"dir`)
	outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
	tool := newTestTool(t, Config{
		InputFile:    "stdin",
		OutputFile:   outputFile,
		BuildDir:     projectDir,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
		NoStrict:     true,
	})

	tool.Parse([]string{
		"make: Entering directory '" + childDir + "'",
		"gcc -c child.c",
		"make: Leaving directory '" + childDir + "'",
		"gcc -c parent.c",
	})
	commands := readCompilerTestCommands(t, outputFile)
	if len(commands) != 2 || commands[0].Directory != ConvertPath(childDir) ||
		commands[1].Directory != ConvertPath(projectDir) {
		t.Fatalf("quotes inside Make directory were not preserved: %#v", commands)
	}
}

func TestParseConfirmsMakeCommandDirectoryFrame(t *testing.T) {
	projectDir := t.TempDir()
	childDir := filepath.Join(projectDir, "sub")
	outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
	tool := newTestTool(t, Config{
		InputFile:    "stdin",
		OutputFile:   outputFile,
		BuildDir:     projectDir,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
		NoStrict:     true,
	})
	tool.Parse([]string{
		"make -C sub",
		"make[1]: Entering directory '" + childDir + "'",
		"gcc -c child.c",
		"make[1]: Leaving directory '" + childDir + "'",
		"gcc -c parent.c",
	})

	commands := readCompilerTestCommands(t, outputFile)
	if len(commands) != 2 || commands[0].Directory != ConvertPath(childDir) ||
		commands[1].Directory != ConvertPath(projectDir) {
		t.Fatalf("make directory frame was duplicated: %#v", commands)
	}
}

func TestParseResolvesRelativeMakeDirectoryForMacrosAndPath(t *testing.T) {
	projectDir := t.TempDir()
	workingDir := filepath.Join(projectDir, "sub")
	compilerDir := filepath.Join(workingDir, "toolchain")
	if err := os.MkdirAll(compilerDir, 0o755); err != nil {
		t.Fatalf("create compiler directory failed: %v", err)
	}
	compiler := filepath.Join(compilerDir, "fake-gcc")
	if err := os.WriteFile(compiler, []byte("#!/bin/sh\necho '#define FROM_MAKE_C 1'\n"), 0o755); err != nil {
		t.Fatalf("write compiler failed: %v", err)
	}
	t.Setenv("PATH", "toolchain")

	outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
	tool := newTestTool(t, Config{
		InputFile:    filepath.Join(projectDir, "build.log"),
		OutputFile:   outputFile,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
		NoStrict:     true,
		Macros:       true,
		FullPath:     true,
	})
	tool.Parse([]string{"make -C sub", "fake-gcc -c main.c"})

	commands := readCompilerTestCommands(t, outputFile)
	if len(commands) != 1 {
		t.Fatalf("expected one command, got %#v", commands)
	}
	if commands[0].Directory != ConvertPath(workingDir) || commands[0].Arguments[0] != ConvertPath(compiler) ||
		!slices.Contains(commands[0].Arguments, "-DFROM_MAKE_C=1") {
		t.Fatalf("relative make directory was not resolved: %#v", commands[0])
	}
}
