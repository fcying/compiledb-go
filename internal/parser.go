package internal

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"

	log "github.com/sirupsen/logrus"
)

type Command struct {
	Directory string   `json:"directory"`
	Command   string   `json:"command,omitempty"`
	Arguments []string `json:"arguments,omitempty"`
	File      string   `json:"file"`
}

var RegexCompile string = `^.*-?(gcc|clang|cc|g\+\+|c\+\+|clang\+\+)-?.*(\.exe)?`
var RegexFile string = `^.*\s-c.*\s(.*\.(c|cpp|cc|cxx|c\+\+|s|m|mm|cu))(\s.*$|$)`

// Internal variables used to parse build log entries
var sh_regex = regexp.MustCompile(`^.*(;|&&|&|\|)`)
var compile_regex *regexp.Regexp
var file_regex *regexp.Regexp

// Leverage `make --print-directory` option
var make_enter_dir = regexp.MustCompile("^\\s?make.*?: Entering directory .*['`\"](.*)['`\"]$")
var make_leave_dir = regexp.MustCompile(`^\s?make.*?: Leaving directory .*'(.*)'$`)

// We want to skip such lines from configure to avoid spurious MAKE expansion errors.
var checking_make = regexp.MustCompile(`^\s?checking whether .*(yes|no)$`)

func commandProcess(line string, workingDir string) ([]string, string) {
	arguments := []string{}
	filepath := ""
	if compile_regex.MatchString(line) {
		// not escape \", json.MarshalIndent will do it
		line = strings.ReplaceAll(line, `\"`, `"`)

		arguments = strings.Fields(line)

		// check compile word
		findCompile := false
		for i, word := range arguments {
			if compile_regex.MatchString(word) {
				findCompile = true
				arguments = arguments[i:]
				index := sh_regex.FindStringIndex(word)
				if index != nil {
					arguments[0] = word[index[1]:]
				}
				break
			}
		}
		if findCompile == false {
			return nil, ""
		}

		group := file_regex.FindStringSubmatch(line)
		if group != nil {
			if len(group) > 1 {
				filepath = group[1]
			} else {
				log.Fatalln("invalid file_regex")
			}
		}
	}
	return arguments, filepath
}

func Parse(buildLog []string) {
	var (
		err           error
		workingDir    string
		exclude_regex *regexp.Regexp
		cmdCnt        = 0
		result        []Command
	)

	// check workingDir
	if ParseConfig.BuildDir != "" {
		workingDir = ParseConfig.BuildDir
	} else {
		if ParseConfig.InputFile != "stdin" {
			absPath, _ := filepath.Abs(ParseConfig.InputFile)
			workingDir = filepath.Dir(absPath)
		} else {
			workingDir, _ = os.Getwd()
		}
	}
	workingDir = ConvertPath(workingDir)
	log.Infof("workingDir: %s", workingDir)

	dirStack := []string{workingDir}

	//init regex
	if ParseConfig.Exclude != "" {
		exclude_regex, err = regexp.Compile(ParseConfig.Exclude)
		if err != nil {
			log.Fatalln("invalid exclude regex:", err)
		}
	}
	compile_regex, err = regexp.Compile(ParseConfig.RegexCompile)
	if err != nil {
		log.Fatalln("invalid compile_regex:", err)
	}
	file_regex, err = regexp.Compile(ParseConfig.RegexFile)
	if err != nil {
		log.Fatalln("invalid file_regex:", err)
	}

	for _, line := range buildLog {
		if line == "" {
			continue
		}
		line = strings.TrimSpace(line)
		log.Debug("New command:", line)

		// Parse directory that make entering/leaving
		if make_enter_dir.MatchString(line) {
			group := make_enter_dir.FindStringSubmatch(line)
			if group != nil && len(group) >= 2 {
				enterDir := group[1]
				dirStack = append([]string{ConvertPath(enterDir)}, dirStack...)
				workingDir = dirStack[0]
				log.Infof("change workingDir: %s", workingDir)
			}
			continue
		} else if make_leave_dir.MatchString(line) {
			if len(dirStack) > 0 {
				dirStack = dirStack[1:]
				if len(dirStack) > 0 {
					workingDir = dirStack[0]
				}
				log.Infof("change workingDir: %s", workingDir)
			}
			continue
		}

		if checking_make.MatchString(line) {
			continue
		}

		// Parse command
		arguments, filePath := commandProcess(line, workingDir)
		compileFullPath := ""
		if filePath != "" {
			if ParseConfig.NoStrict == false {
				fileFullPath := filePath
				if IsAbsPath(filePath) == false {
					fileFullPath = workingDir + "/" + filePath
				}
				if FileExist(fileFullPath) == false {
					log.Warnf("file %s not exist", fileFullPath)
					continue
				}
			}

			if ParseConfig.Exclude != "" {
				if exclude_regex.MatchString(filePath) {
					log.Infof("file %s exclude", filePath)
					continue
				}
			}

			if ParseConfig.FullPath {
				compileFullPath = GetBinFullPath(arguments[0])
				if compileFullPath != "" {
					compileFullPath = ConvertPath(compileFullPath)
					arguments[0] = compileFullPath
				}
			}

			if ParseConfig.Macros != "" {
				arguments = append(arguments, strings.Fields(ParseConfig.Macros)...)
			}

			command := strings.Join(arguments, " ")
			if ParseConfig.CommandStyle {
				result = append(result, Command{
					Directory: workingDir,
					Command:   command,
					File:      filePath,
				})
			} else {
				result = append(result, Command{
					Directory: workingDir,
					Arguments: arguments,
					File:      filePath,
				})
			}
			log.Infof("Adding command %d: %s", cmdCnt, command)
			cmdCnt += 1
		}
	}

	WriteJSON(ParseConfig.OutputFile, cmdCnt, &result)
}
