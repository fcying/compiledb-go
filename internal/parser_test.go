package internal

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mattn/go-shellwords"
	"github.com/sirupsen/logrus"
)

func TestParseMergesTrailingContinuation(t *testing.T) {
	tmpDir := t.TempDir()
	outputFile := filepath.Join(tmpDir, "compile_commands.json")

	tool := newTestTool(t, Config{
		InputFile:    "stdin",
		OutputFile:   outputFile,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
		NoStrict:     true,
	})

	tool.Parse([]string{
		"gcc -DMODE=1 \\",
		"-c src/main.c",
	})

	data, err := os.ReadFile(outputFile)
	if err != nil {
		t.Fatalf("read output failed: %v", err)
	}
	got := string(data)
	if got == "[]" || got == "[]\n" {
		t.Fatalf("expected at least one command, got %q", got)
	}
}

func TestParsePreservesContinuationShellSemantics(t *testing.T) {
	for name, test := range map[string]struct {
		lines          []string
		wantFile       string
		wantArgument   string
		wantCommands   int
		wantDiagnostic string
	}{
		"concatenates words": {
			lines:        []string{"gcc -DNAME=foo\\", "bar -c concat.c"},
			wantFile:     "concat.c",
			wantArgument: "-DNAME=foobar",
			wantCommands: 1,
		},
		"preserves quoted whitespace": {
			lines:        []string{`gcc "-DNAME=foo\`, ` bar" -c quoted.c`},
			wantFile:     "quoted.c",
			wantArgument: "-DNAME=foo bar",
			wantCommands: 1,
		},
		"does not continue even backslashes": {
			lines:        []string{`gcc -DVALUE=foo\\`, `-c even.c`},
			wantCommands: 0,
		},
		"does not continue after trailing space": {
			lines:        []string{"gcc -DVALUE=foo\\  ", `-c spaced.c`},
			wantCommands: 0,
		},
		"blank physical line ends continuation": {
			lines:        []string{"gcc -DVALUE=foo\\", "", `-c blank.c`},
			wantCommands: 0,
		},
		"comment backslash does not continue": {
			lines:        []string{"gcc -DVALUE=foo # comment\\", `-c comment.c`},
			wantCommands: 0,
		},
		"continues after escaped space before hash": {
			lines:        []string{`gcc -DVALUE=foo\ #bar \`, `-c escaped-comment.c`},
			wantFile:     "escaped-comment.c",
			wantArgument: "-DVALUE=foo #bar",
			wantCommands: 1,
		},
		"preserves word state across physical lines": {
			lines:        []string{`gcc -DVALUE=foo\`, `#bar \`, `-c continued-word.c`},
			wantFile:     "continued-word.c",
			wantArgument: "-DVALUE=foo#bar",
			wantCommands: 1,
		},
		"drops unterminated continuation": {
			lines:          []string{"gcc -c incomplete.c\\"},
			wantCommands:   0,
			wantDiagnostic: "unterminated line continuation",
		},
	} {
		t.Run(name, func(t *testing.T) {
			outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
			var logs bytes.Buffer
			tool := newTestTool(t, Config{
				InputFile:    "stdin",
				OutputFile:   outputFile,
				RegexCompile: RegexCompile,
				RegexFile:    RegexFile,
				NoStrict:     true,
			})
			tool.Logger.SetOutput(&logs)
			tool.Parse(test.lines)

			commands := readCompilerTestCommands(t, outputFile)
			if len(commands) != test.wantCommands {
				t.Fatalf("unexpected commands: want %d, got %#v", test.wantCommands, commands)
			}
			if test.wantCommands == 1 {
				if commands[0].File != test.wantFile || !slices.Contains(commands[0].Arguments, test.wantArgument) {
					t.Fatalf("continuation changed command: %#v", commands[0])
				}
			}
			if test.wantDiagnostic != "" && !strings.Contains(logs.String(), test.wantDiagnostic) {
				t.Fatalf("missing diagnostic %q in %q", test.wantDiagnostic, logs.String())
			}
			if tool.StatusCode != 0 {
				t.Fatalf("recoverable continuation failure changed status: %d", tool.StatusCode)
			}
		})
	}
}

func TestParsePhysicalLineLimit(t *testing.T) {
	outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
	buildDir := t.TempDir()
	var logs bytes.Buffer
	tool := newTestTool(t, Config{
		InputFile:    "stdin",
		OutputFile:   outputFile,
		BuildDir:     buildDir,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
		NoStrict:     true,
	})
	tool.buildLogLineLimit = 32
	tool.Logger.SetLevel(logrus.ErrorLevel)
	tool.Logger.SetOutput(&logs)
	atLimit := "cc -c at-limit.c"
	atLimit += strings.Repeat(" ", tool.buildLogLineLimit-len(atLimit))

	tool.Parse([]string{
		atLimit,
		"cc -DINTERRUPTED=1 \\",
		strings.Repeat("x", 33),
		"cc -c valid.c",
		strings.Repeat("x", 32) + "\\",
		"cc -c joined.c",
		"cc -c after.c",
	})

	commands := readCompilerTestCommands(t, outputFile)
	if tool.StatusCode != 0 || len(commands) != 3 || commands[0].File != "at-limit.c" ||
		commands[1].File != "valid.c" || commands[2].File != "after.c" {
		t.Fatalf("physical line limit stopped parsing: status=%d commands=%#v", tool.StatusCode, commands)
	}
	diagnostic := logs.String()
	for _, want := range []string{"build log line 3", trackedPathToSlash(buildDir), "physical line exceeds 32 byte limit", "at byte 32"} {
		if !strings.Contains(diagnostic, want) {
			t.Fatalf("physical line diagnostic lacks %q: %q", want, diagnostic)
		}
	}
	if strings.Contains(diagnostic, strings.Repeat("x", 16)) {
		t.Fatalf("physical line diagnostic exposed line contents: %q", diagnostic)
	}
}

func TestParseCommandStyleQuotesArguments(t *testing.T) {
	tmpDir := t.TempDir()
	outputFile := filepath.Join(tmpDir, "compile_commands.json")

	tool := newTestTool(t, Config{
		InputFile:    "stdin",
		OutputFile:   outputFile,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
		NoStrict:     true,
		CommandStyle: true,
	})

	tool.Parse([]string{`clang -DNAME="hello world" -c "src dir/test space.c" -o "obj/test space.o"`})

	data, err := os.ReadFile(outputFile)
	if err != nil {
		t.Fatalf("read output failed: %v", err)
	}

	var commands []Command
	if err := json.Unmarshal(data, &commands); err != nil {
		t.Fatalf("unmarshal output failed: %v", err)
	}

	if len(commands) != 1 {
		t.Fatalf("expected 1 command, got %d", len(commands))
	}

	want := `clang '-DNAME=hello world' -c 'src dir/test space.c' -o 'obj/test space.o'`
	if commands[0].Command != want {
		t.Fatalf("unexpected command output\nwant: %q\ngot:  %q", want, commands[0].Command)
	}
}

func TestParsePreservesQuotedShellSeparators(t *testing.T) {
	outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
	tool := newTestTool(t, Config{
		InputFile:    "stdin",
		OutputFile:   outputFile,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
		NoStrict:     true,
	})

	tool.Parse([]string{`gcc '-DNAME=a;b' '-DALT=x&&y||z' -c semicolon.c`})
	commands := readCompilerTestCommands(t, outputFile)
	if len(commands) != 1 || !slices.Contains(commands[0].Arguments, "-DNAME=a;b") ||
		!slices.Contains(commands[0].Arguments, "-DALT=x&&y||z") {
		t.Fatalf("quoted shell separator was split: %#v", commands)
	}
}

func TestParseRunsBackticksInTrackedWorkingDirectory(t *testing.T) {
	workingDir := t.TempDir()
	outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
	tool := newTestTool(t, Config{
		InputFile:    "stdin",
		OutputFile:   outputFile,
		BuildDir:     workingDir,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
		NoStrict:     true,
	})

	tool.Parse([]string{"gcc -I`pwd` -c main.c"})
	commands := readCompilerTestCommands(t, outputFile)
	want := "-I" + hostPathToDatabasePath(workingDir)
	if len(commands) != 1 || !slices.Contains(commands[0].Arguments, want) {
		t.Fatalf("backtick command used the wrong working directory: want %q, got %#v", want, commands)
	}
}

func TestParseExpandsBackticksBeforeInlineCD(t *testing.T) {
	projectDir := t.TempDir()
	childDir := filepath.Join(projectDir, "sub")
	if err := os.Mkdir(childDir, 0o755); err != nil {
		t.Fatalf("create child directory failed: %v", err)
	}
	outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
	tool := newTestTool(t, Config{
		OutputFile:   outputFile,
		BuildDir:     projectDir,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
		NoStrict:     true,
	})

	tool.Parse([]string{"cd `printf sub` && gcc -c main.c"})

	commands := readCompilerTestCommands(t, outputFile)
	if len(commands) != 1 || commands[0].Directory != trackedPathToSlash(childDir) {
		t.Fatalf("backtick-driven cd was not tracked: %#v", commands)
	}
}

func TestParseCancelsBacktickCommand(t *testing.T) {
	workingDir := t.TempDir()
	startedFile := filepath.Join(workingDir, "backtick.started")
	outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
	ctx, cancel := context.WithCancel(context.Background())
	tool := newTestTool(t, Config{
		InputFile:    "stdin",
		OutputFile:   outputFile,
		BuildDir:     workingDir,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
		NoStrict:     true,
	})
	tool.Context = ctx
	nested := "printf started > " + ShellJoinArgs([]string{startedFile}) + "; exec sleep 30"
	done := make(chan struct{})
	go func() {
		tool.Parse([]string{"gcc -I`" + nested + "` -c main.c"})
		close(done)
	}()

	waitForTestFile(t, startedFile)
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("backtick command did not return after cancellation")
	}
	if tool.StatusCode == 0 {
		t.Fatal("canceled backtick parse returned a successful status")
	}
	if _, err := os.Stat(outputFile); !os.IsNotExist(err) {
		t.Fatalf("canceled backtick parse wrote a compilation database: %v", err)
	}
}

func TestParseBackticksDoNotRequireExternalShell(t *testing.T) {
	workingDir := t.TempDir()
	outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
	var logs bytes.Buffer
	tool := newTestTool(t, Config{
		InputFile:    "stdin",
		OutputFile:   outputFile,
		BuildDir:     workingDir,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
		NoStrict:     true,
	})
	tool.Logger.SetLevel(logrus.ErrorLevel)
	tool.Logger.SetOutput(&logs)
	t.Setenv("PATH", t.TempDir())

	tool.Parse([]string{
		"gcc -I`pwd` -DVALUE=`printf pure-go` -c builtin.c",
		"cc -c valid.c",
	})

	commands := readCompilerTestCommands(t, outputFile)
	if len(commands) != 2 || commands[0].File != "builtin.c" || commands[1].File != "valid.c" ||
		!slices.Contains(commands[0].Arguments, "-I"+hostPathToDatabasePath(workingDir)) ||
		!slices.Contains(commands[0].Arguments, "-DVALUE=pure-go") {
		t.Fatalf("embedded shell produced unexpected commands: %#v", commands)
	}
	if logs.Len() != 0 {
		t.Fatalf("embedded shell emitted an unexpected diagnostic: %q", logs.String())
	}
	if tool.StatusCode != 0 {
		t.Fatalf("embedded shell changed parser status to %d", tool.StatusCode)
	}
}

func TestParseReportsMissingBacktickExecutable(t *testing.T) {
	outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
	var logs bytes.Buffer
	tool := newTestTool(t, Config{
		InputFile:    "stdin",
		OutputFile:   outputFile,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
		NoStrict:     true,
	})
	tool.Logger.SetOutput(&logs)
	t.Setenv("PATH", t.TempDir())

	tool.Parse([]string{
		"gcc -I`compiledb-missing-backtick-executable` -c skipped.c",
		"cc -c valid.c",
	})

	commands := readCompilerTestCommands(t, outputFile)
	if len(commands) != 1 || commands[0].File != "valid.c" {
		t.Fatalf("missing backtick executable did not skip only the affected command: %#v", commands)
	}
	if !strings.Contains(logs.String(), "Error executing nested command") ||
		!strings.Contains(logs.String(), "exit status 127") {
		t.Fatalf("missing backtick execution diagnostic: %q", logs.String())
	}
	if tool.StatusCode != 0 {
		t.Fatalf("recoverable backtick execution failure changed parser status to %d", tool.StatusCode)
	}
}

func TestParseRespectsQuotedCommandSubstitutions(t *testing.T) {
	projectDir := t.TempDir()
	marker := filepath.Join(projectDir, "executed")
	outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
	tool := newTestTool(t, Config{
		InputFile:    "stdin",
		OutputFile:   outputFile,
		BuildDir:     projectDir,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
		NoStrict:     true,
	})

	tool.Parse([]string{
		"gcc '-DVALUE=`touch " + marker + "`' -c literal.c",
		`gcc "-I$(pwd)" -c unsupported.c`,
		`gcc '-DVALUE=$(pwd)' -c single-quoted.c`,
	})
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("single-quoted backtick was executed: %v", err)
	}
	commands := readCompilerTestCommands(t, outputFile)
	if len(commands) != 2 || commands[0].File != "literal.c" || commands[1].File != "single-quoted.c" {
		t.Fatalf("quoted substitutions were handled incorrectly: %#v", commands)
	}
}

func TestParseDoesNotReevaluateBacktickOutput(t *testing.T) {
	projectDir := t.TempDir()
	marker := filepath.Join(projectDir, "executed-twice")
	outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
	tool := newTestTool(t, Config{
		InputFile:    "stdin",
		OutputFile:   outputFile,
		BuildDir:     projectDir,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
		NoStrict:     true,
	})

	tool.Parse([]string{`gcc -DVALUE=` + "`printf '\\140touch " + marker + "\\140'`" + ` -c main.c`})
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("backtick output was evaluated as another command: %v", err)
	}
}

func TestParsePreservesBacktickOutputAsData(t *testing.T) {
	projectDir := t.TempDir()
	outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
	tool := newTestTool(t, Config{
		InputFile:    "stdin",
		OutputFile:   outputFile,
		BuildDir:     projectDir,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
		NoStrict:     true,
	})
	nested := `printf '  a\047b\134c  '`

	tool.Parse([]string{
		"gcc -DVALUE=`" + nested + "` -c unquoted.c",
		"gcc \"-DVALUE=`" + nested + "`\" -c quoted.c",
	})

	commands := readCompilerTestCommands(t, outputFile)
	if len(commands) != 2 || !slices.Contains(commands[0].Arguments, `-DVALUE=a'b\c`) ||
		!slices.Contains(commands[1].Arguments, "-DVALUE=  a'b\\c  ") {
		t.Fatalf("backtick output changed shell lexical state: %#v", commands)
	}
}

