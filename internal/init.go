package internal

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/sirupsen/logrus"
)

type Config struct {
	InputFile    string
	OutputFile   string
	BuildDir     string
	Exclude      string
	Macros       []string
	RegexCompile string
	RegexFile    string
	Encoding     string
	CommandStyle bool
	FullPath     bool
	NoBuild      bool
	NoStrict     bool
	Overwrite    bool
}

type Tool struct {
	Config     Config
	Logger     *logrus.Logger
	StatusCode int
}

func NewTool(cfg Config, logger *logrus.Logger) *Tool {
	return &Tool{
		Config: cfg,
		Logger: logger,
	}
}

func (t *Tool) compilationDatabaseBuildDir() string {
	if t.Config.BuildDir != "" {
		return compilationDatabaseBuildDir(t.Config.BuildDir)
	}
	if t.Config.InputFile != "" && t.Config.InputFile != "stdin" {
		if absolute, err := filepath.Abs(t.Config.InputFile); err == nil {
			return ConvertPath(filepath.Dir(absolute))
		}
	}
	return compilationDatabaseBuildDir("")
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
	return strings.HasPrefix(value, "/") || isExplicitWindowsPath(value)
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
	if !isExplicitWindowsPath(entry.Directory) && !isExplicitWindowsPath(entry.File) {
		return "", false
	}

	file := strings.ReplaceAll(entry.File, `\`, "/")
	if isExplicitWindowsPath(entry.File) {
		return strings.ToLower(file), true
	}
	if strings.HasPrefix(file, "/") {
		return "", false
	}
	directory := strings.TrimRight(strings.ReplaceAll(entry.Directory, `\`, "/"), "/")
	return strings.ToLower(directory + "/" + file), true
}

func windowsPathKey(value string) (string, bool) {
	if !isExplicitWindowsPath(value) {
		return "", false
	}
	return strings.ToLower(strings.ReplaceAll(value, `\`, "/")), true
}

func isExplicitWindowsPath(value string) bool {
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
			return ConvertPath(absolute)
		}
	}
	if cwd, err := os.Getwd(); err == nil {
		return ConvertPath(cwd)
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
		return strings.TrimSuffix(ConvertPath(buildDir), "/") + "/" + entry.File, true
	}

	normalizedBuildDir := strings.TrimSuffix(ConvertPath(buildDir), "/")
	compareBuildDir := normalizedBuildDir
	compareDirectory := relativeDirectory
	if isExplicitWindowsPath(normalizedBuildDir) {
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
		if _, err := os.Stat(sourcePath); err == nil {
			filtered = append(filtered, data)
		}
	}
	return filtered
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
		if _, err := os.Stdout.Write(jsonData); err != nil {
			t.Logger.Fatalf("write stdout failed! err:%v", err)
		}
		if _, err := os.Stdout.Write([]byte("\n")); err != nil {
			t.Logger.Fatalf("write stdout newline failed! err:%v", err)
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

	outfile, err := os.Create(filename)
	if err != nil {
		t.Logger.Fatalf("create %v failed! err:%v", filename, err)
	}
	defer outfile.Close()

	_, err = outfile.Write(jsonData)
	if err != nil {
		t.Logger.Fatalf("write %v failed! err:%v", filename, err)
	}
	t.Logger.Infof("write %d entries to %s", len(entries), filename)
}

func (t *Tool) Generate() {
	var (
		buildLog []string
		scanner  *bufio.Scanner
		file     *os.File
		err      error
	)

	if t.Config.InputFile != "stdin" {
		file, err = os.OpenFile(t.Config.InputFile, os.O_RDONLY, 0o444)
		if err != nil {
			t.Logger.Fatalf("open %v failed!", t.Config.InputFile)
		}
		defer file.Close()

		scanner = bufio.NewScanner(file)
		t.Logger.Debugf("Build from file")
	} else {
		scanner = bufio.NewScanner(os.Stdin)
		t.Logger.Debugf("Build from stdin")
	}

	scanner.Buffer(make([]byte, 1024*1024), 1024*1024*100)
	for scanner.Scan() {
		buildLog = append(buildLog, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		t.Logger.Fatalf("read build log failed: %v", err)
	}

	t.Parse(buildLog)
}
