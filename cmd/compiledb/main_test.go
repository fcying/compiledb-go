package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/fcying/compiledb-go/internal"

	log "github.com/sirupsen/logrus"
	"github.com/urfave/cli/v2"
)

func init() {
	log.SetOutput(os.Stdout)
	log.SetLevel(log.DebugLevel)
}

func TestParser(t *testing.T) {
	log.Info("TestParser")
	app := newApp()
	arguments := []string{
		"compiledb",
		"--parse", "../../tests/build.log",
		"--output", "compile_commands.json",
	}
	if err := app.Run(arguments); err != nil {
		t.Fatalf("CLI run failed: %v", err)
	}
}

func TestInvalidBuildDirFailsFast(t *testing.T) {
	app := newApp()
	missingDir := filepath.Join(t.TempDir(), "missing")
	arguments := []string{
		"compiledb",
		"--build-dir", missingDir,
		"--parse", "../../tests/build.log",
		"--output", filepath.Join(t.TempDir(), "compile_commands.json"),
	}
	if err := app.Run(arguments); err == nil {
		t.Fatal("expected invalid build-dir error")
	}
}

func TestRelativeBuildDirIsStoredAsAbsolutePath(t *testing.T) {
	oldWd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd failed: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWd) })

	root := t.TempDir()
	buildDir := filepath.Join(root, "build")
	if err := os.Mkdir(buildDir, 0o755); err != nil {
		t.Fatalf("create build directory failed: %v", err)
	}
	if err := os.Chdir(root); err != nil {
		t.Fatalf("chdir failed: %v", err)
	}

	app := newApp()
	app.Action = func(ctx *cli.Context) error {
		cfg, err := createConfig(ctx, false)
		if err != nil {
			return err
		}
		if cfg.BuildDir != buildDir {
			t.Fatalf("unexpected build directory: want %q, got %q", buildDir, cfg.BuildDir)
		}
		cwd, err := os.Getwd()
		if err != nil {
			t.Fatalf("getwd after createConfig failed: %v", err)
		}
		if cwd != root {
			t.Fatalf("createConfig changed cwd: want %q, got %q", root, cwd)
		}
		return nil
	}
	if err := app.Run([]string{"compiledb", "--build-dir", "build"}); err != nil {
		t.Fatalf("CLI run failed: %v", err)
	}
}

func TestDirectParseIgnoresMakeOutputEncoding(t *testing.T) {
	for name, test := range map[string]struct {
		arguments []string
		envValue  string
	}{
		"flag":        {arguments: []string{"--encoding", "latin1"}},
		"environment": {envValue: "latin1"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(encodingEnvVar, test.envValue)
			tmpDir := t.TempDir()
			buildLog := filepath.Join(tmpDir, "build.log")
			if err := os.WriteFile(buildLog, []byte("cc -c main.c\n"), 0o644); err != nil {
				t.Fatalf("write build log failed: %v", err)
			}
			arguments := append([]string{"compiledb"}, test.arguments...)
			arguments = append(arguments,
				"--parse", buildLog,
				"--output", filepath.Join(tmpDir, "compile_commands.json"),
				"--no-strict",
			)
			if err := newApp().Run(arguments); err != nil {
				t.Fatalf("direct parse rejected unused Make encoding: %v", err)
			}
		})
	}
}

func TestMakeValidatesOutputEncoding(t *testing.T) {
	for name, test := range map[string]struct {
		arguments []string
		envValue  string
	}{
		"flag":        {arguments: []string{"--encoding", "latin1"}},
		"environment": {envValue: "latin1"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(encodingEnvVar, test.envValue)
			app := newApp()
			arguments := append([]string{"compiledb"}, test.arguments...)
			arguments = append(arguments, "make")
			if err := app.Run(arguments); err == nil {
				t.Fatal("expected invalid Make output encoding error")
			}
		})
	}
}

func TestMakeEncodingFlagOverridesEnv(t *testing.T) {
	t.Setenv(encodingEnvVar, "latin1")
	app := newApp()
	app.Commands[0].Action = func(ctx *cli.Context) error {
		cfg, err := createConfig(ctx, true)
		if err != nil {
			return err
		}
		if cfg.Encoding != internal.EncodingRaw {
			t.Fatalf("unexpected Make output encoding: %q", cfg.Encoding)
		}
		return nil
	}
	if err := app.Run([]string{"compiledb", "--encoding", "raw", "make"}); err != nil {
		t.Fatalf("expected --encoding to override environment value, got: %v", err)
	}
}

