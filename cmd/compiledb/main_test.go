package main

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

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
	os.Args = []string{
		"compiledb",
		"--parse", "../../tests/build.log",
		"--output", "compile_commands.json",
	}
	if err := app.Run(os.Args); err != nil {
		t.Fatalf("CLI run failed: %v", err)
	}
}

func TestInvalidBuildDirFailsFast(t *testing.T) {
	app := newApp()
	missingDir := filepath.Join(t.TempDir(), "missing")
	os.Args = []string{
		"compiledb",
		"--build-dir", missingDir,
		"--parse", "../../tests/build.log",
		"--output", filepath.Join(t.TempDir(), "compile_commands.json"),
	}
	if err := app.Run(os.Args); err == nil {
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
		cfg, err := createConfig(ctx)
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

func TestInvalidEncodingFailsFast(t *testing.T) {
	app := newApp()
	os.Args = []string{
		"compiledb",
		"--encoding", "latin1",
		"--parse", "../../tests/build.log",
		"--output", filepath.Join(t.TempDir(), "compile_commands.json"),
	}
	if err := app.Run(os.Args); err == nil {
		t.Fatal("expected invalid encoding error")
	}
}

func TestInvalidEncodingFromEnvFailsFast(t *testing.T) {
	t.Setenv(encodingEnvVar, "latin1")

	app := newApp()
	os.Args = []string{
		"compiledb",
		"--parse", "../../tests/build.log",
		"--output", filepath.Join(t.TempDir(), "compile_commands.json"),
	}
	if err := app.Run(os.Args); err == nil {
		t.Fatal("expected invalid encoding from environment variable")
	}
}

func TestEncodingFlagOverridesEnv(t *testing.T) {
	t.Setenv(encodingEnvVar, "latin1")

	app := newApp()
	os.Args = []string{
		"compiledb",
		"--encoding", "raw",
		"--parse", "../../tests/build.log",
		"--output", filepath.Join(t.TempDir(), "compile_commands.json"),
	}
	if err := app.Run(os.Args); err != nil {
		t.Fatalf("expected --encoding to override environment value, got: %v", err)
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
	os.Args = []string{
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

	runErr := app.Run(os.Args)
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
				cfg, err := createConfig(ctx)
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
		cfg, err := createConfig(ctx)
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
		cfg, err := createConfig(ctx)
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