func TestParsePreservesEscapedQuotes(t *testing.T) {
	outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
	tool := newTestTool(t, Config{
		InputFile:    "stdin",
		OutputFile:   outputFile,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
		NoStrict:     true,
	})
	tool.Parse([]string{`gcc -DSTR=\"quoted\" -c main.c`})

	commands := readCompilerTestCommands(t, outputFile)
	if len(commands) != 1 || !slices.Contains(commands[0].Arguments, `-DSTR="quoted"`) {
		t.Fatalf("escaped quotes were changed: %#v", commands)
	}
}

func TestParsePreservesDoubleQuotedBackslashes(t *testing.T) {
	outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
	tool := newTestTool(t, Config{
		InputFile:    "stdin",
		OutputFile:   outputFile,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
		NoStrict:     true,
	})
	tool.Parse([]string{`gcc "-DREGEX=\d+" "-DQUOTE=\"value\"" -c main.c`})

	commands := readCompilerTestCommands(t, outputFile)
	if len(commands) != 1 || !slices.Contains(commands[0].Arguments, `-DREGEX=\d+`) ||
		!slices.Contains(commands[0].Arguments, `-DQUOTE="value"`) {
		t.Fatalf("double-quoted backslashes were changed: %#v", commands)
	}
}

func TestParseHonorsKnownConditionalBranches(t *testing.T) {
	projectDir := t.TempDir()
	subDir := filepath.Join(projectDir, "sub")
	if err := os.Mkdir(subDir, 0o755); err != nil {
		t.Fatalf("create subdirectory failed: %v", err)
	}
	for _, filename := range []string{"failed-cd-fallback.c", "true-and.c"} {
		if err := os.WriteFile(filepath.Join(projectDir, filename), nil, 0o644); err != nil {
			t.Fatalf("create source %s failed: %v", filename, err)
		}
	}
	outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
	tool := newTestTool(t, Config{
		InputFile:    "stdin",
		OutputFile:   outputFile,
		BuildDir:     projectDir,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
	})

	tool.Parse([]string{
		"false && gcc -c false-and.c",
		"true || gcc -c true-or.c",
		"cd sub || gcc -c successful-cd-fallback.c",
		"cd missing || gcc -c failed-cd-fallback.c",
		"true && gcc -c true-and.c",
	})
	commands := readCompilerTestCommands(t, outputFile)
	if len(commands) != 2 || commands[0].File != "failed-cd-fallback.c" ||
		commands[0].Directory != trackedPathToSlash(projectDir) || commands[1].File != "true-and.c" {
		t.Fatalf("conditional branches were parsed incorrectly: %#v", commands)
	}
}

func TestParseEvaluatesShellASTConservatively(t *testing.T) {
	outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
	tool := newTestTool(t, Config{
		InputFile:    "stdin",
		OutputFile:   outputFile,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
		NoStrict:     true,
	})

	tool.Parse([]string{
		"false && gcc -c skipped-and.c || gcc -c false-and-or.c",
		"true || gcc -c skipped-or.c && gcc -c true-or-and.c",
		"unknown-check && gcc -c unknown-and.c",
		"unknown-check || gcc -c unknown-or.c",
		"unknown-check && gcc -c skipped-unknown.c; gcc -c sequential.c",
		"false && gcc -c skipped-nested.c || false || gcc -c nested-fallback.c",
		"unknown-check || true && gcc -c absorbed-or.c",
		"unknown-check && false || gcc -c absorbed-and.c",
		"gcc -c first.c || true && gcc -c after-compiler.c",
		"printf '中文'; gcc -c unicode-offset.c",
	})

	commands := readCompilerTestCommands(t, outputFile)
	files := make([]string, 0, len(commands))
	for _, command := range commands {
		files = append(files, command.File)
	}
	want := []string{
		"false-and-or.c",
		"true-or-and.c",
		"sequential.c",
		"nested-fallback.c",
		"absorbed-or.c",
		"absorbed-and.c",
		"first.c",
		"after-compiler.c",
		"unicode-offset.c",
	}
	if !slices.Equal(files, want) {
		t.Fatalf("shell AST branches were evaluated incorrectly:\nwant: %v\ngot:  %v", want, files)
	}
}

func TestParseHandlesAssignmentPrefixedCommands(t *testing.T) {
	projectDir := t.TempDir()
	outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
	tool := newTestTool(t, Config{
		InputFile:    "stdin",
		OutputFile:   outputFile,
		BuildDir:     projectDir,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
		NoStrict:     true,
	})

	tool.Parse([]string{
		"MODE=release gcc -c compiler.c",
		"MODE=release true ignored && gcc -c true.c",
		"MODE=release false ignored || gcc -c false.c",
	})

	commands := readCompilerTestCommands(t, outputFile)
	if len(commands) != 3 {
		t.Fatalf("assignment-prefixed commands changed command count: %#v", commands)
	}
	wantFiles := []string{"compiler.c", "true.c", "false.c"}
	for index, want := range wantFiles {
		if commands[index].File != want {
			t.Fatalf("assignment-prefixed command %d: want %q, got %#v", index, want, commands)
		}
	}
	for _, command := range commands {
		if command.Directory != trackedPathToSlash(projectDir) {
			t.Fatalf("assignment-prefixed command changed cwd: %#v", commands)
		}
	}
}

func TestParseRejectsAssignmentPrefixedTrackedState(t *testing.T) {
	projectDir := t.TempDir()
	outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
	tool := newTestTool(t, Config{
		InputFile:    "stdin",
		OutputFile:   outputFile,
		BuildDir:     projectDir,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
		NoStrict:     true,
	})

	tool.Parse([]string{
		"CDPATH=/ cd tmp; gcc -c cd.c",
		"PATH=/nonexistent make -C /forged",
		"MODE=release mkdir -p virtual; cd virtual; gcc -c mkdir.c",
		"CDPATH=/ :; cd tmp; gcc -c colon.c",
		"gcc -c parent.c",
	})

	commands := readCompilerTestCommands(t, outputFile)
	if len(commands) != 1 || commands[0].File != "parent.c" ||
		commands[0].Directory != trackedPathToSlash(projectDir) {
		t.Fatalf("assignment-prefixed tracked state changed cwd: %#v", commands)
	}
}

func TestParseRejectsDynamicTrackedDirectories(t *testing.T) {
	projectDir := t.TempDir()
	outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
	tool := newTestTool(t, Config{
		InputFile:    "stdin",
		OutputFile:   outputFile,
		BuildDir:     projectDir,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
		NoStrict:     true,
	})

	tool.Parse([]string{
		`cd "$DIR"; gcc -c variable.c`,
		`cd ~/sub; gcc -c tilde.c`,
		`cd sub*; gcc -c glob.c`,
		`cd ""; gcc -c empty.c`,
		`cd -; gcc -c previous.c`,
		`make -C "$DIR"`,
		`make -C sub "$TARGET"`,
		`make -C sub *`,
		"gcc -c parent.c",
	})

	commands := readCompilerTestCommands(t, outputFile)
	if len(commands) != 1 || commands[0].File != "parent.c" ||
		commands[0].Directory != trackedPathToSlash(projectDir) {
		t.Fatalf("dynamic tracked directory was treated as a literal path: %#v", commands)
	}
}

func TestParseRejectsUnsafeStaticShellAnalysis(t *testing.T) {
	projectDir := t.TempDir()
	marker := filepath.Join(projectDir, "unsafe-shell-executed")
	outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
	tool := newTestTool(t, Config{
		InputFile:    "stdin",
		OutputFile:   outputFile,
		BuildDir:     projectDir,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
		NoStrict:     true,
	})

	nested := "printf marker > " + ShellJoinArgs([]string{marker})
	tool.Parse([]string{
		"e\\xit 0; gcc -I`" + nested + "` -c escaped-exit.c",
		"MODE=${COMPILEDB_AST_UNSET:?stop} true; gcc -I`" + nested + "` -c fatal-expansion.c",
		`foo\make -C /forged; true`,
		"gcc -c parent.c",
	})

	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("unsupported shell line executed backtick: %v", err)
	}
	commands := readCompilerTestCommands(t, outputFile)
	if len(commands) != 1 || commands[0].File != "parent.c" ||
		commands[0].Directory != trackedPathToSlash(projectDir) {
		t.Fatalf("unsafe shell analysis changed parser state: %#v", commands)
	}
}

func TestParseStopsAfterUncertainTrackedState(t *testing.T) {
	projectDir := t.TempDir()
	outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
	tool := newTestTool(t, Config{
		InputFile:    "stdin",
		OutputFile:   outputFile,
		BuildDir:     projectDir,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
		NoStrict:     true,
	})

	tool.Parse([]string{
		"unknown-check || cd sub; gcc -c uncertain-cd.c",
		"unknown-check || make -C child; gcc -c uncertain-make.c",
		"unknown-check || cd sub && false || gcc -c absorbed-after-cd.c",
		"gcc -c parent.c",
	})

	commands := readCompilerTestCommands(t, outputFile)
	if len(commands) != 1 || commands[0].File != "parent.c" ||
		commands[0].Directory != trackedPathToSlash(projectDir) {
		t.Fatalf("uncertain tracked state leaked into later commands: %#v", commands)
	}
}

func TestParseFailsClosedForComplexShellStructures(t *testing.T) {
	outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
	tool := newTestTool(t, Config{
		InputFile:    "stdin",
		OutputFile:   outputFile,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
		NoStrict:     true,
	})

	tool.Parse([]string{
		`{ cd sub; gcc -c brace.c; }`,
		`build() { :; gcc -c function.c; }`,
		`if false; then :; gcc -c conditional.c; fi`,
		`case value in value) gcc -c case.c;; esac`,
		`while false; do gcc -c while.c; done; gcc -c while-tail.c`,
		`for value in one; do gcc -c for.c; done; gcc -c for-tail.c`,
		`true | gcc -c pipeline.c; gcc -c pipeline-tail.c`,
		`true & gcc -c background.c`,
		`! false; gcc -c negated.c`,
		`exit 0; gcc -c exit.c`,
		`exec true; gcc -c exec.c`,
		`eval 'true'; gcc -c eval.c`,
		`. ./settings; gcc -c dot.c`,
		`source ./settings; gcc -c source.c`,
		`echo $(printf '); gcc -c substitution.c;')`,
		`echo $((1 + 2)); gcc -c arithmetic.c`,
		`( true;# comment ); gcc -c grouped-comment.c`,
		`true ># comment; gcc -c redirected-comment.c`,
		`true 2># comment; gcc -c fd-comment.c`,
		`gcc -c valid.c`,
	})

	commands := readCompilerTestCommands(t, outputFile)
	if len(commands) != 1 || commands[0].File != "valid.c" {
		t.Fatalf("complex shell structure leaked commands: %#v", commands)
	}
}

func TestParseContinuesPastUnrelatedRedirectedCommands(t *testing.T) {
	projectDir := t.TempDir()
	outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
	tool := newTestTool(t, Config{
		InputFile:    "stdin",
		OutputFile:   outputFile,
		BuildDir:     projectDir,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
		NoStrict:     true,
		MakeCommand:  "build-tool",
	})

	tool.Parse([]string{
		`printf 'building' >&2; gcc -c after-prefix.c`,
		`gcc -c redirected-compiler.c >/dev/null; gcc -c after-compiler.c`,
		`gcc -c before-quoted.c; 'FOO=bar' >/dev/null; gcc -c after-quoted.c`,
		`cd sub >/dev/null; gcc -c uncertain-cwd.c`,
		`gcc -c before-uncertain.c; cd sub >/dev/null; gcc -c after-uncertain.c`,
		`gcc -c before-glob.c; c? sub >/dev/null; gcc -c after-glob.c`,
		`gcc -c before-tilde.c; ~tool >/dev/null; gcc -c after-tilde.c`,
		`gcc -c before-make.c; make -C sub >/dev/null; gcc -c after-make.c`,
		`gcc -c before-configured-make.c; build-tool -C sub >/dev/null; gcc -c after-configured-make.c`,
		`gcc -c before-mkdir.c; mkdir -p sub >/dev/null; gcc -c after-mkdir.c`,
		`gcc -c before-conditional.c; printf x >/dev/null && gcc -c after-conditional.c`,
		`gcc -c before-pipeline.c; printf x >/dev/null | cat; gcc -c after-pipeline.c`,
		`gcc -c before-group.c; { printf x >/dev/null; }; gcc -c after-group.c`,
		`gcc -c before-dynamic-redir.c; printf x >"$OUTPUT"; gcc -c after-dynamic-redir.c`,
		`gcc -c before-fatal-redir.c; printf x >${COMPILEDB_REDIRECT_UNSET:?stop}; gcc -c after-fatal-redir.c`,
		`gcc -c before-invalid-fd.c; printf x >&not-a-fd; gcc -c after-invalid-fd.c`,
		`gcc -c before-colon.c; : >/definitely/missing/path; gcc -c after-colon.c`,
		`gcc -c before-times.c; times >/definitely/missing/path; gcc -c after-times.c`,
		`tool\? >/dev/null; gcc -c escaped-command.c`,
		`printf x >out\*; gcc -c escaped-redir.c`,
		`gcc -c parent.c`,
	})

	commands := readCompilerTestCommands(t, outputFile)
	wantFiles := []string{
		"after-prefix.c",
		"after-compiler.c",
		"before-quoted.c",
		"after-quoted.c",
		"escaped-command.c",
		"escaped-redir.c",
		"parent.c",
	}
	if len(commands) != len(wantFiles) {
		t.Fatalf("redirected command changed command count: %#v", commands)
	}
	for index, want := range wantFiles {
		if commands[index].File != want || commands[index].Directory != trackedPathToSlash(projectDir) {
			t.Fatalf("redirected command %d: want %q in parent cwd, got %#v", index, want, commands)
		}
	}
}

