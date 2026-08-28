package internal

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
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
)

func TestMakeWrapNoBuildStopsOnDryRunFailure(t *testing.T) {
	tmpDir := t.TempDir()
	outputFile := filepath.Join(tmpDir, "compile_commands.json")
	script := filepath.Join(tmpDir, "fake-make.sh")

	if err := os.WriteFile(script, []byte("#!/bin/sh\necho 'gcc -c src/main.c'\nexit 2\n"), 0o755); err != nil {
		t.Fatalf("write fake make failed: %v", err)
	}

	oldMakePath := makePath
	makePath = script
	defer func() { makePath = oldMakePath }()

	tool := newTestTool(t, Config{OutputFile: outputFile, NoBuild: true, NoStrict: true})
	tool.MakeWrap(nil)

	if tool.StatusCode != 2 {
		t.Fatalf("expected status code 2, got %d", tool.StatusCode)
	}
	if _, err := os.Stat(outputFile); !os.IsNotExist(err) {
		t.Fatalf("expected no compilation database to be written, stat err=%v", err)
	}
}

func TestMakeWrapNoBuildDoesNotInvokeRealMake(t *testing.T) {
	tmpDir := t.TempDir()
	outputFile := filepath.Join(tmpDir, "compile_commands.json")
	invocationsFile := filepath.Join(tmpDir, "invocations")
	realInvoked := filepath.Join(tmpDir, "real-invoked")
	script := filepath.Join(tmpDir, "fake-make.sh")
	contents := `#!/bin/sh
printf '%s\n' "$*" >> ` + ShellJoinArgs([]string{invocationsFile}) + `
case " $* " in
  *" -Bnkw "*) echo 'cc -c no-build.c' ;;
  *) : > ` + ShellJoinArgs([]string{realInvoked}) + ` ;;
esac
`
	if err := os.WriteFile(script, []byte(contents), 0o755); err != nil {
		t.Fatalf("write fake make failed: %v", err)
	}

	oldMakePath := makePath
	makePath = script
	defer func() { makePath = oldMakePath }()

	tool := newTestTool(t, Config{
		OutputFile:   outputFile,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
		NoBuild:      true,
		NoStrict:     true,
	})
	tool.MakeWrap([]string{"target"})

	commands := readCompilerTestCommands(t, outputFile)
	if tool.StatusCode != 0 || len(commands) != 1 || commands[0].File != "no-build.c" {
		t.Fatalf("no-build discovery failed: status=%d commands=%#v", tool.StatusCode, commands)
	}
	invocations, err := os.ReadFile(invocationsFile)
	if err != nil {
		t.Fatalf("read make invocations failed: %v", err)
	}
	if want := "target -Bnkw -j1 --print-directory\n"; string(invocations) != want {
		t.Fatalf("expected exactly one discovery invocation %q, got %q", want, invocations)
	}
	if _, err := os.Stat(realInvoked); !os.IsNotExist(err) {
		t.Fatalf("real Make was invoked with --no-build: %v", err)
	}
}

func TestMakeWrapSkipsOversizedDiscoveryLine(t *testing.T) {
	tmpDir := t.TempDir()
	outputFile := filepath.Join(tmpDir, "compile_commands.json")
	script := filepath.Join(tmpDir, "fake-make.sh")
	oversized := strings.Repeat("x", 32) + "\\"
	contents := "#!/bin/sh\nprintf '%s\\n' " + ShellJoinArgs([]string{oversized, "cc -c joined.c", "cc -c valid.c"}) + "\n"
	if err := os.WriteFile(script, []byte(contents), 0o755); err != nil {
		t.Fatalf("write fake make failed: %v", err)
	}

	oldMakePath := makePath
	makePath = script
	defer func() { makePath = oldMakePath }()

	var logs bytes.Buffer
	tool := newTestTool(t, Config{
		OutputFile:   outputFile,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
		NoBuild:      true,
		NoStrict:     true,
	})
	tool.buildLogLineLimit = 32
	tool.Logger.SetLevel(logrus.ErrorLevel)
	tool.Logger.SetOutput(&logs)
	tool.MakeWrap(nil)

	commands := readCompilerTestCommands(t, outputFile)
	if tool.StatusCode != 0 || len(commands) != 1 || commands[0].File != "valid.c" {
		t.Fatalf("oversized discovery line stopped MakeWrap: status=%d commands=%#v", tool.StatusCode, commands)
	}
	diagnostic := logs.String()
	for _, want := range []string{"build log line 1", "cwd", "physical line exceeds 32 byte limit", "at byte 32"} {
		if !strings.Contains(diagnostic, want) {
			t.Fatalf("physical line diagnostic lacks %q: %q", want, diagnostic)
		}
	}
}

func TestMakeWrapDoesNotParseDiscoveryStderr(t *testing.T) {
	tmpDir := t.TempDir()
	script := filepath.Join(tmpDir, "fake-make.sh")
	contents := "#!/bin/sh\necho 'gcc -c stdout.c'\necho 'gcc -c stderr.c' >&2\n"
	if err := os.WriteFile(script, []byte(contents), 0o755); err != nil {
		t.Fatalf("write fake make failed: %v", err)
	}
	oldMakePath := makePath
	makePath = script
	defer func() { makePath = oldMakePath }()

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

	tool := newTestTool(t, Config{
		OutputFile:   "-",
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
		NoBuild:      true,
		NoStrict:     true,
	})
	tool.MakeWrap(nil)
	_ = stdoutW.Close()
	_ = stderrW.Close()
	stdout, err := io.ReadAll(stdoutR)
	if err != nil {
		t.Fatalf("read stdout failed: %v", err)
	}
	stderr, err := io.ReadAll(stderrR)
	if err != nil {
		t.Fatalf("read stderr failed: %v", err)
	}

	var commands []Command
	if err := json.Unmarshal(stdout, &commands); err != nil {
		t.Fatalf("discovery stdout is not valid JSON: %q: %v", stdout, err)
	}
	if len(commands) != 1 || commands[0].File != "stdout.c" {
		t.Fatalf("discovery stderr was parsed as commands: %#v", commands)
	}
	if !strings.Contains(string(stderr), "gcc -c stderr.c") {
		t.Fatalf("discovery stderr was hidden: %q", stderr)
	}
}

func TestMakeWrapDryRunFailureLeavesExistingDatabaseUntouched(t *testing.T) {
	for name, noBuild := range map[string]bool{
		"normal":   false,
		"no build": true,
	} {
		t.Run(name, func(t *testing.T) {
			tmpDir := t.TempDir()
			outputFile := filepath.Join(tmpDir, "compile_commands.json")
			writeTestJSON(t, outputFile, []any{map[string]any{
				"directory": "/project",
				"command":   "cc -c keep.c",
				"file":      "keep.c",
				"extension": map[string]any{"owner": "old"},
			}})
			script := filepath.Join(tmpDir, "fake-make.sh")
			contents := `#!/bin/sh
case " $* " in
  *" -Bnkw "*)
    echo 'gcc -c partial.c'
    exit 2
    ;;
esac
exit 0
`
			if err := os.WriteFile(script, []byte(contents), 0o755); err != nil {
				t.Fatalf("write fake make failed: %v", err)
			}
			oldMakePath := makePath
			makePath = script
			defer func() { makePath = oldMakePath }()

			tool := newTestTool(t, Config{
				OutputFile: outputFile,
				NoBuild:    noBuild,
				NoStrict:   true,
				Overwrite:  true,
			})
			tool.MakeWrap(nil)
			if tool.StatusCode != 2 {
				t.Fatalf("unexpected dry-run status: %d", tool.StatusCode)
			}

			entries := readTestDatabase(t, outputFile)
			if len(entries) != 1 || entries[0]["file"] != "keep.c" {
				t.Fatalf("dry-run failure changed existing database: %#v", entries)
			}
			if _, ok := entries[0]["extension"]; !ok {
				t.Fatalf("dry-run failure lost existing raw fields: %#v", entries[0])
			}
		})
	}
}

func TestMakeWrapReportsRecoverableTokenizerFailure(t *testing.T) {
	for name, noBuild := range map[string]bool{
		"normal":   false,
		"no build": true,
	} {
		t.Run(name, func(t *testing.T) {
			tmpDir := t.TempDir()
			script := filepath.Join(tmpDir, "fake-make.sh")
			contents := `#!/bin/sh
case " $* " in
  *" -Bnkw "*)
    echo "gcc -DSECRET=value -c 'broken.c"
    printf '\140printf gcc\140 -c \047expanded-broken.c\n'
    printf 'gcc -I\140pwd -c unmatched.c\n'
    echo 'cc -c valid.c'
    ;;
esac
`
			if err := os.WriteFile(script, []byte(contents), 0o755); err != nil {
				t.Fatalf("write fake make failed: %v", err)
			}
			oldMakePath := makePath
			makePath = script
			defer func() { makePath = oldMakePath }()

			var logs bytes.Buffer
			tool := newTestTool(t, Config{
				OutputFile:   filepath.Join(tmpDir, "compile_commands.json"),
				RegexCompile: RegexCompile,
				RegexFile:    RegexFile,
				NoBuild:      noBuild,
				NoStrict:     true,
			})
			tool.Logger.SetLevel(logrus.ErrorLevel)
			tool.Logger.SetOutput(&logs)
			tool.MakeWrap(nil)

			commands := readCompilerTestCommands(t, tool.Config.OutputFile)
			if tool.StatusCode != 0 || len(commands) != 1 || commands[0].File != "valid.c" {
				t.Fatalf("recoverable tokenizer failure changed MakeWrap result: status=%d commands=%#v", tool.StatusCode, commands)
			}
			diagnostic := logs.String()
			if !strings.Contains(diagnostic, "build log line 1") ||
				!strings.Contains(diagnostic, "build log line 2") ||
				!strings.Contains(diagnostic, "build log line 3") ||
				!strings.Contains(diagnostic, "unterminated quote") ||
				!strings.Contains(diagnostic, "unterminated backtick") {
				t.Fatalf("missing tokenizer diagnostic: %q", diagnostic)
			}
			if strings.Contains(diagnostic, "SECRET") {
				t.Fatalf("tokenizer diagnostic exposed command contents: %q", diagnostic)
			}
		})
	}
}

