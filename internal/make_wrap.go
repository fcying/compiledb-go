package internal

import (
	"bytes"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"github.com/sirupsen/logrus"
)

var makePath = "make"

func (t *Tool) MakeWrap(args []string) {
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		// append log
		args = append([]string{"-Bnkw"}, args...)
		cmd := exec.Command(makePath, args...)

		var stdoutBuf bytes.Buffer
		cmd.Stdout = &stdoutBuf
		cmd.Stderr = &stdoutBuf
		cmd.Run()

		level := t.Logger.GetLevel()

		// only print make log
		if t.Config.NoBuild == false {
			t.Logger.SetLevel(logrus.PanicLevel)
		}

		buildLog := strings.Split(stdoutBuf.String(), "\n")
		t.Parse(buildLog)

		// restore log level
		if t.Config.NoBuild == false {
			t.Logger.SetLevel(level)
		}

		wg.Done()
	}()

	if t.Config.NoBuild == false {
		cmd := exec.Command(makePath, args...)
		// cmd.Stdout = os.Stdout
		// cmd.Stderr = os.Stderr
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

		go TransferPrintScanner(stdout)
		go TransferPrintScanner(stderr)

		if err := cmd.Wait(); err != nil {
			t.StatusCode = cmd.ProcessState.ExitCode()
			fmt.Printf("make failed! errorCode: %d\n", t.StatusCode)
		}
	}

out:
	wg.Wait()
}
