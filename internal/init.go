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

func (t *Tool) WriteJSON(filename string, cmdCnt int, data *[]Command) {
	payload := []Command{}
	if data != nil && *data != nil {
		payload = *data
	}

	jsonData, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		t.Logger.Fatalf("Error encoding JSON:%v", err)
	}

	if filename == "-" {
		if _, err := os.Stdout.Write(jsonData); err != nil {
			t.Logger.Fatalf("write stdout failed! err:%v", err)
		}
		if _, err := os.Stdout.Write([]byte("\n")); err != nil {
			t.Logger.Fatalf("write stdout newline failed! err:%v", err)
		}
		return
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
	t.Logger.Infof("write %d entries to %s", cmdCnt, filename)
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
