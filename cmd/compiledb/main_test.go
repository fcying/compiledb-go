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
	"runtime"
	"runtime/debug"
	"slices"
	"strings"
	"testing"

	"github.com/fcying/compiledb-go/internal"

	log "github.com/sirupsen/logrus"
	"github.com/urfave/cli/v2"
)

func TestFormatVersion(t *testing.T) {
	for name, test := range map[string]struct {
		version  string
		settings []debug.BuildSetting
		want     string
	}{
		"build info revision": {
			version:  "v1.7.0",
			settings: []debug.BuildSetting{{Key: "vcs.revision", Value: "abcdef0123456789"}},
			want:     "v1.7.0 (abcdef012345)",
		},
		"dirty build": {
			version: "v1.7.0",
			settings: []debug.BuildSetting{
				{Key: "vcs.revision", Value: "abcdef0123456789"},
				{Key: "vcs.modified", Value: "true"},
			},
			want: "v1.7.0 (abcdef012345-dirty)",
		},
		"no revision": {version: "v1.7.0", want: "v1.7.0"},
	} {
		t.Run(name, func(t *testing.T) {
			if got := formatVersion(test.version, test.settings); got != test.want {
				t.Fatalf("unexpected version: want %q, got %q", test.want, got)
			}
		})
	}
}

func TestBuiltHelpIncludesCommit(t *testing.T) {
	revisionOutput, err := exec.Command("git", "rev-parse", "--short=12", "HEAD").Output()
	if err != nil {
		t.Skipf("Git revision is unavailable: %v", err)
	}
	revision := strings.TrimSpace(string(revisionOutput))
	if revision == "" {
		t.Fatal("Git returned an empty revision")
	}

	executable := filepath.Join(t.TempDir(), "compiledb")
	if runtime.GOOS == "windows" {
		executable += ".exe"
	}
	build := exec.Command("go", "build", "-buildvcs=true", "-ldflags", "-X main.Version=integration-version", "-o", executable, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build CLI failed: %v\n%s", err, output)
	}
	help, err := exec.Command(executable, "--help").Output()
	if err != nil {
		t.Fatalf("show built CLI help failed: %v", err)
	}
	want := "compiledb-go integration-version (" + revision
	if !strings.HasPrefix(string(help), want) {
		t.Fatalf("built help does not include commit: want prefix %q, got %q", want, help)
	}
}

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
	physicalRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("resolve temporary root failed: %v", err)
	}
	physicalBuildDir, err := filepath.EvalSymlinks(buildDir)
	if err != nil {
		t.Fatalf("resolve build directory failed: %v", err)
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
		if cfg.BuildDir != physicalBuildDir {
			t.Fatalf("unexpected build directory: want %q, got %q", physicalBuildDir, cfg.BuildDir)
		}
		cwd, err := os.Getwd()
		if err != nil {
			t.Fatalf("getwd after createConfig failed: %v", err)
		}
		if cwd != physicalRoot {
			t.Fatalf("createConfig changed cwd: want %q, got %q", physicalRoot, cwd)
		}
		return nil
	}
	if err := app.Run([]string{"compiledb", "--build-dir", "build"}); err != nil {
		t.Fatalf("CLI run failed: %v", err)
	}
}

