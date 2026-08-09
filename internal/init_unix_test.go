//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package internal

import (
	"context"
	"os"
	"path/filepath"
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