func TestMakeWrapBackticksDoNotRequireExternalShell(t *testing.T) {
	for name, noBuild := range map[string]bool{
		"normal":   false,
		"no build": true,
	} {
		t.Run(name, func(t *testing.T) {
			tmpDir := t.TempDir()
			script := filepath.Join(tmpDir, "fake-make.sh")
			contents := `#!/bin/sh
case " $* " in
  *" -Bnkw "*)
    printf '%s\n' 'gcc -I` + "`pwd`" + ` -c builtin.c'
    printf '%s\n' 'gcc app.o -o app'
    printf '%s\n' 'echo ` + "`pwd`" + `'
    printf '%s\n' 'cc -c valid.c'
    ;;
esac
`
			if err := os.WriteFile(script, []byte(contents), 0o755); err != nil {
				t.Fatalf("write fake make failed: %v", err)
			}
			oldMakePath := makePath
			makePath = script
			defer func() { makePath = oldMakePath }()
			t.Setenv("PATH", t.TempDir())

			var logs bytes.Buffer
			tool := newTestTool(t, Config{
				OutputFile:   filepath.Join(tmpDir, "compile_commands.json"),
				RegexCompile: RegexCompile,
				RegexFile:    RegexFile,
				NoBuild:      noBuild,
				NoStrict:     true,
			})
			tool.Logger.SetLevel(logrus.ErrorLevel)
			tool.Logger.SetOutput(&logs)
			tool.MakeWrap(nil)

			commands := readCompilerTestCommands(t, tool.Config.OutputFile)
			if tool.StatusCode != 0 || len(commands) != 2 || commands[0].File != "builtin.c" ||
				commands[1].File != "valid.c" {
				t.Fatalf("embedded shell changed MakeWrap result: status=%d commands=%#v", tool.StatusCode, commands)
			}
			if logs.Len() != 0 {
				t.Fatalf("embedded shell emitted an unexpected diagnostic: %q", logs.String())
			}
		})
	}
}

func TestMakeWrapForcesSerializedDirectoryAwareDryRun(t *testing.T) {
	tmpDir := t.TempDir()
	outputFile := filepath.Join(tmpDir, "compile_commands.json")
	argumentsFile := filepath.Join(tmpDir, "arguments")
	script := filepath.Join(tmpDir, "fake-make.sh")
	contents := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + argumentsFile + "\n"
	if err := os.WriteFile(script, []byte(contents), 0o755); err != nil {
		t.Fatalf("write fake make failed: %v", err)
	}

	oldMakePath := makePath
	makePath = script
	defer func() { makePath = oldMakePath }()

	tool := newTestTool(t, Config{OutputFile: outputFile, NoBuild: true, NoStrict: true})
	tool.MakeWrap([]string{"-j8", "--no-print-directory", "--", "-C", "target"})
	if tool.StatusCode != 0 {
		t.Fatalf("unexpected make status: %d", tool.StatusCode)
	}
	data, err := os.ReadFile(argumentsFile)
	if err != nil {
		t.Fatalf("read make arguments failed: %v", err)
	}
	arguments := strings.Fields(string(data))
	if slices.Contains(arguments, "--no-print-directory") {
		t.Fatalf("conflicting directory output option was retained: %v", arguments)
	}
	wantSuffix := []string{"-Bnkw", "-j1", "--print-directory", "--", "-C", "target"}
	if len(arguments) < len(wantSuffix) || !slices.Equal(arguments[len(arguments)-len(wantSuffix):], wantSuffix) {
		t.Fatalf("discovery options do not override user settings: %v", arguments)
	}
}

func TestDiscoveryMakeArgumentsPreserveOptionOperands(t *testing.T) {
	arguments := []string{"-C", "-qdir", "-f", "-", "-EX=cc -c test.c", "-Otarget", "--dir", "-qdir", "-p", "--print-data-base", "--ques", "--tou", "--no-print-directory"}
	got := discoveryMakeArguments(arguments)
	want := []string{"-C", "-qdir", "-f", "-", "-EX=cc -c test.c", "-Otarget", "--dir", "-qdir", "-Bnkw", "-j1", "--print-directory"}
	if !slices.Equal(got, want) {
		t.Fatalf("Make option operands were changed:\nwant: %v\ngot:  %v", want, got)
	}
}

func TestDiscoveryMakeEnvironmentRemovesConflictingModes(t *testing.T) {
	environment := []string{
		"PATH=/usr/bin",
		"MAKEFLAGS=qp --no-print-directory --include-dir=/tmp/path\\ with\\ space FOO=$$(BAR)",
		"GNUMAKEFLAGS=tp --warn-undefined-variables",
	}
	got := discoveryMakeEnvironment(environment)
	want := []string{
		"PATH=/usr/bin",
		"MAKEFLAGS=--include-dir=/tmp/path\\ with\\ space FOO=$$(BAR)",
		"GNUMAKEFLAGS=--warn-undefined-variables",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("conflicting Make environment modes were retained:\nwant: %v\ngot:  %v", want, got)
	}
}

func TestDiscoveryMakeEnvironmentPreservesQuotesAndTargetBoundary(t *testing.T) {
	environment := []string{
		"MAKEFLAGS=-f - FOO='x",
		"GNUMAKEFLAGS=p goal",
	}
	got := discoveryMakeEnvironment(environment)
	want := []string{
		"MAKEFLAGS=-f - FOO='x",
		"GNUMAKEFLAGS=-- goal",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("Make environment syntax changed:\nwant: %v\ngot:  %v", want, got)
	}
	if !makeEnvironmentUsesStdinMakefile(environment) {
		t.Fatal("stdin Makefile was hidden by an unmatched quote in MAKEFLAGS")
	}
}

func TestDiscoveryMakeEnvironmentAddsBoundaryAfterAssignments(t *testing.T) {
	got := discoveryMakeEnvironment([]string{"MAKEFLAGS=p FOO=value goal"})
	want := []string{"MAKEFLAGS=FOO=value -- goal"}
	if !slices.Equal(got, want) {
		t.Fatalf("Make target boundary was lost:\nwant: %v\ngot:  %v", want, got)
	}
}

func TestDiscoveryMakeEnvironmentMovesOptionsBeforeGoals(t *testing.T) {
	jobsOperand := makeOptionTakesFollowingArgument("--jobs", []string{"4"})
	loadOperand := makeOptionTakesFollowingArgument("-l", []string{"1"})
	if !jobsOperand || !loadOperand {
		t.Fatalf("separated numeric Make option operands were not recognized: jobs=%v load=%v", jobsOperand, loadOperand)
	}
	got := discoveryMakeEnvironment([]string{
		"MAKEFLAGS=p goal -k -j 8 --load-average 2.5",
		"GNUMAKEFLAGS=p goal -f - --jobs 4 -l 1",
	})
	want := []string{
		"MAKEFLAGS=-k -j 8 --load-average 2.5 -- goal",
		"GNUMAKEFLAGS=-f - --jobs 4 -l 1 -- goal",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("Make options after goals were changed:\nwant: %v\ngot:  %v", want, got)
	}
}

func TestMakeOptionalNumericArgumentsFollowGNUmakeSyntax(t *testing.T) {
	for name, test := range map[string]struct {
		option string
		value  string
		want   bool
	}{
		"integer jobs":          {option: "--jobs", value: "4", want: true},
		"fractional jobs":       {option: "--jobs", value: "4.5", want: false},
		"decimal load":          {option: "--load-average", value: ".5", want: true},
		"scientific load":       {option: "--max-load", value: "1e-2", want: true},
		"overflowing load":      {option: "--load-average", value: "1e309", want: true},
		"hexadecimal load":      {option: "--load-average", value: "0x1p2", want: true},
		"signed separate value": {option: "--load-average", value: "+1", want: false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := makeOptionTakesFollowingArgument(test.option, []string{test.value}); got != test.want {
				t.Fatalf("unexpected operand classification for %s %s: want %v, got %v", test.option, test.value, test.want, got)
			}
		})
	}
}

func TestMakeWrapResolvesMakeFromBuildDirectoryRelativePATH(t *testing.T) {
	root := t.TempDir()
	buildDir := filepath.Join(root, "build")
	toolDir := filepath.Join(root, "toolchain")
	if err := os.MkdirAll(buildDir, 0o755); err != nil {
		t.Fatalf("create build directory failed: %v", err)
	}
	if err := os.MkdirAll(toolDir, 0o755); err != nil {
		t.Fatalf("create tool directory failed: %v", err)
	}
	makeExecutable := filepath.Join(toolDir, "make")
	if err := os.WriteFile(makeExecutable, []byte("#!/bin/sh\necho 'cc -c main.c'\n"), 0o755); err != nil {
		t.Fatalf("write fake Make failed: %v", err)
	}
	t.Setenv("PATH", "../toolchain")
	oldMakePath := makePath
	makePath = "make"
	defer func() { makePath = oldMakePath }()

	outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
	tool := newTestTool(t, Config{
		BuildDir:     buildDir,
		OutputFile:   outputFile,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
		NoBuild:      true,
		NoStrict:     true,
	})
	tool.MakeWrap(nil)

	commands := readCompilerTestCommands(t, outputFile)
	if tool.StatusCode != 0 || len(commands) != 1 || commands[0].File != "main.c" {
		t.Fatalf("relative PATH did not resolve Make from build directory: status=%d commands=%#v", tool.StatusCode, commands)
	}
}