func TestRelativeBuildDirPreservesStrictLegacyEntry(t *testing.T) {
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
	physicalRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("resolve temporary root failed: %v", err)
	}
	for _, filename := range []string{"main.c", "empty.log"} {
		if err := os.WriteFile(filepath.Join(buildDir, filename), nil, 0o644); err != nil {
			t.Fatalf("create build file failed: %v", err)
		}
	}
	outputFile := filepath.Join(root, "compile_commands.json")
	existing := `[{"directory":".","command":"cc -c main.c","file":"main.c","extension":{"keep":true}}]`
	if err := os.WriteFile(outputFile, []byte(existing), 0o644); err != nil {
		t.Fatalf("write existing database failed: %v", err)
	}
	if err := os.Chdir(root); err != nil {
		t.Fatalf("chdir failed: %v", err)
	}

	if err := newApp().Run([]string{
		"compiledb",
		"--build-dir", "build",
		"--parse", "empty.log",
		"--output", "compile_commands.json",
	}); err != nil {
		t.Fatalf("CLI run failed: %v", err)
	}

	data, err := os.ReadFile(outputFile)
	if err != nil {
		t.Fatalf("read compilation database failed: %v", err)
	}
	var entries []map[string]any
	if err := json.Unmarshal(data, &entries); err != nil {
		t.Fatalf("decode compilation database failed: %v", err)
	}
	if len(entries) != 1 || entries[0]["file"] != "main.c" {
		t.Fatalf("valid legacy entry was removed: %#v", entries)
	}
	extension, ok := entries[0]["extension"].(map[string]any)
	if !ok || extension["keep"] != true {
		t.Fatalf("legacy raw fields were not preserved: %#v", entries[0])
	}
	if cwd, err := os.Getwd(); err != nil || cwd != physicalRoot {
		t.Fatalf("CLI changed cwd: cwd=%q err=%v", cwd, err)
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

func TestParseMakeArguments(t *testing.T) {
	for name, test := range map[string]struct {
		input       []string
		wantCommand string
		wantArgs    []string
		wantHelp    bool
		wantError   bool
	}{
		"long": {
			input:       []string{"--cmd", "gmake", "-f", "Makefile", "target"},
			wantCommand: "gmake",
			wantArgs:    []string{"-f", "Makefile", "target"},
		},
		"short": {
			input:       []string{"-c", "mingw32-make", "-j2"},
			wantCommand: "mingw32-make",
			wantArgs:    []string{"-j2"},
		},
		"attached values and last wins": {
			input:       []string{"-cgmake", "--cmd=/opt/make", "goal"},
			wantCommand: "/opt/make",
			wantArgs:    []string{"goal"},
		},
		"short cluster": {
			input:       []string{"-xc", "gmake", "target"},
			wantCommand: "gmake",
			wantArgs:    []string{"-x", "target"},
		},
		"short cluster attached value": {
			input:       []string{"-xcy", "target"},
			wantCommand: "y",
			wantArgs:    []string{"-x", "target"},
		},
		"short equals is value data": {
			input:       []string{"-c=gmake"},
			wantCommand: "=gmake",
		},
		"unknown options preserved": {
			input:    []string{"--eval=all:;cc -c main.c", "-C", "build"},
			wantArgs: []string{"--eval=all:;cc -c main.c", "-C", "build"},
		},
		"terminator is consumed": {
			input:    []string{"--", "--cmd", "gmake", "-c", "target"},
			wantArgs: []string{"--cmd", "gmake", "-c", "target"},
		},
		"help": {
			input:    []string{"--help"},
			wantHelp: true,
		},
		"help after terminator": {
			input:    []string{"--", "--help"},
			wantArgs: []string{"--help"},
		},
		"help in short cluster": {
			input:    []string{"-xh"},
			wantArgs: []string{"-x"},
			wantHelp: true,
		},
		"Click compatible attached Make option": {
			input:       []string{"-fcore/main.mk"},
			wantCommand: "ore/main.mk",
			wantArgs:    []string{"-f"},
		},
		"Click compatible cluster value": {
			input:       []string{"-Csrc", "target"},
			wantCommand: "target",
			wantArgs:    []string{"-Csr"},
		},
		"missing long value": {
			input:     []string{"--cmd"},
			wantError: true,
		},
		"empty attached value falls back": {
			input: []string{"--cmd="},
		},
		"empty separate value falls back": {
			input: []string{"-c", ""},
		},
		"last empty value wins": {
			input: []string{"-c", "gmake", "--cmd="},
		},
		"help value is invalid": {
			input:     []string{"--help=x"},
			wantError: true,
		},
		"help before missing command value": {
			input:     []string{"--help", "-c"},
			wantError: true,
		},
		"help cluster before missing command value": {
			input:     []string{"-hc"},
			wantError: true,
		},
		"help before invalid help value": {
			input:     []string{"--help", "--help=value"},
			wantError: true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			command, arguments, help, err := parseMakeArguments(test.input)
			if (err != nil) != test.wantError {
				t.Fatalf("unexpected error: %v", err)
			}
			if command != test.wantCommand || !slices.Equal(arguments, test.wantArgs) || help != test.wantHelp {
				t.Fatalf("unexpected parse result: command=%q args=%#v help=%v", command, arguments, help)
			}
		})
	}
}

