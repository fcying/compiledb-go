//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package internal

import (
	"context"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

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
