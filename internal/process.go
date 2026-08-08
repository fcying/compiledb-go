package internal

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
)

func waitProcessCommand(cmd *exec.Cmd, ctx context.Context) error {
	if err := startProcessCommand(cmd); err != nil {
		return err
	}
	return waitStartedProcessCommand(cmd, ctx)
}

func waitStartedProcessCommand(cmd *exec.Cmd, ctx context.Context) error {
	done := make(chan struct{})
	watchDone := make(chan bool, 1)
	go func() {
		select {
		case <-ctx.Done():
			_ = terminateProcessTree(cmd.Process, ctx)
			watchDone <- true
		case <-done:
			watchDone <- false
		}
	}()
	err := cmd.Wait()
	markProcessExited(cmd.Process)
	close(done)
	if <-watchDone && err == nil {
		return context.Cause(ctx)
	}
	return err
}

func outputProcessCommand(cmd *exec.Cmd, ctx context.Context) ([]byte, error) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := startProcessCommand(cmd); err != nil {
		return nil, err
	}
	err := waitStartedProcessCommand(cmd, ctx)
	if cmd.Process != nil {
		cleanupErr := cleanupExitedProcessTree(cmd.Process)
		if cleanupErr != nil && !errors.Is(cleanupErr, os.ErrProcessDone) && err == nil {
			err = cleanupErr
		}
	}
	releaseProcessTree(cmd.Process)
	var exitError *exec.ExitError
	if errors.As(err, &exitError) {
		exitError.Stderr = append([]byte(nil), stderr.Bytes()...)
	}
	return stdout.Bytes(), err
}
