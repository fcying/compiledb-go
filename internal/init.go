package internal

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"sync"

	log "github.com/sirupsen/logrus"
)

type Config struct {
	InputFile    string
	OutputFile   string
	BuildDir     string
	Exclude      string
	Macros       string
	CommandStyle bool
	FullPath     bool
	NoBuild      bool
	NoStrict     bool
}

var (
	CommandCnt  = 0
	ParseConfig Config
	ParseResult []interface{}
)

func WriteJSON(filename string) {
	if CommandCnt == 0 {
		return
	}

	// format
	jsonData, err := json.MarshalIndent(ParseResult, "", "  ")
	if err != nil {
		log.Fatalf("Error encoding JSON:%v", err)
	}

	// write file
	if filename == "-" {
		println(string(jsonData))
	} else {
		outfile, err := os.Create(filename)
		if err != nil {
			log.Fatalf("create %v failed! err:%v", filename, err)
		}
		defer outfile.Close()

		_, err = outfile.Write(jsonData)
		if err != nil {
			log.Fatalf("write %v failed! err:%v", filename, err)
		}
		log.Printf("write %d entries to %s", CommandCnt, filename)
	}
}

func MakeWrap(args []string) {
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		// append log
		args = append([]string{"-Bnkw"}, args...)
		cmd := exec.Command("make", args...)

		var stdoutBuf bytes.Buffer
		cmd.Stdout = &stdoutBuf
		cmd.Stderr = &stdoutBuf
		cmd.Run()

		level := log.GetLevel()

		// only print make log
		if ParseConfig.NoBuild == false {
			log.SetLevel(log.PanicLevel)
		}

		buildLog := strings.Split(stdoutBuf.String(), "\n")
		Parse(buildLog)

		// restore log level
		if ParseConfig.NoBuild == false {
			log.SetLevel(level)
		}

		wg.Done()
	}()

	if ParseConfig.NoBuild == false {
		cmd := exec.Command("make", args...)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		cmd.Run()
	}

	wg.Wait()
}

func Generate() {
	var (
		buildLog []string
		scnner   *bufio.Scanner
		file     *os.File
		err      error
	)
	defer file.Close()

	if ParseConfig.InputFile != "stdin" {
		file, err = os.OpenFile(ParseConfig.InputFile, os.O_RDONLY, 0444)
		if err != nil {
			log.Fatalf("open %v failed!", ParseConfig.InputFile)
		}
		scnner = bufio.NewScanner(file)
		log.Println("Build from file")
	} else {
		scnner = bufio.NewScanner(os.Stdin)
		log.Println("Build from stdin")
	}

	scnner.Buffer(make([]byte, 1024*1024), 1024*1024*100)
	for scnner.Scan() {
		buildLog = append(buildLog, scnner.Text())
	}
	Parse(buildLog)
}
