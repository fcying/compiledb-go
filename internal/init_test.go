package internal

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
		InputFile:  "stdin",
		OutputFile: outputFile,
		NoStrict:   true,
	})

	tool.Generate()
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
	})

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
