package internal

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
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

func TestParseWarnsForSourceFilesWithoutCompileOnlyFlag(t *testing.T) {
	tmpDir := t.TempDir()
	outputFile := filepath.Join(tmpDir, "compile_commands.json")
	var logs bytes.Buffer

	tool := newTestTool(t, Config{
		InputFile:    "stdin",
		OutputFile:   outputFile,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
		NoStrict:     true,
	})
	tool.Logger.SetOutput(&logs)

	tool.Parse([]string{"cc -Iinc -o app a.c b.c -lm"})

	data, err := os.ReadFile(outputFile)
	if err != nil {
		t.Fatalf("read output failed: %v", err)
	}
	if string(data) != "[]" {
		t.Fatalf("expected empty JSON array, got %q", string(data))
	}
	if !strings.Contains(logs.String(), "source files found without -c; command ignored") {
		t.Fatalf("expected missing -c warning, got %q", logs.String())
	}
	if strings.Contains(logs.String(), "cc -Iinc -o app a.c b.c -lm") {
		t.Fatalf("expected command to be omitted from warning, got %q", logs.String())
	}
}

func TestParseDoesNotWarnForLinkOnlyOrUnrelatedOutput(t *testing.T) {
	tmpDir := t.TempDir()
	outputFile := filepath.Join(tmpDir, "compile_commands.json")
	var logs bytes.Buffer

	tool := newTestTool(t, Config{
		InputFile:    "stdin",
		OutputFile:   outputFile,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
		NoStrict:     true,
	})
	tool.Logger.SetOutput(&logs)

	tool.Parse([]string{
		"cc -o app a.o b.o -lm",
		"cc -c a.c -o a.o",
		"echo cc -o app a.c b.c",
	})

	if strings.Contains(logs.String(), "source files found without -c; command ignored") {
		t.Fatalf("unexpected missing -c warning: %q", logs.String())
	}
}

func TestParseAppendsRepeatedMacros(t *testing.T) {
	tmpDir := t.TempDir()
	outputFile := filepath.Join(tmpDir, "compile_commands.json")

	tool := newTestTool(t, Config{
		InputFile:    "stdin",
		OutputFile:   outputFile,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
		NoStrict:     true,
		Macros:       []string{"-DMODE=1", `-DNAME="hello world"`, "-UDEBUG"},
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
	wantSuffix := []string{"-DMODE=1", "-DNAME=hello world", "-UDEBUG"}
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

func TestParseMacroValueDoesNotSplitComma(t *testing.T) {
	tmpDir := t.TempDir()
	outputFile := filepath.Join(tmpDir, "compile_commands.json")

	tool := newTestTool(t, Config{
		InputFile:    "stdin",
		OutputFile:   outputFile,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
		NoStrict:     true,
		Macros:       []string{`-DTEST_BOARD,-m32`},
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
