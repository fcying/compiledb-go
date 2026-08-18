package internal

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	log "github.com/sirupsen/logrus"
)

func newTestTool(t *testing.T, cfg Config) *Tool {
	t.Helper()

	logger := log.New()
	logger.SetOutput(io.Discard)

	return NewTool(cfg, logger)
}

func TestGenerateFromStdinDoesNotPanic(t *testing.T) {
	tmpDir := t.TempDir()
	outputFile := filepath.Join(tmpDir, "compile_commands.json")

	oldStdin := os.Stdin
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe failed: %v", err)
	}
	defer r.Close()

	if _, err := w.WriteString("gcc -c missing.c\n"); err != nil {
		t.Fatalf("write stdin failed: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close writer failed: %v", err)
	}

	os.Stdin = r
	defer func() { os.Stdin = oldStdin }()

	tool := newTestTool(t, Config{
		InputFile:  "-",
		OutputFile: outputFile,
		NoStrict:   true,
	})

	tool.Generate()
}

func TestScanBuildLogPhysicalLineLimit(t *testing.T) {
	if reason := buildLogLineLimitReason(maxBuildLogLineSize); reason != "physical line exceeds 100 MiB limit" {
		t.Fatalf("unexpected production line-limit diagnostic: %q", reason)
	}

	const limit = 8
	for name, input := range map[string]string{
		"LF":   strings.Repeat("x", limit) + "\n",
		"CRLF": strings.Repeat("x", limit) + "\r\n",
		"EOF":  strings.Repeat("x", limit),
	} {
		t.Run(name, func(t *testing.T) {
			lines := scanBuildLogWithLimit([]byte(input), limit)
			if len(lines) != 1 || lines[0].oversized || lines[0].text != strings.Repeat("x", limit) {
				t.Fatalf("line at limit was not preserved: %#v", lines)
			}
		})
	}

	lines := scanBuildLogWithLimit([]byte(strings.Repeat("x", limit+1)+"\r\nok\n"), limit)
	if len(lines) != 2 || !lines[0].oversized || lines[0].limit != limit || lines[0].text != "" ||
		lines[1].oversized || lines[1].text != "ok" {
		t.Fatalf("oversized line was not discarded cleanly: %#v", lines)
	}
}

func TestGenerateSkipsOversizedPhysicalLine(t *testing.T) {
	projectDir := t.TempDir()
	buildLog := filepath.Join(projectDir, "build.log")
	outputFile := filepath.Join(projectDir, "compile_commands.json")
	contents := strings.Repeat("x", 32) + "\\\ncc -c joined.c\ncc -c valid.c\n"
	if err := os.WriteFile(buildLog, []byte(contents), 0o644); err != nil {
		t.Fatalf("write build log failed: %v", err)
	}

	var logs bytes.Buffer
	tool := newTestTool(t, Config{
		InputFile:    buildLog,
		OutputFile:   outputFile,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
		NoStrict:     true,
	})
	tool.buildLogLineLimit = 32
	tool.Logger.SetLevel(log.ErrorLevel)
	tool.Logger.SetOutput(&logs)
	tool.Generate()

	commands := readCompilerTestCommands(t, outputFile)
	if tool.StatusCode != 0 || len(commands) != 1 || commands[0].File != "valid.c" {
		t.Fatalf("oversized build-log line stopped Generate: status=%d commands=%#v", tool.StatusCode, commands)
	}
	diagnostic := logs.String()
	for _, want := range []string{"build log line 1", "cwd", "physical line exceeds 32 byte limit", "at byte 32"} {
		if !strings.Contains(diagnostic, want) {
			t.Fatalf("physical line diagnostic lacks %q: %q", want, diagnostic)
		}
	}
}