func TestParseStopsAtShellComments(t *testing.T) {
	outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
	tool := newTestTool(t, Config{
		InputFile:    "stdin",
		OutputFile:   outputFile,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
		NoStrict:     true,
	})

	tool.Parse([]string{
		`gcc -c commented-marker.c # make: Entering directory '/tmp'`,
		`gcc -c following.c`,
		`make -C sub && # incomplete conditional`,
		`gcc -c skipped.c && # incomplete conditional`,
		`gcc -c main.c # generated`,
		`gcc -DVALUE='# literal' -c quoted.c # initialized with {0}`,
		`gcc -DVALUE=foo#bar -c embedded.c`,
		`gcc -DVALUE=foo\ #bar -c escaped-space.c`,
		`gcc -DVALUE=foo\;#bar -c escaped-semicolon.c`,
	})

	commands := readCompilerTestCommands(t, outputFile)
	if len(commands) != 7 {
		t.Fatalf("shell comments changed command count: %#v", commands)
	}
	wantArguments := [][]string{
		{"gcc", "-c", "commented-marker.c"},
		{"gcc", "-c", "following.c"},
		{"gcc", "-c", "main.c"},
		{"gcc", "-DVALUE=# literal", "-c", "quoted.c"},
		{"gcc", "-DVALUE=foo#bar", "-c", "embedded.c"},
		{"gcc", "-DVALUE=foo #bar", "-c", "escaped-space.c"},
		{"gcc", "-DVALUE=foo;#bar", "-c", "escaped-semicolon.c"},
	}
	workingDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("get working directory failed: %v", err)
	}
	for i, want := range wantArguments {
		if !slices.Equal(commands[i].Arguments, want) {
			t.Fatalf("unexpected arguments for %s:\nwant: %v\ngot:  %v", commands[i].File, want, commands[i].Arguments)
		}
		if commands[i].Directory != trackedPathToSlash(workingDir) {
			t.Fatalf("commented Make command changed cwd to %q", commands[i].Directory)
		}
	}
}

func TestParseBuildLogFixture(t *testing.T) {
	fixture := filepath.Join("..", "tests", "build.log")
	outputFile := filepath.Join(t.TempDir(), "compile_commands.json")

	tool := newTestTool(t, Config{
		InputFile:    fixture,
		OutputFile:   outputFile,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
		NoStrict:     true,
	})
	tool.Generate()

	data, err := os.ReadFile(outputFile)
	if err != nil {
		t.Fatalf("read output failed: %v", err)
	}

	var commands []Command
	if err := json.Unmarshal(data, &commands); err != nil {
		t.Fatalf("unmarshal output failed: %v", err)
	}

	if len(commands) != 9 {
		t.Fatalf("expected 9 unique commands, got %d", len(commands))
	}

	type expectedCommand struct {
		directory string
		file      string
	}

	checks := []expectedCommand{
		{directory: "/opt/compiledb_test", file: "src/test1.c"},
		{directory: "/opt/compiledb_test/src", file: "src dir/test space.c"},
		{directory: "/opt/compiledb_test/sub", file: "nested/sub_file.c"},
		{directory: "/opt/compiledb_test/build", file: "../quoted-name.c"},
		{directory: "/opt/compiledb_test/relative-build", file: "src3.cc"},
	}

	passed := 0
	failed := []string{}

	for _, check := range checks {
		found := false
		for _, cmd := range commands {
			if cmd.Directory == check.directory && cmd.File == check.file {
				found = true
				break
			}
		}

		if found {
			passed++
			continue
		}

		failed = append(failed, fmt.Sprintf("directory=%s file=%s", check.directory, check.file))
	}

	t.Logf("fixture checks: passed=%d failed=%d total=%d", passed, len(failed), len(checks))
	if len(failed) > 0 {
		t.Fatalf("fixture checks: passed=%d failed=%d total=%d\nfailed entries:\n%s", passed, len(failed), len(checks), strings.Join(failed, "\n"))
	}
}

func TestWriteJSONWritesEmptyArrayForZeroCommands(t *testing.T) {
	tmpDir := t.TempDir()
	outputFile := filepath.Join(tmpDir, "compile_commands.json")

	if err := os.WriteFile(outputFile, []byte("stale"), 0o644); err != nil {
		t.Fatalf("seed output failed: %v", err)
	}

	tool := newTestTool(t, Config{OutputFile: outputFile})
	commands := []Command{}
	tool.WriteJSON(outputFile, 0, &commands)

	data, err := os.ReadFile(outputFile)
	if err != nil {
		t.Fatalf("read output failed: %v", err)
	}
	if string(data) != "[]\n" {
		t.Fatalf("expected empty JSON array, got %q", string(data))
	}
}

func TestConfigureMakeFilterMatchesOnlyVariableExpansionCheck(t *testing.T) {
	for name, test := range map[string]struct {
		line string
		want bool
	}{
		"yes":            {line: "checking whether make sets $(MAKE)... yes", want: true},
		"no":             {line: "checking whether gmake sets $(MAKE)... no", want: true},
		"ordinary check": {line: "checking whether the compiler works... yes"},
		"suffix text":    {line: "checking whether the compiler plays piano"},
	} {
		t.Run(name, func(t *testing.T) {
			if got := checkingMake.MatchString(test.line); got != test.want {
				t.Fatalf("unexpected configure filter result for %q: want %t, got %t", test.line, test.want, got)
			}
		})
	}
}

func TestParseWritesEmptyArrayWhenNoCommandsFound(t *testing.T) {
	tmpDir := t.TempDir()
	outputFile := filepath.Join(tmpDir, "compile_commands.json")

	tool := newTestTool(t, Config{
		InputFile:    "stdin",
		OutputFile:   outputFile,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
		NoStrict:     true,
	})

	tool.Parse([]string{"echo not-a-compile-command"})

	data, err := os.ReadFile(outputFile)
	if err != nil {
		t.Fatalf("read output failed: %v", err)
	}
	if string(data) != "[]\n" {
		t.Fatalf("expected empty JSON array, got %q", string(data))
	}
}

func TestParseIgnoresCompilerLineWithoutSourceFile(t *testing.T) {
	tmpDir := t.TempDir()
	outputFile := filepath.Join(tmpDir, "compile_commands.json")

	tool := newTestTool(t, Config{
		InputFile:    "stdin",
		OutputFile:   outputFile,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
		NoStrict:     true,
	})

	tool.Parse([]string{"gcc -v"})

	data, err := os.ReadFile(outputFile)
	if err != nil {
		t.Fatalf("read output failed: %v", err)
	}
	if string(data) != "[]\n" {
		t.Fatalf("expected empty JSON array, got %q", string(data))
	}
}

func TestParseGeneratesEntriesForSourceFilesWithoutCompileOnlyFlag(t *testing.T) {
	tmpDir := t.TempDir()
	outputFile := filepath.Join(tmpDir, "compile_commands.json")

	tool := newTestTool(t, Config{
		InputFile:    "stdin",
		OutputFile:   outputFile,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
		NoStrict:     true,
	})

	tool.Parse([]string{"cc -Iinc -o app a.c b.c -lm"})

	commands := readCompilerTestCommands(t, outputFile)
	if len(commands) != 2 || commands[0].File != "a.c" || commands[1].File != "b.c" {
		t.Fatalf("expected one entry per source, got %#v", commands)
	}
}

func TestParseExpandsCompilerResponseFiles(t *testing.T) {
	projectDir := t.TempDir()
	buildDir := filepath.Join(projectDir, "build")
	sourceDir := filepath.Join(projectDir, "src")
	if err := os.MkdirAll(buildDir, 0o755); err != nil {
		t.Fatalf("create build directory failed: %v", err)
	}
	if err := os.MkdirAll(sourceDir, 0o755); err != nil {
		t.Fatalf("create source directory failed: %v", err)
	}
	for _, name := range []string{"main.c", "second.cpp", "after.c"} {
		if err := os.WriteFile(filepath.Join(sourceDir, name), nil, 0o644); err != nil {
			t.Fatalf("create source %s failed: %v", name, err)
		}
	}
	if err := os.WriteFile(filepath.Join(buildDir, "nested.rsp"), []byte("../src/second.cpp"), 0o644); err != nil {
		t.Fatalf("write nested response file failed: %v", err)
	}
	response := "-include fake.c -MF deps.c -target x86_64 -c ../src/main.c @nested.rsp -- ../src/after.c"
	if err := os.WriteFile(filepath.Join(buildDir, "arguments.rsp"), []byte(response), 0o644); err != nil {
		t.Fatalf("write response file failed: %v", err)
	}

	outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
	tool := newTestTool(t, Config{
		InputFile:    "stdin",
		OutputFile:   outputFile,
		BuildDir:     projectDir,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
		AddArgs:      []string{"-DADDED=1"},
	})
	tool.Parse([]string{"env -C build ccache clang @arguments.rsp"})

	commands := readCompilerTestCommands(t, outputFile)
	wantFiles := []string{"../src/main.c", "../src/second.cpp", "../src/after.c"}
	if len(commands) != len(wantFiles) {
		t.Fatalf("expected one entry per response-file source, got %#v", commands)
	}
	for index, command := range commands {
		if command.File != wantFiles[index] || command.Directory != trackedPathToSlash(buildDir) {
			t.Fatalf("unexpected response-file entry %d: %#v", index, command)
		}
		if slices.ContainsFunc(command.Arguments, func(argument string) bool { return strings.HasPrefix(argument, "@") }) {
			t.Fatalf("response file was not flattened: %v", command.Arguments)
		}
		if !slices.Contains(command.Arguments, "--target=x86_64") {
			t.Fatalf("response argument was not normalized: %v", command.Arguments)
		}
		added := slices.Index(command.Arguments, "-DADDED=1")
		terminator := slices.Index(command.Arguments, "--")
		if added < 0 || terminator <= added {
			t.Fatalf("added argument was not inserted before response terminator: %v", command.Arguments)
		}
	}
}

func TestParseAppliesWorkingDirectoryFromResponseFile(t *testing.T) {
	projectDir := t.TempDir()
	sourceDir := filepath.Join(projectDir, "src")
	if err := os.Mkdir(sourceDir, 0o755); err != nil {
		t.Fatalf("create source directory failed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sourceDir, "main.c"), nil, 0o644); err != nil {
		t.Fatalf("create source failed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, "arguments.rsp"), []byte("-working-directory src -c main.c"), 0o644); err != nil {
		t.Fatalf("write response file failed: %v", err)
	}

	outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
	tool := newTestTool(t, Config{
		InputFile:    "stdin",
		OutputFile:   outputFile,
		BuildDir:     projectDir,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
	})
	tool.Parse([]string{"gcc @arguments.rsp"})

	commands := readCompilerTestCommands(t, outputFile)
	if len(commands) != 1 || commands[0].File != "main.c" || commands[0].Directory != trackedPathToSlash(sourceDir) ||
		!slices.Contains(commands[0].Arguments, trackedPathToSlash(sourceDir)) {
		t.Fatalf("response working directory was not applied: %#v", commands)
	}
}

func TestParseResponseFileFailureIsRecoverable(t *testing.T) {
	projectDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(projectDir, "malformed.rsp"), []byte("-DSECRET=value 'unterminated"), 0o644); err != nil {
		t.Fatalf("write malformed response file failed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, "cycle.rsp"), []byte("@cycle.rsp"), 0o644); err != nil {
		t.Fatalf("write cyclic response file failed: %v", err)
	}

	var logs bytes.Buffer
	outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
	tool := newTestTool(t, Config{
		InputFile:    "stdin",
		OutputFile:   outputFile,
		BuildDir:     projectDir,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
		NoStrict:     true,
	})
	tool.Logger.SetLevel(logrus.ErrorLevel)
	tool.Logger.SetOutput(&logs)
	tool.Parse([]string{
		"gcc @missing.rsp -c outside.c",
		"gcc @malformed.rsp -c outside.c",
		"gcc @cycle.rsp -c outside.c",
		"cc -c valid.c",
	})

	commands := readCompilerTestCommands(t, outputFile)
	if tool.StatusCode != 0 || len(commands) != 1 || commands[0].File != "valid.c" {
		t.Fatalf("response failure stopped parsing: status=%d commands=%#v", tool.StatusCode, commands)
	}
	diagnostic := logs.String()
	for _, want := range []string{"build log line 1", "build log line 2", "build log line 3", "response file", "unterminated quote", "at byte", "recursive expansion", "cwd"} {
		if !strings.Contains(diagnostic, want) {
			t.Fatalf("response diagnostic lacks %q: %q", want, diagnostic)
		}
	}
	if strings.Contains(diagnostic, "SECRET") || strings.Contains(diagnostic, "outside.c") {
		t.Fatalf("response diagnostic exposed command or file contents: %q", diagnostic)
	}
}

func TestParseResponseFileOutputLimitIncludesFinalArguments(t *testing.T) {
	projectDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(projectDir, "arguments.rsp"), []byte(strings.Repeat("source.c ", 100)), 0o644); err != nil {
		t.Fatalf("write response file failed: %v", err)
	}
	var logs bytes.Buffer
	outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
	tool := newTestTool(t, Config{
		InputFile:    "stdin",
		OutputFile:   outputFile,
		BuildDir:     projectDir,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
		AddArgs:      []string{"-DVALUE=" + strings.Repeat("x", 1024*1024)},
		NoStrict:     true,
	})
	tool.Logger.SetLevel(logrus.ErrorLevel)
	tool.Logger.SetOutput(&logs)
	tool.Parse([]string{"gcc @arguments.rsp", "cc -c valid.c"})

	commands := readCompilerTestCommands(t, outputFile)
	if tool.StatusCode != 0 || len(commands) != 1 || commands[0].File != "valid.c" {
		t.Fatalf("response output limit stopped parsing: status=%d commands=%#v", tool.StatusCode, commands)
	}
	if diagnostic := logs.String(); !strings.Contains(diagnostic, "expanded entries exceed output limit") ||
		!strings.Contains(diagnostic, "build log line 1") {
		t.Fatalf("response output limit was not diagnosed: %q", diagnostic)
	}
}

func TestParseLeavesUnsupportedResponseFileModesOpaque(t *testing.T) {
	projectDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(projectDir, "arguments.rsp"), []byte("-c hidden.c"), 0o644); err != nil {
		t.Fatalf("write response file failed: %v", err)
	}
	outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
	tool := newTestTool(t, Config{
		InputFile:    "stdin",
		OutputFile:   outputFile,
		BuildDir:     projectDir,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
		NoStrict:     true,
	})
	tool.Parse([]string{
		"clang-cl @arguments.rsp -c explicit-cl.c",
		"clang --driver-mode=cl @arguments.rsp -c explicit-mode.c",
		"clang --rsp-quoting=windows @arguments.rsp -c explicit-quoting.c",
	})

	commands := readCompilerTestCommands(t, outputFile)
	wantFiles := []string{"explicit-cl.c", "explicit-mode.c", "explicit-quoting.c"}
	if len(commands) != len(wantFiles) {
		t.Fatalf("unsupported response modes changed explicit sources: %#v", commands)
	}
	for index, command := range commands {
		if command.File != wantFiles[index] || !slices.Contains(command.Arguments, "@arguments.rsp") {
			t.Fatalf("unsupported response mode was not kept opaque: %#v", command)
		}
	}
}

