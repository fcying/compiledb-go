package internal

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"

	log "github.com/sirupsen/logrus"
)

// Internal variables used to parse build log entries
var cc_compile_regex = regexp.MustCompile(`^.*-?(gcc|clang)-?.*(\.exe)?$`)
var cpp_compile_regex = regexp.MustCompile(`^.*-?([gc]|clang)\+\+-?.*(\.exe)?$`)

var file_regex = regexp.MustCompile(`^.*-c\s(.*\.(c|cpp|cc|cxx|c\+\+|s|m|mm|cu))(\s.*$|$)`)
var compiler_wrappers []string = []string{"ccache", "icecc", "sccache"}

// Leverage `make --print-directory` option
var make_enter_dir = regexp.MustCompile(`^\s?make.*?: Entering directory .*'(.*)'$`)
var make_leave_dir = regexp.MustCompile(`^\s?make.*?: Leaving directory .*'(.*)'$`)

// We want to skip such lines from configure to avoid spurious MAKE expansion errors.
var checking_make = regexp.MustCompile(`^\s?checking whether .*(yes|no)$`)

func commandProcess(line string, workingDir string) ([]string, string) {
	arguments := []string{}
	filepath := ""
	if cc_compile_regex.MatchString(line) ||
		cpp_compile_regex.MatchString(line) {
		arguments = strings.Fields(line)
		group := file_regex.FindStringSubmatch(line)
		if group != nil {
			filepath = group[1]
		}
	}
	return arguments, filepath
}

func Parse(buildLog []string) {
	var (
		err           error
		workingDir    string
		exclude_regex *regexp.Regexp
	)

	// check workingDir
	if ParseConfig.BuildDir != "" {
		workingDir = ParseConfig.BuildDir
	} else {
		workingDir, err = os.Getwd()
		if err != nil {
			log.Fatalf("get workingDir failed! %v", err)
		}
		if ParseConfig.InputFile != "stdin" {
			absPath, _ := filepath.Abs(ParseConfig.InputFile)
			workingDir = filepath.Dir(absPath)
		}
	}
	log.Printf("workingDir: %s", workingDir)

	dirStack := []string{workingDir}

	//init exclude
	if ParseConfig.Exclude != "" {
		exclude_regex, err = regexp.Compile(ParseConfig.Exclude)
		if err != nil {
			log.Fatalln("invalid exclude regex:", err)
		}
	}

	for _, line := range buildLog {
		if line == "" {
			continue
		}
		line = strings.TrimSpace(line)
		log.Println("New command:", line)

		// Parse directory that make entering/leaving
		if make_enter_dir.MatchString(line) {
			group := make_enter_dir.FindStringSubmatch(line)
			if group != nil {
				dirStack = append([]string{group[1]}, dirStack...)
				workingDir = dirStack[0]
				log.Printf("change workingDir: %s", workingDir)
			}
			continue
		} else if make_leave_dir.MatchString(line) {
			if len(dirStack) > 0 {
				dirStack = dirStack[1:]
				if len(dirStack) > 0 {
					workingDir = dirStack[0]
				}
				log.Printf("change workingDir: %s", workingDir)
			}
			continue
		}

		if checking_make.MatchString(line) {
			continue
		}

		// Parse command
		arguments, filepath := commandProcess(line, workingDir)
		command := ""
		compileFullPath := ""
		if filepath != "" {
			if ParseConfig.NoStrict == false {
				if FileExist(filepath) == false {
					log.Printf("file %s not exist", filepath)
					continue
				}
			}

			if ParseConfig.Exclude != "" {
				if exclude_regex.MatchString(filepath) {
					log.Printf("file %s exclude", filepath)
					continue
				}
			}

			if ParseConfig.FullPath {
				compileFullPath = GetBinFullPath(arguments[0])
				if compileFullPath != "" {
					arguments[0] = compileFullPath
				}
			}

			if ParseConfig.Macros != "" {
				arguments = append(arguments, strings.Fields(ParseConfig.Macros)...)
			}

			if ParseConfig.CommandStyle {
				command = strings.Join(arguments, " ")
				data := struct {
					Directory string `json:"directory"`
					Command   string `json:"command"`
					File      string `json:"file"`
				}{
					Directory: workingDir,
					Command:   command,
					File:      filepath,
				}
				ParseResult = append(ParseResult, data)
			} else {
				data := struct {
					Directory string   `json:"directory"`
					Arguments []string `json:"arguments"`
					File      string   `json:"file"`
				}{
					Directory: workingDir,
					Arguments: arguments,
					File:      filepath,
				}
				ParseResult = append(ParseResult, data)
			}
			log.Printf("Adding command %d: %s", CommandCnt, line)
			CommandCnt += 1
		}
	}

	WriteJSON(ParseConfig.OutputFile)
}