func TestGenerateResolvesResponseFileFromBuildLogDirectory(t *testing.T) {
	projectDir := t.TempDir()
	buildLog := filepath.Join(projectDir, "build.log")
	responseFile := filepath.Join(projectDir, "argument files.rsp")
	outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
	if err := os.WriteFile(buildLog, []byte("gcc '@argument files.rsp'\n"), 0o644); err != nil {
		t.Fatalf("write build log failed: %v", err)
	}
	if err := os.WriteFile(responseFile, []byte("-c src/main.c"), 0o644); err != nil {
		t.Fatalf("write response file failed: %v", err)
	}

	tool := newTestTool(t, Config{
		InputFile:    buildLog,
		OutputFile:   outputFile,
		RegexCompile: RegexCompile,
		RegexFile:    RegexFile,
		NoStrict:     true,
	})
	tool.Generate()

	commands := readCompilerTestCommands(t, outputFile)
	if tool.StatusCode != 0 || len(commands) != 1 || commands[0].File != "src/main.c" ||
		commands[0].Directory != trackedPathToSlash(projectDir) || slices.Contains(commands[0].Arguments, "@argument files.rsp") {
		t.Fatalf("build-log response file was not resolved from its directory: status=%d commands=%#v", tool.StatusCode, commands)
	}
}

func TestGenerateFromStdinReturnsWhenCanceled(t *testing.T) {
	oldStdin := os.Stdin
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe failed: %v", err)
	}
	os.Stdin = r
	t.Cleanup(func() {
		os.Stdin = oldStdin
		_ = r.Close()
		_ = w.Close()
	})

	ctx, cancel := context.WithCancel(context.Background())
	tool := newTestTool(t, Config{
		InputFile:  "stdin",
		OutputFile: filepath.Join(t.TempDir(), "compile_commands.json"),
		NoStrict:   true,
	})
	tool.Context = ctx
	done := make(chan struct{})
	go func() {
		tool.Generate()
		close(done)
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Generate did not return after its context was canceled")
	}
	if tool.StatusCode == 0 {
		t.Fatal("canceled Generate returned a successful status")
	}
	if _, err := w.WriteString("unread after cancellation\n"); err != nil {
		t.Fatalf("write after cancellation failed: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close stdin writer failed: %v", err)
	}
	remaining, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read after cancellation failed: %v", err)
	}
	if string(remaining) != "unread after cancellation\n" {
		t.Fatalf("canceled stdin reader remained active: %q", remaining)
	}
	if _, err := os.Stat(tool.Config.OutputFile); !os.IsNotExist(err) {
		t.Fatalf("canceled Generate wrote a compilation database: %v", err)
	}
}

func TestWriteJSONUpdatesExistingDatabase(t *testing.T) {
	tmpDir := t.TempDir()
	outputFile := filepath.Join(tmpDir, "compile_commands.json")
	existing := []map[string]any{
		{
			"directory": tmpDir,
			"command":   "cc -c keep.c",
			"file":      "keep.c",
			"output":    "keep.o",
			"extension": map[string]any{"owner": "parent"},
		},
		{
			"directory": tmpDir,
			"command":   "cc -DOLD -c replace.c",
			"file":      "replace.c",
			"output":    "replace.o",
		},
	}
	writeTestJSON(t, outputFile, existing)

	tool := newTestTool(t, Config{OutputFile: outputFile, NoStrict: true})
	commands := []Command{
		{Directory: tmpDir, Arguments: []string{"cc", "-DNEW", "-c", "replace.c"}, File: "replace.c"},
		{Directory: tmpDir, Arguments: []string{"cc", "-DONE", "-c", "new.c"}, File: "new.c"},
		{Directory: tmpDir, Arguments: []string{"cc", "-DTWO", "-c", "new.c"}, File: "new.c"},
	}
	tool.WriteJSON(outputFile, len(commands), &commands)

	entries := readTestDatabase(t, outputFile)
	if len(entries) != 3 {
		t.Fatalf("expected 3 merged entries, got %d", len(entries))
	}

	keep := findTestEntry(t, entries, "keep.c")
	if keep["output"] != "keep.o" {
		t.Fatalf("expected old output field to be preserved, got %#v", keep)
	}
	extension, ok := keep["extension"].(map[string]any)
	if !ok || extension["owner"] != "parent" {
		t.Fatalf("expected old extension field to be preserved, got %#v", keep)
	}

	replaced := findTestEntry(t, entries, "replace.c")
	if _, found := replaced["output"]; found {
		t.Fatalf("expected new entry to replace the complete old entry, got %#v", replaced)
	}
	assertTestArgument(t, replaced, 1, "-DNEW")

	newEntry := findTestEntry(t, entries, "new.c")
	assertTestArgument(t, newEntry, 1, "-DTWO")
}