func TestParseBackticksTrackMakeAndGeneratedDirectories(t *testing.T) {
	t.Run("make directory", func(t *testing.T) {
		projectDir := t.TempDir()
		childDir := filepath.Join(projectDir, "sub")
		if err := os.Mkdir(childDir, 0o755); err != nil {
			t.Fatalf("create child directory failed: %v", err)
		}
		outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
		tool := newTestTool(t, Config{OutputFile: outputFile, BuildDir: projectDir, RegexCompile: RegexCompile, RegexFile: RegexFile, NoStrict: true})
		tool.Parse([]string{"make -C `printf sub`", "cc -c child.c"})
		commands := readCompilerTestCommands(t, outputFile)
		if len(commands) != 1 || commands[0].Directory != trackedPathToSlash(childDir) {
			t.Fatalf("backtick Make directory tracking failed: %#v", commands)
		}
	})

	t.Run("generated directory", func(t *testing.T) {
		projectDir := t.TempDir()
		if err := os.WriteFile(filepath.Join(projectDir, "main.c"), []byte("int main;\n"), 0o644); err != nil {
			t.Fatalf("write source file failed: %v", err)
		}
		outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
		tool := newTestTool(t, Config{OutputFile: outputFile, BuildDir: projectDir, RegexCompile: RegexCompile, RegexFile: RegexFile})
		tool.makeDirectoryMarkers = true
		tool.Parse([]string{"mkdir -p `printf generated`", "cd generated && cc -c " + ShellJoinArgs([]string{filepath.Join(projectDir, "main.c")})})
		commands := readCompilerTestCommands(t, outputFile)
		if len(commands) != 1 || commands[0].Directory != trackedPathToSlash(filepath.Join(projectDir, "generated")) {
			t.Fatalf("backtick generated directory tracking failed: %#v", commands)
		}
	})
}

func TestParseBacktickFailureDoesNotChangeDirectory(t *testing.T) {
	projectDir := t.TempDir()
	var logs bytes.Buffer
	outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
	tool := newTestTool(t, Config{
		OutputFile:   outputFile,
		BuildDir:     projectDir,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
		NoStrict:     true,
	})
	tool.Logger.SetOutput(&logs)
	t.Setenv("PATH", t.TempDir())
	tool.Parse([]string{
		"make -C `compiledb-missing-backtick-executable`",
		"cc -c valid.c",
	})
	commands := readCompilerTestCommands(t, outputFile)
	if tool.StatusCode != 0 || len(commands) != 1 || commands[0].Directory != trackedPathToSlash(projectDir) {
		t.Fatalf("backtick failure changed parser state: status=%d commands=%#v", tool.StatusCode, commands)
	}
	if !strings.Contains(logs.String(), "Error executing nested command") {
		t.Fatalf("missing backtick failure diagnostic: %q", logs.String())
	}
}

func TestParseRecognizesDefaultSourceSuffixes(t *testing.T) {
	outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
	tool := newTestTool(t, Config{
		OutputFile:   outputFile,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
		NoStrict:     true,
	})
	files := []string{"one.c", "two.C", "three.cc", "four.cpp", "five.cxx", "six.c++", "seven.s", "eight.S", "nine.m", "ten.mm", "eleven.cu", "unknown.xyz"}
	lines := make([]string, 0, len(files)+1)
	for _, filename := range files {
		lines = append(lines, "cc -c "+filename)
	}
	lines = append(lines, "cc -x c -c extensionless")
	tool.Parse(lines)
	commands := readCompilerTestCommands(t, outputFile)
	want := append(files[:len(files)-1], "extensionless")
	if len(commands) != len(want) {
		t.Fatalf("unexpected source detection result: %#v", commands)
	}
	for index, filename := range want {
		if commands[index].File != filename {
			t.Fatalf("source suffix %q generated %#v", filename, commands[index])
		}
	}
}

func TestParseCustomFileRegexDoesNotScanResponseContents(t *testing.T) {
	workingDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(workingDir, "arguments.rsp"), []byte("generated.custom"), 0o644); err != nil {
		t.Fatalf("write response file failed: %v", err)
	}
	outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
	tool := newTestTool(t, Config{
		OutputFile:   outputFile,
		BuildDir:     workingDir,
		RegexCompile: RegexCompile,
		RegexFile:    `(?P<file>[^ ]+\.custom)`,
		NoStrict:     true,
	})
	tool.Parse([]string{"cc @arguments.rsp", "cc direct.custom"})
	commands := readCompilerTestCommands(t, outputFile)
	if len(commands) != 1 || commands[0].File != "direct.custom" {
		t.Fatalf("custom regex scanned response file contents: %#v", commands)
	}
}

func TestParseResponseFilePreservesUTF8InJSON(t *testing.T) {
	workingDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(workingDir, "arguments.rsp"), []byte("-D名称=值 -c 源码.c"), 0o644); err != nil {
		t.Fatalf("write UTF-8 response file failed: %v", err)
	}
	outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
	tool := newTestTool(t, Config{
		OutputFile:   outputFile,
		BuildDir:     workingDir,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
		NoStrict:     true,
	})
	tool.Parse([]string{"cc @arguments.rsp"})
	commands := readCompilerTestCommands(t, outputFile)
	if len(commands) != 1 || commands[0].File != "源码.c" || !slices.Contains(commands[0].Arguments, "-D名称=值") {
		t.Fatalf("UTF-8 response arguments changed before JSON output: %#v", commands)
	}
}

func TestRestoreWindowsResponseFileArguments(t *testing.T) {
	for name, test := range map[string]struct {
		command string
		want    []string
	}{
		"drive path": {
			command: `gcc @C:\work\arguments.rsp`,
			want:    []string{"gcc", "@C:/work/arguments.rsp"},
		},
		"quoted path": {
			command: `gcc @"C:\work dir\arguments.rsp"`,
			want:    []string{"gcc", "@C:/work dir/arguments.rsp"},
		},
		"UNC path": {
			command: `gcc @\\server\share\arguments.rsp`,
			want:    []string{"gcc", "@//server/share/arguments.rsp"},
		},
		"relative path in Windows context": {
			command: `C:\tool\gcc.exe @sub\arguments.rsp`,
			want:    []string{"C:/tool/gcc.exe", "@sub/arguments.rsp"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			arguments, parseErr := splitShellArguments(test.command)
			if parseErr != nil {
				t.Fatalf("split command failed: %v", parseErr)
			}
			rawArguments, ok := splitMakeCommand(test.command)
			if !ok {
				t.Fatal("split raw command failed")
			}
			restoreWindowsArguments(arguments, rawArguments, parseCompilerInvocation(arguments))
			if !slices.Equal(arguments, test.want) {
				t.Fatalf("unexpected restored arguments:\nwant: %#v\ngot:  %#v", test.want, arguments)
			}
		})
	}
}

func TestParseRejectsNonCompilerClangTools(t *testing.T) {
	outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
	tool := newTestTool(t, Config{
		InputFile:    "stdin",
		OutputFile:   outputFile,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
		NoStrict:     true,
	})

	tool.Parse([]string{
		"clang-format format.c",
		"clang-tidy tidy.c",
		"clang-18-tidy versioned-tidy.c",
		"gcc-12-ar versioned-ar.c",
		"/usr/lib/gcc/x86_64-linux-gnu/13/cc1 -quiet internal.c",
		"clang -fsyntax-only compile.c",
		"arm-none-eabi-gcc cross.c",
		"gcc13 versioned.c",
		"g++13 versioned.cpp",
		"gcc-mp-14 macports.c",
		"g++-mp-14 macports.cpp",
		"clang-18.1 versioned-clang.c",
		"gcc-13.2-posix versioned-posix.c",
		"x86_64-w64-mingw32-gcc-posix mingw-posix.c",
		"x86_64-w64-mingw32-g++-win32 mingw-win32.cpp",
	})

	commands := readCompilerTestCommands(t, outputFile)
	want := []string{"compile.c", "cross.c", "versioned.c", "versioned.cpp", "macports.c", "macports.cpp", "versioned-clang.c", "versioned-posix.c", "mingw-posix.c", "mingw-win32.cpp"}
	if len(commands) != len(want) {
		t.Fatalf("non-compiler Clang tools produced entries: %#v", commands)
	}
	for i, file := range want {
		if commands[i].File != file {
			t.Fatalf("unexpected compiler driver entry at %d: want %q, got %#v", i, file, commands)
		}
	}
}

func TestParseSupportsKnownProcessLaunchers(t *testing.T) {
	outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
	tool := newTestTool(t, Config{
		InputFile:    "stdin",
		OutputFile:   outputFile,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
		NoStrict:     true,
	})

	tool.Parse([]string{
		"time gcc -c timed.c",
		"time -p clang -c portable.c",
		"nice gcc -c nice.c",
		"nice -n 5 clang -c adjusted.c",
		"time nice gcc -c nested.c",
		"env MODE=1 time clang -c env-time.c",
		"env -- time gcc13 -c env-separator.c",
		"echo gcc -c fake.c",
		"echo time gcc -c fake-time.c",
		"time -- -p gcc -c false-positive-time.c",
		"nice -- -5 gcc -c false-positive-nice.c",
	})

	commands := readCompilerTestCommands(t, outputFile)
	want := []string{"timed.c", "portable.c", "nice.c", "adjusted.c", "nested.c", "env-time.c", "env-separator.c"}
	if len(commands) != len(want) {
		t.Fatalf("known launcher commands were not parsed: %#v", commands)
	}
	for i, file := range want {
		if commands[i].File != file {
			t.Fatalf("unexpected launcher entry at %d: want %q, got %#v", i, file, commands)
		}
	}
}

func TestParseExpandsBacktickCompilerTokenBeforeDriverValidation(t *testing.T) {
	tmpDir := t.TempDir()
	outputFile := filepath.Join(tmpDir, "compile_commands.json")
	marker := filepath.Join(tmpDir, "echo-backtick-ran")
	tool := newTestTool(t, Config{
		InputFile:    "stdin",
		OutputFile:   outputFile,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
		NoStrict:     true,
	})

	tool.Parse([]string{
		"`printf gcc` -c generated.c",
		"time `printf gcc` -c timed.c",
		"env MODE=1 `printf gcc` -c env.c",
		"ccache `printf gcc` -c wrapped.c",
		"echo `touch " + ShellJoinArgs([]string{marker}) + "; printf gcc` -c fake.c",
	})

	commands := readCompilerTestCommands(t, outputFile)
	if len(commands) != 4 || commands[0].File != "generated.c" || commands[1].File != "timed.c" ||
		commands[2].File != "env.c" || commands[3].File != "wrapped.c" {
		t.Fatalf("backtick compiler token was not validated after expansion: %#v", commands)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("non-compiler backtick was executed: %v", err)
	}
}

func TestParseNoStrictTracksMissingInlineDirectory(t *testing.T) {
	outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
	tool := newTestTool(t, Config{
		InputFile:    "stdin",
		OutputFile:   outputFile,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
		NoStrict:     true,
	})

	tool.Parse([]string{"cd /remote/build && gcc -c main.c"})

	commands := readCompilerTestCommands(t, outputFile)
	if len(commands) != 1 || commands[0].Directory != "/remote/build" || commands[0].File != "main.c" {
		t.Fatalf("no-strict rejected missing tracked directory: %#v", commands)
	}
}

func TestParseGeneratesEntryWhenCompileOnlyFlagIsAnOptionOperand(t *testing.T) {
	outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
	tool := newTestTool(t, Config{
		InputFile:    "stdin",
		OutputFile:   outputFile,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
		NoStrict:     true,
	})

	tool.Parse([]string{"gcc -u -c main.c"})

	commands := readCompilerTestCommands(t, outputFile)
	if len(commands) != 1 || commands[0].File != "main.c" {
		t.Fatalf("expected source command entry, got %#v", commands)
	}
}

func TestParseIgnoresLinkOnlyOrUnrelatedOutput(t *testing.T) {
	tmpDir := t.TempDir()
	outputFile := filepath.Join(tmpDir, "compile_commands.json")

	tool := newTestTool(t, Config{
		InputFile:    "stdin",
		OutputFile:   outputFile,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
		NoStrict:     true,
	})

	tool.Parse([]string{
		"cc -o app a.o b.o -lm",
		"cc -c a.c -o a.o",
		"echo cc -c fake.c",
		"echo cc -o app a.c b.c",
	})

	commands := readCompilerTestCommands(t, outputFile)
	if len(commands) != 1 || commands[0].File != "a.c" {
		t.Fatalf("unrelated output produced compilation entries: %#v", commands)
	}
}

func TestParseAppendsRepeatedAddArgs(t *testing.T) {
	tmpDir := t.TempDir()
	outputFile := filepath.Join(tmpDir, "compile_commands.json")

	tool := newTestTool(t, Config{
		InputFile:    "stdin",
		OutputFile:   outputFile,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
		NoStrict:     true,
		AddArgs:      []string{"-DMODE=1", `-DNAME="hello world"`, "-UDEBUG"},
	})

	tool.Parse([]string{"gcc -c src/main.c"})

	data, err := os.ReadFile(outputFile)
	if err != nil {
		t.Fatalf("read output failed: %v", err)
	}

	var commands []Command
	if err := json.Unmarshal(data, &commands); err != nil {
		t.Fatalf("unmarshal output failed: %v", err)
	}
	if len(commands) != 1 {
		t.Fatalf("expected 1 command, got %d", len(commands))
	}

	got := commands[0].Arguments
	wantSuffix := []string{"-DMODE=1", `-DNAME="hello world"`, "-UDEBUG"}
	if len(got) < len(wantSuffix) {
		t.Fatalf("arguments too short: %v", got)
	}

	suffix := got[len(got)-len(wantSuffix):]
	for i := range wantSuffix {
		if suffix[i] != wantSuffix[i] {
			t.Fatalf("unexpected macro suffix\nwant: %v\ngot:  %v", wantSuffix, suffix)
		}
	}
}

