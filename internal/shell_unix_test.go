//go:build darwin || linux

package internal

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestRunShellProgramFallsBackToTextExecutable(t *testing.T) {
	workingDir := t.TempDir()
	scriptName := "compiledb-text-executable"
	writeTextExecutable(t, filepath.Join(workingDir, scriptName), `
printf '%s|%s|%s|%s' "$0" "$1" "$2" "$VALUE"
: > fallback-ran
exit "${EXIT_STATUS:-0}"
`)
	t.Setenv("PATH", workingDir)

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	program := "VALUE=exported " + scriptName + " -leading 'two words'; " +
		"EXIT_STATUS=7 " + scriptName + " ignored >/dev/null || printf '|recovered'"
	if err := runShellProgram(context.Background(), program, workingDir, &stdout, &stderr); err != nil {
		t.Fatalf("run embedded shell failed: %v: %s", err, stderr.String())
	}
	want := scriptName + "|-leading|two words|exported|recovered"
	if stdout.String() != want {
		t.Fatalf("text executable produced unexpected output: want %q, got %q", want, stdout.String())
	}
	if _, err := os.Stat(filepath.Join(workingDir, "fallback-ran")); err != nil {
		t.Fatalf("text executable did not use the tracked working directory: %v", err)
	}
}

func TestParseBacktickFallsBackToTextExecutable(t *testing.T) {
	workingDir := t.TempDir()
	scriptName := "compiledb-backtick-text-executable"
	writeTextExecutable(t, filepath.Join(workingDir, scriptName), "printf generated\n")
	t.Setenv("PATH", workingDir)
	outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
	tool := newTestTool(t, Config{
		InputFile:    "stdin",
		OutputFile:   outputFile,
		BuildDir:     workingDir,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
		NoStrict:     true,
	})

	tool.Parse([]string{"gcc -DVALUE=`" + scriptName + "` -c fallback.c"})

	commands := readCompilerTestCommands(t, outputFile)
	if len(commands) != 1 || commands[0].File != "fallback.c" ||
		!slices.Contains(commands[0].Arguments, "-DVALUE=generated") {
		t.Fatalf("text executable backtick was not expanded: %#v", commands)
	}
}

func writeTextExecutable(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o755); err != nil {
		t.Fatalf("write text executable failed: %v", err)
	}
}
