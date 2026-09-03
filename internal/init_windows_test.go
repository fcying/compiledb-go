//go:build windows

package internal

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestWriteFileAtomicallyFallsBackWhenReaderBlocksReplacement(t *testing.T) {
	tmpDir := t.TempDir()
	outputFile := filepath.Join(tmpDir, "compile_commands.json")
	if err := os.WriteFile(outputFile, []byte("old database\n"), 0o600); err != nil {
		t.Fatalf("create original output failed: %v", err)
	}
	reader, err := os.Open(outputFile)
	if err != nil {
		t.Fatalf("open original output reader failed: %v", err)
	}
	defer reader.Close()
	before, err := reader.Stat()
	if err != nil {
		t.Fatalf("stat original output reader failed: %v", err)
	}

	newData := []byte("new database\n")
	if err := writeFileAtomically(outputFile, newData); err != nil {
		t.Fatalf("sharing-blocked replacement did not fall back: %v", err)
	}

	after, err := os.Stat(outputFile)
	if err != nil {
		t.Fatalf("stat rewritten output failed: %v", err)
	}
	if !os.SameFile(before, after) {
		t.Fatal("sharing-blocked replacement did not rewrite the original file in place")
	}
	got, err := os.ReadFile(outputFile)
	if err != nil {
		t.Fatalf("read rewritten output failed: %v", err)
	}
	if !bytes.Equal(got, newData) {
		t.Fatalf("unexpected rewritten output: want %q, got %q", newData, got)
	}
	entries, err := os.ReadDir(tmpDir)
	if err != nil {
		t.Fatalf("read output directory failed: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != filepath.Base(outputFile) {
		t.Fatalf("fallback left temporary files: %#v", entries)
	}
}