func TestParseFlagUsesDashForStdin(t *testing.T) {
	for name, arguments := range map[string][]string{
		"default":       {"compiledb"},
		"explicit dash": {"compiledb", "--parse", "-"},
	} {
		t.Run(name, func(t *testing.T) {
			app := newApp()
			app.Action = func(ctx *cli.Context) error {
				cfg, err := createConfig(ctx, false)
				if err != nil {
					return err
				}
				if cfg.InputFile != "-" {
					t.Fatalf("expected stdin sentinel, got %q", cfg.InputFile)
				}
				return nil
			}
			if err := app.Run(arguments); err != nil {
				t.Fatalf("CLI run failed: %v", err)
			}
		})
	}
}

func TestParseFlagCanReadFileNamedStdin(t *testing.T) {
	oldWd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd failed: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWd) })

	tmpDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmpDir, "stdin"), []byte("cc -c main.c\n"), 0o644); err != nil {
		t.Fatalf("write build log failed: %v", err)
	}
	if err := os.Chdir(tmpDir); err != nil {
		t.Fatalf("chdir failed: %v", err)
	}
	outputFile := filepath.Join(tmpDir, "compile_commands.json")
	if err := newApp().Run([]string{
		"compiledb", "--parse", "stdin", "--output", outputFile, "--no-strict",
	}); err != nil {
		t.Fatalf("CLI run failed: %v", err)
	}
	data, err := os.ReadFile(outputFile)
	if err != nil {
		t.Fatalf("read output failed: %v", err)
	}
	var commands []struct {
		File string `json:"file"`
	}
	if err := json.Unmarshal(data, &commands); err != nil {
		t.Fatalf("decode output failed: %v", err)
	}
	if len(commands) != 1 || commands[0].File != "main.c" {
		t.Fatalf("unexpected commands: %#v", commands)
	}
}

