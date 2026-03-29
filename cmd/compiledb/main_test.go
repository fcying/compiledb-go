package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	log "github.com/sirupsen/logrus"
)

func init() {
	log.SetOutput(os.Stdout)
	log.SetLevel(log.DebugLevel)
}

func TestParser(t *testing.T) {
	log.Info("TestParser")
	app := newApp()
	os.Args = []string{
		"compiledb",
		"--parse", "../../tests/build.log",
		"--output", "compile_commands.json",
	}
	if err := app.Run(os.Args); err != nil {
		t.Fatalf("CLI run failed: %v", err)
	}
}

func TestInvalidBuildDirFailsFast(t *testing.T) {
	app := newApp()
	missingDir := filepath.Join(t.TempDir(), "missing")
	os.Args = []string{
		"compiledb",
		"--build-dir", missingDir,
		"--parse", "../../tests/build.log",
		"--output", filepath.Join(t.TempDir(), "compile_commands.json"),
	}
	if err := app.Run(os.Args); err == nil {
		t.Fatal("expected invalid build-dir error")
	}
}

func TestInvalidEncodingFailsFast(t *testing.T) {
	app := newApp()
	os.Args = []string{
		"compiledb",
		"--encoding", "latin1",
		"--parse", "../../tests/build.log",
		"--output", filepath.Join(t.TempDir(), "compile_commands.json"),
	}
	if err := app.Run(os.Args); err == nil {
		t.Fatal("expected invalid encoding error")
	}
}

func TestInvalidEncodingFromEnvFailsFast(t *testing.T) {
	t.Setenv(encodingEnvVar, "latin1")

	app := newApp()
	os.Args = []string{
		"compiledb",
		"--parse", "../../tests/build.log",
		"--output", filepath.Join(t.TempDir(), "compile_commands.json"),
	}
	if err := app.Run(os.Args); err == nil {
		t.Fatal("expected invalid encoding from environment variable")
	}
}

func TestEncodingFlagOverridesEnv(t *testing.T) {
	t.Setenv(encodingEnvVar, "latin1")

	app := newApp()
	os.Args = []string{
		"compiledb",
		"--encoding", "raw",
		"--parse", "../../tests/build.log",
		"--output", filepath.Join(t.TempDir(), "compile_commands.json"),
	}
	if err := app.Run(os.Args); err != nil {
		t.Fatalf("expected --encoding to override environment value, got: %v", err)
	}
}

func TestOutputDashWritesToStdout(t *testing.T) {
	oldWd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd failed: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWd) })

	tmpDir := t.TempDir()
	if err := os.Chdir(tmpDir); err != nil {
		t.Fatalf("chdir failed: %v", err)
	}

	buildLog := filepath.Join(tmpDir, "build.log")
	if err := os.WriteFile(buildLog, []byte("clang -c src/main.c\n"), 0o644); err != nil {
		t.Fatalf("write build log failed: %v", err)
	}

	oldStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("create stdout pipe failed: %v", err)
	}
	os.Stdout = w
	t.Cleanup(func() { os.Stdout = oldStdout })

	app := newApp()
	os.Args = []string{
		"compiledb",
		"--parse", buildLog,
		"--output", "-",
		"--no-strict",
	}

	runErr := app.Run(os.Args)
	if err := w.Close(); err != nil {
		t.Fatalf("close stdout writer failed: %v", err)
	}
	if runErr != nil {
		t.Fatalf("CLI run failed: %v", runErr)
	}

	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read stdout failed: %v", err)
	}

	var commands []map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(out))), &commands); err != nil {
		t.Fatalf("stdout should be valid JSON, got %q: %v", string(out), err)
	}
	if len(commands) == 0 {
		t.Fatal("expected stdout JSON to contain at least one command")
	}

	if _, err := os.Stat(filepath.Join(tmpDir, "-")); !os.IsNotExist(err) {
		t.Fatalf("expected no file named '-', stat err=%v", err)
	}
}
