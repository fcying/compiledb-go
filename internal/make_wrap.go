package internal

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"

	"github.com/sirupsen/logrus"
)

var makePath = "make"

func commandExitCode(err error) int {
	var exitErr *exec.ExitError
	if err == nil {
		return 0
	}
	if strings.Contains(err.Error(), "executable file not found") {
		return 127
	}
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return 1
}

func (t *Tool) MakeWrap(args []string) {
	var (
		wg            sync.WaitGroup
		dryRunMakeErr error
		dryRunStatus  int
		buildStatus   int
	)
	buildArgs := append([]string(nil), args...)
	dryRunArgs := append([]string{"-Bnkw"}, args...)

	wg.Add(1)
	go func() {
		defer wg.Done()

		cmd := exec.Command(makePath, dryRunArgs...)

		var stdoutBuf bytes.Buffer
		cmd.Stdout = &stdoutBuf
		cmd.Stderr = &stdoutBuf
		if err := cmd.Run(); err != nil {
			dryRunMakeErr = err
			dryRunStatus = commandExitCode(err)
			t.Logger.Warnf("dry-run make failed: %v", err)
			return
		}

		level := t.Logger.GetLevel()
		if !t.Config.NoBuild {
			// Keep parser errors visible while the real make output is streaming.
			t.Logger.SetLevel(logrus.ErrorLevel)
		}

		buildLog := strings.Split(stdoutBuf.String(), "\n")
		t.Parse(buildLog)

		if !t.Config.NoBuild {
			// Restore the caller's log level after parsing dry-run output.
			t.Logger.SetLevel(level)
		}
	}()

	if !t.Config.NoBuild {
		cmd := exec.Command(makePath, buildArgs...)
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			fmt.Println("stdout Error:", err)
			goto out
		}
		stderr, err := cmd.StderrPipe()
		if err != nil {
			fmt.Println("stderr Error:", err)
			goto out
		}

		if err := cmd.Start(); err != nil {
			fmt.Println("start Error:", err)
			goto out
		}

		go TransferPrint(stdout, os.Stdout, t.Config.Encoding)
		go TransferPrint(stderr, os.Stderr, t.Config.Encoding)

		if err := cmd.Wait(); err != nil {
			buildStatus = cmd.ProcessState.ExitCode()
			fmt.Printf("make failed! errorCode: %d\n", buildStatus)
		}
	}

out:
	wg.Wait()
	if !t.Config.NoBuild {
		if buildStatus != 0 {
			t.StatusCode = buildStatus
		} else if dryRunStatus != 0 {
			t.StatusCode = dryRunStatus
		}
		return
	}

	if dryRunMakeErr != nil {
		t.StatusCode = commandExitCode(dryRunMakeErr)
	}
}