func TestMakeWrapUsesConfiguredMakeCommandForBuildAndDiscovery(t *testing.T) {
	tmpDir := t.TempDir()
	invocations := filepath.Join(tmpDir, "invocations")
	makeExecutable := filepath.Join(tmpDir, "custom-make")
	contents := `#!/bin/sh
printf '%s\n' "$*" >> ` + ShellJoinArgs([]string{invocations}) + `
case " $* " in
  *" -Bnkw "*) echo 'cc -c main.c' ;;
esac
`
	if err := os.WriteFile(makeExecutable, []byte(contents), 0o755); err != nil {
		t.Fatalf("write custom Make failed: %v", err)
	}

	outputFile := filepath.Join(tmpDir, "compile_commands.json")
	tool := newTestTool(t, Config{
		OutputFile:   outputFile,
		MakeCommand:  makeExecutable,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
		NoStrict:     true,
		Encoding:     EncodingRaw,
	})
	tool.MakeWrap([]string{"-f", "Project.mk", "target"})

	commands := readCompilerTestCommands(t, outputFile)
	if tool.StatusCode != 0 || len(commands) != 1 || commands[0].File != "main.c" {
		t.Fatalf("configured Make command failed: status=%d commands=%#v", tool.StatusCode, commands)
	}
	data, err := os.ReadFile(invocations)
	if err != nil {
		t.Fatalf("read invocations failed: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 || lines[0] != "-f Project.mk target" ||
		!strings.HasPrefix(lines[1], "-f Project.mk target ") || !strings.Contains(lines[1], "-Bnkw") {
		t.Fatalf("configured Make command did not receive both invocations: %q", data)
	}
}

func TestMakeWrapMissingConfiguredMakeCommandPath(t *testing.T) {
	tool := newTestTool(t, Config{
		MakeCommand: filepath.Join(t.TempDir(), "missing-make"),
		OutputFile:  filepath.Join(t.TempDir(), "compile_commands.json"),
		NoBuild:     true,
		NoStrict:    true,
	})
	tool.MakeWrap(nil)
	if tool.StatusCode != 127 {
		t.Fatalf("missing configured Make command returned %d instead of 127", tool.StatusCode)
	}
}

func TestCommandExitCodeMissingWorkingDirectory(t *testing.T) {
	tmpDir := t.TempDir()
	makeExecutable := filepath.Join(tmpDir, "custom-make")
	if err := os.WriteFile(makeExecutable, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write custom Make failed: %v", err)
	}
	tool := newTestTool(t, Config{BuildDir: filepath.Join(tmpDir, "missing"), MakeCommand: makeExecutable})
	cmd := tool.makeCommand()
	err := cmd.Run()
	if code := commandExitCode(err); code != 1 {
		t.Fatalf("missing working directory returned %d instead of 1: %v", code, err)
	}
}

func TestMakeWrapConfiguredMakeCommandPermissionError(t *testing.T) {
	tool := newTestTool(t, Config{
		MakeCommand: t.TempDir(),
		OutputFile:  filepath.Join(t.TempDir(), "compile_commands.json"),
		NoBuild:     true,
		NoStrict:    true,
	})
	tool.MakeWrap(nil)
	if tool.StatusCode != 1 {
		t.Fatalf("invalid configured Make command returned %d instead of 1", tool.StatusCode)
	}
}

func TestMakeCommandDoesNotResolveRelativePATHFromProcessDirectory(t *testing.T) {
	oldWorkingDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("get working directory failed: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWorkingDir) })

	root := t.TempDir()
	processDir := filepath.Join(root, "process")
	buildDir := filepath.Join(root, "build")
	toolDir := filepath.Join(processDir, "tools")
	for _, directory := range []string{processDir, buildDir, toolDir} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatalf("create directory failed: %v", err)
		}
	}
	marker := filepath.Join(root, "executed")
	makeExecutable := filepath.Join(toolDir, "custom-make")
	contents := "#!/bin/sh\ntouch " + ShellJoinArgs([]string{marker}) + "\n"
	if err := os.WriteFile(makeExecutable, []byte(contents), 0o755); err != nil {
		t.Fatalf("write fake Make failed: %v", err)
	}
	if err := os.Chdir(processDir); err != nil {
		t.Fatalf("change working directory failed: %v", err)
	}
	t.Setenv("PATH", "tools")

	tool := newTestTool(t, Config{BuildDir: buildDir, MakeCommand: "custom-make"})
	cmd := tool.makeCommand()
	if !errors.Is(cmd.Err, exec.ErrNotFound) {
		t.Fatalf("relative PATH unexpectedly resolved outside BuildDir: path=%q err=%v", cmd.Path, cmd.Err)
	}
	if err := cmd.Run(); !errors.Is(err, exec.ErrNotFound) {
		t.Fatalf("unexpected command error: %v", err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("Make executable from process directory ran unexpectedly: %v", err)
	}
}

func TestMakeFlagBackslashEscapesAnyCharacter(t *testing.T) {
	arguments := splitMakeFlagArguments(`FOO=a\b BAR=a\\b`)
	wantArguments := []string{"FOO=ab", `BAR=a\b`}
	if !slices.Equal(arguments, wantArguments) {
		t.Fatalf("unexpected Make flag arguments:\nwant: %v\ngot:  %v", wantArguments, arguments)
	}
	if got, want := joinMakeFlagArguments(arguments), `FOO=ab BAR=a\\b`; got != want {
		t.Fatalf("Make flag escaping changed: want %q, got %q", want, got)
	}
}

func TestCanonicalMakeLongOptionRejectsAmbiguousPrefix(t *testing.T) {
	if name, ok := canonicalMakeLongOption("--qu"); ok {
		t.Fatalf("ambiguous Make option was resolved as %q", name)
	}
}

func TestMakeWrapSanitizesConflictingDiscoveryModes(t *testing.T) {
	makeExecutable, err := exec.LookPath("make")
	if err != nil {
		t.Skip("GNU Make is not available")
	}
	projectDir := t.TempDir()
	makefile := filepath.Join(projectDir, "Makefile")
	if err := os.WriteFile(makefile, []byte("all:\n\tcc -c main.c\nunused:\n\tcc -c never-built.c\n"), 0o644); err != nil {
		t.Fatalf("write Makefile failed: %v", err)
	}

	oldMakePath := makePath
	makePath = makeExecutable
	defer func() { makePath = oldMakePath }()
	for name, arguments := range map[string][]string{
		"question":            {"-q"},
		"touch":               {"-t"},
		"no keep going":       {"-S"},
		"clustered modes":     {"-qtS"},
		"long mode options":   {"--question", "--touch", "--no-keep-going"},
		"print database":      {"-p"},
		"long print database": {"--print-data-base"},
	} {
		t.Run(name, func(t *testing.T) {
			outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
			tool := newTestTool(t, Config{
				BuildDir:     projectDir,
				OutputFile:   outputFile,
				RegexCompile: RegexCompile,
				RegexFile:    RegexFile,
				NoBuild:      true,
				NoStrict:     true,
			})
			tool.MakeWrap(arguments)
			commands := readCompilerTestCommands(t, outputFile)
			if tool.StatusCode != 0 || len(commands) != 1 || commands[0].File != "main.c" {
				t.Fatalf("discovery mode was not enforced: status=%d commands=%#v", tool.StatusCode, commands)
			}
		})
	}
}

func TestMakeWrapSanitizesConflictingEnvironmentModes(t *testing.T) {
	makeExecutable, err := exec.LookPath("make")
	if err != nil {
		t.Skip("GNU Make is not available")
	}
	projectDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(projectDir, "Makefile"), []byte("all:\n\tcc -c main.c\n"), 0o644); err != nil {
		t.Fatalf("write Makefile failed: %v", err)
	}
	oldMakePath := makePath
	makePath = makeExecutable
	defer func() { makePath = oldMakePath }()

	for _, variable := range []string{"MAKEFLAGS", "GNUMAKEFLAGS"} {
		for _, mode := range []string{"q", "t", "p"} {
			t.Run(variable+"="+mode, func(t *testing.T) {
				t.Setenv(variable, mode)
				outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
				tool := newTestTool(t, Config{
					BuildDir:     projectDir,
					OutputFile:   outputFile,
					RegexCompile: RegexCompile,
					RegexFile:    RegexFile,
					NoBuild:      true,
					NoStrict:     true,
				})
				tool.MakeWrap(nil)
				commands := readCompilerTestCommands(t, outputFile)
				if tool.StatusCode != 0 || len(commands) != 1 || commands[0].File != "main.c" {
					t.Fatalf("environment mode was not sanitized: status=%d commands=%#v", tool.StatusCode, commands)
				}
			})
		}
	}
}

func TestMakeWrapPreservesMakeFlagVariableReferences(t *testing.T) {
	makeExecutable, err := exec.LookPath("make")
	if err != nil {
		t.Skip("GNU Make is not available")
	}
	projectDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(projectDir, "Makefile"), []byte("all:\n\tcc -DVALUE=\"$(FOO)\" -c main.c\n"), 0o644); err != nil {
		t.Fatalf("write Makefile failed: %v", err)
	}
	t.Setenv("MAKEFLAGS", "FOO=$$(BAR) BAR=expected")
	oldMakePath := makePath
	makePath = makeExecutable
	defer func() { makePath = oldMakePath }()

	outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
	tool := newTestTool(t, Config{
		BuildDir:     projectDir,
		OutputFile:   outputFile,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
		NoBuild:      true,
		NoStrict:     true,
	})
	tool.MakeWrap(nil)
	commands := readCompilerTestCommands(t, outputFile)
	if tool.StatusCode != 0 || len(commands) != 1 || !slices.Contains(commands[0].Arguments, "-DVALUE=expected") {
		t.Fatalf("MAKEFLAGS variable reference changed: status=%d commands=%#v", tool.StatusCode, commands)
	}
}

func TestMakeWrapPreservesQuotedMakeFlagAssignments(t *testing.T) {
	makeExecutable, err := exec.LookPath("make")
	if err != nil {
		t.Skip("GNU Make is not available")
	}
	projectDir := t.TempDir()
	makefile := "ifeq ($(FOO),'expected')\nSOURCE=preserved.c\nelse\nSOURCE=changed.c\nendif\nall:\n\tcc -c $(SOURCE)\n"
	if err := os.WriteFile(filepath.Join(projectDir, "Makefile"), []byte(makefile), 0o644); err != nil {
		t.Fatalf("write Makefile failed: %v", err)
	}
	t.Setenv("MAKEFLAGS", "FOO='$$b' b=expected")
	oldMakePath := makePath
	makePath = makeExecutable
	defer func() { makePath = oldMakePath }()

	outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
	tool := newTestTool(t, Config{BuildDir: projectDir, OutputFile: outputFile, RegexCompile: RegexCompile, RegexFile: RegexFile, NoBuild: true, NoStrict: true})
	tool.MakeWrap(nil)
	commands := readCompilerTestCommands(t, outputFile)
	if tool.StatusCode != 0 || len(commands) != 1 || commands[0].File != "preserved.c" {
		t.Fatalf("quoted MAKEFLAGS assignment changed: status=%d commands=%#v", tool.StatusCode, commands)
	}
}