func TestRepeatedAddArgsPreserveArguments(t *testing.T) {
	oldWd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd failed: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWd) })

	tmpDir := t.TempDir()
	if err := os.Chdir(tmpDir); err != nil {
		t.Fatalf("chdir failed: %v", err)
	}

	buildLog := filepath.Join(tmpDir, "build.log")
	if err := os.WriteFile(buildLog, []byte("clang -c src/main.c\n"), 0o644); err != nil {
		t.Fatalf("write build log failed: %v", err)
	}

	oldStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("create stdout pipe failed: %v", err)
	}
	os.Stdout = w
	t.Cleanup(func() { os.Stdout = oldStdout })

	app := newApp()
	arguments := []string{
		"compiledb",
		"--parse", buildLog,
		"--output", "-",
		"--no-strict",
		"--add-arg", "-DCSV=a,b",
		"-a", `-DNAME="hello world"`,
		"--add-arg", `-DREGEX=\d+`,
		"-a=-target",
		"--add-arg=pi32v2",
	}

	runErr := app.Run(arguments)
	if err := w.Close(); err != nil {
		t.Fatalf("close stdout writer failed: %v", err)
	}
	if runErr != nil {
		t.Fatalf("CLI run failed: %v", runErr)
	}

	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read stdout failed: %v", err)
	}

	var commands []struct {
		Arguments []string `json:"arguments"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(out))), &commands); err != nil {
		t.Fatalf("stdout should be valid JSON, got %q: %v", string(out), err)
	}
	if len(commands) != 1 {
		t.Fatalf("expected one command entry, got %d", len(commands))
	}

	args := commands[0].Arguments
	want := []string{"-DCSV=a,b", `-DNAME="hello world"`, `-DREGEX=\d+`, "-target", "pi32v2"}
	if len(args) < len(want) {
		t.Fatalf("arguments too short: %v", args)
	}
	for i, value := range want {
		if got := args[len(args)-len(want)+i]; got != value {
			t.Fatalf("unexpected trailing args: want %v, got %v", want, args)
		}
	}
}

func TestMacrosAndAddArgFlagsCreateConfig(t *testing.T) {
	for name, arguments := range map[string][]string{
		"long then short": {"compiledb", "-m", "--add-arg", "-DTEST_BOARD", "-a", "-m32"},
		"short then long": {"compiledb", "-m", "-a", "-DTEST_BOARD", "--add-arg", "-m32"},
	} {
		t.Run(name, func(t *testing.T) {
			app := newApp()
			app.Action = func(ctx *cli.Context) error {
				cfg, err := createConfig(ctx, false)
				if err != nil {
					return err
				}
				if !cfg.Macros {
					t.Fatal("expected -m to enable predefined macros")
				}

				want := []string{"-DTEST_BOARD", "-m32"}
				if len(cfg.AddArgs) != len(want) {
					t.Fatalf("unexpected add args: %v", cfg.AddArgs)
				}
				for i := range want {
					if cfg.AddArgs[i] != want[i] {
						t.Fatalf("unexpected add args: want %v, got %v", want, cfg.AddArgs)
					}
				}
				return nil
			}

			if err := app.Run(arguments); err != nil {
				t.Fatalf("CLI run failed: %v", err)
			}
		})
	}
}

func TestAddArgsDoNotLeakAcrossAppRuns(t *testing.T) {
	app := newApp()
	var got [][]string
	app.Action = func(ctx *cli.Context) error {
		cfg, err := createConfig(ctx, false)
		if err != nil {
			return err
		}
		got = append(got, cfg.AddArgs)
		return nil
	}
	for _, arguments := range [][]string{
		{"compiledb", "--add-arg=FIRST"},
		{"compiledb"},
		{"compiledb", "-a=SECOND"},
	} {
		if err := app.Run(arguments); err != nil {
			t.Fatalf("CLI run failed: %v", err)
		}
	}
	want := [][]string{{"FIRST"}, nil, {"SECOND"}}
	if len(got) != len(want) {
		t.Fatalf("unexpected configs: %#v", got)
	}
	for i := range want {
		if !slices.Equal(got[i], want[i]) {
			t.Fatalf("add arguments leaked across runs:\nwant: %#v\ngot:  %#v", want, got)
		}
	}
}

func TestAddArgsDoNotLeakAcrossAppRunContexts(t *testing.T) {
	app := newApp()
	var got [][]string
	app.Action = func(ctx *cli.Context) error {
		cfg, err := createConfig(ctx, false)
		if err != nil {
			return err
		}
		got = append(got, cfg.AddArgs)
		return nil
	}
	for _, arguments := range [][]string{
		{"compiledb", "--add-arg=FIRST"},
		{"compiledb"},
	} {
		if err := app.RunContext(context.Background(), arguments); err != nil {
			t.Fatalf("CLI run failed: %v", err)
		}
	}
	if want := [][]string{{"FIRST"}, nil}; !slices.EqualFunc(got, want, slices.Equal[[]string]) {
		t.Fatalf("add arguments leaked across RunContext calls:\nwant: %#v\ngot:  %#v", want, got)
	}
}

func TestExcludePatternsPreserveOrderAndDoNotLeak(t *testing.T) {
	app := newApp()
	var got []internal.Config
	app.Action = func(ctx *cli.Context) error {
		cfg, err := createConfig(ctx, false)
		if err != nil {
			return err
		}
		got = append(got, cfg)
		return nil
	}
	for _, arguments := range [][]string{
		{"compiledb", "--exclude=vendor/", "-e=generated/"},
		{"compiledb"},
	} {
		if err := app.Run(arguments); err != nil {
			t.Fatalf("CLI run failed: %v", err)
		}
	}
	if len(got) != 2 || !slices.Equal(got[0].Exclude, []string{"vendor/", "generated/"}) || got[1].Exclude != nil {
		t.Fatalf("exclude patterns were not preserved per invocation: %#v", got)
	}
}

func TestMacroFailureKeepsStdoutAsJSON(t *testing.T) {
	tmpDir := t.TempDir()
	buildLog := filepath.Join(tmpDir, "build.log")
	if err := os.WriteFile(buildLog, []byte("definitely-missing-gcc -c main.c\n"), 0o644); err != nil {
		t.Fatalf("write build log failed: %v", err)
	}

	oldStdout, oldStderr := os.Stdout, os.Stderr
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		t.Fatalf("create stdout pipe failed: %v", err)
	}
	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		t.Fatalf("create stderr pipe failed: %v", err)
	}
	os.Stdout, os.Stderr = stdoutW, stderrW
	t.Cleanup(func() {
		os.Stdout, os.Stderr = oldStdout, oldStderr
	})

	app := newApp()
	runErr := app.Run([]string{
		"compiledb",
		"--parse", buildLog,
		"--output", "-",
		"--no-strict",
		"--macros",
	})
	if err := stdoutW.Close(); err != nil {
		t.Fatalf("close stdout writer failed: %v", err)
	}
	if err := stderrW.Close(); err != nil {
		t.Fatalf("close stderr writer failed: %v", err)
	}
	if runErr != nil {
		t.Fatalf("CLI run failed: %v", runErr)
	}

	stdout, err := io.ReadAll(stdoutR)
	if err != nil {
		t.Fatalf("read stdout failed: %v", err)
	}
	stderr, err := io.ReadAll(stderrR)
	if err != nil {
		t.Fatalf("read stderr failed: %v", err)
	}
	var commands []struct {
		File string `json:"file"`
	}
	if err := json.Unmarshal(stdout, &commands); err != nil {
		t.Fatalf("stdout should be valid JSON, got %q: %v", stdout, err)
	}
	if len(commands) != 1 || commands[0].File != "main.c" {
		t.Fatalf("unexpected commands: %#v", commands)
	}
	if !strings.Contains(string(stderr), "failed to get predefined macros") {
		t.Fatalf("expected macro failure on stderr, got %q", stderr)
	}
}

func TestDefaultDiagnosticsUseStderr(t *testing.T) {
	tmpDir := t.TempDir()
	buildLog := filepath.Join(tmpDir, "build.log")
	contents := "gcc -I`false` -c failed.c\ngcc -c 'tokenizer.c\ngcc -c valid.c\n"
	if err := os.WriteFile(buildLog, []byte(contents), 0o644); err != nil {
		t.Fatalf("write build log failed: %v", err)
	}

	oldStdout, oldStderr := os.Stdout, os.Stderr
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		t.Fatalf("create stdout pipe failed: %v", err)
	}
	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		t.Fatalf("create stderr pipe failed: %v", err)
	}
	os.Stdout, os.Stderr = stdoutW, stderrW
	t.Cleanup(func() { os.Stdout, os.Stderr = oldStdout, oldStderr })

	runErr := newApp().Run([]string{
		"compiledb",
		"--parse", buildLog,
		"--output", filepath.Join(tmpDir, "compile_commands.json"),
		"--no-strict",
	})
	_ = stdoutW.Close()
	_ = stderrW.Close()
	if runErr != nil {
		t.Fatalf("CLI run failed: %v", runErr)
	}
	stdout, err := io.ReadAll(stdoutR)
	if err != nil {
		t.Fatalf("read stdout failed: %v", err)
	}
	stderr, err := io.ReadAll(stderrR)
	if err != nil {
		t.Fatalf("read stderr failed: %v", err)
	}
	if len(stdout) != 0 {
		t.Fatalf("diagnostics were written to stdout: %q", stdout)
	}
	if !strings.Contains(string(stderr), "Error executing nested command") {
		t.Fatalf("expected parser error on stderr, got %q", stderr)
	}
	if strings.Contains(string(stderr), "parse failed") {
		t.Fatalf("default ErrorLevel exposed tokenizer warning: %q", stderr)
	}
}

func TestFatalDiagnosticsUseStderr(t *testing.T) {
	if mode := os.Getenv("COMPILEDB_FATAL_DIAGNOSTIC_HELPER"); mode != "" {
		switch mode {
		case "internal":
			os.Args = []string{"compiledb", "--parse", "-", "--regex-compile", "[", "--output", os.Getenv("COMPILEDB_FATAL_OUTPUT")}
		case "action":
			os.Args = []string{"compiledb", "--encoding", "latin1", "make"}
		case "usage":
			os.Args = []string{"compiledb", "--definitely-invalid"}
		}
		main()
		return
	}

	for name, want := range map[string]string{
		"internal": "invalid parser regex",
		"action":   "unsupported encoding",
		"usage":    "Incorrect Usage",
	} {
		t.Run(name, func(t *testing.T) {
			executable, err := os.Executable()
			if err != nil {
				t.Fatalf("resolve test executable failed: %v", err)
			}
			cmd := exec.Command(executable, "-test.run=^TestFatalDiagnosticsUseStderr$")
			cmd.Env = append(os.Environ(),
				"COMPILEDB_FATAL_DIAGNOSTIC_HELPER="+name,
				"COMPILEDB_FATAL_OUTPUT="+filepath.Join(t.TempDir(), "compile_commands.json"),
			)
			var stdout, stderr bytes.Buffer
			cmd.Stdout = &stdout
			cmd.Stderr = &stderr
			if err := cmd.Run(); err == nil {
				t.Fatal("fatal diagnostic helper exited successfully")
			}
			if stdout.Len() != 0 {
				t.Fatalf("fatal diagnostic was written to stdout: %q", stdout.String())
			}
			if !strings.Contains(stderr.String(), want) {
				t.Fatalf("expected %q on stderr, got %q", want, stderr.String())
			}
		})
	}
}

func TestHelpUsesStdout(t *testing.T) {
	var stdout, stderr bytes.Buffer
	app := newApp()
	app.Writer = &stdout
	app.ErrWriter = &stderr
	if err := app.Run([]string{"compiledb", "--help"}); err != nil {
		t.Fatalf("show help failed: %v", err)
	}
	if !strings.Contains(stdout.String(), "USAGE:") {
		t.Fatalf("help was not written to stdout: %q", stdout.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("help wrote to stderr: %q", stderr.String())
	}
}

func TestMakeFailureDiagnosticsUseStderr(t *testing.T) {
	if mode := os.Getenv("COMPILEDB_MAKE_FAILURE_HELPER"); mode != "" {
		switch mode {
		case "dry-run":
			os.Args = []string{"compiledb", "--no-build", "--output", os.Getenv("COMPILEDB_MAKE_FAILURE_OUTPUT"), "make"}
		case "real":
			os.Args = []string{"compiledb", "--output", os.Getenv("COMPILEDB_MAKE_FAILURE_OUTPUT"), "make"}
		}
		main()
		return
	}

	executable, err := os.Executable()
	if err != nil {
		t.Fatalf("resolve test executable failed: %v", err)
	}
	for name, test := range map[string]struct {
		path   string
		want   string
		status int
	}{
		"dry-run": {path: t.TempDir(), want: "dry-run make failed", status: 127},
		"real":    {path: makeFailureTestPath(t), want: "make failed with status 7", status: 7},
	} {
		t.Run(name, func(t *testing.T) {
			cmd := exec.Command(executable, "-test.run=^TestMakeFailureDiagnosticsUseStderr$")
			cmd.Env = replaceTestEnvironment(os.Environ(), "PATH", test.path)
			cmd.Env = append(cmd.Env,
				"COMPILEDB_MAKE_FAILURE_HELPER="+name,
				"COMPILEDB_MAKE_FAILURE_OUTPUT="+filepath.Join(t.TempDir(), "compile_commands.json"),
			)
			var stdout, stderr bytes.Buffer
			cmd.Stdout = &stdout
			cmd.Stderr = &stderr
			err := cmd.Run()
			var exitError *exec.ExitError
			if !errors.As(err, &exitError) || exitError.ExitCode() != test.status {
				t.Fatalf("unexpected Make failure status: want %d, got %v", test.status, err)
			}
			if stdout.Len() != 0 {
				t.Fatalf("Make failure diagnostic was written to stdout: %q", stdout.String())
			}
			if !strings.Contains(stderr.String(), test.want) {
				t.Fatalf("expected %q on stderr, got %q", test.want, stderr.String())
			}
		})
	}
}

func makeFailureTestPath(t *testing.T) string {
	t.Helper()
	directory := t.TempDir()
	script := filepath.Join(directory, "make")
	contents := `#!/bin/sh
case " $* " in
  *" -Bnkw "*) exit 0 ;;
  *) exit 7 ;;