func TestParseCommandStylePreservesAddedArgument(t *testing.T) {
	outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
	tool := newTestTool(t, Config{
		InputFile:    "stdin",
		OutputFile:   outputFile,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
		NoStrict:     true,
		CommandStyle: true,
		AddArgs:      []string{`-DNAME="hello world"`, `-DREGEX=\d+`},
	})

	tool.Parse([]string{"gcc -c src/main.c"})
	data, err := os.ReadFile(outputFile)
	if err != nil {
		t.Fatalf("read output failed: %v", err)
	}
	var commands []Command
	if err := json.Unmarshal(data, &commands); err != nil {
		t.Fatalf("unmarshal output failed: %v", err)
	}
	if len(commands) != 1 {
		t.Fatalf("expected one command, got %#v", commands)
	}

	toolArgs, err := shellwords.Parse(commands[0].Command)
	if err != nil {
		t.Fatalf("parse command output failed: %v", err)
	}
	wantSuffix := []string{`-DNAME="hello world"`, `-DREGEX=\d+`}
	if len(toolArgs) < len(wantSuffix) || !slices.Equal(toolArgs[len(toolArgs)-len(wantSuffix):], wantSuffix) {
		t.Fatalf("command does not round-trip added arguments: %v", toolArgs)
	}
}

func TestParseAddArgDoesNotSplitComma(t *testing.T) {
	tmpDir := t.TempDir()
	outputFile := filepath.Join(tmpDir, "compile_commands.json")

	tool := newTestTool(t, Config{
		InputFile:    "stdin",
		OutputFile:   outputFile,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
		NoStrict:     true,
		AddArgs:      []string{`-DTEST_BOARD,-m32`},
	})

	tool.Parse([]string{"gcc -c src/main.c"})

	data, err := os.ReadFile(outputFile)
	if err != nil {
		t.Fatalf("read output failed: %v", err)
	}

	var commands []Command
	if err := json.Unmarshal(data, &commands); err != nil {
		t.Fatalf("unmarshal output failed: %v", err)
	}
	if len(commands) != 1 {
		t.Fatalf("expected 1 command, got %d", len(commands))
	}

	got := commands[0].Arguments
	wantSuffix := []string{"-DTEST_BOARD,-m32"}
	if len(got) < len(wantSuffix) {
		t.Fatalf("arguments too short: %v", got)
	}

	suffix := got[len(got)-len(wantSuffix):]
	for i := range wantSuffix {
		if suffix[i] != wantSuffix[i] {
			t.Fatalf("unexpected macro suffix\nwant: %v\ngot:  %v", wantSuffix, suffix)
		}
	}
}

func TestParseNormalizesTargetArgumentForClangd(t *testing.T) {
	tmpDir := t.TempDir()
	outputFile := filepath.Join(tmpDir, "compile_commands.json")

	tool := newTestTool(t, Config{
		InputFile:    "stdin",
		OutputFile:   outputFile,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
		NoStrict:     true,
	})

	tool.Parse([]string{`clang -target pi32v2 -c "test2.c"`})

	data, err := os.ReadFile(outputFile)
	if err != nil {
		t.Fatalf("read output failed: %v", err)
	}

	var commands []Command
	if err := json.Unmarshal(data, &commands); err != nil {
		t.Fatalf("unmarshal output failed: %v", err)
	}
	if len(commands) != 1 {
		t.Fatalf("expected 1 command, got %d", len(commands))
	}

	if len(commands[0].Arguments) < 4 {
		t.Fatalf("unexpected arguments: %v", commands[0].Arguments)
	}

	want := []string{"clang", "--target=pi32v2", "-c", "test2.c"}
	got := commands[0].Arguments[:4]
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("unexpected target normalization\nwant: %v\ngot:  %v", want, got)
		}
	}
}

func TestParseDoesNotNormalizeAddedTargetArguments(t *testing.T) {
	outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
	tool := newTestTool(t, Config{
		InputFile:    "stdin",
		OutputFile:   outputFile,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
		NoStrict:     true,
		AddArgs:      []string{"-target", "pi32v2"},
	})

	tool.Parse([]string{"clang -c test.c"})
	var commands []Command
	data, err := os.ReadFile(outputFile)
	if err != nil {
		t.Fatalf("read output failed: %v", err)
	}
	if err := json.Unmarshal(data, &commands); err != nil {
		t.Fatalf("unmarshal output failed: %v", err)
	}
	wantSuffix := []string{"-target", "pi32v2"}
	if len(commands) != 1 || !slices.Equal(commands[0].Arguments[len(commands[0].Arguments)-2:], wantSuffix) {
		t.Fatalf("added arguments were normalized: %#v", commands)
	}
}

func TestParseInsertsAddedArgumentsBeforeCompilerOptionTerminator(t *testing.T) {
	for name, command := range map[string]string{
		"compiler":         "clang -c -- test.c",
		"wrapped compiler": "ccache clang -c -- test.c",
	} {
		t.Run(name, func(t *testing.T) {
			outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
			tool := newTestTool(t, Config{
				InputFile:    "stdin",
				OutputFile:   outputFile,
				RegexCompile: RegexCompile,
				RegexFile:    RegexFile,
				NoStrict:     true,
				AddArgs:      []string{"-DADDED=1"},
			})
			tool.Parse([]string{command})

			var commands []Command
			data, err := os.ReadFile(outputFile)
			if err != nil {
				t.Fatalf("read output failed: %v", err)
			}
			if err := json.Unmarshal(data, &commands); err != nil {
				t.Fatalf("unmarshal output failed: %v", err)
			}
			if len(commands) != 1 {
				t.Fatalf("expected one command, got %#v", commands)
			}
			compilerIndex := compilerArgumentIndex(commands[0].Arguments)
			terminator := slices.Index(commands[0].Arguments[compilerIndex+1:], "--") + compilerIndex + 1
			added := slices.Index(commands[0].Arguments, "-DADDED=1")
			if added < compilerIndex || terminator <= added {
				t.Fatalf("added argument must precede compiler terminator: %v", commands[0].Arguments)
			}
		})
	}
}

func TestParseDoesNotTreatOptionOperandAsTerminator(t *testing.T) {
	for name, command := range map[string]string{
		"output":     "gcc -c test.c -o --",
		"dependency": "gcc -c test.c -MF --",
	} {
		t.Run(name, func(t *testing.T) {
			outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
			tool := newTestTool(t, Config{
				InputFile:    "stdin",
				OutputFile:   outputFile,
				RegexCompile: RegexCompile,
				RegexFile:    RegexFile,
				NoStrict:     true,
				AddArgs:      []string{"-DADDED=1"},
			})
			tool.Parse([]string{command})

			commands := readCompilerTestCommands(t, outputFile)
			if len(commands) != 1 || commands[0].Arguments[len(commands[0].Arguments)-2] != "--" ||
				commands[0].Arguments[len(commands[0].Arguments)-1] != "-DADDED=1" {
				t.Fatalf("option operand was treated as a terminator: %#v", commands)
			}
		})
	}
}

func TestParseSupportsImplicitDistributedCompiler(t *testing.T) {
	for _, wrapper := range []string{"distcc", "icecc"} {
		t.Run(wrapper, func(t *testing.T) {
			outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
			tool := newTestTool(t, Config{
				InputFile:    "stdin",
				OutputFile:   outputFile,
				RegexCompile: RegexCompile,
				RegexFile:    RegexFile,
				NoStrict:     true,
				Macros:       true,
				FullPath:     true,
			})
			var compilerName string
			tool.compilerCommand = func(name string, _ ...string) *exec.Cmd {
				compilerName = name
				cmd := exec.Command(os.Args[0], "-test.run=^TestCompilerMacrosHelperProcess$")
				cmd.Env = append(os.Environ(), "COMPILEDB_TEST_COMPILER_HELPER=success")
				return cmd
			}
			tool.Parse([]string{wrapper + " -c test.c"})

			commands := readCompilerTestCommands(t, outputFile)
			if wrapper == "icecc" {
				if len(commands) != 1 || compilerName != "" || !slices.Equal(commands[0].Arguments, []string{"icecc", "-c", "test.c"}) {
					t.Fatalf("icecc macro probing was not disabled safely: compiler=%q commands=%#v", compilerName, commands)
				}
				return
			}
			if len(commands) != 1 || executableBase(compilerName) != "cc" ||
				!slices.Equal(commands[0].Arguments[:2], []string{wrapper, "-DORIGINAL=0"}) {
				t.Fatalf("implicit compiler was not handled correctly: compiler=%q commands=%#v", compilerName, commands)
			}
		})
	}
}

func TestParseSelectsSourceInsteadOfOptionOperand(t *testing.T) {
	for name, command := range map[string]string{
		"include":           "gcc -x c++ -c main.c -include config.c",
		"long include":      "gcc -c main.cpp --include config.c",
		"output":            "gcc -c main.cpp -o output.c",
		"long output":       "gcc -c main.cpp --output output.c",
		"dependency":        "gcc -c main.cpp -MF deps.c",
		"clang dependency":  "clang -c main.cpp -dependency-file deps.c",
		"VFS overlay":       "clang -c main.cpp -ivfsoverlay overlay.c",
		"working directory": "clang -c main.cpp -working-directory build.c",
		"diagnostics":       "clang -c main.cpp -serialize-diagnostics diagnostics.c",
		"linker script":     "gcc -c main.cpp -T script.c",
		"framework":         "clang -c main.cpp -F frameworks.c",
		"linker option":     "gcc -z defs.c -c main.cpp",
		"assertion":         "gcc -A predicate.c -c main.cpp",
		"multilib":          "gcc -imultilib multilib.c -c main.cpp",
		"multiarch":         "gcc -imultiarch multiarch.c -c main.cpp",
		"index store":       "clang -index-store-path index.c -c main.cpp",
		"debug directory":   "clang -fdebug-compilation-dir debug.c -c main.cpp",
		"API notes modules": "clang -iapinotes-modules notes.c -c main.cpp",
	} {
		t.Run(name, func(t *testing.T) {
			outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
			tool := newTestTool(t, Config{
				InputFile:    "stdin",
				OutputFile:   outputFile,
				RegexCompile: RegexCompile,
				RegexFile:    RegexFile,
				NoStrict:     true,
			})
			tool.Parse([]string{command})

			commands := readCompilerTestCommands(t, outputFile)
			if len(commands) != 1 || commands[0].File != map[string]string{
				"include": "main.c", "long include": "main.cpp", "output": "main.cpp", "long output": "main.cpp",
				"dependency": "main.cpp", "clang dependency": "main.cpp", "VFS overlay": "main.cpp",
				"working directory": "main.cpp", "diagnostics": "main.cpp", "linker script": "main.cpp", "framework": "main.cpp",
				"linker option": "main.cpp", "assertion": "main.cpp", "multilib": "main.cpp", "multiarch": "main.cpp", "index store": "main.cpp",
				"debug directory": "main.cpp", "API notes modules": "main.cpp",
			}[name] {
				t.Fatalf("option operand was selected as source: %#v", commands)
			}
		})
	}
}

func TestParseDefaultSourceScanner(t *testing.T) {
	outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
	tool := newTestTool(t, Config{
		InputFile:    "stdin",
		OutputFile:   outputFile,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
		NoStrict:     true,
	})

	tool.Parse([]string{
		"gcc before.c -c",
		"gcc -c upper.C",
		"gcc -c startup.S",
		"gcc -c one.c two.cpp",
		"gcc -x c -c extensionless",
	})
	commands := readCompilerTestCommands(t, outputFile)
	files := make([]string, 0, len(commands))
	for _, command := range commands {
		files = append(files, command.File)
	}
	want := []string{"before.c", "upper.C", "startup.S", "one.c", "two.cpp", "extensionless"}
	if !slices.Equal(files, want) {
		t.Fatalf("unexpected source entries:\nwant: %v\ngot:  %v", want, files)
	}
}

func TestParsePreservesOriginalBackslashes(t *testing.T) {
	outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
	tool := newTestTool(t, Config{
		InputFile:    "stdin",
		OutputFile:   outputFile,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
		NoStrict:     true,
	})
	tool.Parse([]string{`gcc '-DREGEX=\d+' -c main.c`})

	commands := readCompilerTestCommands(t, outputFile)
	if len(commands) != 1 || !slices.Contains(commands[0].Arguments, `-DREGEX=\d+`) {
		t.Fatalf("non-path backslashes were changed: %#v", commands)
	}
}

func TestNormalizeCompilerArgsRespectsOptionOperands(t *testing.T) {
	for _, arguments := range [][]string{
		{"clang", "-c", "-o", "-target", "main.c"},
		{"clang", "-c", "-MF", "-target", "main.c"},
		{"clang", "-c", "--", "-target", "main.c"},
	} {
		if got := normalizeCompilerArgs(arguments); !slices.Equal(got, arguments) {
			t.Fatalf("option operand was normalized:\nwant: %v\ngot:  %v", arguments, got)
		}
	}
	want := []string{"clang", "--target=triple", "-c", "main.c"}
	if got := normalizeCompilerArgs([]string{"clang", "-target", "triple", "-c", "main.c"}); !slices.Equal(got, want) {
		t.Fatalf("real target option was not normalized: want %v, got %v", want, got)
	}
}

func TestParseStrictRejectsSourceDirectory(t *testing.T) {
	projectDir := t.TempDir()
	if err := os.Mkdir(filepath.Join(projectDir, "include.c"), 0o755); err != nil {
		t.Fatalf("create source-like directory failed: %v", err)
	}
	outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
	tool := newTestTool(t, Config{
		InputFile:    "stdin",
		OutputFile:   outputFile,
		BuildDir:     projectDir,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
	})
	tool.Parse([]string{"gcc -c include.c"})
	if commands := readCompilerTestCommands(t, outputFile); len(commands) != 0 {
		t.Fatalf("source directory was accepted: %#v", commands)
	}
}

