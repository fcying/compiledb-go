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
	Macros       string
	RegexCompile string
	RegexFile    string
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
	if cmdCnt == 0 {
		return
	}

	// format
	jsonData, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		t.Logger.Fatalf("Error encoding JSON:%v", err)
	}

	// write file
	if filename == "-" {
		println(string(jsonData))
	} else {
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
}

func (t *Tool) Generate() {
	var (
		buildLog []string
		scnner   *bufio.Scanner
		file     *os.File
		err      error
	)
	defer file.Close()

	if t.Config.InputFile != "stdin" {
		file, err = os.OpenFile(t.Config.InputFile, os.O_RDONLY, 0444)
		if err != nil {
			t.Logger.Fatalf("open %v failed!", t.Config.InputFile)
		}
		scnner = bufio.NewScanner(file)
		t.Logger.Debugf("Build from file")
	} else {
		scnner = bufio.NewScanner(os.Stdin)
		t.Logger.Debugf("Build from stdin")
	}

	scnner.Buffer(make([]byte, 1024*1024), 1024*1024*100)
	for scnner.Scan() {
		buildLog = append(buildLog, scnner.Text())
	}
	t.Parse(buildLog)
}
