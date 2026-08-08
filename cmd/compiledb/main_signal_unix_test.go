//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package main

import (
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/urfave/cli/v2"
)

func TestCompiledbAppSecondSignalForcesExit(t *testing.T) {
	if os.Getenv("COMPILEDB_SECOND_SIGNAL_HELPER") == "1" {
		app := newApp()
		app.Action = func(ctx *cli.Context) error {
			if err := os.WriteFile(os.Getenv("COMPILEDB_SECOND_SIGNAL_MARKER"), nil, 0o644); err != nil {
				return err
			}
			<-ctx.Context.Done()
			time.Sleep(30 * time.Second)
			return nil
		}
		_ = app.Run([]string{"compiledb"})
		os.Exit(0)
	}

	marker := filepath.Join(t.TempDir(), "started")
	cmd := exec.Command(os.Args[0], "-test.run=^TestCompiledbAppSecondSignalForcesExit$")
	cmd.Env = append(os.Environ(), "COMPILEDB_SECOND_SIGNAL_HELPER=1", "COMPILEDB_SECOND_SIGNAL_MARKER="+marker)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	signal.Ignore(os.Interrupt)
	if err := cmd.Start(); err != nil {
		signal.Reset(os.Interrupt)
		t.Fatalf("start helper failed: %v", err)
	}
	signal.Reset(os.Interrupt)
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
	})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("helper did not start: %v", err)
	}
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatalf("send first interrupt failed: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatalf("send second interrupt failed: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		var exitError *exec.ExitError
		if !errors.As(err, &exitError) || exitError.ExitCode() != 128+int(syscall.SIGINT) {
			t.Fatalf("second signal did not terminate helper: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("second signal did not terminate helper")
	}
}

func TestCompiledbAppFirstSignalCancelsFIFOInput(t *testing.T) {
	if os.Getenv("COMPILEDB_FIFO_SIGNAL_HELPER") == "1" {
		app := newApp()
		_ = app.Run([]string{
			"compiledb",
			"--parse", os.Getenv("COMPILEDB_FIFO_SIGNAL_PATH"),
			"--output", os.Getenv("COMPILEDB_FIFO_SIGNAL_OUTPUT"),
			"--no-strict",
		})
		os.Exit(0)
	}

	tmpDir := t.TempDir()
	fifo := filepath.Join(tmpDir, "build.log")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatalf("create FIFO failed: %v", err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestCompiledbAppFirstSignalCancelsFIFOInput$")
	cmd.Env = append(os.Environ(),
		"COMPILEDB_FIFO_SIGNAL_HELPER=1",
		"COMPILEDB_FIFO_SIGNAL_PATH="+fifo,
		"COMPILEDB_FIFO_SIGNAL_OUTPUT="+filepath.Join(tmpDir, "compile_commands.json"),
	)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper failed: %v", err)
	}
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
	})

	time.Sleep(200 * time.Millisecond)
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("send termination signal failed: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		var exitError *exec.ExitError
		if !errors.As(err, &exitError) || exitError.ExitCode() != 128+int(syscall.SIGTERM) {
			t.Fatalf("first signal did not cancel FIFO input: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("first signal did not cancel FIFO input")
	}
}