func TestWriteJSONReplacesLegacyPathRepresentations(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("get working directory failed: %v", err)
	}
	for name, test := range map[string]struct {
		existing map[string]any
		command  Command
		buildDir string
	}{
		"relative build directory": {
			existing: map[string]any{
				"directory": "legacy-build",
				"command":   "cc -DOLD -c main.c",
				"file":      "main.c",
				"output":    "main.o",
			},
			command: Command{
				Directory: filepath.Join(cwd, "legacy-build"),
				Arguments: []string{"cc", "-DNEW", "-c", "main.c"},
				File:      "main.c",
			},
			buildDir: filepath.Join(cwd, "legacy-build"),
		},
		"relative build directory with trailing separator": {
			existing: map[string]any{
				"directory": "legacy-build/",
				"command":   "cc -DOLD -c main.c",
				"file":      "main.c",
				"output":    "main.o",
			},
			command: Command{
				Directory: filepath.Join(cwd, "legacy-build"),
				Arguments: []string{"cc", "-DNEW", "-c", "main.c"},
				File:      "main.c",
			},
			buildDir: filepath.Join(cwd, "legacy-build"),
		},
		"Windows separators and case": {
			existing: map[string]any{
				"directory": `C:\WORK`,
				"command":   `cc -DOLD -c C:\WORK\main.c`,
				"file":      `C:\WORK\main.c`,
				"output":    "main.o",
			},
			command: Command{
				Directory: "c:/work",
				Arguments: []string{"cc", "-DNEW", "-c", "c:/work/main.c"},
				File:      "c:/work/main.c",
			},
		},
		"Windows trailing separator": {
			existing: map[string]any{
				"directory": `C:\WORK\`,
				"command":   `cc -DOLD -c main.c`,
				"file":      "main.c",
				"output":    "main.o",
			},
			command: Command{
				Directory: "c:/work",
				Arguments: []string{"cc", "-DNEW", "-c", "main.c"},
				File:      "main.c",
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
			writeTestJSON(t, outputFile, []map[string]any{test.existing})
			tool := newTestTool(t, Config{OutputFile: outputFile, BuildDir: test.buildDir, NoStrict: true})
			commands := []Command{test.command}

			tool.WriteJSON(outputFile, 1, &commands)

			entries := readTestDatabase(t, outputFile)
			if len(entries) != 1 {
				t.Fatalf("legacy and current paths were not merged: %#v", entries)
			}
			if _, found := entries[0]["output"]; found {
				t.Fatalf("legacy entry was not completely replaced: %#v", entries[0])
			}
			assertTestArgument(t, entries[0], 1, "-DNEW")
		})
	}
}

func TestWriteJSONKeepsDistinctPOSIXColonPaths(t *testing.T) {
	outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
	writeTestJSON(t, outputFile, []map[string]any{
		{"directory": "1:a", "command": "cc -c Main.c", "file": "Main.c", "output": "first.o"},
		{"directory": "1:A", "command": "cc -c main.c", "file": "main.c", "output": "second.o"},
	})
	tool := newTestTool(t, Config{OutputFile: outputFile, NoStrict: true})
	commands := []Command{}

	tool.WriteJSON(outputFile, 0, &commands)

	entries := readTestDatabase(t, outputFile)
	if len(entries) != 2 {
		t.Fatalf("distinct POSIX colon paths were merged: %#v", entries)
	}
}

func TestWriteJSONKeepsWindowsDirectoryAndPOSIXAbsoluteFileKeysDistinct(t *testing.T) {
	outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
	writeTestJSON(t, outputFile, []map[string]any{
		{
			"directory": "C:/build",
			"command":   "cc -c /src/../main.c",
			"file":      "/src/../main.c",
			"extension": map[string]any{"raw": "preserve"},
		},
		{
			"directory": "c:/build",
			"command":   "cc -c src/../main.c",
			"file":      "src/../main.c",
			"output":    "relative.o",
		},
	})
	tool := newTestTool(t, Config{OutputFile: outputFile, NoStrict: true})
	commands := []Command{}

	tool.WriteJSON(outputFile, 0, &commands)

	entries := readTestDatabase(t, outputFile)
	if len(entries) != 2 {
		t.Fatalf("distinct absolute and relative keys were merged: %#v", entries)
	}
	absolute := findTestEntry(t, entries, "/src/../main.c")
	extension, ok := absolute["extension"].(map[string]any)
	if !ok || extension["raw"] != "preserve" {
		t.Fatalf("absolute entry fields were not preserved: %#v", absolute)
	}
	findTestEntry(t, entries, "src/../main.c")
}

func TestWriteJSONStrictUsesCurrentDirectoryWithoutBuildDir(t *testing.T) {
	root := t.TempDir()
	originalCWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("get current directory failed: %v", err)
	}
	if err := os.Chdir(root); err != nil {
		t.Fatalf("change current directory failed: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(originalCWD) })
	if err := os.WriteFile("keep.c", nil, 0o644); err != nil {
		t.Fatalf("create source failed: %v", err)
	}
	outputFile := filepath.Join(root, "compile_commands.json")
	writeTestJSON(t, outputFile, []map[string]any{{
		"directory": "./",
		"command":   "cc -c keep.c",
		"file":      "keep.c",
		"extension": map[string]any{"raw": "keep"},
	}})
	tool := newTestTool(t, Config{InputFile: "stdin", OutputFile: outputFile})
	commands := []Command{}

	tool.WriteJSON(outputFile, 0, &commands)

	entries := readTestDatabase(t, outputFile)
	if len(entries) != 1 {
		t.Fatalf("strict merge dropped current-directory entry: %#v", entries)
	}
	extension, ok := entries[0]["extension"].(map[string]any)
	if !ok || extension["raw"] != "keep" {
		t.Fatalf("current-directory entry fields were not preserved: %#v", entries[0])
	}
}

func TestWriteJSONStrictResolvesPOSIXColonRelativePath(t *testing.T) {
	root := t.TempDir()
	buildDir := filepath.Join(root, "1:a")
	if err := os.Mkdir(buildDir, 0o755); err != nil {
		t.Fatalf("create colon build directory failed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(buildDir, "Main.c"), nil, 0o644); err != nil {
		t.Fatalf("create colon source failed: %v", err)
	}
	outputFile := filepath.Join(root, "compile_commands.json")
	writeTestJSON(t, outputFile, []map[string]any{{
		"directory": "1:a/",
		"command":   "cc -c Main.c",
		"file":      "Main.c",
	}})
	tool := newTestTool(t, Config{OutputFile: outputFile, BuildDir: buildDir})
	commands := []Command{}

	tool.WriteJSON(outputFile, 0, &commands)

	entries := readTestDatabase(t, outputFile)
	if len(entries) != 1 || entries[0]["file"] != "Main.c" {
		t.Fatalf("strict merge dropped POSIX colon path: %#v", entries)
	}
}

func TestWriteJSONLegacyRelativePathsDoNotDependOnCWD(t *testing.T) {
	root := t.TempDir()
	buildDir := filepath.Join(root, "project", "build")
	if err := os.MkdirAll(buildDir, 0o755); err != nil {
		t.Fatalf("create build directory failed: %v", err)
	}
	for _, name := range []string{"keep.c", "main.c"} {
		if err := os.WriteFile(filepath.Join(buildDir, name), nil, 0o644); err != nil {
			t.Fatalf("create source %s failed: %v", name, err)
		}
	}
	outputFile := filepath.Join(root, "compile_commands.json")
	writeTestJSON(t, outputFile, []map[string]any{
		{"directory": "project/build", "command": "cc -c keep.c", "file": "keep.c", "output": "keep.o"},
		{"directory": "project/build", "command": "cc -DOLD -c main.c", "file": "main.c"},
	})

	originalCWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("get working directory failed: %v", err)
	}
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatalf("change working directory failed: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(originalCWD) })

	tool := newTestTool(t, Config{OutputFile: outputFile, BuildDir: buildDir})
	commands := []Command{{Directory: buildDir, Arguments: []string{"cc", "-DNEW", "-c", "main.c"}, File: "main.c"}}
	tool.WriteJSON(outputFile, 1, &commands)

	entries := readTestDatabase(t, outputFile)
	if len(entries) != 2 {
		t.Fatalf("legacy relative paths depended on process cwd: %#v", entries)
	}
	if findTestEntry(t, entries, "keep.c")["output"] != "keep.o" {
		t.Fatalf("strict merge dropped the existing relative entry: %#v", entries)
	}
	assertTestArgument(t, findTestEntry(t, entries, "main.c"), 1, "-DNEW")
}

func TestWriteJSONKeepsDistinctPOSIXBackslashPath(t *testing.T) {
	tmpDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmpDir, `src\main.c`), nil, 0o644); err != nil {
		t.Fatalf("create backslash source failed: %v", err)
	}
	if err := os.Mkdir(filepath.Join(tmpDir, "src"), 0o755); err != nil {
		t.Fatalf("create source directory failed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tmpDir, "src", "main.c"), nil, 0o644); err != nil {
		t.Fatalf("create slash source failed: %v", err)
	}
	outputFile := filepath.Join(tmpDir, "compile_commands.json")
	writeTestJSON(t, outputFile, []map[string]any{{
		"directory": tmpDir,
		"command":   `cc -c 'src\main.c'`,
		"file":      `src\main.c`,
		"output":    "backslash.o",
	}})

	tool := newTestTool(t, Config{OutputFile: outputFile})
	commands := []Command{{Directory: tmpDir, Arguments: []string{"cc", "-c", "src/main.c"}, File: "src/main.c"}}
	tool.WriteJSON(outputFile, 1, &commands)

	entries := readTestDatabase(t, outputFile)
	if len(entries) != 2 {
		t.Fatalf("distinct POSIX paths were merged: %#v", entries)
	}
	if findTestEntry(t, entries, `src\main.c`)["output"] != "backslash.o" {
		t.Fatalf("backslash entry fields were not preserved: %#v", entries)
	}
	findTestEntry(t, entries, "src/main.c")
}

func TestCompilationDatabaseKeyUsesSlashSeparatedPaths(t *testing.T) {
	for name, test := range map[string]struct {
		entry compilationDatabaseEntry
		want  string
	}{
		"relative file": {
			entry: compilationDatabaseEntry{Directory: "C:/build", File: "src/main.c"},
			want:  "C:/build/src/main.c",
		},
		"trailing slash": {
			entry: compilationDatabaseEntry{Directory: "C:/build/", File: "src/main.c"},
			want:  "C:/build/src/main.c",
		},
		"absolute file": {
			entry: compilationDatabaseEntry{Directory: "C:/build", File: "C:/src/main.c"},
			want:  "C:/src/main.c",
		},
		"parent segment": {
			entry: compilationDatabaseEntry{Directory: "/opt/src", File: "../main.c"},
			want:  "/opt/src/../main.c",
		},
	} {
		t.Run(name, func(t *testing.T) {
			if got := compilationDatabaseKey(test.entry); got != test.want {
				t.Fatalf("unexpected key: want %q, got %q", test.want, got)
			}
		})
	}
}

func TestCompilationDatabaseCompatibilityKeysPreserveRawWindowsPath(t *testing.T) {
	entry := compilationDatabaseEntry{
		Directory: `C:\Work\Src`,
		File:      `..\Main.c`,
	}

	keys := compilationDatabaseCompatibilityKeys(entry, `C:\Work\Src`)
	want := []string{
		`C:\Work\Src/..\Main.c`,
		"c:/work/src/../main.c",
	}
	if len(keys) != len(want) {
		t.Fatalf("unexpected compatibility keys: want %q, got %q", want, keys)
	}
	for i := range want {
		if keys[i] != want[i] {
			t.Fatalf("unexpected compatibility key %d: want %q, got %q", i, want[i], keys[i])
		}
	}
}

func TestExplicitWindowsAbsolutePath(t *testing.T) {
	for name, test := range map[string]struct {
		path string
		want bool
	}{
		"drive slash":          {path: "C:/src/main.c", want: true},
		"drive backslash":      {path: `c:\src\main.c`, want: true},
		"UNC slash":            {path: "//server/share/main.c", want: true},
		"UNC backslash":        {path: `\\server\share\main.c`, want: true},
		"drive relative":       {path: `C:src\main.c`, want: false},
		"non-letter drive":     {path: "1:/src/main.c", want: false},
		"POSIX absolute":       {path: "/src/main.c", want: false},
		"MSYS drive":           {path: "/c/src/main.c", want: false},
		"relative backslash":   {path: `src\main.c`, want: false},
		"single leading slash": {path: `\src\main.c`, want: false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := isExplicitWindowsAbsolutePath(test.path); got != test.want {
				t.Fatalf("unexpected Windows path classification for %q: want %v, got %v", test.path, test.want, got)
			}
		})
	}
}

func TestWriteJSONKeepsParentSegmentKeysDistinct(t *testing.T) {
	outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
	writeTestJSON(t, outputFile, []map[string]any{
		{"directory": "/opt/src", "command": "cc -c ../main.c", "file": "../main.c", "extension": map[string]any{"keep": true}},
		{"directory": "/opt", "command": "cc -c main.c", "file": "main.c", "output": "main.o"},
	})

	tool := newTestTool(t, Config{OutputFile: outputFile, NoStrict: true})
	commands := []Command{}
	tool.WriteJSON(outputFile, 0, &commands)

	entries := readTestDatabase(t, outputFile)
	if len(entries) != 2 {
		t.Fatalf("raw parent-segment keys were normalized together: %#v", entries)
	}
	parent := findTestEntry(t, entries, "../main.c")
	if extension, ok := parent["extension"].(map[string]any); !ok || extension["keep"] != true {
		t.Fatalf("raw parent-segment entry was not preserved: %#v", parent)
	}
	findTestEntry(t, entries, "main.c")
}

func TestWriteJSONOverwriteSkipsExistingDatabase(t *testing.T) {
	tmpDir := t.TempDir()
	outputFile := filepath.Join(tmpDir, "compile_commands.json")
	writeTestJSON(t, outputFile, []map[string]any{{
		"directory": tmpDir,
		"command":   "cc -c old.c",
		"file":      "old.c",
	}})

	tool := newTestTool(t, Config{OutputFile: outputFile, NoStrict: true, Overwrite: true})
	commands := []Command{{Directory: tmpDir, Arguments: []string{"cc", "-c", "new.c"}, File: "new.c"}}
	tool.WriteJSON(outputFile, len(commands), &commands)

	entries := readTestDatabase(t, outputFile)
	if len(entries) != 1 || entries[0]["file"] != "new.c" {
		t.Fatalf("expected overwrite to keep only the new entry, got %#v", entries)
	}
}

func TestWriteJSONTreatsInvalidDatabaseAsEmpty(t *testing.T) {
	for name, contents := range map[string]string{
		"empty":         "",
		"invalid JSON":  "not json",
		"invalid entry": `[{"directory":null,"file":"old.c"}]`,
	} {
		t.Run(name, func(t *testing.T) {
			outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
			if err := os.WriteFile(outputFile, []byte(contents), 0o644); err != nil {
				t.Fatalf("seed invalid database failed: %v", err)
			}

			tool := newTestTool(t, Config{OutputFile: outputFile, NoStrict: true})
			commands := []Command{{Directory: "/project", Arguments: []string{"cc", "-c", "new.c"}, File: "new.c"}}
			tool.WriteJSON(outputFile, len(commands), &commands)

			entries := readTestDatabase(t, outputFile)
			if len(entries) != 1 || entries[0]["file"] != "new.c" {
				t.Fatalf("expected invalid database to be replaced with new entries, got %#v", entries)
			}
		})
	}
}

func TestWriteJSONSkipsInvalidEntriesWithoutDroppingValidEntries(t *testing.T) {
	tmpDir := t.TempDir()
	outputFile := filepath.Join(tmpDir, "compile_commands.json")
	existing := `[
  {"directory":"/project","command":"cc -c keep.c","file":"keep.c","output":"keep.o","extension":{"owner":"parent"}},
  {"directory":"/project","command":"cc -shared lib.o"}
]`
	if err := os.WriteFile(outputFile, []byte(existing), 0o644); err != nil {
		t.Fatalf("seed database failed: %v", err)
	}

	tool := newTestTool(t, Config{OutputFile: outputFile, NoStrict: true})
	commands := []Command{{Directory: "/project", Arguments: []string{"cc", "-c", "new.c"}, File: "new.c"}}
	tool.WriteJSON(outputFile, len(commands), &commands)

	entries := readTestDatabase(t, outputFile)
	if len(entries) != 2 {
		t.Fatalf("expected valid old entry and new entry, got %#v", entries)
	}
	keep := findTestEntry(t, entries, "keep.c")
	if keep["output"] != "keep.o" {
		t.Fatalf("expected valid old entry to be preserved, got %#v", keep)
	}
	findTestEntry(t, entries, "new.c")
}

func TestWriteJSONStrictFiltersMergedDatabase(t *testing.T) {
	tmpDir := t.TempDir()
	outputFile := filepath.Join(tmpDir, "compile_commands.json")
	for _, name := range []string{"keep.c", "new.c"} {
		if err := os.WriteFile(filepath.Join(tmpDir, name), nil, 0o644); err != nil {
			t.Fatalf("create source %s failed: %v", name, err)
		}
	}
	notDirectory := filepath.Join(tmpDir, "not-a-directory")
	if err := os.WriteFile(notDirectory, nil, 0o644); err != nil {
		t.Fatalf("create non-directory path failed: %v", err)
	}
	writeTestJSON(t, outputFile, []map[string]any{
		{"directory": tmpDir, "command": "cc -c keep.c", "file": "keep.c", "output": "keep.o"},
		{"directory": tmpDir, "command": "cc -c stale.c", "file": "stale.c"},
		{"directory": notDirectory, "command": "cc -c child.c", "file": "child.c"},
		{"directory": tmpDir, "command": "cc -c source-dir.c", "file": "source-dir.c"},
	})
	if err := os.Mkdir(filepath.Join(tmpDir, "source-dir.c"), 0o755); err != nil {
		t.Fatalf("create source-like directory failed: %v", err)
	}

	tool := newTestTool(t, Config{OutputFile: outputFile})
	commands := []Command{
		{Directory: tmpDir, Arguments: []string{"cc", "-c", "new.c"}, File: "new.c"},
		{Directory: tmpDir, Arguments: []string{"cc", "-c", "missing.c"}, File: "missing.c"},
	}
	tool.WriteJSON(outputFile, len(commands), &commands)

	entries := readTestDatabase(t, outputFile)
	if len(entries) != 2 {
		t.Fatalf("expected 2 existing source entries, got %#v", entries)
	}
	findTestEntry(t, entries, "keep.c")
	findTestEntry(t, entries, "new.c")
}

func TestWriteJSONStdoutUsesOnlyCurrentEntries(t *testing.T) {
	tmpDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmpDir, "new.c"), nil, 0o644); err != nil {
		t.Fatalf("create source failed: %v", err)
	}

	oldStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("create stdout pipe failed: %v", err)
	}
	os.Stdout = w
	t.Cleanup(func() { os.Stdout = oldStdout })

	tool := newTestTool(t, Config{OutputFile: "-"})
	commands := []Command{
		{Directory: tmpDir, Arguments: []string{"cc", "-DONE", "-c", "new.c"}, File: "new.c"},
		{Directory: tmpDir, Arguments: []string{"cc", "-DTWO", "-c", "new.c"}, File: "new.c"},
		{Directory: tmpDir, Arguments: []string{"cc", "-c", "missing.c"}, File: "missing.c"},
	}
	tool.WriteJSON("-", len(commands), &commands)
	if err := w.Close(); err != nil {
		t.Fatalf("close stdout writer failed: %v", err)
	}

	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read stdout failed: %v", err)
	}
	if !strings.HasSuffix(string(data), "\n") {
		t.Fatalf("expected stdout newline, got %q", string(data))
	}

	var entries []map[string]any
	if err := json.Unmarshal(data, &entries); err != nil {
		t.Fatalf("decode stdout failed: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected one deduplicated current entry, got %#v", entries)
	}
	assertTestArgument(t, entries[0], 1, "-DTWO")
}

func TestWriteJSONFileEndsWithOneNewline(t *testing.T) {
	outputFile := filepath.Join(t.TempDir(), "compile_commands.json")
	tool := newTestTool(t, Config{OutputFile: outputFile, NoStrict: true})
	commands := []Command{{Directory: "/project", Arguments: []string{"cc", "-c", "main.c"}, File: "main.c"}}

	tool.WriteJSON(outputFile, len(commands), &commands)

	data, err := os.ReadFile(outputFile)
	if err != nil {
		t.Fatalf("read output failed: %v", err)
	}
	if !strings.HasSuffix(string(data), "\n") || strings.HasSuffix(string(data), "\n\n") {
		t.Fatalf("expected exactly one trailing newline, got %q", string(data))
	}
	var entries []map[string]any
	if err := json.Unmarshal(data, &entries); err != nil {
		t.Fatalf("decode output failed: %v", err)
	}
	if len(entries) != 1 || entries[0]["file"] != "main.c" {
		t.Fatalf("unexpected entries: %#v", entries)
	}
}

func TestWriteJSONCreatesMissingParentDirectory(t *testing.T) {
	outputFile := filepath.Join(t.TempDir(), "build", "nested", "compile_commands.json")
	tool := newTestTool(t, Config{OutputFile: outputFile, NoStrict: true})
	commands := []Command{{Directory: "/project", Arguments: []string{"cc", "-c", "main.c"}, File: "main.c"}}

	tool.WriteJSON(outputFile, len(commands), &commands)

	entries := readTestDatabase(t, outputFile)
	if len(entries) != 1 || entries[0]["file"] != "main.c" {
		t.Fatalf("unexpected entries: %#v", entries)
	}
}

func TestWriteJSONStdoutReturnsCanceledStatusWhenBlocked(t *testing.T) {
	oldStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("create stdout pipe failed: %v", err)
	}
	os.Stdout = w
	t.Cleanup(func() {
		os.Stdout = oldStdout
		_ = r.Close()
		_ = w.Close()
	})

	ctx, cancel := context.WithCancelCause(context.Background())
	tool := newTestTool(t, Config{OutputFile: "-", NoStrict: true})
	tool.Context = ctx
	commands := make([]Command, 10000)
	for i := range commands {
		file := fmt.Sprintf("main-%d-%s.c", i, strings.Repeat("x", 128))
		commands[i] = Command{Directory: "/tmp", Arguments: []string{"cc", "-c", file}, File: file}
	}
	done := make(chan struct{})
	go func() {
		tool.WriteJSON("-", len(commands), &commands)
		close(done)
	}()
	time.Sleep(100 * time.Millisecond)
	cancel(SignalError{ProcessSignal: os.Interrupt})
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("blocked JSON output did not stop after cancellation")
	}
	if tool.StatusCode == 0 {
		t.Fatal("canceled JSON output returned status 0")
	}
}

func writeTestJSON(t *testing.T, filename string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("encode test JSON failed: %v", err)
	}
	if err := os.WriteFile(filename, data, 0o644); err != nil {
		t.Fatalf("write test JSON failed: %v", err)
	}
}

func readTestDatabase(t *testing.T, filename string) []map[string]any {
	t.Helper()
	data, err := os.ReadFile(filename)
	if err != nil {
		t.Fatalf("read compilation database failed: %v", err)
	}
	var entries []map[string]any
	if err := json.Unmarshal(data, &entries); err != nil {
		t.Fatalf("decode compilation database failed: %v", err)
	}
	return entries
}

func findTestEntry(t *testing.T, entries []map[string]any, file string) map[string]any {
	t.Helper()
	for _, entry := range entries {
		if entry["file"] == file {
			return entry
		}
	}
	t.Fatalf("entry for %s not found in %#v", file, entries)
	return nil
}

func assertTestArgument(t *testing.T, entry map[string]any, index int, want string) {
	t.Helper()
	arguments, ok := entry["arguments"].([]any)
	if !ok || len(arguments) <= index || arguments[index] != want {
		t.Fatalf("expected argument %d to be %q, got %#v", index, want, entry)
	}
}