func TestMakeWrapDetectsEnvironmentStdinBeforeUnmatchedQuote(t *testing.T) {
	t.Setenv("MAKEFLAGS", "-f - FOO='x")
	tmpDir := t.TempDir()
	makeExecutable := filepath.Join(tmpDir, "fake-make.sh")
	contents := `#!/bin/sh
input=$(cat)
case "$input" in
  *"cc -c unmatched-quote.c"*) echo 'cc -c unmatched-quote.c' ;;
  *) exit 9 ;;
esac
`
	if err := os.WriteFile(makeExecutable, []byte(contents), 0o755); err != nil {
		t.Fatalf("write fake make failed: %v", err)
	}
	stdin, err := os.CreateTemp(tmpDir, "Makefile")
	if err != nil {
		t.Fatalf("create stdin Makefile failed: %v", err)
	}
	if _, err := stdin.WriteString("all:\n\tcc -c unmatched-quote.c\n"); err != nil {
		t.Fatalf("write stdin Makefile failed: %v", err)
	}
	if _, err := stdin.Seek(0, 0); err != nil {
		t.Fatalf("rewind stdin Makefile failed: %v", err)
	}
	defer stdin.Close()
	oldStdin := os.Stdin
	os.Stdin = stdin
	t.Cleanup(func() { os.Stdin = oldStdin })
	oldMakePath := makePath
	makePath = makeExecutable
	defer func() { makePath = oldMakePath }()

	outputFile := filepath.Join(tmpDir, "compile_commands.json")
	tool := newTestTool(t, Config{
		OutputFile:   outputFile,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
		NoBuild:      true,
		NoStrict:     true,
		Encoding:     EncodingRaw,
	})
	tool.MakeWrap(nil)
	commands := readCompilerTestCommands(t, outputFile)
	if tool.StatusCode != 0 || len(commands) != 1 || commands[0].File != "unmatched-quote.c" {
		t.Fatalf("unmatched quote hid stdin Makefile: status=%d commands=%#v", tool.StatusCode, commands)
	}
}

