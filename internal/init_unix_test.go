//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package internal

import (
	"context"
	"os"
	"path/filepath"
	"slices"
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
