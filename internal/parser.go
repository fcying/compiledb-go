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
	RegexCompile string = `^.*-?(gcc|clang|cc|g\+\+|c\+\+|clang\+\+)-?.*(\.exe)?`
	RegexFile    string = `^.*\s+-c.*\s(?:(?:"|')(.*?\.(?:c|cpp|cc|cxx|c\+\+|s|m|mm|cu))(?:"|')|([^\s"']+\.(?:c|cpp|cc|cxx|c\+\+|s|m|mm|cu)))(\s|$)`

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

type parserPatterns struct {
	compile *regexp.Regexp
	file    *regexp.Regexp
	exclude *regexp.Regexp
}

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

func mergeLogicalLines(lines []string) []string {
	merged := make([]string, 0, len(lines))
	var builder strings.Builder

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		if before, ok := strings.CutSuffix(line, "\\"); ok {
			builder.WriteString(before)
			builder.WriteString(" ")
			continue
		}

		builder.WriteString(line)
		merged = append(merged, builder.String())
		builder.Reset()
	}

	if builder.Len() > 0 {
		merged = append(merged, strings.TrimSpace(builder.String()))
	}

	return merged
}

func compilePatterns(cfg Config) (parserPatterns, error) {
	patterns := parserPatterns{}

	if cfg.Exclude != "" {
		excludeRegex, err := regexp.Compile(cfg.Exclude)
		if err != nil {
			return patterns, err
		}
		patterns.exclude = excludeRegex
	}

	compileRegex, err := regexp.Compile(cfg.RegexCompile)
	if err != nil {
		return patterns, err
	}
	patterns.compile = compileRegex

	fileRegex, err := regexp.Compile(cfg.RegexFile)
	if err != nil {
		return patterns, err
	}
	patterns.file = fileRegex

	return patterns, nil
}

func (t *Tool) processCompileCommand(command string, workingDir string, patterns parserPatterns) ([]string, string) {
	arguments := t.splitArgs(command)
	if len(arguments) == 0 {
		return nil, ""
	}

	findCompile := false
	for i, word := range arguments {
		if patterns.compile.MatchString(word) {
			findCompile = true
			arguments = arguments[i:]
			break
		}
	}
	if !findCompile || len(arguments) == 0 {
		return nil, ""
	}

	if t.Config.FullPath {
		compileFullPath := GetBinFullPath(arguments[0])
		if compileFullPath != "" {
			arguments[0] = ConvertPath(compileFullPath)
		}
	}

	group := patterns.file.FindStringSubmatch(command)
	filePath := ""
	if len(group) > 1 {
		for _, candidate := range group[1:] {
			if candidate != "" {
				filePath = candidate
				break
			}
		}
	}
	if filePath == "" {
		t.Logger.Debugf("found compile:%s, but not found file, ignore command", arguments[0])
		return nil, ""
	}

	if patterns.exclude != nil && patterns.exclude.MatchString(filePath) {
		t.Logger.Infof("file %s exclude", filePath)
		return nil, ""
	}

	if !t.Config.NoStrict {
		fileFullPath := filePath
		if !IsAbsPath(filePath) {
			fileFullPath = path.Join(workingDir, filePath)
		}
		if !FileExist(fileFullPath) {
			t.Logger.Warnf("file %s not exist", fileFullPath)
			return nil, ""
		}
	}

	if len(t.Config.Macros) > 0 {
		for _, macro := range t.Config.Macros {
			arguments = append(arguments, t.splitArgs(macro)...)
		}
	}

	return arguments, filePath
}

func (t *Tool) Parse(buildLog []string) {
	var (
		workingDir       string
		backupWorkingDir string
		cmdCnt           int
		result           []Command
		matchGroup       []string
	)

	// Resolve initial working directory {{{
	if t.Config.BuildDir != "" {
		workingDir = t.Config.BuildDir
	} else if t.Config.InputFile != "stdin" {
		absPath, _ := filepath.Abs(t.Config.InputFile)
		workingDir = filepath.Dir(absPath)
	} else {
		workingDir, _ = os.Getwd()
	}
	workingDir = ConvertPath(workingDir)
	t.Logger.Infof("workingDir: %s", workingDir)

	// Compile parser regexes {{{
	patterns, err := compilePatterns(t.Config)
	if err != nil {
		t.Logger.Fatalln("invalid parser regex:", err)
		return
	}

	dirStack := []string{workingDir}

	for _, line := range mergeLogicalLines(buildLog) {
		t.Logger.Debug("New command:", line)

		// Restore temporary working directory {{{
		if backupWorkingDir != "" {
			workingDir = backupWorkingDir
			backupWorkingDir = ""
			t.Logger.Infof("Restore workingDir: %s", workingDir)
		}

		// Track make-reported directory changes {{{
		if makeEnterDir.MatchString(line) {
			group := makeEnterDir.FindStringSubmatch(line)
			if len(group) >= 2 {
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
			if len(group) >= 2 {
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

		if patterns.compile.MatchString(line) {
			// Expand nested shell commands {{{
			for {
				matchGroup = nestedCmdRegex.FindStringSubmatch(line)
				if matchGroup == nil {
					break
				}

				nestedCmd := matchGroup[1]
				out, err := exec.Command("sh", "-c", nestedCmd).Output()
				if err != nil {
					t.Logger.Error("Error executing nested command:", err)
					out = nil
				}
				line = strings.Replace(line, matchGroup[0], strings.TrimSpace(string(out)), 1)
			}

			// Normalize escaped double quotes here; json.MarshalIndent will re-escape them when writing JSON.
			line = strings.ReplaceAll(line, `\"`, `"`)

			for _, v := range t.splitCommands(line) {
				// Apply inline cd command {{{
				matchGroup = cdRegex.FindStringSubmatch(v)
				if len(matchGroup) >= 2 {
					backupWorkingDir = workingDir
					cdPath := matchGroup[1]
					if !IsAbsPath(cdPath) {
						workingDir = path.Join(workingDir, cdPath)
					} else {
						workingDir = cdPath
					}
					t.Logger.Infof("Temporarily change workingDir: %s", workingDir)
					continue
				}

				// Extract compile command entry {{{
				if patterns.compile.MatchString(v) {
					arguments, filePath := t.processCompileCommand(v, workingDir, patterns)
					if filePath == "" {
						continue
					}

					command := ShellJoinArgs(arguments)
					if t.Config.CommandStyle {
						result = append(result, Command{Directory: workingDir, Command: command, File: filePath})
					} else {
						result = append(result, Command{Directory: workingDir, Arguments: arguments, File: filePath})
					}
					t.Logger.Infof("Adding command %d: %s", cmdCnt, command)
					cmdCnt++
				}
			}
		}
	}

	t.WriteJSON(t.Config.OutputFile, cmdCnt, &result)
}
