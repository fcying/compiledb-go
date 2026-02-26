package internal

import (
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/mattn/go-shellwords"
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
	makeEnterDir = regexp.MustCompile("^.*-?make.*?: Entering directory .*['`\"](.*)['`\"]$")
	makeLeaveDir = regexp.MustCompile(`^.*-?make.*?: Leaving directory .*'(.*)'$`)

	// parse make -C xxx
	makeCmdDir = regexp.MustCompile(`^\s*make.*?-C\s+(.*?)(\s|$)`)

	// We want to skip such lines from configure to avoid spurious MAKE expansion errors.
	checkingMake = regexp.MustCompile(`^\s?checking whether .*(yes|no)$`)
)

func (t *Tool) splitArgs(input string) []string {
	p := shellwords.NewParser()
	args, err := p.Parse(input)
	if err != nil {
		t.Logger.Warnf("parse failed, input: %s", input)
		return nil
	}

	for i := range args {
		args[i] = strings.ReplaceAll(args[i], "\\", "/")
	}

	return args
}

func (t *Tool) splitCommands(commands string) []string {
	result := []string{}
	for _, v := range shRegex.Split(commands, -1) {
		command := strings.TrimSpace(v)
		if command != "" {
			result = append(result, command)
		}
	}
	return result
}

func (t *Tool) processCompileCommand(command string, workingDir string) ([]string, string) {
	arguments := []string{}
	filePath := ""
	arguments = t.splitArgs(command)

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

	if t.Config.FullPath {
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
		t.Logger.Debugf("found compile:%s, but not found file, ignore command", arguments[0])
		return nil, ""
	}

	if t.Config.Exclude != "" {
		if excludeRegex.MatchString(filePath) {
			t.Logger.Infof("file %s exclude", filePath)
			return nil, ""
		}
	}

	if t.Config.NoStrict == false {
		fileFullPath := filePath
		if IsAbsPath(filePath) == false {
			fileFullPath = path.Join(workingDir, filePath)
		}
		if FileExist(fileFullPath) == false {
			t.Logger.Warnf("file %s not exist", fileFullPath)
			return nil, ""
		}
	}

	if t.Config.Macros != "" {
		arguments = append(arguments, t.splitArgs(t.Config.Macros)...)
	}

	return arguments, filePath
}

func (t *Tool) Parse(buildLog []string) {
	var (
		err              error
		workingDir       = ""
		backupWorkingDir = ""
		cmdCnt           = 0
		result           []Command
		matchGroup       []string
		fullLineBuilder  strings.Builder // Buffer for merging multi-line commands
	)

	// check workingDir
	if t.Config.BuildDir != "" {
		workingDir = t.Config.BuildDir
	} else {
		if t.Config.InputFile != "stdin" {
			absPath, _ := filepath.Abs(t.Config.InputFile)
			workingDir = filepath.Dir(absPath)
		} else {
			workingDir, _ = os.Getwd()
		}
	}
	workingDir = ConvertPath(workingDir)
	t.Logger.Infof("workingDir: %s", workingDir)

	dirStack := []string{workingDir}

	// init regex
	if t.Config.Exclude != "" {
		excludeRegex, err = regexp.Compile(t.Config.Exclude)
		if err != nil {
			t.Logger.Fatalln("invalid exclude regex:", err)
			return
		}
	}
	compileRegex, err = regexp.Compile(t.Config.RegexCompile)
	if err != nil {
		t.Logger.Fatalln("invalid compile_regex:", err)
		return
	}
	fileRegex, err = regexp.Compile(t.Config.RegexFile)
	if err != nil {
		t.Logger.Fatalln("invalid file_regex:", err)
		return
	}

	for _, line := range buildLog {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		if before, ok :=strings.CutSuffix(line, "\\"); ok  {
			// If ending with '\', remove '\' and append a space, then continue to read the next line
			fullLineBuilder.WriteString(before)
			fullLineBuilder.WriteString(" ")
			continue
		} else {
			// Otherwise, it is the last line of the command (or a single-line command), append to buffer
			fullLineBuilder.WriteString(line)
		}
		// Get the complete merged line
		line = fullLineBuilder.String()
		// Reset buffer for the next command
		fullLineBuilder.Reset()
		t.Logger.Debug("New command:", line)

		// Restore workingDir {{{
		if backupWorkingDir != "" {
			workingDir = backupWorkingDir
			backupWorkingDir = ""
			t.Logger.Infof("Restore workingDir: %s", workingDir)
		}

		// Parse directory that make entering/leaving {{{
		if makeEnterDir.MatchString(line) {
			group := makeEnterDir.FindStringSubmatch(line)
			if group != nil && len(group) >= 2 {
				enterDir := group[1]
				dirStack = append([]string{ConvertPath(enterDir)}, dirStack...)
				workingDir = dirStack[0]
				t.Logger.Infof("entering change workingDir: %s", workingDir)
			}
			continue
		} else if makeLeaveDir.MatchString(line) {
			if len(dirStack) > 0 {
				dirStack = dirStack[1:]
				if len(dirStack) > 0 {
					workingDir = dirStack[0]
				}
				t.Logger.Infof("leaving change workingDir: %s", workingDir)
			}
			continue
		}

		if makeCmdDir.MatchString(line) {
			group := makeCmdDir.FindStringSubmatch(line)
			if group != nil && len(group) >= 2 {
				enterDir := group[1]
				dirStack = append([]string{ConvertPath(enterDir)}, dirStack...)
				if dirStack[0] != "." {
					workingDir = dirStack[0]
				}
				t.Logger.Infof("make cmd change workingDir: %s", workingDir)
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
						t.Logger.Error("Error executing nested command:", err)
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

			for _, v := range t.splitCommands(line) {
				// t.Logger.Error(v)

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
					t.Logger.Infof("Temporarily change workingDir: %s", workingDir)
					continue
				}

				// Parse compile command {{{
				if compileRegex.MatchString(v) {
					arguments, filePath := t.processCompileCommand(v, workingDir)
					if filePath == "" {
						continue
					}

					// append to result
					command := strings.Join(arguments, " ")
					if t.Config.CommandStyle {
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
					t.Logger.Infof("Adding command %d: %s", cmdCnt, command)
					cmdCnt += 1
				}
			}
		}
	}

	t.WriteJSON(t.Config.OutputFile, cmdCnt, &result)
}