esac
`
	if err := os.WriteFile(script, []byte(contents), 0o755); err != nil {
		t.Fatalf("write fake Make failed: %v", err)
	}
	return directory
}

func replaceTestEnvironment(environment []string, name, value string) []string {
	prefix := name + "="
	result := make([]string, 0, len(environment)+1)
	for _, variable := range environment {
		if !strings.HasPrefix(variable, prefix) {
			result = append(result, variable)
		}
	}
	return append(result, prefix+value)
}

func TestOverwriteFlagsReplaceExistingDatabase(t *testing.T) {
	for _, flag := range []string{"-f", "--overwrite"} {
		t.Run(flag, func(t *testing.T) {
			tmpDir := t.TempDir()
			buildLog := filepath.Join(tmpDir, "build.log")
			outputFile := filepath.Join(tmpDir, "compile_commands.json")
			if err := os.WriteFile(buildLog, []byte("clang -c new.c\n"), 0o644); err != nil {
				t.Fatalf("write build log failed: %v", err)
			}
			existing := `[{"directory":"/old","command":"cc -c old.c","file":"old.c"}]`
			if err := os.WriteFile(outputFile, []byte(existing), 0o644); err != nil {
				t.Fatalf("seed output failed: %v", err)
			}

			app := newApp()
			args := []string{
				"compiledb",
				flag,
				"--parse", buildLog,
				"--output", outputFile,
				"--no-strict",
			}
			if err := app.Run(args); err != nil {
				t.Fatalf("CLI run failed: %v", err)
			}

			data, err := os.ReadFile(outputFile)
			if err != nil {
				t.Fatalf("read output failed: %v", err)
			}
			var commands []struct {
				File string `json:"file"`
			}
			if err := json.Unmarshal(data, &commands); err != nil {
				t.Fatalf("decode output failed: %v", err)
			}
			if len(commands) != 1 || commands[0].File != "new.c" {
				t.Fatalf("expected overwrite to keep only new.c, got %#v", commands)
			}
		})
	}
}
