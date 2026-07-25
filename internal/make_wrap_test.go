package internal

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMakeWrapNoBuildStopsOnDryRunFailure(t *testing.T) {
	tmpDir := t.TempDir()
	outputFile := filepath.Join(tmpDir, "compile_commands.json")
	script := filepath.Join(tmpDir, "fake-make.sh")

	if err := os.WriteFile(script, []byte("#!/bin/sh\necho 'gcc -c src/main.c'\nexit 2\n"), 0o755); err != nil {
		t.Fatalf("write fake make failed: %v", err)
	}

	oldMakePath := makePath
	makePath = script
	defer func() { makePath = oldMakePath }()

	tool := newTestTool(t, Config{OutputFile: outputFile, NoBuild: true, NoStrict: true})
	tool.MakeWrap(nil)

	if tool.StatusCode != 2 {
		t.Fatalf("expected status code 2, got %d", tool.StatusCode)
	}
	if _, err := os.Stat(outputFile); !os.IsNotExist(err) {
		t.Fatalf("expected no compilation database to be written, stat err=%v", err)
	}
}

func TestMakeWrapReportsSourceFilesWithoutCompileOnlyFlag(t *testing.T) {
	tmpDir := t.TempDir()
	outputFile := filepath.Join(tmpDir, "compile_commands.json")
	script := filepath.Join(tmpDir, "fake-make.sh")
	var logs bytes.Buffer

	if err := os.WriteFile(script, []byte("#!/bin/sh\necho 'cc -o app a.c b.c'\n"), 0o755); err != nil {
		t.Fatalf("write fake make failed: %v", err)
	}

	oldMakePath := makePath
	makePath = script
	defer func() { makePath = oldMakePath }()

	tool := newTestTool(t, Config{OutputFile: outputFile, NoStrict: true})
	tool.Logger.SetOutput(&logs)
	tool.MakeWrap(nil)

	if tool.StatusCode != 0 {
		t.Fatalf("expected status code 0, got %d", tool.StatusCode)
	}
	if !strings.Contains(logs.String(), "compiler command contains source files but no -c") {
		t.Fatalf("expected missing -c warning, got %q", logs.String())
	}
}