func TestMakeWrapPreservesDirectoryOperandStartingWithModeFlag(t *testing.T) {
	makeExecutable, err := exec.LookPath("make")
	if err != nil {
		t.Skip("GNU Make is not available")
	}
	projectDir := t.TempDir()
	buildDir := filepath.Join(projectDir, "-qdir")
	if err := os.Mkdir(buildDir, 0o755); err != nil {
		t.Fatalf("create build directory failed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(buildDir, "Makefile"), []byte("all:\n\tcc -c operand.c\n"), 0o644); err != nil {
		t.Fatalf("write Makefile failed: %v", err)
	}
	oldMakePath := makePath
	makePath = makeExecutable
	defer func() { makePath = oldMakePath }()

	outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
	tool := newTestTool(t, Config{
		BuildDir:     projectDir,
		OutputFile:   outputFile,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
		NoBuild:      true,
		NoStrict:     true,
	})
	tool.MakeWrap([]string{"-C", "-qdir"})
	commands := readCompilerTestCommands(t, outputFile)
	physicalBuildDir, err := filepath.EvalSymlinks(buildDir)
	if err != nil {
		t.Fatalf("resolve build directory failed: %v", err)
	}
	if tool.StatusCode != 0 || len(commands) != 1 || commands[0].File != "operand.c" ||
		commands[0].Directory != trackedPathToSlash(physicalBuildDir) {
		t.Fatalf("-C operand was changed: status=%d commands=%#v", tool.StatusCode, commands)
	}
}

func TestMakeWrapTracksRecursiveMakeDirectory(t *testing.T) {
	makeExecutable, err := exec.LookPath("make")
	if err != nil {
		t.Skip("GNU Make is not available")
	}
	projectDir := t.TempDir()
	childDir := filepath.Join(projectDir, "child")
	if err := os.Mkdir(childDir, 0o755); err != nil {
		t.Fatalf("create child directory failed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, "Makefile"), []byte("all:\n\t$(MAKE) -C child\n"), 0o644); err != nil {
		t.Fatalf("write parent Makefile failed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(childDir, "Makefile"), []byte("all:\n\tcc -c child.c\n"), 0o644); err != nil {
		t.Fatalf("write child Makefile failed: %v", err)
	}
	oldMakePath := makePath
	makePath = makeExecutable
	defer func() { makePath = oldMakePath }()

	outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
	tool := newTestTool(t, Config{
		BuildDir:     projectDir,
		OutputFile:   outputFile,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
		NoBuild:      true,
		NoStrict:     true,
	})
	tool.MakeWrap(nil)

	commands := readCompilerTestCommands(t, outputFile)
	physicalChildDir, err := filepath.EvalSymlinks(childDir)
	if err != nil {
		t.Fatalf("resolve child directory failed: %v", err)
	}
	if tool.StatusCode != 0 || len(commands) != 1 || commands[0].File != "child.c" ||
		commands[0].Directory != trackedPathToSlash(physicalChildDir) {
		t.Fatalf("recursive Make directory was not tracked: status=%d commands=%#v", tool.StatusCode, commands)
	}
}

func TestMakeWrapForcesDirectoryMarkersForRecursiveMake(t *testing.T) {
	makeExecutable, err := exec.LookPath("make")
	if err != nil {
		t.Skip("GNU Make is not available")
	}
	proxyTempDir := filepath.Join(t.TempDir(), "proxy path '$#")
	if err := os.Mkdir(proxyTempDir, 0o755); err != nil {
		t.Fatalf("create proxy temporary directory failed: %v", err)
	}
	t.Setenv("TMPDIR", proxyTempDir)
	projectDir := t.TempDir()
	for _, directory := range []string{"raylib", "raylib/nested", "tests"} {
		if err := os.MkdirAll(filepath.Join(projectDir, directory), 0o755); err != nil {
			t.Fatalf("create %s directory failed: %v", directory, err)
		}
	}
	parentMakefile := `all:
	"$(MAKE)" --no-print-directory -C raylib
	"$(MAKE)" --no-print-directory -C . root
	"$(MAKE)" --no-print-directory -C tests

root:
	cc -c root.c
`
	raylibMakefile := `all:
	cc -c raylib.c
	"$(MAKE)" --no-print-directory -C nested
`
	if err := os.WriteFile(filepath.Join(projectDir, "Makefile"), []byte(parentMakefile), 0o644); err != nil {
		t.Fatalf("write parent Makefile failed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, "raylib", "Makefile"), []byte(raylibMakefile), 0o644); err != nil {
		t.Fatalf("write raylib Makefile failed: %v", err)
	}
	for directory, source := range map[string]string{
		"raylib/nested": "nested.c",
		"tests":         "tests.c",
	} {
		contents := "all:\n\tcc -c " + source + "\n"
		if err := os.WriteFile(filepath.Join(projectDir, directory, "Makefile"), []byte(contents), 0o644); err != nil {
			t.Fatalf("write %s Makefile failed: %v", directory, err)
		}
	}

	oldMakePath := makePath
	makePath = makeExecutable
	defer func() { makePath = oldMakePath }()
	outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
	tool := newTestTool(t, Config{
		BuildDir:     projectDir,
		OutputFile:   outputFile,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
		NoBuild:      true,
		NoStrict:     true,
	})
	tool.MakeWrap(nil)

	commands := readCompilerTestCommands(t, outputFile)
	wantDirectories := map[string]string{
		"raylib.c": filepath.Join(projectDir, "raylib"),
		"nested.c": filepath.Join(projectDir, "raylib", "nested"),
		"root.c":   projectDir,
		"tests.c":  filepath.Join(projectDir, "tests"),
	}
	if tool.StatusCode != 0 || len(commands) != len(wantDirectories) {
		t.Fatalf("recursive Make discovery failed: status=%d commands=%#v", tool.StatusCode, commands)
	}
	for _, command := range commands {
		want, ok := wantDirectories[command.File]
		if !ok {
			t.Fatalf("unexpected recursive command: %#v", command)
		}
		physical, err := filepath.EvalSymlinks(want)
		if err != nil {
			t.Fatalf("resolve %s directory failed: %v", command.File, err)
		}
		if command.Directory != trackedPathToSlash(physical) {
			t.Fatalf("unexpected %s directory: want %q, got %q", command.File, physical, command.Directory)
		}
	}
}

func TestRecursiveMakeArgumentsForceDirectoryMarkers(t *testing.T) {
	for name, test := range map[string]struct {
		arguments []string
		want      []string
	}{
		"remove full option": {
			arguments: []string{"--no-print-directory", "-C", "child", "all"},
			want:      []string{"--print-directory", "-C", "child", "all"},
		},
		"remove abbreviation": {
			arguments: []string{"--no-print-dir", "all"},
			want:      []string{"--print-directory", "all"},
		},
		"preserve assignment": {
			arguments: []string{"VALUE=--no-print-directory", "all"},
			want:      []string{"--print-directory", "VALUE=--no-print-directory", "all"},
		},
		"preserve invalid attached value": {
			arguments: []string{"--no-print-directory=value", "all"},
			want:      []string{"--print-directory", "--no-print-directory=value", "all"},
		},
		"preserve option operand": {
			arguments: []string{"-f", "--no-print-directory", "all"},
			want:      []string{"--print-directory", "-f", "--no-print-directory", "all"},
		},
		"preserve goal after terminator": {
			arguments: []string{"--", "--no-print-directory"},
			want:      []string{"--print-directory", "--", "--no-print-directory"},
		},
		"terminator used as operand": {
			arguments: []string{"-f", "--", "all"},
			want:      []string{"--print-directory", "-f", "--", "all"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			if got := recursiveMakeArguments(test.arguments); !slices.Equal(got, test.want) {
				t.Fatalf("unexpected recursive Make arguments: want %#v, got %#v", test.want, got)
			}
		})
	}
}

func TestConfigureDiscoveryMakeProxy(t *testing.T) {
	command := exec.Command("/tools/gmake", "all")
	originalPath := command.Path
	originalEnvironment := append([]string(nil), command.Env...)
	cleanup, err := configureDiscoveryMakeProxy(command)
	if err != nil {
		t.Fatalf("configure Make proxy failed: %v", err)
	}
	proxyPath := command.Args[0]
	if command.Path != originalPath {
		t.Fatalf("proxy changed the selected Make executable: %q", command.Path)
	}
	if !shellSafeMakeProxyPath(proxyPath) || executableBase(proxyPath) != "make" ||
		!strings.HasPrefix(filepath.Base(filepath.Dir(proxyPath)), makeProxyDirectoryPrefix) {
		t.Fatalf("unexpected recursive Make proxy: %q", proxyPath)
	}
	proxyExecutable := proxyPath
	metadata, err := os.ReadFile(filepath.Join(filepath.Dir(proxyExecutable), makeProxyMetadataName))
	if err != nil || string(metadata) != originalPath {
		t.Fatalf("unexpected recursive Make metadata: contents=%q err=%v", metadata, err)
	}
	if !slices.Equal(command.Env, originalEnvironment) {
		t.Fatalf("proxy changed the Make environment: %#v", command.Env)
	}
	cleanup()
	if _, err := os.Stat(filepath.Dir(proxyExecutable)); !os.IsNotExist(err) {
		t.Fatalf("proxy directory was not removed: %v", err)
	}
}

func TestCreateDiscoveryMakeProxyUsesAbsolutePath(t *testing.T) {
	processDir := t.TempDir()
	oldWorkingDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("get working directory failed: %v", err)
	}
	if err := os.Chdir(processDir); err != nil {
		t.Fatalf("change working directory failed: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWorkingDir) })
	if err := os.Mkdir("relative-tmp", 0o755); err != nil {
		t.Fatalf("create relative temporary directory failed: %v", err)
	}
	t.Setenv("TMPDIR", "relative-tmp")

	proxyPath, cleanup, err := createDiscoveryMakeProxy("/tools/gmake")
	if runtime.GOOS == "windows" {
		if err == nil {
			cleanup()
			t.Fatalf("relative temporary directory was accepted on Windows: %q", proxyPath)
		}
		return
	}
	if err != nil {
		t.Fatalf("create Make proxy failed: %v", err)
	}
	defer cleanup()
	if !filepath.IsAbs(proxyPath) {
		t.Fatalf("Make proxy path is relative: %q", proxyPath)
	}
	if filepath.Clean(filepath.Dir(filepath.Dir(proxyPath))) != "/tmp" {
		t.Fatalf("relative temporary directory did not fall back to /tmp: %q", proxyPath)
	}
}

func TestMakeWrapProxyPreservesDefaultMakeOrigin(t *testing.T) {
	makeExecutable, err := exec.LookPath("make")
	if err != nil {
		t.Skip("GNU Make is not available")
	}
	projectDir := t.TempDir()
	makefile := `ifeq ($(origin MAKE),default)
SOURCE = default.c
else
SOURCE = changed.c
endif
all:
	cc -c $(SOURCE)
`
	if err := os.WriteFile(filepath.Join(projectDir, "Makefile"), []byte(makefile), 0o644); err != nil {
		t.Fatalf("write Makefile failed: %v", err)
	}
	oldMakePath := makePath
	makePath = makeExecutable
	defer func() { makePath = oldMakePath }()

	outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
	tool := newTestTool(t, Config{
		BuildDir:     projectDir,
		OutputFile:   outputFile,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
		NoBuild:      true,
		NoStrict:     true,
	})
	tool.MakeWrap(nil)
	commands := readCompilerTestCommands(t, outputFile)
	if tool.StatusCode != 0 || len(commands) != 1 || commands[0].File != "default.c" {
		t.Fatalf("proxy changed the default MAKE variable: status=%d commands=%#v", tool.StatusCode, commands)
	}
}

func TestMakeWrapProxyDoesNotLeakEnvironment(t *testing.T) {
	makeExecutable, err := exec.LookPath("make")
	if err != nil {
		t.Skip("GNU Make is not available")
	}
	const proxyEnvironment = "COMPILEDB_INTERNAL_MAKE_PROXY"
	oldValue, existed := os.LookupEnv(proxyEnvironment)
	if err := os.Unsetenv(proxyEnvironment); err != nil {
		t.Fatalf("unset proxy environment failed: %v", err)
	}
	t.Cleanup(func() {
		if existed {
			_ = os.Setenv(proxyEnvironment, oldValue)
		} else {
			_ = os.Unsetenv(proxyEnvironment)
		}
	})

	projectDir := t.TempDir()
	childDir := filepath.Join(projectDir, "child")
	if err := os.Mkdir(childDir, 0o755); err != nil {
		t.Fatalf("create child directory failed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, "Makefile"), []byte("all:\n\t$(MAKE) --no-print-directory -C child\n"), 0o644); err != nil {
		t.Fatalf("write parent Makefile failed: %v", err)
	}
	childMakefile := `ifdef COMPILEDB_INTERNAL_MAKE_PROXY
SOURCE = leaked.c
else
SOURCE = clean.c
endif
all:
	cc -c $(SOURCE)
`
	if err := os.WriteFile(filepath.Join(childDir, "Makefile"), []byte(childMakefile), 0o644); err != nil {
		t.Fatalf("write child Makefile failed: %v", err)
	}
	oldMakePath := makePath
	makePath = makeExecutable
	defer func() { makePath = oldMakePath }()

	outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
	tool := newTestTool(t, Config{
		BuildDir:     projectDir,
		OutputFile:   outputFile,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
		NoBuild:      true,
		NoStrict:     true,
	})
	tool.MakeWrap(nil)
	commands := readCompilerTestCommands(t, outputFile)
	if tool.StatusCode != 0 || len(commands) != 1 || commands[0].File != "clean.c" {
		t.Fatalf("proxy environment leaked into recursive Make: status=%d commands=%#v", tool.StatusCode, commands)
	}
}

func TestMakeWrapProxyPreservesRecursiveEnvironmentOverrides(t *testing.T) {
	makeExecutable, err := exec.LookPath("make")
	if err != nil {
		t.Skip("GNU Make is not available")
	}
	projectDir := t.TempDir()
	childDir := filepath.Join(projectDir, "child")
	grandchildDir := filepath.Join(projectDir, "grandchild")
	for _, directory := range []string{childDir, grandchildDir} {
		if err := os.Mkdir(directory, 0o755); err != nil {
			t.Fatalf("create directory failed: %v", err)
		}
	}
	if err := os.WriteFile(filepath.Join(projectDir, "Makefile"), []byte("all:\n\t$(MAKE) -e --no-print-directory -C child\n"), 0o644); err != nil {
		t.Fatalf("write parent Makefile failed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(childDir, "Makefile"), []byte("MAKE = /bin/false\nall:\n\t$(MAKE) -C ../grandchild\n"), 0o644); err != nil {
		t.Fatalf("write child Makefile failed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(grandchildDir, "Makefile"), []byte("all:\n\tcc -c should-not-run.c\n"), 0o644); err != nil {
		t.Fatalf("write grandchild Makefile failed: %v", err)
	}
	oldMakePath := makePath
	makePath = makeExecutable
	defer func() { makePath = oldMakePath }()

	outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
	tool := newTestTool(t, Config{BuildDir: projectDir, OutputFile: outputFile, NoBuild: true, NoStrict: true})
	tool.MakeWrap(nil)
	if tool.StatusCode == 0 {
		t.Fatal("recursive -e no longer honored the child Makefile's MAKE value")
	}
	if _, err := os.Stat(outputFile); !os.IsNotExist(err) {
		t.Fatalf("failed discovery updated the database: %v", err)
	}
}

func TestMakeWrapProxyPreservesExplicitMakeAssignment(t *testing.T) {
	makeExecutable, err := exec.LookPath("make")
	if err != nil {
		t.Skip("GNU Make is not available")
	}
	projectDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(projectDir, "Makefile"), []byte("all:\n\t$(MAKE) recursive\n\nrecursive:\n\tcc -c should-not-run.c\n"), 0o644); err != nil {
		t.Fatalf("write Makefile failed: %v", err)
	}
	oldMakePath := makePath
	makePath = makeExecutable
	defer func() { makePath = oldMakePath }()

	outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
	tool := newTestTool(t, Config{BuildDir: projectDir, OutputFile: outputFile, NoBuild: true, NoStrict: true})
	tool.MakeWrap([]string{"MAKE:=/bin/false"})
	if tool.StatusCode == 0 {
		t.Fatal("proxy overrode an explicit MAKE assignment")
	}
	if _, err := os.Stat(outputFile); !os.IsNotExist(err) {
		t.Fatalf("failed discovery updated the database: %v", err)
	}
}

func TestMakeWrapProxyUsesSelectedMakeExecutableRecursively(t *testing.T) {
	makeExecutable, err := exec.LookPath("make")
	if err != nil {
		t.Skip("GNU Make is not available")
	}
	projectDir := t.TempDir()
	childDir := filepath.Join(projectDir, "child")
	if err := os.Mkdir(childDir, 0o755); err != nil {
		t.Fatalf("create child directory failed: %v", err)
	}
	makeAlias := filepath.Join(projectDir, "g make'$")
	if err := os.Symlink(makeExecutable, makeAlias); err != nil {
		t.Skipf("create Make alias failed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, "Makefile"), []byte("all:\n\t$(MAKE) --no-print-directory -C child\n"), 0o644); err != nil {
		t.Fatalf("write parent Makefile failed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(childDir, "Makefile"), []byte("all:\n\tcc -c custom.c\n"), 0o644); err != nil {
		t.Fatalf("write child Makefile failed: %v", err)
	}

	outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
	tool := newTestTool(t, Config{
		BuildDir:     projectDir,
		OutputFile:   outputFile,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
		MakeCommand:  makeAlias,
		NoBuild:      true,
		NoStrict:     true,
	})
	tool.MakeWrap(nil)
	commands := readCompilerTestCommands(t, outputFile)
	physicalChildDir, err := filepath.EvalSymlinks(childDir)
	if err != nil {
		t.Fatalf("resolve child directory failed: %v", err)
	}
	if tool.StatusCode != 0 || len(commands) != 1 || commands[0].File != "custom.c" ||
		commands[0].Directory != trackedPathToSlash(physicalChildDir) {
		t.Fatalf("selected Make was not proxied recursively: status=%d commands=%#v", tool.StatusCode, commands)
	}
}

func TestMakeWrapRemovesProxyAfterDiscoveryFailure(t *testing.T) {
	makeExecutable, err := exec.LookPath("make")
	if err != nil {
		t.Skip("GNU Make is not available")
	}
	tempDir := t.TempDir()
	t.Setenv("TMPDIR", tempDir)
	projectDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(projectDir, "Makefile"), []byte("all:\n\t$(MAKE) --no-print-directory missing-target\n"), 0o644); err != nil {
		t.Fatalf("write Makefile failed: %v", err)
	}
	oldMakePath := makePath
	makePath = makeExecutable
	defer func() { makePath = oldMakePath }()

	tool := newTestTool(t, Config{
		BuildDir:   projectDir,
		OutputFile: filepath.Join(t.TempDir(), "compile_commands.json"),
		NoBuild:    true,
		NoStrict:   true,
	})
	tool.MakeWrap(nil)
	if tool.StatusCode == 0 {
		t.Fatal("recursive Make failure was not propagated")
	}
	entries, err := os.ReadDir(tempDir)
	if err != nil {
		t.Fatalf("read temporary directory failed: %v", err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), makeProxyDirectoryPrefix) {
			t.Fatalf("proxy directory was not removed: %q", entry.Name())
		}
	}
}

func TestMakeWrapProxyIsolatesRealAndDiscoveryPhases(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("phase record fixture requires a POSIX shell")
	}
	for _, name := range []string{"MAKE", "MAKE_COMMAND", "MAKEFLAGS", "GNUMAKEFLAGS", "MAKEFILES", "MAKELEVEL", "MFLAGS", "MAKEOVERRIDES"} {
		value, exists := os.LookupEnv(name)
		if err := os.Unsetenv(name); err != nil {
			t.Fatalf("unset %s failed: %v", name, err)
		}
		t.Cleanup(func() {
			if exists {
				_ = os.Setenv(name, value)
			} else {
				_ = os.Unsetenv(name)
			}
		})
	}
	makeExecutable := requireGNUmake(t)
	projectDir := t.TempDir()
	childDir := filepath.Join(projectDir, "child")
	if err := os.Mkdir(childDir, 0o755); err != nil {
		t.Fatalf("create child directory failed: %v", err)
	}
	rootMakefile := "export MAKE\nall:\n\t+printf 'root %s\\n' \"$${MAKE}\" >> make-records\n\t+\"$${MAKE}\" --no-print-directory -C child\n"
	childMakefile := "export MAKE\nall:\n\t+printf 'child %s\\n' \"$${MAKE}\" >> ../make-records\n"
	if err := os.WriteFile(filepath.Join(projectDir, "Makefile"), []byte(rootMakefile), 0o644); err != nil {
		t.Fatalf("write root Makefile failed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(childDir, "Makefile"), []byte(childMakefile), 0o644); err != nil {
		t.Fatalf("write child Makefile failed: %v", err)
	}

	tool := newTestTool(t, Config{
		BuildDir:     projectDir,
		OutputFile:   filepath.Join(t.TempDir(), "compile_commands.json"),
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
		MakeCommand:  makeExecutable,
		NoStrict:     true,
		Encoding:     EncodingRaw,
	})
	tool.MakeWrap(nil)
	if tool.StatusCode != 0 {
		t.Fatalf("MakeWrap failed: %d", tool.StatusCode)
	}
	data, err := os.ReadFile(filepath.Join(projectDir, "make-records"))
	if err != nil {
		t.Fatalf("read Make phase records failed: %v", err)
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if len(lines) != 4 {
		t.Fatalf("unexpected Make phase record count: %q", data)
	}
	labels := []string{"root", "child", "root", "child"}
	paths := make([]string, len(lines))
	for index, line := range lines {
		label, path, ok := strings.Cut(line, " ")
		if !ok || label != labels[index] {
			t.Fatalf("unexpected Make phase records: %q", data)
		}
		paths[index] = path
	}
	if paths[0] != makeExecutable || paths[1] != makeExecutable {
		t.Fatalf("real Make did not retain its executable: %q", data)
	}
	for _, proxy := range paths[2:] {
		if proxy == makeExecutable || executableBase(proxy) != "make" || !strings.HasPrefix(filepath.Base(filepath.Dir(proxy)), makeProxyDirectoryPrefix) {
			t.Fatalf("discovery Make did not use its isolated proxy: %q", data)
		}
	}
}

func requireGNUmake(t *testing.T) string {
	t.Helper()
	for _, name := range []string{"make", "gmake", "mingw32-make"} {
		executable, err := exec.LookPath(name)
		if err != nil {
			continue
		}
		command := exec.Command(executable, "--version")
		command.Env = makeTestEnvironment()
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

func makeTestEnvironment() []string {
	environment := make([]string, 0, len(os.Environ()))
	for _, variable := range os.Environ() {
		name, _, _ := strings.Cut(variable, "=")
		switch strings.ToUpper(name) {
		case "MAKE", "MAKE_COMMAND", "MAKEFLAGS", "GNUMAKEFLAGS", "MAKEFILES", "MAKELEVEL", "MFLAGS", "MAKEOVERRIDES":
			continue
		}
		environment = append(environment, variable)
	}
	return environment
}

func TestMakeWrapTracksGeneratedDryRunDirectory(t *testing.T) {
	tmpDir := t.TempDir()
	makeScript := filepath.Join(tmpDir, "fake-make.sh")
	generatedDir := filepath.Join(tmpDir, "generated")
	contents := "#!/bin/sh\necho 'mkdir -p " + filepath.Join(generatedDir, "sub") + "; cd " + generatedDir + " && cc -c ../main.c'\n"
	if err := os.WriteFile(makeScript, []byte(contents), 0o755); err != nil {
		t.Fatalf("write fake make failed: %v", err)
	}
	oldMakePath := makePath
	makePath = makeScript
	defer func() { makePath = oldMakePath }()

	outputFile := filepath.Join(tmpDir, "compile_commands.json")
	tool := newTestTool(t, Config{
		BuildDir:     tmpDir,
		OutputFile:   outputFile,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
		NoBuild:      true,
		NoStrict:     true,
	})
	tool.MakeWrap(nil)
	commands := readCompilerTestCommands(t, outputFile)
	if tool.StatusCode != 0 || len(commands) != 1 || commands[0].Directory != trackedPathToSlash(generatedDir) {
		t.Fatalf("generated dry-run directory was not tracked: status=%d commands=%#v", tool.StatusCode, commands)
	}
}

func TestMakeWrapProvidesStdinMakefileToBothCommands(t *testing.T) {
	tmpDir := t.TempDir()
	makeScript := filepath.Join(tmpDir, "fake-make.sh")
	contents := `#!/bin/sh
input=$(cat)
case "$input" in
  *"cc -c stdin.c"*) ;;
  *) exit 9 ;;
esac
case " $* " in
  *" -Bnkw "*) echo 'cc -c stdin.c' ;;
esac
`
	if err := os.WriteFile(makeScript, []byte(contents), 0o755); err != nil {
		t.Fatalf("write fake make failed: %v", err)
	}
	stdin, err := os.CreateTemp(tmpDir, "Makefile")
	if err != nil {
		t.Fatalf("create stdin Makefile failed: %v", err)
	}
	if _, err := stdin.WriteString("all:\n\tcc -c stdin.c\n"); err != nil {
		t.Fatalf("write stdin Makefile failed: %v", err)
	}
	if _, err := stdin.Seek(0, 0); err != nil {
		t.Fatalf("rewind stdin Makefile failed: %v", err)
	}
	defer stdin.Close()
	oldStdin := os.Stdin
	os.Stdin = stdin
	t.Cleanup(func() { os.Stdin = oldStdin })
	oldMakePath := makePath
	makePath = makeScript
	defer func() { makePath = oldMakePath }()

	outputFile := filepath.Join(tmpDir, "compile_commands.json")
	tool := newTestTool(t, Config{
		OutputFile:   outputFile,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
		NoStrict:     true,
		Encoding:     EncodingRaw,
	})
	tool.MakeWrap([]string{"-f", "-"})
	commands := readCompilerTestCommands(t, outputFile)
	if tool.StatusCode != 0 || len(commands) != 1 || commands[0].File != "stdin.c" {
		t.Fatalf("stdin Makefile was not provided independently: status=%d commands=%#v", tool.StatusCode, commands)
	}
}

func TestMakeWrapProvidesEnvironmentStdinMakefileToBothCommands(t *testing.T) {
	t.Setenv("MAKEFLAGS", "-f -")
	tmpDir := t.TempDir()
	makeScript := filepath.Join(tmpDir, "fake-make.sh")
	contents := `#!/bin/sh
input=$(cat)
case "$input" in
  *"cc -c env-stdin.c"*) ;;
  *) exit 9 ;;
esac
case " $* " in
  *" -Bnkw "*) echo 'cc -c env-stdin.c' ;;
esac
`
	if err := os.WriteFile(makeScript, []byte(contents), 0o755); err != nil {
		t.Fatalf("write fake make failed: %v", err)
	}
	stdin, err := os.CreateTemp(tmpDir, "Makefile")
	if err != nil {
		t.Fatalf("create stdin Makefile failed: %v", err)
	}
	if _, err := stdin.WriteString("all:\n\tcc -c env-stdin.c\n"); err != nil {
		t.Fatalf("write stdin Makefile failed: %v", err)
	}
	if _, err := stdin.Seek(0, 0); err != nil {
		t.Fatalf("rewind stdin Makefile failed: %v", err)
	}
	defer stdin.Close()
	oldStdin := os.Stdin
	os.Stdin = stdin
	t.Cleanup(func() { os.Stdin = oldStdin })
	oldMakePath := makePath
	makePath = makeScript
	defer func() { makePath = oldMakePath }()

	outputFile := filepath.Join(tmpDir, "compile_commands.json")
	tool := newTestTool(t, Config{OutputFile: outputFile, RegexCompile: RegexCompile, RegexFile: RegexFile, NoStrict: true, Encoding: EncodingRaw})
	tool.MakeWrap(nil)
	commands := readCompilerTestCommands(t, outputFile)
	if tool.StatusCode != 0 || len(commands) != 1 || commands[0].File != "env-stdin.c" {
		t.Fatalf("environment stdin Makefile was not provided independently: status=%d commands=%#v", tool.StatusCode, commands)
	}
}

func TestMakeWrapStatusPrecedence(t *testing.T) {
	for name, test := range map[string]struct {
		realExit int
		dryExit  int
		noBuild  bool
		want     int
	}{
		"real failure wins":                    {realExit: 7, dryExit: 2, want: 7},
		"dry failure follows successful build": {realExit: 0, dryExit: 2, want: 2},
		"no-build dry failure":                 {dryExit: 2, noBuild: true, want: 2},
	} {
		t.Run(name, func(t *testing.T) {
			tmpDir := t.TempDir()
			script := filepath.Join(tmpDir, "fake-make.sh")
			contents := `#!/bin/sh
case " $* " in
  *" -Bnkw "*) exit ` + strconv.Itoa(test.dryExit) + ` ;;
  *) exit ` + strconv.Itoa(test.realExit) + ` ;;
esac
`
			if err := os.WriteFile(script, []byte(contents), 0o755); err != nil {
				t.Fatalf("write fake make failed: %v", err)
			}
			oldMakePath := makePath
			makePath = script
			defer func() { makePath = oldMakePath }()

			tool := newTestTool(t, Config{
				OutputFile: filepath.Join(tmpDir, "compile_commands.json"),
				NoBuild:    test.noBuild,
				NoStrict:   true,
				Encoding:   EncodingRaw,
			})
			tool.MakeWrap(nil)
			if tool.StatusCode != test.want {
				t.Fatalf("unexpected status: want %d, got %d", test.want, tool.StatusCode)
			}
		})
	}
}

func TestMakeWrapRunsDiscoveryAfterSuccessfulBuild(t *testing.T) {
	tmpDir := t.TempDir()
	buildFinished := filepath.Join(tmpDir, "build-finished")
	script := filepath.Join(tmpDir, "fake-make.sh")
	contents := `#!/bin/sh
case " $* " in
  *" -Bnkw "*)
    test -f ` + ShellJoinArgs([]string{buildFinished}) + ` || exit 9
    echo 'gcc -c ordered.c'
    ;;
  *)
    sleep 0.1
    touch ` + ShellJoinArgs([]string{buildFinished}) + `
    ;;
esac
`
	if err := os.WriteFile(script, []byte(contents), 0o755); err != nil {
		t.Fatalf("write fake make failed: %v", err)
	}
	oldMakePath := makePath
	makePath = script
	defer func() { makePath = oldMakePath }()

	outputFile := filepath.Join(tmpDir, "compile_commands.json")
	tool := newTestTool(t, Config{
		OutputFile:   outputFile,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
		NoStrict:     true,
		Encoding:     EncodingRaw,
	})
	tool.MakeWrap(nil)

	commands := readCompilerTestCommands(t, outputFile)
	if tool.StatusCode != 0 || len(commands) != 1 || commands[0].File != "ordered.c" {
		t.Fatalf("discovery ran before successful build: status=%d commands=%#v", tool.StatusCode, commands)
	}
}

func TestMakeWrapDoesNotRunDiscoveryAfterBuildFailure(t *testing.T) {
	tmpDir := t.TempDir()
	discoveryStarted := filepath.Join(tmpDir, "discovery-started")
	script := filepath.Join(tmpDir, "fake-make.sh")
	contents := `#!/bin/sh
case " $* " in
  *" -Bnkw "*)
    touch ` + ShellJoinArgs([]string{discoveryStarted}) + `
    echo 'gcc -c should-not-exist.c'
    ;;
  *) exit 7 ;;
esac
`
	if err := os.WriteFile(script, []byte(contents), 0o755); err != nil {
		t.Fatalf("write fake make failed: %v", err)
	}
	oldMakePath := makePath
	makePath = script
	defer func() { makePath = oldMakePath }()

	outputFile := filepath.Join(tmpDir, "compile_commands.json")
	tool := newTestTool(t, Config{
		OutputFile: outputFile,
		NoStrict:   true,
		Encoding:   EncodingRaw,
	})
	tool.MakeWrap(nil)

	if tool.StatusCode != 7 {
		t.Fatalf("unexpected build failure status: %d", tool.StatusCode)
	}
	if _, err := os.Stat(discoveryStarted); !os.IsNotExist(err) {
		t.Fatalf("discovery ran after build failure: %v", err)
	}
	if _, err := os.Stat(outputFile); !os.IsNotExist(err) {
		t.Fatalf("build failure wrote compilation database: %v", err)
	}
}

func TestMakeOutputIncompleteIsVisibleAtDefaultLevel(t *testing.T) {
	logger := logrus.New()
	logger.SetLevel(logrus.ErrorLevel)
	var output bytes.Buffer
	logger.SetOutput(&output)

	if !reportMakeOutputError(logger, errProcessOutputIncomplete) {
		t.Fatal("incomplete Make output was not recognized")
	}
	if !strings.Contains(output.String(), "make output incomplete") {
		t.Fatalf("incomplete Make output was hidden at ErrorLevel: %q", output.String())
	}
}

func TestMakeWrapKeepsSuccessfulDryRunStatusWhenOutputIsIncomplete(t *testing.T) {
	tmpDir := t.TempDir()
	makeScript := filepath.Join(tmpDir, "fake-make.sh")
	contents := "#!/bin/sh\necho 'gcc -c main.c'\nsleep 30 &\n"
	if err := os.WriteFile(makeScript, []byte(contents), 0o755); err != nil {
		t.Fatalf("write fake make failed: %v", err)
	}
	oldMakePath := makePath
	makePath = makeScript
	defer func() { makePath = oldMakePath }()

	var logs bytes.Buffer
	tool := newTestTool(t, Config{
		OutputFile:   filepath.Join(tmpDir, "compile_commands.json"),
		NoBuild:      true,
		NoStrict:     true,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
	})
	tool.Logger.SetLevel(logrus.ErrorLevel)
	tool.Logger.SetOutput(&logs)
	tool.MakeWrap(nil)

	commands := readCompilerTestCommands(t, tool.Config.OutputFile)
	if tool.StatusCode != 0 || len(commands) != 1 || commands[0].File != "main.c" {
		t.Fatalf("incomplete successful dry run changed result: status=%d commands=%#v", tool.StatusCode, commands)
	}
	if !strings.Contains(logs.String(), "make output incomplete") {
		t.Fatalf("incomplete dry-run output was not diagnosed: %q", logs.String())
	}
}

func TestMakeWrapPropagatesParserCancellation(t *testing.T) {
	tmpDir := t.TempDir()
	startedFile := filepath.Join(tmpDir, "backtick.started")
	makeScript := filepath.Join(tmpDir, "fake-make.sh")
	nested := "printf started > " + ShellJoinArgs([]string{startedFile}) + "; exec sleep 30"
	contents := "#!/bin/sh\necho 'gcc -I`" + nested + "` -c main.c'\n"
	if err := os.WriteFile(makeScript, []byte(contents), 0o755); err != nil {
		t.Fatalf("write fake make failed: %v", err)
	}
	oldMakePath := makePath
	makePath = makeScript
	defer func() { makePath = oldMakePath }()

	ctx, cancel := context.WithCancel(context.Background())
	tool := newTestTool(t, Config{
		OutputFile:   filepath.Join(tmpDir, "compile_commands.json"),
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
		NoBuild:      true,
		NoStrict:     true,
	})
	tool.Context = ctx
	done := make(chan struct{})
	go func() {
		tool.MakeWrap(nil)
		close(done)
	}()
	waitForTestFile(t, startedFile)
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("MakeWrap parser did not return after cancellation")
	}
	if tool.StatusCode == 0 {
		t.Fatal("MakeWrap lost the parser cancellation status")
	}
}

func TestMakeWrapGeneratesSourceCommandsWithoutCompileOnlyFlag(t *testing.T) {
	tmpDir := t.TempDir()
	outputFile := filepath.Join(tmpDir, "compile_commands.json")
	script := filepath.Join(tmpDir, "fake-make.sh")

	if err := os.WriteFile(script, []byte("#!/bin/sh\necho 'cc -o app a.c b.c'\n"), 0o755); err != nil {
		t.Fatalf("write fake make failed: %v", err)
	}

	oldMakePath := makePath
	makePath = script
	defer func() { makePath = oldMakePath }()

	tool := newTestTool(t, Config{
		OutputFile:   outputFile,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
		NoBuild:      true,
		NoStrict:     true,
	})
	tool.MakeWrap(nil)

	if tool.StatusCode != 0 {
		t.Fatalf("expected status code 0, got %d", tool.StatusCode)
	}
	commands := readCompilerTestCommands(t, outputFile)
	if len(commands) != 2 || commands[0].File != "a.c" || commands[1].File != "b.c" {
		t.Fatalf("expected one entry per source, got %#v", commands)
	}
}

func TestMakeWrapExpandsCompilerResponseFile(t *testing.T) {
	projectDir := t.TempDir()
	outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
	responseFile := filepath.Join(projectDir, "arguments.rsp")
	makeScript := filepath.Join(projectDir, "fake-make.sh")
	if err := os.WriteFile(responseFile, []byte("-c src/main.c"), 0o644); err != nil {
		t.Fatalf("write response file failed: %v", err)
	}
	if err := os.WriteFile(makeScript, []byte("#!/bin/sh\necho 'gcc @arguments.rsp'\n"), 0o755); err != nil {
		t.Fatalf("write fake make failed: %v", err)
	}

	oldMakePath := makePath
	makePath = makeScript
	defer func() { makePath = oldMakePath }()
	tool := newTestTool(t, Config{
		OutputFile:   outputFile,
		BuildDir:     projectDir,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
		NoBuild:      true,
		NoStrict:     true,
	})
	tool.MakeWrap(nil)

	commands := readCompilerTestCommands(t, outputFile)
	if tool.StatusCode != 0 || len(commands) != 1 || commands[0].File != "src/main.c" ||
		commands[0].Directory != trackedPathToSlash(projectDir) || slices.Contains(commands[0].Arguments, "@arguments.rsp") {
		t.Fatalf("Make response file was not expanded: status=%d commands=%#v", tool.StatusCode, commands)
	}
}

func TestMakeWrapResponseFileFailureIsRecoverable(t *testing.T) {
	projectDir := t.TempDir()
	outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
	makeScript := filepath.Join(projectDir, "fake-make.sh")
	if err := os.WriteFile(filepath.Join(projectDir, "arguments.rsp"), []byte("-DSECRET=value 'unterminated"), 0o644); err != nil {
		t.Fatalf("write response file failed: %v", err)
	}
	if err := os.WriteFile(makeScript, []byte("#!/bin/sh\necho 'gcc @arguments.rsp -c hidden.c'\necho 'cc -c valid.c'\n"), 0o755); err != nil {
		t.Fatalf("write fake make failed: %v", err)
	}

	oldMakePath := makePath
	makePath = makeScript
	defer func() { makePath = oldMakePath }()
	var logs bytes.Buffer
	tool := newTestTool(t, Config{
		OutputFile:   outputFile,
		BuildDir:     projectDir,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
		NoBuild:      true,
		NoStrict:     true,
	})
	tool.Logger.SetLevel(logrus.ErrorLevel)
	tool.Logger.SetOutput(&logs)
	tool.MakeWrap(nil)

	commands := readCompilerTestCommands(t, outputFile)
	if tool.StatusCode != 0 || len(commands) != 1 || commands[0].File != "valid.c" {
		t.Fatalf("Make response failure stopped parsing: status=%d commands=%#v", tool.StatusCode, commands)
	}
	diagnostic := logs.String()
	if !strings.Contains(diagnostic, "response file") || !strings.Contains(diagnostic, "unterminated quote") ||
		!strings.Contains(diagnostic, "build log line 1") {
		t.Fatalf("response-file failure was not logged by MakeWrap: %q", diagnostic)
	}
	if strings.Contains(diagnostic, "SECRET") || strings.Contains(diagnostic, "hidden.c") {
		t.Fatalf("Make response diagnostic exposed contents: %q", diagnostic)
	}
}

func TestMakeWrapRecoversFromParserCommandFailures(t *testing.T) {
	tmpDir := t.TempDir()
	outputFile := filepath.Join(tmpDir, "compile_commands.json")
	script := filepath.Join(tmpDir, "fake-make.sh")
	contents := `#!/bin/sh
printf "%s\n" "gcc -c 'broken.c"
printf "%s\n" 'gcc -I` + "`false`" + ` -c backtick.c'
printf "%s\n" 'gcc -c valid.c'
`
	if err := os.WriteFile(script, []byte(contents), 0o755); err != nil {
		t.Fatalf("write fake make failed: %v", err)
	}

	oldMakePath := makePath
	makePath = script
	defer func() { makePath = oldMakePath }()
	tool := newTestTool(t, Config{
		OutputFile:   outputFile,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
		NoBuild:      true,
		NoStrict:     true,
	})

	tool.MakeWrap(nil)

	commands := readCompilerTestCommands(t, outputFile)
	if tool.StatusCode != 0 || len(commands) != 1 || commands[0].File != "valid.c" {
		t.Fatalf("recoverable Make parser failure stopped parsing: status=%d commands=%#v", tool.StatusCode, commands)
	}
}

func TestMakeWrapNoBuildAddsPredefinedMacrosAndArguments(t *testing.T) {
	tmpDir := t.TempDir()
	outputFile := filepath.Join(tmpDir, "compile_commands.json")
	compiler := filepath.Join(tmpDir, "fake-gcc")
	makeScript := filepath.Join(tmpDir, "fake-make.sh")

	if err := os.WriteFile(compiler, []byte("#!/bin/sh\necho '#define FROM_MAKE 1'\n"), 0o755); err != nil {
		t.Fatalf("write fake compiler failed: %v", err)
	}
	makeContents := "#!/bin/sh\necho '" + compiler + " -c src/main.c'\n"
	if err := os.WriteFile(makeScript, []byte(makeContents), 0o755); err != nil {
		t.Fatalf("write fake make failed: %v", err)
	}

	oldMakePath := makePath
	makePath = makeScript
	defer func() { makePath = oldMakePath }()

	tool := newTestTool(t, Config{
		OutputFile:   outputFile,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
		NoBuild:      true,
		NoStrict:     true,
		Macros:       true,
		AddArgs:      []string{"-DADDED=1"},
	})
	tool.MakeWrap(nil)
	if tool.StatusCode != 0 {
		t.Fatalf("expected status code 0, got %d", tool.StatusCode)
	}

	data, err := os.ReadFile(outputFile)
	if err != nil {
		t.Fatalf("read output failed: %v", err)
	}
	var commands []Command
	if err := json.Unmarshal(data, &commands); err != nil {
		t.Fatalf("decode output failed: %v", err)
	}
	if len(commands) != 1 ||
		!slices.Contains(commands[0].Arguments, "-DFROM_MAKE=1") ||
		!slices.Contains(commands[0].Arguments, "-DADDED=1") {
		t.Fatalf("unexpected commands: %#v", commands)
	}
}

func TestMakeWrapRedirectsBuildOutputWhenDatabaseUsesStdout(t *testing.T) {
	tmpDir := t.TempDir()
	makeScript := filepath.Join(tmpDir, "fake-make.sh")
	contents := `#!/bin/sh
case " $* " in
  *" -Bnkw "*) echo 'gcc -c main.c' ;;
  *) echo 'real build output' ;;
esac
`
	if err := os.WriteFile(makeScript, []byte(contents), 0o755); err != nil {
		t.Fatalf("write fake make failed: %v", err)
	}

	oldMakePath := makePath
	makePath = makeScript
	defer func() { makePath = oldMakePath }()

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

	tool := newTestTool(t, Config{
		OutputFile:   "-",
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
		NoStrict:     true,
	})
	tool.MakeWrap(nil)
	_ = stdoutW.Close()
	_ = stderrW.Close()

	stdout, err := io.ReadAll(stdoutR)
	if err != nil {
		t.Fatalf("read stdout failed: %v", err)
	}
	stderr, err := io.ReadAll(stderrR)
	if err != nil {
		t.Fatalf("read stderr failed: %v", err)
	}
	var commands []Command
	if err := json.Unmarshal(stdout, &commands); err != nil {
		t.Fatalf("stdout is not valid JSON: %q: %v", stdout, err)
	}
	if len(commands) != 1 || !strings.Contains(string(stderr), "real build output") {
		t.Fatalf("unexpected output: commands=%#v stderr=%q", commands, stderr)
	}
}

func TestMakeWrapDoesNotWaitForBackgroundProcessOutput(t *testing.T) {
	tmpDir := t.TempDir()
	makeScript := filepath.Join(tmpDir, "fake-make.sh")
	buildPID := filepath.Join(tmpDir, "build.pid")
	contents := `#!/bin/sh
case " $* " in
  *" -Bnkw "*)
    echo 'gcc -c main.c'
	    exit 0
    ;;
  *)
	    sleep 30 &
	    echo $! > "` + buildPID + `"
	    ;;
esac
`
	if err := os.WriteFile(makeScript, []byte(contents), 0o755); err != nil {
		t.Fatalf("write fake make failed: %v", err)
	}
	for _, pidFile := range []string{buildPID} {
		pidFile := pidFile
		t.Cleanup(func() {
			data, err := os.ReadFile(pidFile)
			if err != nil {
				return
			}
			pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
			if err != nil {
				return
			}
			if process, err := os.FindProcess(pid); err == nil {
				_ = process.Kill()
			}
		})
	}

	oldMakePath := makePath
	makePath = makeScript
	defer func() { makePath = oldMakePath }()
	oldStdout := os.Stdout
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		t.Fatalf("create stdout pipe failed: %v", err)
	}
	os.Stdout = stdoutW
	t.Cleanup(func() { os.Stdout = oldStdout })

	tool := newTestTool(t, Config{
		OutputFile: filepath.Join(tmpDir, "compile_commands.json"),
		Encoding:   EncodingRaw,
		NoStrict:   true,
	})
	start := time.Now()
	tool.MakeWrap(nil)
	if elapsed := time.Since(start); elapsed > 4*time.Second {
		t.Fatalf("make wrapper waited for inherited output pipes: %v", elapsed)
	}
	if tool.StatusCode != 0 {
		t.Fatalf("successful make must keep status 0 when inherited output is incomplete, got %d", tool.StatusCode)
	}
	_ = stdoutW.Close()
	readDone := make(chan error, 1)
	go func() {
		_, err := io.ReadAll(stdoutR)
		readDone <- err
	}()
	select {
	case err := <-readDone:
		if err != nil {
			t.Fatalf("read stdout failed: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("background process kept the caller's stdout pipe open")
	}
}

func TestMakeWrapPreservesFailureWithBackgroundProcess(t *testing.T) {
	tmpDir := t.TempDir()
	makeScript := filepath.Join(tmpDir, "fake-make.sh")
	backgroundPID := filepath.Join(tmpDir, "background.pid")
	contents := `#!/bin/sh
case " $* " in
  *" -Bnkw "*) echo 'gcc -c main.c' ;;
  *)
    sleep 30 &
    echo $! > "` + backgroundPID + `"
    exit 7
    ;;
esac
`
	if err := os.WriteFile(makeScript, []byte(contents), 0o755); err != nil {
		t.Fatalf("write fake make failed: %v", err)
	}
	t.Cleanup(func() {
		data, err := os.ReadFile(backgroundPID)
		if err != nil {
			return
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
		if err != nil {
			return
		}
		if process, err := os.FindProcess(pid); err == nil {
			_ = process.Kill()
		}
	})

	oldMakePath := makePath
	makePath = makeScript
	defer func() { makePath = oldMakePath }()

	tool := newTestTool(t, Config{
		OutputFile: filepath.Join(tmpDir, "compile_commands.json"),
		Encoding:   EncodingRaw,
		NoStrict:   true,
	})
	start := time.Now()
	tool.MakeWrap(nil)
	if elapsed := time.Since(start); elapsed > 4*time.Second {
		t.Fatalf("make wrapper waited for inherited output pipes: %v", elapsed)
	}
	if tool.StatusCode != 7 {
		t.Fatalf("expected status code 7, got %d", tool.StatusCode)
	}
}

func TestMakeWrapDecodesGB18030Output(t *testing.T) {
	tmpDir := t.TempDir()
	makeScript := filepath.Join(tmpDir, "fake-make.sh")
	contents := `#!/bin/sh
case " $* " in
  *" -Bnkw "*) echo 'gcc -c main.c' ;;
  *) printf '\304\343\272\303\n' ;;
esac
`
	if err := os.WriteFile(makeScript, []byte(contents), 0o755); err != nil {
		t.Fatalf("write fake make failed: %v", err)
	}

	oldMakePath := makePath
	makePath = makeScript
	defer func() { makePath = oldMakePath }()

	oldStdout := os.Stdout
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		t.Fatalf("create stdout pipe failed: %v", err)
	}
	os.Stdout = stdoutW
	t.Cleanup(func() { os.Stdout = oldStdout })

	tool := newTestTool(t, Config{
		OutputFile: filepath.Join(tmpDir, "compile_commands.json"),
		Encoding:   EncodingGB18030,
		NoStrict:   true,
	})
	tool.MakeWrap(nil)
	_ = stdoutW.Close()
	stdout, err := io.ReadAll(stdoutR)
	if err != nil {
		t.Fatalf("read stdout failed: %v", err)
	}
	if string(stdout) != "你好\n" {
		t.Fatalf("unexpected decoded output: %q", stdout)
	}
}
