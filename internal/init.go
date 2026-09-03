package internal

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/sirupsen/logrus"
)

type Config struct {
	InputFile    string
	OutputFile   string
	BuildDir     string
	Exclude      []string
	AddArgs      []string
	RegexCompile string
	RegexFile    string
	Encoding     string
	Macros       bool
	CommandStyle bool
	FullPath     bool
	MakeCommand  string
	NoBuild      bool
	NoStrict     bool
	Overwrite    bool
}

type Tool struct {
	Config     Config
	Logger     *logrus.Logger
	StatusCode int
	Context    context.Context

	predefinedMacros       map[string][]string
	compilerCommand        func(name string, arg ...string) *exec.Cmd
	compilerCommandContext func(context.Context, string, ...string) *exec.Cmd
	buildLogLineLimit      int
	makeDirectoryMarkers   bool
}

func NewTool(cfg Config, logger *logrus.Logger) *Tool {
	return &Tool{
		Config:                 cfg,
		Logger:                 logger,
		Context:                context.Background(),
		predefinedMacros:       make(map[string][]string),
		compilerCommandContext: exec.CommandContext,
	}
}

func (t *Tool) operationContext() context.Context {
	if t.Context != nil {
		return t.Context
	}
	return context.Background()
}

func (t *Tool) physicalLineLimit() int {
	if t.buildLogLineLimit > 0 {
		return t.buildLogLineLimit
	}
	return maxBuildLogLineSize
}

func (t *Tool) compilationDatabaseBuildDir() string {
	if t.Config.BuildDir != "" {
		return compilationDatabaseBuildDir(t.Config.BuildDir)
	}
	if t.Config.InputFile != "" && !isStdinInput(t.Config.InputFile) {
		if absolute, err := filepath.Abs(t.Config.InputFile); err == nil {
			return hostPathToDatabasePath(filepath.Dir(absolute))
		}
	}
	return compilationDatabaseBuildDir("")
}

func isStdinInput(filename string) bool {
	return filename == "-" || filename == "stdin"
}

type compilationDatabaseEntry struct {
	Directory string
	File      string
}

func decodeCompilationDatabaseEntry(data json.RawMessage) (compilationDatabaseEntry, bool) {
	var fields struct {
		Directory *string `json:"directory"`
		File      *string `json:"file"`
	}
	if err := json.Unmarshal(data, &fields); err != nil || fields.Directory == nil || fields.File == nil {
		return compilationDatabaseEntry{}, false
	}

	return compilationDatabaseEntry{Directory: *fields.Directory, File: *fields.File}, true
}

func compilationDatabaseKey(entry compilationDatabaseEntry) string {
	if isCompilationDatabaseAbsolutePath(entry.File) {
		return entry.File
	}
	if entry.Directory == "" {
		return entry.File
	}
	if entry.Directory[len(entry.Directory)-1] == '/' {
		return entry.Directory + entry.File
	}
	return entry.Directory + "/" + entry.File
}

func isCompilationDatabaseAbsolutePath(value string) bool {
	return strings.HasPrefix(value, "/") || isExplicitWindowsAbsolutePath(value)
}

func compilationDatabaseCompatibilityKeys(entry compilationDatabaseEntry, buildDir string) []string {
	buildDir = compilationDatabaseBuildDir(buildDir)
	key := compilationDatabaseKey(entry)
	keys := []string{key}
	appendKey := func(candidate string) {
		for _, existing := range keys {
			if existing == candidate {
				return
			}
		}
		keys = append(keys, candidate)
	}

	if windowsKey, ok := windowsCompilationDatabaseKey(entry); ok {
		appendKey(windowsKey)
	}
	if resolved, ok := resolveLegacyCompilationDatabasePath(entry, buildDir); ok {
		appendKey(resolved)
		if windowsKey, ok := windowsPathKey(resolved); ok {
			appendKey(windowsKey)
		}
	}
	return keys
}