func TestParseUsesCompilerWorkingDirectory(t *testing.T) {
	projectDir := t.TempDir()
	subDir := filepath.Join(projectDir, "sub")
	if err := os.Mkdir(subDir, 0o755); err != nil {
		t.Fatalf("create working directory failed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(subDir, "main.c"), []byte("int value;\n"), 0o644); err != nil {
		t.Fatalf("write source failed: %v", err)
	}
	outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
	tool := newTestTool(t, Config{
		InputFile:    "stdin",
		OutputFile:   outputFile,
		BuildDir:     projectDir,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
	})

	tool.Parse([]string{"clang -working-directory sub -c main.c"})
	commands := readCompilerTestCommands(t, outputFile)
	if len(commands) != 1 || commands[0].Directory != trackedPathToSlash(subDir) ||
		!slices.Contains(commands[0].Arguments, hostPathToDatabasePath(subDir)) {
		t.Fatalf("compiler working directory was not applied: %#v", commands)
	}
}

func TestParseFiltersSourcesBeforeMacroProbe(t *testing.T) {
	for name, config := range map[string]Config{
		"excluded": {NoStrict: true, Exclude: []string{`excluded[.]c`}},
		"missing":  {},
	} {
		t.Run(name, func(t *testing.T) {
			outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
			config.InputFile = "stdin"
			config.OutputFile = outputFile
			config.BuildDir = t.TempDir()
			config.RegexCompile = RegexCompile
			config.RegexFile = RegexFile
			config.Macros = true
			tool := newTestTool(t, config)
			calls := 0
			tool.compilerCommand = func(_ string, _ ...string) *exec.Cmd {
				calls++
				return exec.Command(os.Args[0])
			}
			file := "missing.c"
			if name == "excluded" {
				file = "excluded.c"
			}
			tool.Parse([]string{"gcc -c " + file})
			if calls != 0 {
				t.Fatalf("filtered source started macro probe %d times", calls)
			}
		})
	}
}

func TestParseExcludesMultiplePatternsOnlyFromPathStart(t *testing.T) {
	outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
	tool := newTestTool(t, Config{
		InputFile:    "stdin",
		OutputFile:   outputFile,
		Exclude:      []string{`vendor/`, `generated/.*[.]c`},
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
		NoStrict:     true,
	})

	tool.Parse([]string{
		"cc -c vendor/one.c",
		"cc -c generated/two.c",
		"cc -c src/vendor/three.c",
		"cc -c src/main.c",
	})

	commands := readCompilerTestCommands(t, outputFile)
	if len(commands) != 2 || commands[0].File != "src/vendor/three.c" || commands[1].File != "src/main.c" {
		t.Fatalf("exclude patterns did not use path-prefix matching: %#v", commands)
	}
}

func TestParseRecoversFromCommandFailures(t *testing.T) {
	for name, failedCommand := range map[string]string{
		"tokenizer": "gcc -c 'broken.c",
		"backtick":  "gcc -I`false` -c broken.c",
	} {
		t.Run(name, func(t *testing.T) {
			outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
			tool := newTestTool(t, Config{
				InputFile:    "stdin",
				OutputFile:   outputFile,
				RegexCompile: RegexCompile,
				RegexFile:    RegexFile,
				NoStrict:     true,
			})

			tool.Parse([]string{failedCommand, "cc -c valid.c"})

			commands := readCompilerTestCommands(t, outputFile)
			if tool.StatusCode != 0 || len(commands) != 1 || commands[0].File != "valid.c" {
				t.Fatalf("recoverable parser failure stopped parsing: status=%d commands=%#v", tool.StatusCode, commands)
			}
		})
	}
}

func TestParseContinuesAfterDynamicBacktickFailure(t *testing.T) {
	outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
	tool := newTestTool(t, Config{
		InputFile:    "stdin",
		OutputFile:   outputFile,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
		NoStrict:     true,
	})
	t.Setenv("PATH", t.TempDir())

	tool.Parse([]string{
		"`missing-backtick-tool` -c skipped.c; gcc -c valid.c",
		"cd `false`; gcc -c uncertain-cwd.c",
	})

	commands := readCompilerTestCommands(t, outputFile)
	if tool.StatusCode != 0 || len(commands) != 1 || commands[0].File != "valid.c" {
		t.Fatalf("dynamic backtick failure stopped an independent command: status=%d commands=%#v", tool.StatusCode, commands)
	}
}

func TestParseReportsTokenizationFailureWithoutCommandContents(t *testing.T) {
	outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
	var logs bytes.Buffer
	tool := newTestTool(t, Config{
		InputFile:    "stdin",
		OutputFile:   outputFile,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
		NoStrict:     true,
	})
	tool.Logger.SetLevel(logrus.ErrorLevel)
	tool.Logger.SetOutput(&logs)
	tool.Parse([]string{
		"echo ordinary 'output",
		"gcc -DSECRET=value -c \\",
		"'broken.c",
		"gcc -I`pwd -c unmatched.c",
		"cd `pwd",
		"make -C `pwd",
		"`printf gcc` -c 'expanded-broken.c",
		"cc -c valid.c",
	})

	commands := readCompilerTestCommands(t, outputFile)
	if tool.StatusCode != 0 || len(commands) != 1 || commands[0].File != "valid.c" {
		t.Fatalf("tokenization failure changed parser result: status=%d commands=%#v", tool.StatusCode, commands)
	}
	diagnostic := logs.String()
	for _, want := range []string{
		"build log line 2",
		"unterminated quote at byte 22",
		"build log line 4",
		"build log line 5",
		"build log line 6",
		"unterminated backtick",
		"build log line 7",
		"unterminated quote at byte 16",
		"cwd",
	} {
		if !strings.Contains(diagnostic, want) {
			t.Fatalf("tokenization diagnostic lacks %q: %q", want, diagnostic)
		}
	}
	if strings.Contains(diagnostic, "SECRET") || strings.Contains(diagnostic, "ordinary") {
		t.Fatalf("tokenization diagnostic exposed command contents or logged unrelated output: %q", diagnostic)
	}
}

func TestParseDoesNotExecuteBackticksAfterShellParseError(t *testing.T) {
	projectDir := t.TempDir()
	marker := filepath.Join(projectDir, "parse-error-executed")
	outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
	var logs bytes.Buffer
	tool := newTestTool(t, Config{
		InputFile:    "stdin",
		OutputFile:   outputFile,
		BuildDir:     projectDir,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
		NoStrict:     true,
	})
	tool.Logger.SetLevel(logrus.ErrorLevel)
	tool.Logger.SetOutput(&logs)

	tool.Parse([]string{
		"`printf marker > " + ShellJoinArgs([]string{marker}) + "; printf gcc` -c malformed.c && # incomplete",
		"`printf marker > " + ShellJoinArgs([]string{marker}) + "; printf gcc` -c bare-and.c &&",
		"`printf marker > " + ShellJoinArgs([]string{marker}) + "; printf gcc` -c bare-or.c ||",
		"cc -c valid.c",
	})

	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("AST parse-error path executed backtick: %v", err)
	}
	commands := readCompilerTestCommands(t, outputFile)
	if tool.StatusCode != 0 || len(commands) != 1 || commands[0].File != "valid.c" {
		t.Fatalf("AST parse error changed parser result: status=%d commands=%#v", tool.StatusCode, commands)
	}
	if !strings.Contains(logs.String(), "skip malformed command") {
		t.Fatalf("missing AST parse-error diagnostic: %q", logs.String())
	}
}

func TestParseReportsRelevantShellGrammarErrors(t *testing.T) {
	outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
	var logs bytes.Buffer
	tool := newTestTool(t, Config{
		InputFile:    "stdin",
		OutputFile:   outputFile,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
		NoStrict:     true,
	})
	tool.Logger.SetLevel(logrus.ErrorLevel)
	tool.Logger.SetOutput(&logs)

	tool.Parse([]string{
		"echo ordinary >",
		`echo 'gcc -c quoted.c' >`,
		"gcc -DSECRET=value -c malformed.c >",
		"cd sub >",
		"make -C sub |",
		"true; gcc -DSECRET=sequence -c sequence.c >",
		"printf x; cd nested >",
		"echo x; make -C nested |",
		"gcc -c arithmetic.c $((1 SECRET 2))",
		"cc -c valid.c",
	})

	commands := readCompilerTestCommands(t, outputFile)
	if tool.StatusCode != 0 || len(commands) != 1 || commands[0].File != "valid.c" {
		t.Fatalf("shell grammar errors changed parser result: status=%d commands=%#v", tool.StatusCode, commands)
	}
	diagnostic := logs.String()
	for _, want := range []string{"build log line 3", "build log line 4", "build log line 5", "build log line 6",
		"build log line 7", "build log line 8", "build log line 9", "at byte", "cwd"} {
		if !strings.Contains(diagnostic, want) {
			t.Fatalf("shell grammar diagnostic lacks %q: %q", want, diagnostic)
		}
	}
	if strings.Count(diagnostic, "skip malformed command") != 7 {
		t.Fatalf("unexpected shell grammar diagnostic count: %q", diagnostic)
	}
	if strings.Contains(diagnostic, "SECRET") || strings.Contains(diagnostic, "ordinary") || strings.Contains(diagnostic, "quoted.c") {
		t.Fatalf("shell grammar diagnostic exposed command contents or logged unrelated output: %q", diagnostic)
	}
}

func TestParseSupportsKnownCompilerLaunchers(t *testing.T) {
	outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
	tool := newTestTool(t, Config{
		InputFile:    "stdin",
		OutputFile:   outputFile,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
		NoStrict:     true,
	})
	tool.Parse([]string{
		"env BUILD_MODE=release gcc -c env.c",
		"xcrun --sdk macosx clang -c xcrun.c",
		"xcrun --run clang -c xcrun-run.c",
		"xcrun -r clang -c xcrun-short-run.c",
		"./libtool --mode=compile gcc -c libtool.c",
		"/bin/sh ./libtool --tag=CC --mode=compile gcc -c shell-libtool.c",
		"libtool: compile: gcc -c logged-libtool.c",
		"echo env gcc -c fake.c",
	})
	commands := readCompilerTestCommands(t, outputFile)
	want := []string{"env.c", "xcrun.c", "xcrun-run.c", "xcrun-short-run.c", "libtool.c", "shell-libtool.c", "logged-libtool.c"}
	files := make([]string, 0, len(commands))
	for _, command := range commands {
		files = append(files, command.File)
	}
	if !slices.Equal(files, want) {
		t.Fatalf("known compiler launchers were not handled safely: %#v", commands)
	}
}

func TestParseFullPathBehindSafeEnvironmentLauncher(t *testing.T) {
	compilerDir := t.TempDir()
	compiler := filepath.Join(compilerDir, "fake-gcc")
	if err := os.WriteFile(compiler, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write fake compiler failed: %v", err)
	}
	t.Setenv("PATH", compilerDir)
	outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
	tool := newTestTool(t, Config{
		InputFile:    "stdin",
		OutputFile:   outputFile,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
		NoStrict:     true,
		FullPath:     true,
	})

	tool.Parse([]string{"env BUILD_MODE=release fake-gcc -c env.c"})
	commands := readCompilerTestCommands(t, outputFile)
	if len(commands) != 1 || commands[0].Arguments[0] != hostPathToDatabasePath(compiler) {
		t.Fatalf("full path was not resolved behind a safe env launcher: %#v", commands)
	}
}

func TestParseAppliesEnvironmentLauncherDirectory(t *testing.T) {
	for name, option := range map[string]string{
		"short": "-C sub",
		"long":  "--chdir=sub",
	} {
		t.Run(name, func(t *testing.T) {
			projectDir := t.TempDir()
			subDir := filepath.Join(projectDir, "sub")
			if err := os.Mkdir(subDir, 0o755); err != nil {
				t.Fatalf("create subdirectory failed: %v", err)
			}
			if err := os.WriteFile(filepath.Join(subDir, "main.c"), []byte("int value;\n"), 0o644); err != nil {
				t.Fatalf("write source failed: %v", err)
			}
			compiler := filepath.Join(subDir, "fake-gcc")
			if err := os.WriteFile(compiler, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
				t.Fatalf("write compiler failed: %v", err)
			}
			outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
			tool := newTestTool(t, Config{
				InputFile:    "stdin",
				OutputFile:   outputFile,
				BuildDir:     projectDir,
				RegexCompile: RegexCompile,
				RegexFile:    RegexFile,
				FullPath:     true,
			})

			tool.Parse([]string{"env " + option + " ./fake-gcc -c main.c"})

			commands := readCompilerTestCommands(t, outputFile)
			if len(commands) != 1 || commands[0].Directory != trackedPathToSlash(subDir) ||
				commands[0].Arguments[0] != hostPathToDatabasePath(compiler) {
				t.Fatalf("env chdir was not applied to compiler command: %#v", commands)
			}
		})
	}
}

func TestParseKeepsPOSIXEscapedRelativeSource(t *testing.T) {
	projectDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(projectDir, "srcmain.c"), []byte("int value;\n"), 0o644); err != nil {
		t.Fatalf("write source failed: %v", err)
	}
	outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
	tool := newTestTool(t, Config{
		InputFile:    "stdin",
		OutputFile:   outputFile,
		BuildDir:     projectDir,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
	})
	tool.Parse([]string{`gcc -I a:b -c src\main.c`})
	commands := readCompilerTestCommands(t, outputFile)
	if len(commands) != 1 || commands[0].File != "srcmain.c" {
		t.Fatalf("POSIX escaped source was treated as a Windows path: %#v", commands)
	}
}

