package internal

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"

	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"
)

const shellCleanupCommand = "/__compiledb_internal_cleanup"

type shellCleanupContextKey struct{}

func runShellProgram(ctx context.Context, program, workingDir string, stdout, stderr io.Writer) error {
	file, err := syntax.NewParser(syntax.Variant(syntax.LangPOSIX)).Parse(strings.NewReader(program), "")
	if err != nil {
		return err
	}
	return runParsedShellProgram(ctx, file, workingDir, expand.ListEnviron(os.Environ()...), nil,
		nil, synchronizedWriter(stdout), synchronizedWriter(stderr))
}

func runParsedShellProgram(
	ctx context.Context,
	file *syntax.File,
	workingDir string,
	environment expand.Environ,
	params []string,
	stdin io.Reader,
	stdout, stderr io.Writer,
) error {
	cleanup, err := syntax.NewParser(syntax.Variant(syntax.LangPOSIX)).Parse(
		strings.NewReader(shellCleanupCommand), "")
	if err != nil {
		return err
	}

	execHandler := func(_ interp.ExecHandlerFunc) interp.ExecHandlerFunc {
		return func(ctx context.Context, arguments []string) error {
			handler := interp.HandlerCtx(ctx)
			if ctx.Value(shellCleanupContextKey{}) != nil &&
				len(arguments) == 1 && arguments[0] == shellCleanupCommand {
				if err := handler.Builtin(ctx, []string{"trap", "-", "EXIT"}); err != nil {
					return err
				}
				return handler.Builtin(ctx, []string{"wait"})
			}
			executable, err := interp.LookPathDir(handler.Dir, handler.Env, arguments[0])
			if err != nil {
				fmt.Fprintln(handler.Stderr, err)
				return interp.ExitStatus(127)
			}
			cmd := exec.CommandContext(ctx, executable, arguments[1:]...)
			cmd.Args = arguments
			cmd.Env = shellEnvironment(handler.Env)
			cmd.Dir = handler.Dir
			cmd.Stdin = handler.Stdin
			cmd.Stdout = handler.Stdout
			cmd.Stderr = handler.Stderr
			configureProcessCommand(cmd, ctx)
			err = waitProcessCommand(cmd, ctx)
			if cmd.Process != nil {
				cleanupErr := cleanupExitedProcessTree(cmd.Process)
				if cleanupErr != nil && !errors.Is(cleanupErr, os.ErrProcessDone) && err == nil {
					err = cleanupErr
				}
			}
			releaseProcessTree(cmd.Process)
			if ctx.Err() != nil {
				return context.Cause(ctx)
			}
			if cmd.Process == nil && errors.Is(err, syscall.ENOEXEC) {
				script, openErr := os.Open(executable)
				if openErr != nil {
					fmt.Fprintln(handler.Stderr, openErr)
					return interp.ExitStatus(126)
				}
				file, parseErr := syntax.NewParser(syntax.Variant(syntax.LangPOSIX)).Parse(script, arguments[0])
				closeErr := script.Close()
				if parseErr != nil {
					fmt.Fprintln(handler.Stderr, parseErr)
					return interp.ExitStatus(2)
				}
				if closeErr != nil {
					fmt.Fprintln(handler.Stderr, closeErr)
					return interp.ExitStatus(126)
				}
				scriptEnvironment := expand.ListEnviron(shellEnvironment(handler.Env)...)
				return runParsedShellProgram(ctx, file, handler.Dir, scriptEnvironment, arguments[1:],
					handler.Stdin, handler.Stdout, handler.Stderr)
			}
			var exitError *exec.ExitError
			if errors.As(err, &exitError) {
				if status, ok := signaledExitCode(exitError); ok {
					return shellExitStatus(status)
				}
				return shellExitStatus(exitError.ExitCode())
			}
			return err
		}
	}

	options := []interp.RunnerOption{
		interp.Dir(workingDir),
		interp.Env(environment),
		interp.StdIO(stdin, stdout, stderr),
		interp.ExecHandlers(execHandler),
	}
	if params != nil {
		options = append(options, interp.Params(append([]string{"--"}, params...)...))
	}
	runner, err := interp.New(options...)
	if err != nil {
		return err
	}
	runCtx, cancel := context.WithCancel(ctx)
	runErr := runner.Run(runCtx, file)
	cancel()
	// Prevent background jobs from outliving the interpreter and its output
	// buffers. Clear the EXIT trap because the main run already invoked it.
	cleanupCtx := context.WithValue(context.WithoutCancel(ctx), shellCleanupContextKey{}, true)
	waitErr := runner.Run(cleanupCtx, cleanup)
	if runErr != nil {
		return runErr
	}
	return waitErr
}

func shellExitStatus(status int) interp.ExitStatus {
	if status <= 0 {
		return interp.ExitStatus(1)
	}
	if status > 255 {
		return interp.ExitStatus(255)
	}
	return interp.ExitStatus(status)
}

type lockedWriter struct {
	mu     sync.Mutex
	writer io.Writer
}

func synchronizedWriter(writer io.Writer) io.Writer {
	if writer == nil {
		return nil
	}
	return &lockedWriter{writer: writer}
}

func (w *lockedWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.writer.Write(data)
}

func shellEnvironment(environment expand.Environ) []string {
	values := make([]string, 0, 64)
	environment.Each(func(name string, variable expand.Variable) bool {
		if !variable.IsSet() {
			for i, value := range values {
				if strings.HasPrefix(value, name+"=") {
					values[i] = ""
				}
			}
		}
		if variable.Exported && variable.Kind == expand.String {
			values = append(values, name+"="+variable.String())
		}
		return true
	})
	return values
}