func windowsCompilationDatabaseKey(entry compilationDatabaseEntry) (string, bool) {
	if !isExplicitWindowsAbsolutePath(entry.Directory) && !isExplicitWindowsAbsolutePath(entry.File) {
		return "", false
	}

	file := strings.ReplaceAll(entry.File, `\`, "/")
	if isExplicitWindowsAbsolutePath(entry.File) {
		return strings.ToLower(file), true
	}
	if strings.HasPrefix(file, "/") {
		return "", false
	}
	directory := strings.TrimRight(strings.ReplaceAll(entry.Directory, `\`, "/"), "/")
	return strings.ToLower(directory + "/" + file), true
}

func windowsPathKey(value string) (string, bool) {
	if !isExplicitWindowsAbsolutePath(value) {
		return "", false
	}
	return strings.ToLower(strings.ReplaceAll(value, `\`, "/")), true
}

func isExplicitWindowsAbsolutePath(value string) bool {
	slashPath := strings.ReplaceAll(value, `\`, "/")
	return strings.HasPrefix(slashPath, "//") || len(slashPath) > 2 &&
		((slashPath[0] >= 'a' && slashPath[0] <= 'z') || (slashPath[0] >= 'A' && slashPath[0] <= 'Z')) &&
		slashPath[1] == ':' && slashPath[2] == '/'
}

func compilationDatabaseBuildDir(buildDir string) string {
	if isCompilationDatabaseAbsolutePath(buildDir) {
		return buildDir
	}
	if buildDir != "" {
		if absolute, err := filepath.Abs(buildDir); err == nil {
			return hostPathToDatabasePath(absolute)
		}
	}
	if cwd, err := os.Getwd(); err == nil {
		return hostPathToDatabasePath(cwd)
	}
	return buildDir
}

func resolveLegacyCompilationDatabasePath(entry compilationDatabaseEntry, buildDir string) (string, bool) {
	key := compilationDatabaseKey(entry)
	if isCompilationDatabaseAbsolutePath(key) || !isCompilationDatabaseAbsolutePath(buildDir) {
		return "", false
	}

	relativeDirectory := strings.TrimRight(strings.ReplaceAll(entry.Directory, `\`, "/"), "/")
	for strings.HasPrefix(relativeDirectory, "./") {
		relativeDirectory = strings.TrimPrefix(relativeDirectory, "./")
	}
	if relativeDirectory == "" || relativeDirectory == "." {
		return strings.TrimSuffix(slashPath(buildDir), "/") + "/" + entry.File, true
	}

	normalizedBuildDir := strings.TrimSuffix(slashPath(buildDir), "/")
	compareBuildDir := normalizedBuildDir
	compareDirectory := relativeDirectory
	if isExplicitWindowsAbsolutePath(normalizedBuildDir) {
		compareBuildDir = strings.ToLower(compareBuildDir)
		compareDirectory = strings.ToLower(compareDirectory)
	}
	for prefix := compareDirectory; prefix != ""; {
		if compareBuildDir == prefix || strings.HasSuffix(compareBuildDir, "/"+prefix) {
			remainder := relativeDirectory[len(prefix):]
			return normalizedBuildDir + remainder + "/" + entry.File, true
		}
		separator := strings.LastIndex(prefix, "/")
		if separator < 0 {
			break
		}
		prefix = prefix[:separator]
	}
	return "", false
}

func loadCompilationDatabase(filename string) []json.RawMessage {
	info, err := os.Stat(filename)
	if err != nil || !info.Mode().IsRegular() {
		return nil
	}
	data, err := os.ReadFile(filename)
	if err != nil {
		return nil
	}

	var entries []json.RawMessage
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil
	}

	return entries
}

func mergeCompilationDatabase(entries []json.RawMessage, strict bool, buildDir string) []json.RawMessage {
	buildDir = compilationDatabaseBuildDir(buildDir)
	merged := make([]json.RawMessage, 0, len(entries))
	entryIndexes := make(map[string]int, len(entries))

	for _, data := range entries {
		entry, ok := decodeCompilationDatabaseEntry(data)
		if !ok {
			continue
		}

		keys := compilationDatabaseCompatibilityKeys(entry, buildDir)
		index := -1
		for _, key := range keys {
			if foundIndex, found := entryIndexes[key]; found {
				index = foundIndex
				break
			}
		}
		if index >= 0 {
			merged[index] = data
		} else {
			index = len(merged)
			merged = append(merged, data)
		}
		for _, key := range keys {
			entryIndexes[key] = index
		}
	}

	if !strict {
		return merged
	}

	filtered := make([]json.RawMessage, 0, len(merged))
	for _, data := range merged {
		entry, _ := decodeCompilationDatabaseEntry(data)
		sourcePath := compilationDatabaseKey(entry)
		if !isCompilationDatabaseAbsolutePath(sourcePath) {
			var ok bool
			sourcePath, ok = resolveLegacyCompilationDatabasePath(entry, buildDir)
			if !ok {
				continue
			}
		}
		if strictSourceFile(sourcePath) == nil {
			filtered = append(filtered, data)
		}
	}
	return filtered
}

func resolveOutputFilename(filename string) (string, error) {
	resolved, err := filepath.EvalSymlinks(filename)
	if err == nil {
		return resolved, nil
	}
	if !os.IsNotExist(err) {
		return "", err
	}

	info, lstatErr := os.Lstat(filename)
	if lstatErr == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return "", err
		}
		return filename, nil
	}
	if !os.IsNotExist(lstatErr) {
		return "", lstatErr
	}
	directory, base := filepath.Split(filename)
	if directory == "" {
		directory = "."
	}
	directory, err = filepath.EvalSymlinks(directory)
	if err != nil {
		return "", err
	}
	return filepath.Join(directory, base), nil
}

func createAtomicTempFile(directory string, mode os.FileMode) (*os.File, error) {
	prefix := filepath.Join(directory, ".compiledb-tmp-")
	for range 100 {
		outfile, err := os.OpenFile(prefix+rand.Text(), os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
		if os.IsExist(err) {
			continue
		}
		return outfile, err
	}
	return nil, &os.PathError{Op: "open", Path: prefix + "*", Err: os.ErrExist}
}

func writeFileInPlace(filename string, data []byte, mode os.FileMode) error {
	outfile, err := os.OpenFile(filename, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := outfile.Write(data); err != nil {
		_ = outfile.Close()
		return err
	}
	return outfile.Close()
}

func replaceFileAtomically(filename string, data []byte, mode os.FileMode, targetExists bool) error {
	outfile, err := createAtomicTempFile(filepath.Dir(filename), mode)
	if err != nil {
		return err
	}
	temporaryFilename := outfile.Name()
	defer os.Remove(temporaryFilename)

	if targetExists {
		if err := outfile.Chmod(mode); err != nil {
			_ = outfile.Close()
			return err
		}
	}
	if _, err := outfile.Write(data); err != nil {
		_ = outfile.Close()
		return err
	}
	if err := outfile.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryFilename, filename)
}

func writeFileAtomically(filename string, data []byte) error {
	filename, err := resolveOutputFilename(filename)
	if err != nil {
		return err
	}

	mode := os.FileMode(0o666)
	targetExists := false
	if info, statErr := os.Stat(filename); statErr == nil {
		if !info.Mode().IsRegular() {
			return fmt.Errorf("output %q is not a regular file", filename)
		}
		mode = info.Mode().Perm()
		targetExists = true
	} else if !os.IsNotExist(statErr) {
		return statErr
	}

	atomicErr := replaceFileAtomically(filename, data, mode, targetExists)
	if atomicErr == nil {
		return nil
	}
	if fallbackErr := writeFileInPlace(filename, data, mode); fallbackErr != nil {
		return fmt.Errorf("atomic replace failed (%v), then in-place write failed: %w", atomicErr, fallbackErr)
	}
	return nil
}

func (t *Tool) WriteJSON(filename string, _ int, data *[]Command) {
	payload := []Command{}
	if data != nil && *data != nil {
		payload = *data
	}

	entries := make([]json.RawMessage, 0, len(payload))
	for _, command := range payload {
		entry, err := json.Marshal(command)
		if err != nil {
			t.Logger.Fatalf("Error encoding JSON:%v", err)
		}
		entries = append(entries, entry)
	}

	if filename == "-" {
		entries = mergeCompilationDatabase(entries, !t.Config.NoStrict, t.compilationDatabaseBuildDir())
		jsonData, err := json.MarshalIndent(entries, "", "  ")
		if err != nil {
			t.Logger.Fatalf("Error encoding JSON:%v", err)
		}
		jsonData = append(jsonData, '\n')
		if err := writeFileWithContext(t.operationContext(), os.Stdout, jsonData); err != nil {
			if t.operationContext().Err() != nil {
				t.StatusCode = contextExitCode(t.operationContext())
				return
			}
			t.Logger.Fatalf("write stdout failed! err:%v", err)
		}
		if t.operationContext().Err() != nil {
			t.StatusCode = contextExitCode(t.operationContext())
		}
		return
	}

	if !t.Config.Overwrite {
		entries = append(loadCompilationDatabase(filename), entries...)
	}
	entries = mergeCompilationDatabase(entries, !t.Config.NoStrict, t.compilationDatabaseBuildDir())

	jsonData, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		t.Logger.Fatalf("Error encoding JSON:%v", err)
	}
	jsonData = append(jsonData, '\n')

	if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
		t.Logger.Fatalf("create directory %v failed! err:%v", filepath.Dir(filename), err)
	}

	if err := writeFileAtomically(filename, jsonData); err != nil {
		t.Logger.Fatalf("write %v failed! err:%v", filename, err)
	}
	t.Logger.Infof("write %d entries to %s", len(entries), filename)
}

func (t *Tool) Generate() {
	var (
		data []byte
		err  error
	)

	if !isStdinInput(t.Config.InputFile) {
		data, err = readPathWithContext(t.operationContext(), t.Config.InputFile)
		if err != nil {
			if t.operationContext().Err() != nil {
				t.StatusCode = contextExitCode(t.operationContext())
				return
			}
			t.Logger.Fatalf("open %v failed!", t.Config.InputFile)
		}
		t.Logger.Debugf("Build from file")
	} else {
		data, err = readFileWithContext(t.operationContext(), os.Stdin)
		if err != nil {
			if t.operationContext().Err() != nil {
				t.StatusCode = contextExitCode(t.operationContext())
				return
			}
			t.Logger.Fatalf("read stdin failed: %v", err)
		}
		t.Logger.Debugf("Build from stdin")
	}

	if t.operationContext().Err() != nil {
		t.StatusCode = contextExitCode(t.operationContext())
		return
	}
	buildLog := scanBuildLogWithLimit(data, t.physicalLineLimit())
	if t.operationContext().Err() != nil {
		t.StatusCode = contextExitCode(t.operationContext())
		return
	}
	t.parseBuildLog(buildLog)
}

func scanBuildLog(data []byte) []buildLogLine {
	return scanBuildLogWithLimit(data, maxBuildLogLineSize)
}

func scanBuildLogWithLimit(data []byte, limit int) []buildLogLine {
	lineCount := bytes.Count(data, []byte{'\n'})
	if len(data) > 0 && data[len(data)-1] != '\n' {
		lineCount++
	}
	lines := make([]buildLogLine, 0, lineCount)
	for len(data) > 0 {
		line := data
		if newline := bytes.IndexByte(data, '\n'); newline >= 0 {
			line = data[:newline]
			data = data[newline+1:]
		} else {
			data = nil
		}
		line = bytes.TrimSuffix(line, []byte{'\r'})
		if len(line) > limit {
			lines = append(lines, buildLogLine{raw: line, oversized: true, limit: limit})
			continue
		}
		lines = append(lines, buildLogLine{text: string(line)})
	}
	return lines
}
