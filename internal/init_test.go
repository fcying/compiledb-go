package internal

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	log "github.com/sirupsen/logrus"
)

func newTestTool(t *testing.T, cfg Config) *Tool {
	t.Helper()

	logger := log.New()
	logger.SetOutput(io.Discard)

	return NewTool(cfg, logger)
}

func TestGenerateFromStdinDoesNotPanic(t *testing.T) {
	tmpDir := t.TempDir()
	outputFile := filepath.Join(tmpDir, "compile_commands.json")

	oldStdin := os.Stdin
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe failed: %v", err)
	}
	defer r.Close()

	if _, err := w.WriteString("gcc -c missing.c\n"); err != nil {
		t.Fatalf("write stdin failed: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close writer failed: %v", err)
	}

	os.Stdin = r
	defer func() { os.Stdin = oldStdin }()

	tool := newTestTool(t, Config{
		InputFile:  "stdin",
		OutputFile: outputFile,
		NoStrict:   true,
	})

	tool.Generate()
}