func TestHelpShowsMakeCommandOption(t *testing.T) {
	for name, arguments := range map[string][]string{
		"top level": {"compiledb", "--help"},
		"make":      {"compiledb", "make", "--help"},
	} {
		t.Run(name, func(t *testing.T) {
			var output bytes.Buffer
			app := newApp()
			app.Writer = &output
			if err := app.Run(arguments); err != nil {
				t.Fatalf("help failed: %v", err)
			}
			if !strings.Contains(output.String(), "--cmd") || !strings.Contains(output.String(), "-c") ||
				!strings.Contains(output.String(), "Command to be used as make executable") {
				t.Fatalf("make command option missing from help: %q", output.String())
			}
			if name == "make" {
				if strings.Contains(output.String(), "COMMANDS:") || strings.Contains(output.String(), "help, h") ||
					!strings.Contains(output.String(), "compiledb-go make [command options] [MAKE_ARGS]...") {
					t.Fatalf("unexpected make help structure: %q", output.String())
				}
			}
		})
	}
}

func TestMakeCommandOptionDoesNotConflictWithGlobalCommandStyle(t *testing.T) {
	tmpDir := t.TempDir()
	invocationFile := filepath.Join(tmpDir, "invocation")
	makeExecutable := filepath.Join(tmpDir, "custom-make")
	script := `#!/bin/sh
printf '%s\n' "$@" > ` + internal.ShellJoinArgs([]string{invocationFile}) + `
echo 'cc -c main.c'
`
	if err := os.WriteFile(makeExecutable, []byte(script), 0o755); err != nil {
		t.Fatalf("write custom Make failed: %v", err)
	}
	outputFile := filepath.Join(tmpDir, "compile_commands.json")
	if err := newApp().Run([]string{
		"compiledb",
		"--no-build",
		"-c",
		"--output", outputFile,
		"--no-strict",
		"make",
		"-c", makeExecutable,
		"-f", "Project.mk",
		"target",
	}); err != nil {
		t.Fatalf("CLI run failed: %v", err)
	}

	data, err := os.ReadFile(outputFile)
	if err != nil {
		t.Fatalf("read compilation database failed: %v", err)
	}
	var commands []internal.Command
	if err := json.Unmarshal(data, &commands); err != nil {
		t.Fatalf("decode compilation database failed: %v", err)
	}
	if len(commands) != 1 || commands[0].Command == "" || commands[0].Arguments != nil {
		t.Fatalf("global command-style option was not applied: %#v", commands)
	}

	invocation, err := os.ReadFile(invocationFile)
	if err != nil {
		t.Fatalf("read Make invocation failed: %v", err)
	}
	arguments := strings.Split(strings.TrimSpace(string(invocation)), "\n")
	if len(arguments) < 3 || !slices.Equal(arguments[:3], []string{"-f", "Project.mk", "target"}) {
		t.Fatalf("Make arguments were not forwarded in order: %#v", arguments)
	}
	if slices.Contains(arguments, "-c") || slices.Contains(arguments, makeExecutable) {
		t.Fatalf("compiledb make option was forwarded to Make: %#v", arguments)
	}
}

func TestMakeCommandOptionTerminatorIsNotForwarded(t *testing.T) {
	tmpDir := t.TempDir()
	invocationFile := filepath.Join(tmpDir, "invocation")
	makeExecutable := filepath.Join(tmpDir, "custom-make")
	script := `#!/bin/sh
printf '%s\n' "$@" > ` + internal.ShellJoinArgs([]string{invocationFile}) + `
echo 'cc -c main.c'
`
	if err := os.WriteFile(makeExecutable, []byte(script), 0o755); err != nil {
		t.Fatalf("write custom Make failed: %v", err)
	}
	if err := newApp().Run([]string{
		"compiledb", "--no-build", "--output", filepath.Join(tmpDir, "compile_commands.json"), "--no-strict",
		"make", "--cmd", makeExecutable, "--", "--cmd", "gmake", "-c", "target",
	}); err != nil {
		t.Fatalf("CLI run failed: %v", err)
	}

	invocation, err := os.ReadFile(invocationFile)
	if err != nil {
		t.Fatalf("read Make invocation failed: %v", err)
	}
	arguments := strings.Split(strings.TrimSpace(string(invocation)), "\n")
	if len(arguments) < 4 || !slices.Equal(arguments[:4], []string{"--cmd", "gmake", "-c", "target"}) {
		t.Fatalf("arguments after wrapper terminator were not preserved: %#v", arguments)
	}
}

