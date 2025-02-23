package internal

import (
	"os"
	"os/exec"
	"path"
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

var (
	compileRegex *regexp.Regexp
	RegexCompile string = `^.*-?(gcc|clang|cc|g\+\+|c\+\+|clang\+\+)-?.*(\.exe)?`
	fileRegex    *regexp.Regexp
	RegexFile    string = `^.*\s+-c.*\s(?:"|')?(.*\.(?:c|cpp|cc|cxx|c\+\+|s|m|mm|cu))(?:"|')?(\s|$)`
	excludeRegex *regexp.Regexp

	// Internal regex used to parse build log entries
	cdRegex        = regexp.MustCompile(`^cd\s+(.*)`)
	shRegex        = regexp.MustCompile(`\s*(;|&&|\|\|)\s*`)
	nestedCmdRegex = regexp.MustCompile("`([^`]+)`")

	// Leverage `make --print-directory` option
	makeEnterDir = regexp.MustCompile("^\\s?make.*?: Entering directory .*['`\"](.*)['`\"]$")
	makeLeaveDir = regexp.MustCompile(`^\s?make.*?: Leaving directory .*'(.*)'$`)

	// parse make -C xxx
	makeCmdDir = regexp.MustCompile(`^\s*make.*?-C\s+(.*?)(\s|$)`)

	// We want to skip such lines from configure to avoid spurious MAKE expansion errors.
	checkingMake = regexp.MustCompile(`^\s?checking whether .*(yes|no)$`)
)

func splitCommands(commands string) []string {
	result := []string{}
	for _, v := range shRegex.Split(commands, -1) {
		command := strings.TrimSpace(v)
		if command != "" {
			result = append(result, command)
		}
	}
	return result
}

func processCompileCommand(command string, workingDir string) ([]string, string) {
	arguments := []string{}
	filePath := ""
	arguments = strings.Fields(command)

	// check compile word
	findCompile := false
	for i, word := range arguments {
		if compileRegex.MatchString(word) {
			findCompile = true
			arguments = arguments[i:]
			break
		}
	}
	if findCompile == false {
		return nil, ""
	}

	if ParseConfig.FullPath {
		compileFullPath := ""
		compileFullPath = GetBinFullPath(arguments[0])
		if compileFullPath != "" {
			compileFullPath = ConvertPath(compileFullPath)
			arguments[0] = compileFullPath
		}
	}

	group := fileRegex.FindStringSubmatch(command)
	if group != nil {
		filePath = group[1]
	} else {
		log.Debugf("found compile:%s, but not found file, ignore command", arguments[0])
		return nil, ""
	}

	if ParseConfig.Exclude != "" {
		if excludeRegex.MatchString(filePath) {
			log.Infof("file %s exclude", filePath)
			return nil, ""
		}
	}

	if ParseConfig.NoStrict == false {
		fileFullPath := filePath
		if IsAbsPath(filePath) == false {
			fileFullPath = path.Join(workingDir, filePath)
		}
		if FileExist(fileFullPath) == false {
			log.Warnf("file %s not exist", fileFullPath)
			return nil, ""
		}
	}

	if ParseConfig.Macros != "" {
		arguments = append(arguments, strings.Fields(ParseConfig.Macros)...)
	}

	return arguments, filePath
}

func Parse(buildLog []string) {
	var (
		err              error
		workingDir       = ""
		backupWorkingDir = ""
		cmdCnt           = 0
		result           []Command
		matchGroup       []string
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

	// init regex
	if ParseConfig.Exclude != "" {
		excludeRegex, err = regexp.Compile(ParseConfig.Exclude)
		if err != nil {
			log.Fatalln("invalid exclude regex:", err)
			return
		}
	}
	compileRegex, err = regexp.Compile(ParseConfig.RegexCompile)
	if err != nil {
		log.Fatalln("invalid compile_regex:", err)
		return
	}
	fileRegex, err = regexp.Compile(ParseConfig.RegexFile)
	if err != nil {
		log.Fatalln("invalid file_regex:", err)
		return
	}

	for _, line := range buildLog {
		// Restore workingDir {{{
		if backupWorkingDir != "" {
			workingDir = backupWorkingDir
			backupWorkingDir = ""
			log.Infof("Restore workingDir: %s", workingDir)
		}

		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		log.Debug("New command:", line)

		// Parse directory that make entering/leaving {{{
		if makeEnterDir.MatchString(line) {
			group := makeEnterDir.FindStringSubmatch(line)
			if group != nil && len(group) >= 2 {
				enterDir := group[1]
				dirStack = append([]string{ConvertPath(enterDir)}, dirStack...)
				workingDir = dirStack[0]
				log.Infof("entering change workingDir: %s", workingDir)
			}
			continue
		} else if makeLeaveDir.MatchString(line) {
			if len(dirStack) > 0 {
				dirStack = dirStack[1:]
				if len(dirStack) > 0 {
					workingDir = dirStack[0]
				}
				log.Infof("leaving change workingDir: %s", workingDir)
			}
			continue
		}

		if makeCmdDir.MatchString(line) {
			group := makeCmdDir.FindStringSubmatch(line)
			if group != nil && len(group) >= 2 {
				enterDir := group[1]
				dirStack = append([]string{ConvertPath(enterDir)}, dirStack...)
				workingDir = dirStack[0]
				log.Infof("make cmd change workingDir: %s", workingDir)
			}
		}

		if checkingMake.MatchString(line) {
			continue
		}

		if compileRegex.MatchString(line) {
			// process nestedCmd
			for {
				matchGroup = nestedCmdRegex.FindStringSubmatch(line)
				if matchGroup != nil {
					nestedCmd := matchGroup[1]
					out, err := exec.Command("sh", "-c", nestedCmd).Output()
					if err != nil {
						log.Error("Error executing nested command:", err)
						out = nil
					}
					// update line
					line = strings.Replace(line, matchGroup[0], strings.TrimSpace(string(out)), 1)
				} else {
					break
				}
			}

			// not escape \", json.MarshalIndent will do it
			line = strings.ReplaceAll(line, `\"`, `"`)

			for _, v := range splitCommands(line) {
				// log.Error(v)

				// Parse cd xx {{{
				matchGroup = cdRegex.FindStringSubmatch(v)
				if matchGroup != nil {
					backupWorkingDir = workingDir
					cdPath := matchGroup[1]
					if IsAbsPath(cdPath) == false {
						workingDir = path.Join(workingDir, cdPath)
					} else {
						workingDir = cdPath
					}
					log.Infof("Temporarily change workingDir: %s", workingDir)
					continue
				}

				// Parse compile command {{{
				if compileRegex.MatchString(v) {
					arguments, filePath := processCompileCommand(v, workingDir)
					if filePath == "" {
						continue
					}

					// append to result
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
		}
	}

	WriteJSON(ParseConfig.OutputFile, cmdCnt, &result)
}
