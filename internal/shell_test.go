package internal

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"
)

func TestRunShellProgramSupportsPOSIXSemantics(t *testing.T) {
	workingDir := t.TempDir()
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	program := `
value=pure-go
trap 'printf trap' EXIT
printf '%s\n' "$value" > value.txt
read saved < value.txt
(
	i=0
	while [ "$i" -lt 1000 ]; do i=$((i + 1)); done
	printf '%s\n' "$saved" | { read piped; printf '[%s]' "$piped"; }
) &
printf prefix
wait
exit 0
`

	if err := runShellProgram(context.Background(), program, workingDir, &stdout, &stderr); err != nil {
		t.Fatalf("run embedded shell failed: %v: %s", err, stderr.String())
	}
	output := stdout.String()
	if !strings.Contains(output, "prefix") || !strings.Contains(output, "[pure-go]") {
		t.Fatalf("embedded shell produced unexpected output: %q", output)
	}
	if strings.Count(output, "trap") != 1 {
		t.Fatalf("embedded shell ran its exit trap more than once: %q", output)
	}
}

func TestRunShellProgramStopsUnwaitedBackgroundJobs(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	done := make(chan error, 1)
	go func() {
		done <- runShellProgram(context.Background(), "(while :; do :; done) & printf done", t.TempDir(), &stdout, &stderr)
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run embedded shell failed: %v: %s", err, stderr.String())
		}
		if stdout.String() != "done" {
			t.Fatalf("embedded shell produced unexpected output: %q", stdout.String())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("embedded shell did not stop its background job")
	}
}

func TestRunShellProgramCleanupCannotBeShadowed(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	program := `
trap 'printf trap' EXIT
builtin() { :; }
trap() { :; }
wait() { :; }
printf main
`

	if err := runShellProgram(context.Background(), program, t.TempDir(), &stdout, &stderr); err != nil {
		t.Fatalf("run embedded shell failed: %v: %s", err, stderr.String())
	}
	if stdout.String() != "maintrap" {
		t.Fatalf("embedded shell cleanup was shadowed: %q", stdout.String())
	}
}

func TestRunShellProgramPreservesExitStatus(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	err := runShellProgram(context.Background(), "false", t.TempDir(), &stdout, &stderr)
	if err == nil || err.Error() != "exit status 1" {
		t.Fatalf("embedded shell changed the program status: %v", err)
	}
}