func TestMakeCommandUsageError(t *testing.T) {
	if mode := os.Getenv("COMPILEDB_MAKE_USAGE_HELPER"); mode != "" {
		switch mode {
		case "missing command":
			os.Args = []string{"compiledb", "make", "-c"}
		case "help before missing command":
			os.Args = []string{"compiledb", "make", "--help", "-c"}
		case "help cluster before missing command":
			os.Args = []string{"compiledb", "make", "-hc"}
		case "help before invalid help value":
			os.Args = []string{"compiledb", "make", "--help", "--help=value"}
		}
		main()
		return
	}

	executable, err := os.Executable()
	if err != nil {
		t.Fatalf("resolve test executable failed: %v", err)
	}
	for name, want := range map[string]string{
		"missing command":                     "Option '-c' requires an argument.",
		"help before missing command":         "Option '-c' requires an argument.",
		"help cluster before missing command": "Option '-c' requires an argument.",
		"help before invalid help value":      "Option '--help' does not take a value.",
	} {
		t.Run(name, func(t *testing.T) {
			cmd := exec.Command(executable, "-test.run=^TestMakeCommandUsageError$")
			cmd.Env = append(os.Environ(), "COMPILEDB_MAKE_USAGE_HELPER="+name)
			var stdout, stderr bytes.Buffer
			cmd.Stdout = &stdout
			cmd.Stderr = &stderr
			err := cmd.Run()
			var exitError *exec.ExitError
			if !errors.As(err, &exitError) || exitError.ExitCode() != 2 {
				t.Fatalf("unexpected usage error status: %v", err)
			}
			if stdout.Len() != 0 {
				t.Fatalf("usage error was written to stdout: %q", stdout.String())
			}
			if got := stderr.String(); strings.Count(got, want) != 1 || strings.Contains(got, "level=fatal") ||
				strings.Contains(got, "USAGE:") {
				t.Fatalf("unexpected usage error diagnostic: %q", got)
			}
		})
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
	contents := "gcc -I`false` -c failed.c\ngcc -c 'tokenizer.c\ngcc -I`pwd -c unmatched.c\ngcc -c valid.c\n"
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
		"--output", "-",
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
	var commands []internal.Command
	if err := json.Unmarshal(stdout, &commands); err != nil {
		t.Fatalf("stdout should be valid JSON, got %q: %v", stdout, err)
	}
	if len(commands) != 1 || commands[0].File != "valid.c" {
		t.Fatalf("unexpected commands: %#v", commands)
	}
	if !strings.Contains(string(stderr), "Error executing nested command") {
		t.Fatalf("expected parser error on stderr, got %q", stderr)
	}
	if !strings.Contains(string(stderr), "skip malformed command at build log line 2") ||
		!strings.Contains(string(stderr), "unterminated quote") {
		t.Fatalf("expected tokenizer error on stderr, got %q", stderr)
	}
	if !strings.Contains(string(stderr), "skip malformed command at build log line 3") ||
		!strings.Contains(string(stderr), "unterminated backtick") {
		t.Fatalf("expected backtick tokenizer error on stderr, got %q", stderr)
	}
	if strings.Contains(string(stderr), "tokenizer.c") {
		t.Fatalf("tokenizer diagnostic exposed command contents: %q", stderr)
	}
}

func TestResponseFileFailureKeepsStdoutAsJSON(t *testing.T) {
	tmpDir := t.TempDir()
	buildLog := filepath.Join(tmpDir, "build.log")
	if err := os.WriteFile(filepath.Join(tmpDir, "arguments.rsp"), []byte("-DSECRET=value 'unterminated"), 0o644); err != nil {
		t.Fatalf("write response file failed: %v", err)
	}
	if err := os.WriteFile(buildLog, []byte("gcc @arguments.rsp -c hidden.c\ncc -c valid.c\n"), 0o644); err != nil {
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
		"--output", "-",
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

	var commands []internal.Command
	if err := json.Unmarshal(stdout, &commands); err != nil {
		t.Fatalf("stdout should be valid JSON, got %q: %v", stdout, err)
	}
	if len(commands) != 1 || commands[0].File != "valid.c" {
		t.Fatalf("unexpected commands: %#v", commands)
	}
	diagnostic := string(stderr)
	if !strings.Contains(diagnostic, "response file") || !strings.Contains(diagnostic, "unterminated quote") ||
		!strings.Contains(diagnostic, "build log line 1") {
		t.Fatalf("expected response-file diagnostic on stderr, got %q", stderr)
	}
	if strings.Contains(diagnostic, "SECRET") || strings.Contains(diagnostic, "hidden.c") {
		t.Fatalf("response-file diagnostic exposed contents: %q", diagnostic)
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
	if !strings.HasPrefix(stdout.String(), "compiledb-go "+displayVersion()+"\n") ||
		!strings.Contains(stdout.String(), "USAGE:") {
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

func removeTestEnvironment(environment []string, names ...string) []string {
	result := make([]string, 0, len(environment))
	for _, variable := range environment {
		name, _, _ := strings.Cut(variable, "=")
		remove := false
		for _, excluded := range names {
			if strings.EqualFold(name, excluded) {
				remove = true
				break
			}
		}
		if !remove {
			result = append(result, variable)
		}
	}
	return result
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

func TestBuiltCLIVerboseStreamContract(t *testing.T) {
	input := filepath.Join(t.TempDir(), "build.log")
	if err := os.WriteFile(input, []byte("cc -c main.c\n"), 0o644); err != nil {
		t.Fatalf("write build log failed: %v", err)
	}
	executable := buildTestCLI(t)
	for _, verbose := range []bool{false, true} {
		name := "quiet"
		arguments := []string{"--no-strict", "--parse", input, "--output", "-"}
		if verbose {
			name = "verbose"
			arguments = append([]string{"--verbose"}, arguments...)
		}
		t.Run(name, func(t *testing.T) {
			cmd := exec.Command(executable, arguments...)
			cmd.Env = removeTestEnvironment(os.Environ(), "GODEBUG")
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			if err := cmd.Run(); err != nil {
				t.Fatalf("CLI failed: %v; stderr=%q", err, stderr.String())
			}
			var commands []internal.Command
			if err := json.Unmarshal(stdout.Bytes(), &commands); err != nil || len(commands) != 1 || commands[0].File != "main.c" {
				t.Fatalf("stdout is not pure compilation JSON: commands=%#v error=%v stdout=%q", commands, err, stdout.String())
			}
			if verbose {
				if !strings.Contains(stderr.String(), "compiledb-go start") || !strings.Contains(stderr.String(), "Options:") {
					t.Fatalf("verbose diagnostics missing from stderr: %q", stderr.String())
				}
			} else if stderr.Len() != 0 {
				t.Fatalf("quiet execution emitted diagnostics: %q", stderr.String())
			}
		})
	}
}

func TestBuiltCLITopLevelExitContracts(t *testing.T) {
	executable := buildTestCLI(t)
	for name, test := range map[string]struct {
		buildDir func(*testing.T) string
		status   int
		stderr   string
	}{
		"unknown flag": {status: 2, stderr: "Incorrect Usage"},
		"missing build directory": {
			buildDir: func(t *testing.T) string { return filepath.Join(t.TempDir(), "missing") },
			status:   1,
			stderr:   "access build-dir",
		},
		"regular file build directory": {
			buildDir: func(t *testing.T) string {
				filename := filepath.Join(t.TempDir(), "not-directory")
				if err := os.WriteFile(filename, nil, 0o644); err != nil {
					t.Fatalf("write regular file failed: %v", err)
				}
				return filename
			},
			status: 1,
			stderr: "is not a directory",
		},
	} {
		t.Run(name, func(t *testing.T) {
			output := filepath.Join(t.TempDir(), "compile_commands.json")
			arguments := []string{"--output", output, "--definitely-invalid"}
			if test.buildDir != nil {
				arguments = []string{"--no-strict", "--build-dir", test.buildDir(t), "--parse", "-", "--output", output}
			}
			cmd := exec.Command(executable, arguments...)
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			err := cmd.Run()
			var exitError *exec.ExitError
			if !errors.As(err, &exitError) || exitError.ExitCode() != test.status {
				t.Fatalf("unexpected exit status: want %d, got %v", test.status, err)
			}
			if stdout.Len() != 0 || !strings.Contains(stderr.String(), test.stderr) {
				t.Fatalf("unexpected CLI streams: stdout=%q stderr=%q", stdout.String(), stderr.String())
			}
			if _, err := os.Stat(output); !os.IsNotExist(err) {
				t.Fatalf("failed CLI invocation created database: %v", err)
			}
		})
	}
}

func TestBuiltCLIMakeRecursionEndToEnd(t *testing.T) {
	makeExecutable := requireGNUmake(t)
	if _, err := exec.LookPath("cc"); err != nil {
		t.Skipf("C compiler is unavailable: %v", err)
	}
	projectDir := t.TempDir()
	childDir := filepath.Join(projectDir, "child")
	if err := os.Mkdir(childDir, 0o755); err != nil {
		t.Fatalf("create child directory failed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, "Makefile"), []byte("all:\n\t+\"$(MAKE)\" --no-print-directory -C child\n"), 0o644); err != nil {
		t.Fatalf("write root Makefile failed: %v", err)
	}
	childMakefile := "all:\n\tcc -c child.c -o child.o\n"
	if err := os.WriteFile(filepath.Join(childDir, "Makefile"), []byte(childMakefile), 0o644); err != nil {
		t.Fatalf("write child Makefile failed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(childDir, "child.c"), []byte("int child(void) { return 0; }\n"), 0o644); err != nil {
		t.Fatalf("write child source failed: %v", err)
	}

	output := filepath.Join(projectDir, "compile_commands.json")
	cmd := exec.Command(buildTestCLI(t), "--build-dir", projectDir, "--output", output, "make", "--cmd", makeExecutable)
	cmd.Env = removeTestEnvironment(os.Environ(), encodingEnvVar, "MAKE", "MAKE_COMMAND", "MAKEFLAGS", "GNUMAKEFLAGS", "MAKEFILES", "MAKELEVEL", "MFLAGS", "MAKEOVERRIDES")
	if result, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("built CLI Make invocation failed: %v\n%s", err, result)
	}
	commands := readBuiltCLICommands(t, output)
	physicalChildDir, err := filepath.EvalSymlinks(childDir)
	if err != nil {
		t.Fatalf("resolve child directory failed: %v", err)
	}
	if len(commands) != 1 || commands[0].File != "child.c" || commands[0].Directory != filepath.ToSlash(physicalChildDir) {
		t.Fatalf("recursive Make produced wrong database: %#v", commands)
	}
	object := filepath.Join(childDir, "child.o")
	if err := os.Remove(object); err != nil {
		t.Fatalf("remove child object before replay failed: %v", err)
	}
	replay := exec.Command(commands[0].Arguments[0], commands[0].Arguments[1:]...)
	replay.Dir = childDir
	if result, err := replay.CombinedOutput(); err != nil {
		t.Fatalf("compilation database command did not replay: %v\n%s", err, result)
	}
	if _, err := os.Stat(object); err != nil {
		t.Fatalf("replayed command did not rebuild object: %v", err)
	}
}

func readBuiltCLICommands(t *testing.T, filename string) []internal.Command {
	t.Helper()
	data, err := os.ReadFile(filename)
	if err != nil {
		t.Fatalf("read compilation database failed: %v", err)
	}
	var commands []internal.Command
	if err := json.Unmarshal(data, &commands); err != nil {
		t.Fatalf("decode compilation database failed: %v", err)
	}
	return commands
}

func buildTestCLI(t *testing.T) string {
	t.Helper()
	executable := filepath.Join(t.TempDir(), "compiledb")
	if runtime.GOOS == "windows" {
		executable += ".exe"
	}
	build := exec.Command("go", "build", "-o", executable, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build CLI failed: %v\n%s", err, output)
	}
	return executable
}

func requireGNUmake(t *testing.T) string {
	t.Helper()
	for _, name := range []string{"make", "gmake", "mingw32-make"} {
		executable, err := exec.LookPath(name)
		if err != nil {
			continue
		}
		command := exec.Command(executable, "--version")
		command.Env = removeTestEnvironment(os.Environ(), "MAKE", "MAKE_COMMAND", "MAKEFLAGS", "GNUMAKEFLAGS", "MAKEFILES", "MAKELEVEL", "MFLAGS", "MAKEOVERRIDES")
		output, err := command.Output()
		if err == nil && strings.Contains(string(output), "GNU Make") {
			absolute, err := filepath.Abs(executable)
			if err != nil {
				t.Fatalf("resolve GNU Make executable failed: %v", err)
			}
			return absolute
		}
	}
	t.Skip("GNU Make is unavailable")
	return ""
}
