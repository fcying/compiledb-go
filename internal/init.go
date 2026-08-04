package internal

import (
	"bufio"
	"encoding/json"
	"os"

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
	if IsAbsPath(entry.File) {
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

func mergeCompilationDatabase(entries []json.RawMessage, strict bool) []json.RawMessage {
	merged := make([]json.RawMessage, 0, len(entries))
	entryIndexes := make(map[string]int, len(entries))

	for _, data := range entries {
		entry, ok := decodeCompilationDatabaseEntry(data)
		if !ok {
			continue
		}

		key := compilationDatabaseKey(entry)
		if index, found := entryIndexes[key]; found {
			merged[index] = data
			continue
		}

		entryIndexes[key] = len(merged)
		merged = append(merged, data)
	}

	if !strict {
		return merged
	}

	filtered := make([]json.RawMessage, 0, len(merged))
	for _, data := range merged {
		entry, _ := decodeCompilationDatabaseEntry(data)
		if _, err := os.Stat(compilationDatabaseKey(entry)); err == nil {
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
		entries = mergeCompilationDatabase(entries, !t.Config.NoStrict)
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
	entries = mergeCompilationDatabase(entries, !t.Config.NoStrict)

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