func TestParseStrictPreservesPOSIXColonAndBackslashPaths(t *testing.T) {
	projectDir := t.TempDir()
	colonDir := filepath.Join(projectDir, "1:a")
	if err := os.Mkdir(colonDir, 0o755); err != nil {
		t.Fatalf("create colon directory failed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(colonDir, "main.c"), nil, 0o644); err != nil {
		t.Fatalf("create colon source failed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, `src\main.c`), nil, 0o644); err != nil {
		t.Fatalf("create backslash source failed: %v", err)
	}
	outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
	tool := newTestTool(t, Config{
		InputFile:    "stdin",
		OutputFile:   outputFile,
		BuildDir:     projectDir,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
	})

	tool.Parse([]string{
		"cd 1:a && gcc -c main.c",
		`gcc -c 'src\main.c'`,
	})

	commands := readCompilerTestCommands(t, outputFile)
	if len(commands) != 2 || commands[0].Directory != trackedPathToSlash(colonDir) || commands[0].File != "main.c" ||
		commands[1].Directory != trackedPathToSlash(projectDir) || commands[1].File != `src\main.c` {
		t.Fatalf("POSIX colon or backslash path was changed: %#v", commands)
	}
}

func TestParseCustomFileRegexRemainsAuthoritative(t *testing.T) {
	outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
	tool := newTestTool(t, Config{
		InputFile:    "stdin",
		OutputFile:   outputFile,
		RegexCompile: `mycc`,
		RegexFile:    `--src=([^[:space:]]+[.]foo)`,
		NoStrict:     true,
	})

	tool.Parse([]string{"mycc --src=main.foo"})
	commands := readCompilerTestCommands(t, outputFile)
	if len(commands) != 1 || commands[0].File != "main.foo" ||
		!slices.Equal(commands[0].Arguments, []string{"mycc", "--src=main.foo"}) {
		t.Fatalf("custom file regex capture was overwritten: %#v", commands)
	}
}

func TestParseMultipleSourcesSkipsMacroProbe(t *testing.T) {
	outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
	tool := newTestTool(t, Config{
		InputFile:    "stdin",
		OutputFile:   outputFile,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
		NoStrict:     true,
		Macros:       true,
	})
	var logs bytes.Buffer
	tool.Logger.SetOutput(&logs)
	calls := 0
	tool.compilerCommand = func(_ string, _ ...string) *exec.Cmd {
		calls++
		return exec.Command(os.Args[0])
	}

	tool.Parse([]string{"gcc -c one.c two.cpp"})
	commands := readCompilerTestCommands(t, outputFile)
	if len(commands) != 2 || commands[0].File != "one.c" || commands[1].File != "two.cpp" {
		t.Fatalf("expected one entry per source, got %#v", commands)
	}
	if calls != 0 || !strings.Contains(logs.String(), "multiple source inputs are not supported") {
		t.Fatalf("multiple sources must skip macro probing: calls=%d logs=%q", calls, logs.String())
	}
}

func TestParsePreservesWindowsSourcePaths(t *testing.T) {
	for name, test := range map[string]struct {
		command      string
		want         string
		wantArgument string
	}{
		"drive":  {command: `gcc -c C:\work\main.c`, want: "C:/work/main.c"},
		"quoted": {command: `gcc -c "C:\work\main.c"`, want: "C:/work/main.c"},
		"quoted space": {
			command: `gcc -c "C:\work dir\main.c" --include config.c`,
			want:    "C:/work dir/main.c",
		},
		"relative": {command: `C:\toolchain\gcc.exe -c src\main.c`, want: "src/main.c"},
		"attached absolute include establishes context": {
			command: `gcc -IC:\sdk -c src\main.c`,
			want:    "src/main.c",
		},
		"attached absolute sysroot establishes context": {
			command: `gcc --sysroot=C:/sdk -c src\main.c`,
			want:    "src/main.c",
		},
		"attached backslash UNC include establishes context": {
			command:      `gcc -I\\server\share -c src\main.c`,
			want:         "src/main.c",
			wantArgument: "-I//server/share",
		},
		"attached slash UNC sysroot establishes context": {
			command:      `gcc --sysroot=//server/share -c src\main.c`,
			want:         "src/main.c",
			wantArgument: "--sysroot=//server/share",
		},
		"UNC": {command: `gcc -c \\server\share\main.c`, want: "//server/share/main.c"},
		"followed by include": {
			command: `gcc -c C:\work\main.c --include config.c`,
			want:    "C:/work/main.c",
		},
	} {
		t.Run(name, func(t *testing.T) {
			outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
			tool := newTestTool(t, Config{
				InputFile:    "stdin",
				OutputFile:   outputFile,
				RegexCompile: RegexCompile,
				RegexFile:    RegexFile,
				NoStrict:     true,
			})
			tool.Parse([]string{test.command})

			commands := readCompilerTestCommands(t, outputFile)
			if len(commands) != 1 || commands[0].File != test.want || !slices.Contains(commands[0].Arguments, test.want) {
				t.Fatalf("Windows source path was not preserved: %#v", commands)
			}
			if test.wantArgument != "" && !slices.Contains(commands[0].Arguments, test.wantArgument) {
				t.Fatalf("Windows option path was not preserved: %#v", commands)
			}
		})
	}
}

func TestParsePreservesWindowsCompilerAndPathArguments(t *testing.T) {
	outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
	tool := newTestTool(t, Config{
		InputFile:    "stdin",
		OutputFile:   outputFile,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
		NoStrict:     true,
	})

	tool.Parse([]string{`C:\toolchain\bin\gcc.exe '-DREGEX=\d+' '-DROOT=C:\SDK' '-Wl,C:\lib\foo.a' -IC:\sdk\include -include C:\cfg\config.h -c C:\src\main.c -o C:\obj\main.o`})
	commands := readCompilerTestCommands(t, outputFile)
	want := []string{
		"C:/toolchain/bin/gcc.exe", `-DREGEX=\d+`, `-DROOT=C:\SDK`, `-Wl,C:\lib\foo.a`,
		"-IC:/sdk/include", "-include", "C:/cfg/config.h",
		"-c", "C:/src/main.c", "-o", "C:/obj/main.o",
	}
	if len(commands) != 1 || commands[0].File != "C:/src/main.c" || !slices.Equal(commands[0].Arguments, want) {
		t.Fatalf("Windows command arguments were not preserved:\nwant: %v\ngot:  %#v", want, commands)
	}
}

func TestJoinTrackedPathDomains(t *testing.T) {
	for name, test := range map[string]struct {
		base        string
		child       string
		windowsMode bool
		want        string
	}{
		"POSIX parent segment": {
			base: "/project/build", child: "../src", want: "/project/src",
		},
		"Windows absolute resets": {
			base: "C:/project/build", child: `D:\src\..\obj`, windowsMode: true, want: "D:/obj",
		},
		"same drive relative": {
			base: "C:/project/build", child: `c:..\src`, windowsMode: true, want: "C:/project/src",
		},
		"different drive relative": {
			base: "C:/project", child: `D:sub\dir`, windowsMode: true, want: "D:sub/dir",
		},
		"UNC": {
			base: `\\server\share\project`, child: `sub\..\obj`, windowsMode: true, want: "//server/share/project/obj",
		},
		"Windows relative separators": {
			base: "/project", child: `sub\dir`, windowsMode: true, want: "/project/sub/dir",
		},
		"POSIX colon": {
			base: "/project", child: "1:a", want: "/project/1:a",
		},
	} {
		t.Run(name, func(t *testing.T) {
			if got := joinTrackedPathWithWindowsMode(test.base, test.child, test.windowsMode); got != test.want {
				t.Fatalf("unexpected tracked path: want %q, got %q", test.want, got)
			}
		})
	}
}

func TestCleanTrackedPathDomains(t *testing.T) {
	for name, test := range map[string]struct {
		path        string
		windowsMode bool
		want        string
	}{
		"POSIX": {
			path: "/project/build/../src", want: "/project/src",
		},
		"Windows drive": {
			path: `C:\project\build\..\src`, windowsMode: true, want: "C:/project/src",
		},
		"Windows UNC": {
			path: `\\server\share\build\..\src`, windowsMode: true, want: "//server/share/src",
		},
		"POSIX backslash filename": {
			path: `src\build\..\main.c`, want: `src\build\..\main.c`,
		},
	} {
		t.Run(name, func(t *testing.T) {
			if got := cleanTrackedPathWithWindowsMode(test.path, test.windowsMode); got != test.want {
				t.Fatalf("unexpected cleaned tracked path: want %q, got %q", test.want, got)
			}
		})
	}
}

func TestParseRejectsOutputWithoutSourceInput(t *testing.T) {
	outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
	tool := newTestTool(t, Config{
		InputFile:    "stdin",
		OutputFile:   outputFile,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
		NoStrict:     true,
	})
	tool.Parse([]string{"gcc -c -o output.c"})

	if commands := readCompilerTestCommands(t, outputFile); len(commands) != 0 {
		t.Fatalf("output operand was treated as a source: %#v", commands)
	}
}

func TestMakeCommandDirectory(t *testing.T) {
	for name, test := range map[string]struct {
		line string
		base string
		want string
	}{
		"quoted":            {line: `make -C "sub dir"`, base: "/project", want: "/project/sub dir"},
		"attached":          {line: "gmake -Csub", base: "/project", want: "/project/sub"},
		"long":              {line: "mingw32-make --directory=sub", base: "/project", want: "/project/sub"},
		"multiple":          {line: "make -C one --directory two", base: "/project", want: "/project/one/two"},
		"current directory": {line: "make -C .", base: "/project", want: "/project"},
		"UNC":               {line: "make -C sub", base: "//server/share/project", want: "//server/share/project/sub"},
		"absolute resets":   {line: "make -C one -C /other", base: "/project", want: "/other"},
		"option terminator": {line: "make -C one -- -C two", base: "/project", want: "/project/one"},
		"file operand resembles directory": {
			line: "make -C sub -f -Cevil", base: "/project", want: "/project/sub",
		},
		"include operand resembles directory": {
			line: "make -I -Cfake -C real", base: "/project", want: "/project/real",
		},
		"terminator consumed as file operand": {
			line: "make -C sub -f -- -C two", base: "/project", want: "/project/sub/two",
		},
		"long option operand resembles terminator": {
			line: "make -C sub --file -- -C two", base: "/project", want: "/project/sub/two",
		},
		"attached file operand resembles directory": {
			line: "make -C sub -f-Cevil", base: "/project", want: "/project/sub",
		},
		"optional short value ends cluster": {
			line: "make -C sub -lC/ -j8 -Otarget", base: "/project", want: "/project/sub",
		},
		"optional long attached values": {
			line: "make --jobs=2 --debug=b --output-sync=target --shuffle=reverse -C sub",
			base: "/project", want: "/project/sub",
		},
		"Windows drive":    {line: `mingw32-make -C "C:\Program Files\build"`, base: "/project", want: "C:/Program Files/build"},
		"Windows UNC":      {line: `make -C "\\server\share\build"`, base: "/project", want: "//server/share/build"},
		"unquoted UNC":     {line: `make -C \\server\share\build`, base: "/project", want: "//server/share/build"},
		"relative Windows": {line: `mingw32-make -C sub\dir`, base: "/project", want: "/project/sub/dir"},
		"drive relative":   {line: `mingw32-make -C C:sub\dir`, base: "C:/project", want: "C:/project/sub/dir"},
		"POSIX colon":      {line: `make -C 1:a`, base: "/project", want: "/project/1:a"},
	} {
		t.Run(name, func(t *testing.T) {
			got, ok := makeCommandDirectory(test.line, test.base)
			if !ok || got != test.want {
				t.Fatalf("unexpected make directory: want %q, got %q, ok=%v", test.want, got, ok)
			}
		})
	}
}

func TestParseKeepsCurrentDirectoryForMakeDot(t *testing.T) {
	projectDir := t.TempDir()
	outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
	tool := newTestTool(t, Config{
		InputFile:    "stdin",
		OutputFile:   outputFile,
		BuildDir:     projectDir,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
		NoStrict:     true,
	})

	tool.Parse([]string{"make -C .", "gcc -c current.c"})
	commands := readCompilerTestCommands(t, outputFile)
	if len(commands) != 1 || commands[0].File != "current.c" ||
		commands[0].Directory != trackedPathToSlash(projectDir) {
		t.Fatalf("make -C . changed the tracked directory: %#v", commands)
	}
}

func TestMakeCommandDirectoryRejectsUnknownOptions(t *testing.T) {
	for _, line := range []string{
		"make -xC/forged",
		"make -C sub --unknown-option",
		"make --always-make=value -C /forged",
	} {
		t.Run(line, func(t *testing.T) {
			if directory, ok := makeCommandDirectory(line, "/project"); ok {
				t.Fatalf("invalid Make option established directory %q", directory)
			}
		})
	}
}

func TestParseDoesNotApplyMakeDirectoryToSiblingCommand(t *testing.T) {
	for _, separator := range []string{" && ", "; ", " || "} {
		t.Run(strings.TrimSpace(separator), func(t *testing.T) {
			projectDir := t.TempDir()
			outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
			tool := newTestTool(t, Config{
				InputFile:    "stdin",
				OutputFile:   outputFile,
				BuildDir:     projectDir,
				RegexCompile: RegexCompile,
				RegexFile:    RegexFile,
				NoStrict:     true,
			})
			tool.Parse([]string{
				"make -C sub" + separator + "gcc -c parent.c",
				"gcc -c child.c",
			})

			commands := readCompilerTestCommands(t, outputFile)
			if separator == "; " {
				if len(commands) != 2 || commands[0].Directory != trackedPathToSlash(projectDir) ||
					commands[1].Directory != trackedPathToSlash(filepath.Join(projectDir, "sub")) {
					t.Fatalf("make directory leaked into sibling command: %#v", commands)
				}
			} else if len(commands) != 1 || commands[0].Directory != trackedPathToSlash(projectDir) || commands[0].File != "child.c" {
				t.Fatalf("unknown conditional branch produced an entry or directory frame: %#v", commands)
			}
		})
	}
}

func TestParseTracksMakeAfterResolvedUnknownCondition(t *testing.T) {
	for _, line := range []string{
		"unknown-check || true; make -C sub",
		"unknown-check || true && make -C sub",
		"make -C sub; unknown-check || true",
	} {
		t.Run(line, func(t *testing.T) {
			projectDir := t.TempDir()
			outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
			tool := newTestTool(t, Config{
				InputFile:    "stdin",
				OutputFile:   outputFile,
				BuildDir:     projectDir,
				RegexCompile: RegexCompile,
				RegexFile:    RegexFile,
				NoStrict:     true,
			})
			tool.Parse([]string{line, "gcc -c child.c"})

			commands := readCompilerTestCommands(t, outputFile)
			wantDir := trackedPathToSlash(filepath.Join(projectDir, "sub"))
			if len(commands) != 1 || commands[0].File != "child.c" || commands[0].Directory != wantDir {
				t.Fatalf("definitely executed Make command did not establish directory frame: %#v", commands)
			}
		})
	}
}

func TestParseTracksMakeDirectoryWithDynamicTargets(t *testing.T) {
	for _, test := range []struct {
		line string
		want string
	}{
		{line: `make -C sub -- "$TARGET"`, want: "sub"},
		{line: `make -C sub target*`, want: "sub"},
		{line: `make -Csub --directory=child -- "$TARGET"`, want: "sub/child"},
		{line: `make -C sub -f -Cevil`, want: "sub"},
		{line: `make -I -Cfake -C sub`, want: "sub"},
		{line: `make -C sub -f -- -C child`, want: "sub/child"},
		{line: `make -C sub --file -- -C child`, want: "sub/child"},
		{line: `make -C sub -lC/ -j8 -Otarget`, want: "sub"},
		{line: `make --jobs=2 --debug=b --output-sync=target --shuffle=reverse -C sub`, want: "sub"},
	} {
		t.Run(test.line, func(t *testing.T) {
			projectDir := t.TempDir()
			outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
			tool := newTestTool(t, Config{
				InputFile:    "stdin",
				OutputFile:   outputFile,
				BuildDir:     projectDir,
				RegexCompile: RegexCompile,
				RegexFile:    RegexFile,
				NoStrict:     true,
			})
			tool.Parse([]string{test.line, "gcc -c child.c"})

			commands := readCompilerTestCommands(t, outputFile)
			wantDir := filepath.Join(projectDir, filepath.FromSlash(test.want))
			if len(commands) != 1 || commands[0].File != "child.c" ||
				commands[0].Directory != trackedPathToSlash(wantDir) {
				t.Fatalf("dynamic Make target prevented static directory tracking: %#v", commands)
			}
		})
	}
}

func TestParseTracksMakeDirectoryAfterInlineCD(t *testing.T) {
	projectDir := t.TempDir()
	if err := os.Mkdir(filepath.Join(projectDir, "sub"), 0o755); err != nil {
		t.Fatalf("create inline cd directory failed: %v", err)
	}
	outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
	tool := newTestTool(t, Config{
		InputFile:    "stdin",
		OutputFile:   outputFile,
		BuildDir:     projectDir,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
		NoStrict:     true,
	})

	tool.Parse([]string{"cd sub && make -C child", "gcc -c nested.c"})
	commands := readCompilerTestCommands(t, outputFile)
	wantDir := trackedPathToSlash(filepath.Join(projectDir, "sub", "child"))
	if len(commands) != 1 || commands[0].Directory != wantDir {
		t.Fatalf("make directory did not use the preceding cd: %#v", commands)
	}
}

func TestParseDoesNotTrackConditionallySkippedMake(t *testing.T) {
	projectDir := t.TempDir()
	outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
	tool := newTestTool(t, Config{
		InputFile:    "stdin",
		OutputFile:   outputFile,
		BuildDir:     projectDir,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
		NoStrict:     true,
	})

	tool.Parse([]string{"false && make -C sub", "gcc -c parent.c"})
	commands := readCompilerTestCommands(t, outputFile)
	if len(commands) != 1 || commands[0].Directory != trackedPathToSlash(projectDir) {
		t.Fatalf("conditionally skipped make changed directory: %#v", commands)
	}
}

func TestParseMatchesMakeLeaveQuotesAndProtectsBaseFrame(t *testing.T) {
	projectDir := t.TempDir()
	childDir := filepath.Join(projectDir, "sub")
	outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
	tool := newTestTool(t, Config{
		InputFile:    "stdin",
		OutputFile:   outputFile,
		BuildDir:     projectDir,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
		NoStrict:     true,
	})

	tool.Parse([]string{
		`make: Leaving directory "` + projectDir + `"`,
		`make: Entering directory "` + childDir + `"`,
		"gcc -c child.c",
		`make: Leaving directory "` + childDir + `"`,
		"gcc -c parent.c",
	})
	commands := readCompilerTestCommands(t, outputFile)
	if len(commands) != 2 || commands[0].Directory != trackedPathToSlash(childDir) ||
		commands[1].Directory != trackedPathToSlash(projectDir) {
		t.Fatalf("make leave handling corrupted the directory stack: %#v", commands)
	}
}

func TestParseSupportsLegacyMakeDirectoryQuotes(t *testing.T) {
	projectDir := t.TempDir()
	childDir := filepath.Join(projectDir, "sub")
	outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
	tool := newTestTool(t, Config{
		InputFile:    "stdin",
		OutputFile:   outputFile,
		BuildDir:     projectDir,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
		NoStrict:     true,
	})
	tool.Parse([]string{
		"make: Entering directory `" + childDir + "'",
		"gcc -c child.c",
		"make: Leaving directory `" + childDir + "'",
		"gcc -c parent.c",
	})
	commands := readCompilerTestCommands(t, outputFile)
	if len(commands) != 2 || commands[0].Directory != trackedPathToSlash(childDir) ||
		commands[1].Directory != trackedPathToSlash(projectDir) {
		t.Fatalf("legacy Make directory markers were not tracked: %#v", commands)
	}
}

func TestParsePreservesQuotesInsideMakeDirectory(t *testing.T) {
	projectDir := t.TempDir()
	childDir := filepath.Join(projectDir, `child'"dir`)
	outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
	tool := newTestTool(t, Config{
		InputFile:    "stdin",
		OutputFile:   outputFile,
		BuildDir:     projectDir,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
		NoStrict:     true,
	})

	tool.Parse([]string{
		"make: Entering directory '" + childDir + "'",
		"gcc -c child.c",
		"make: Leaving directory '" + childDir + "'",
		"gcc -c parent.c",
	})
	commands := readCompilerTestCommands(t, outputFile)
	if len(commands) != 2 || commands[0].Directory != trackedPathToSlash(childDir) ||
		commands[1].Directory != trackedPathToSlash(projectDir) {
		t.Fatalf("quotes inside Make directory were not preserved: %#v", commands)
	}
}

func TestParseTracksLiteralCommandSubstitutionInMakeDirectory(t *testing.T) {
	projectDir := t.TempDir()
	childDir := filepath.Join(projectDir, "obj$(name)")
	outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
	tool := newTestTool(t, Config{
		InputFile:    "stdin",
		OutputFile:   outputFile,
		BuildDir:     projectDir,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
		NoStrict:     true,
	})

	tool.Parse([]string{
		`make: Entering directory "` + childDir + `"`,
		"gcc -c child.c",
		`make: Leaving directory "` + childDir + `"`,
		`true; make: Entering directory '/forged'`,
		`printf make: Entering directory '/forged-command'`,
		`notmake: Entering directory '/forged-name'`,
		`make: Entering directory '/forged-partial'; if`,
		`make: Entering directory '/forged/garbage' '/..'`,
		`not\make: Entering directory '/forged-escape'`,
		"gcc -c parent.c",
	})

	commands := readCompilerTestCommands(t, outputFile)
	if len(commands) != 2 || commands[0].Directory != trackedPathToSlash(childDir) ||
		commands[1].Directory != trackedPathToSlash(projectDir) {
		t.Fatalf("literal command substitution in Make marker was not preserved: %#v", commands)
	}
}

func TestMakeDirectoryMarkerValue(t *testing.T) {
	for _, test := range []struct {
		value string
		want  string
		ok    bool
	}{
		{value: `'/project/sub'`, want: "/project/sub", ok: true},
		{value: `"/project/sub dir"`, want: "/project/sub dir", ok: true},
		{value: "`/project/sub'", want: "/project/sub", ok: true},
		{value: `'/project/a'b'`, want: `/project/a'b`, ok: true},
		{value: `'/project/a'b c'`, want: `/project/a'b c`, ok: true},
		{value: `'/project/child'"dir'`, want: `/project/child'"dir`, ok: true},
		{value: `/project/sub`},
		{value: `'/project/sub"`},
	} {
		t.Run(test.value, func(t *testing.T) {
			got, ok := makeDirectoryMarkerValue(test.value)
			if got != test.want || ok != test.ok {
				t.Fatalf("makeDirectoryMarkerValue(%q) = %q, %t; want %q, %t",
					test.value, got, ok, test.want, test.ok)
			}
		})
	}
}

func TestParseTracksMakeDirectoryContainingApostropheAndSpace(t *testing.T) {
	projectDir := t.TempDir()
	childDir := filepath.Join(projectDir, "a'b c")
	outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
	tool := newTestTool(t, Config{
		InputFile:    "stdin",
		OutputFile:   outputFile,
		BuildDir:     projectDir,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
		NoStrict:     true,
	})

	tool.Parse([]string{
		"make: Entering directory '" + childDir + "'",
		"gcc -c child.c",
		"make: Leaving directory '" + childDir + "'",
		"gcc -c parent.c",
	})

	commands := readCompilerTestCommands(t, outputFile)
	if len(commands) != 2 || commands[0].Directory != trackedPathToSlash(childDir) ||
		commands[1].Directory != trackedPathToSlash(projectDir) {
		t.Fatalf("Make directory marker containing apostrophe and space was not tracked: %#v", commands)
	}
}

func TestMakeDirectoryMarkerPrefix(t *testing.T) {
	for _, test := range []struct {
		prefix      string
		makeCommand string
		want        bool
	}{
		{prefix: "make", want: true},
		{prefix: "make[1]", want: true},
		{prefix: "/usr/bin/gmake[12]", want: true},
		{prefix: `C:\\tools\\mingw32-make.exe[2]`, want: true},
		{prefix: "/opt/tools/custom-make[3]", want: true},
		{prefix: "/opt/tools/custom-make[3]", makeCommand: "/opt/tools/custom-make", want: true},
		{prefix: "printf make", want: false},
		{prefix: `not\make`, want: false},
		{prefix: `not\custom-make`, makeCommand: "/opt/tools/custom-make", want: false},
		{prefix: "unrelated", makeCommand: "/opt/tools/custom-make", want: false},
		{prefix: "notmake", want: false},
		{prefix: "make[x]", want: false},
		{prefix: "make[]", want: false},
	} {
		t.Run(test.prefix, func(t *testing.T) {
			if got := isMakeDirectoryMarkerPrefix(test.prefix, test.makeCommand); got != test.want {
				t.Fatalf("isMakeDirectoryMarkerPrefix(%q, %q) = %t, want %t",
					test.prefix, test.makeCommand, got, test.want)
			}
		})
	}
}

func TestParseTracksUnconfiguredCustomMakeDirectoryMarkers(t *testing.T) {
	projectDir := t.TempDir()
	childDir := filepath.Join(projectDir, "sub")
	outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
	tool := newTestTool(t, Config{
		InputFile:    "stdin",
		OutputFile:   outputFile,
		BuildDir:     projectDir,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
		NoStrict:     true,
	})

	tool.Parse([]string{
		"custom-make[1]: Entering directory '" + childDir + "'",
		"gcc -c child.c",
		"custom-make[1]: Leaving directory '" + childDir + "'",
		"gcc -c parent.c",
	})

	commands := readCompilerTestCommands(t, outputFile)
	if len(commands) != 2 || commands[0].Directory != trackedPathToSlash(childDir) ||
		commands[1].Directory != trackedPathToSlash(projectDir) {
		t.Fatalf("unconfigured custom Make directory markers were not tracked: %#v", commands)
	}
}

func TestParseTracksConfiguredMakeDirectoryMarkers(t *testing.T) {
	projectDir := t.TempDir()
	childDir := filepath.Join(projectDir, "sub")
	outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
	makeCommand := filepath.Join(projectDir, "tools", "custom-make")
	tool := newTestTool(t, Config{
		InputFile:    "stdin",
		OutputFile:   outputFile,
		BuildDir:     projectDir,
		MakeCommand:  makeCommand,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
		NoStrict:     true,
	})
	tool.Parse([]string{
		"custom-make[1]: Entering directory '" + childDir + "'",
		"gcc -c child.c",
		"custom-make[1]: Leaving directory '" + childDir + "'",
		`not\custom-make[1]: Entering directory '/forged'`,
		"gcc -c parent.c",
	})

	commands := readCompilerTestCommands(t, outputFile)
	if len(commands) != 2 || commands[0].Directory != trackedPathToSlash(childDir) ||
		commands[1].Directory != trackedPathToSlash(projectDir) {
		t.Fatalf("configured Make directory markers were not tracked: %#v", commands)
	}
}

func TestParseConfirmsMakeCommandDirectoryFrame(t *testing.T) {
	projectDir := t.TempDir()
	childDir := filepath.Join(projectDir, "sub")
	outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
	tool := newTestTool(t, Config{
		InputFile:    "stdin",
		OutputFile:   outputFile,
		BuildDir:     projectDir,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
		NoStrict:     true,
	})
	tool.Parse([]string{
		"make -C sub",
		"make[1]: Entering directory '" + childDir + "'",
		"gcc -c child.c",
		"make[1]: Leaving directory '" + childDir + "'",
		"gcc -c parent.c",
	})

	commands := readCompilerTestCommands(t, outputFile)
	if len(commands) != 2 || commands[0].Directory != trackedPathToSlash(childDir) ||
		commands[1].Directory != trackedPathToSlash(projectDir) {
		t.Fatalf("make directory frame was duplicated: %#v", commands)
	}
}

func TestParseTracksNestedMakeDirectoryMarkers(t *testing.T) {
	projectDir := t.TempDir()
	childDir := filepath.Join(projectDir, "child")
	grandchildDir := filepath.Join(childDir, "grandchild")
	outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
	tool := newTestTool(t, Config{
		InputFile:    "stdin",
		OutputFile:   outputFile,
		BuildDir:     projectDir,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
		NoStrict:     true,
	})

	tool.Parse([]string{
		"make[1]: Entering directory '" + childDir + "'",
		"gcc -c child-before.c",
		"make[2]: Entering directory '" + grandchildDir + "'",
		"gcc -c grandchild.c",
		"make[2]: Leaving directory '" + grandchildDir + "'",
		"gcc -c child-after.c",
		"make[1]: Leaving directory '" + childDir + "'",
		"gcc -c parent.c",
	})

	commands := readCompilerTestCommands(t, outputFile)
	wantFiles := []string{"child-before.c", "grandchild.c", "child-after.c", "parent.c"}
	wantDirectories := []string{childDir, grandchildDir, childDir, projectDir}
	if len(commands) != len(wantFiles) {
		t.Fatalf("unexpected nested Make commands: %#v", commands)
	}
	for i := range commands {
		if commands[i].File != wantFiles[i] || commands[i].Directory != trackedPathToSlash(wantDirectories[i]) {
			t.Fatalf("nested Make directory %d was not tracked: %#v", i, commands)
		}
	}
}

func TestParseResolvesRelativeMakeDirectoryForMacrosAndPath(t *testing.T) {
	projectDir := t.TempDir()
	workingDir := filepath.Join(projectDir, "sub")
	compilerDir := filepath.Join(workingDir, "toolchain")
	if err := os.MkdirAll(compilerDir, 0o755); err != nil {
		t.Fatalf("create compiler directory failed: %v", err)
	}
	compiler := filepath.Join(compilerDir, "fake-gcc")
	if err := os.WriteFile(compiler, []byte("#!/bin/sh\necho '#define FROM_MAKE_C 1'\n"), 0o755); err != nil {
		t.Fatalf("write compiler failed: %v", err)
	}
	t.Setenv("PATH", "toolchain")

	outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
	tool := newTestTool(t, Config{
		InputFile:    filepath.Join(projectDir, "build.log"),
		OutputFile:   outputFile,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
		NoStrict:     true,
		Macros:       true,
		FullPath:     true,
	})
	tool.Parse([]string{"make -C sub", "fake-gcc -c main.c"})

	commands := readCompilerTestCommands(t, outputFile)
	if len(commands) != 1 {
		t.Fatalf("expected one command, got %#v", commands)
	}
	if commands[0].Directory != trackedPathToSlash(workingDir) || commands[0].Arguments[0] != hostPathToDatabasePath(compiler) ||
		!slices.Contains(commands[0].Arguments, "-DFROM_MAKE_C=1") {
		t.Fatalf("relative make directory was not resolved: %#v", commands[0])
	}
}
