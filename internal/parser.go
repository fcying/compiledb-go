package internal

import (
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

type Command struct {
	Directory string   `json:"directory"`
	Command   string   `json:"command,omitempty"`
	Arguments []string `json:"arguments,omitempty"`
	File      string   `json:"file"`
}

var (
	RegexCompile string = `(?i)^.*-?(gcc|clang|cc|g\+\+|c\+\+|clang\+\+)-?.*(\.exe)?`
	RegexFile    string = `^.*\s+-c.*\s(?:(?:"|')(.*?\.(?i:c|cpp|cc|cxx|c\+\+|s|m|mm|cu))(?:"|')|([^\s"']+\.(?i:c|cpp|cc|cxx|c\+\+|s|m|mm|cu)))(\s|$)`

	// We want to skip such lines from configure to avoid spurious MAKE expansion errors.
	checkingMake = regexp.MustCompile(`^\s?checking whether .*(yes|no)$`)
)

type parserPatterns struct {
	compile        *regexp.Regexp
	file           *regexp.Regexp
	exclude        *regexp.Regexp
	defaultCompile bool
	defaultFile    bool
}

type shellCommand struct {
	text      string
	separator string
}

type parsedCompileCommand struct {
	arguments []string
	filePath  string
	directory string
}

type defaultCompilerLocation struct {
	index        int
	launcher     bool
	fullPathSafe bool
	workingDir   string
	valid        bool
}

type shellCommandStatus uint8

const (
	shellStatusUnknown shellCommandStatus = iota
	shellStatusSuccess
	shellStatusFailure
)

func (t *Tool) splitArgs(input string) []string {
	args, ok := splitShellArguments(input)
	if !ok {
		t.Logger.Warnf("parse failed, input: %s", input)
		return nil
	}

	return args
}

func splitShellArguments(line string) ([]string, bool) {
	arguments := []string{}
	var token strings.Builder
	var quote byte
	tokenStarted := false
	flush := func() {
		if tokenStarted {
			arguments = append(arguments, token.String())
			token.Reset()
			tokenStarted = false
		}
	}

	for i := 0; i < len(line); i++ {
		character := line[i]
		if quote == '\'' {
			if character == quote {
				quote = 0
			} else {
				token.WriteByte(character)
			}
			continue
		}
		if quote == '"' {
			switch character {
			case '"':
				quote = 0
			case '\\':
				if i+1 >= len(line) {
					token.WriteByte(character)
					continue
				}
				next := line[i+1]
				if strings.ContainsRune("$`\"\\", rune(next)) {
					token.WriteByte(next)
					i++
				} else if next == '\n' {
					i++
				} else {
					token.WriteByte(character)
				}
			default:
				token.WriteByte(character)
			}
			continue
		}

		switch character {
		case '\'', '"':
			quote = character
			tokenStarted = true
		case '\\':
			if i+1 >= len(line) {
				return nil, false
			}
			i++
			if line[i] != '\n' {
				token.WriteByte(line[i])
				tokenStarted = true
			}
		case ' ', '\t', '\r', '\n':
			flush()
		default:
			token.WriteByte(character)
			tokenStarted = true
		}
	}
	if quote != 0 {
		return nil, false
	}
	flush()
	return arguments, true
}

func splitShellCommands(line string) []shellCommand {
	commands := []shellCommand{}
	start := 0
	separator := ""
	var quote byte
	escaped := false
	commandSubstitutionDepth := 0
	groupDepth := 0
	flush := func(end int, nextSeparator string) {
		if command := strings.TrimSpace(line[start:end]); command != "" {
			commands = append(commands, shellCommand{text: command, separator: separator})
		}
		separator = nextSeparator
	}

	for i := 0; i < len(line); i++ {
		character := line[i]
		if escaped {
			escaped = false
			continue
		}
		if quote != 0 {
			if character == '\\' && quote != '\'' {
				escaped = true
			} else if character == quote {
				quote = 0
			}
			continue
		}
		if commandSubstitutionDepth > 0 {
			if character == '(' {
				commandSubstitutionDepth++
			} else if character == ')' {
				commandSubstitutionDepth--
			}
			continue
		}
		if character == '$' && i+1 < len(line) && line[i+1] == '(' {
			commandSubstitutionDepth = 1
			i++
			continue
		}
		switch character {
		case '\\':
			escaped = true
		case '\'', '"', '`':
			quote = character
		case '(':
			groupDepth++
		case ')':
			if groupDepth > 0 {
				groupDepth--
			}
		case '#':
			if i == 0 || strings.ContainsRune(" \t\r\n;|&", rune(line[i-1])) {
				flush(i, "")
				return commands
			}
		case ';':
			if groupDepth > 0 {
				continue
			}
			flush(i, ";")
			start = i + 1
		case '&', '|':
			if groupDepth == 0 && i+1 < len(line) && line[i+1] == character {
				flush(i, line[i:i+2])
				i++
				start = i + 1
			}
		}
	}
	flush(len(line), "")
	return commands
}

func hasUnsupportedShellSyntax(line string) bool {
	var quote byte
	escaped := false
	inBacktick := false
	for i := 0; i < len(line); i++ {
		character := line[i]
		if escaped {
			escaped = false
			continue
		}
		if inBacktick {
			if character == '\\' {
				escaped = true
			} else if character == '`' {
				inBacktick = false
			}
			continue
		}
		if quote == '\'' {
			if character == quote {
				quote = 0
			}
			continue
		}
		if quote == '"' {
			if character == '\\' {
				escaped = true
			} else if character == quote {
				quote = 0
			} else if character == '`' {
				inBacktick = true
			} else if character == '$' && i+1 < len(line) && line[i+1] == '(' {
				return true
			}
			continue
		}
		switch character {
		case '\\':
			escaped = true
		case '\'', '"':
			quote = character
		case '`':
			inBacktick = true
		case '<', '>', '(', ')', '{', '}':
			return true
		case '$':
			if i+1 < len(line) && line[i+1] == '(' {
				return true
			}
		case '|', '&':
			return true
		}
	}
	return false
}

func shellCommandExecution(separator string, previous shellCommandStatus) (bool, bool) {
	switch separator {
	case "", ";":
		return true, true
	case "&&":
		if previous == shellStatusSuccess {
			return true, true
		}
		if previous == shellStatusFailure {
			return false, true
		}
	case "||":
		if previous == shellStatusFailure {
			return true, true
		}
		if previous == shellStatusSuccess {
			return false, true
		}
	}
	return false, false
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
	patterns.defaultCompile = cfg.RegexCompile == RegexCompile

	fileRegex, err := regexp.Compile(cfg.RegexFile)
	if err != nil {
		return patterns, err
	}
	patterns.file = fileRegex
	patterns.defaultFile = cfg.RegexFile == RegexFile

	return patterns, nil
}

func normalizeCompilerArgs(arguments []string) []string {
	invocation := parseCompilerInvocation(arguments)
	if !invocation.valid || len(arguments) < 3 {
		return arguments
	}

	normalized := make([]string, 0, len(arguments))
	normalized = append(normalized, arguments[:invocation.optionsStart]...)

	options := true
	for i := invocation.optionsStart; i < len(arguments); i++ {
		if options && arguments[i] == "--" {
			normalized = append(normalized, arguments[i:]...)
			break
		}
		if options && arguments[i] == "-target" && i+1 < len(arguments) {
			normalized = append(normalized, "--target="+arguments[i+1])
			i++
			continue
		}
		if options && compilerOptionTakesArgument(arguments[i]) && i+1 < len(arguments) {
			normalized = append(normalized, arguments[i], arguments[i+1])
			i++
			continue
		}
		normalized = append(normalized, arguments[i])
	}

	return normalized
}

func insertCompilerArguments(arguments []string, invocation compilerInvocation, added []string) []string {
	if len(added) == 0 {
		return arguments
	}
	insertAt := len(arguments)
	scan := scanCompilerArguments(arguments, invocation.optionsStart, invocation.compiler, "")
	if scan.terminatorIndex >= 0 {
		insertAt = scan.terminatorIndex
	}
	result := make([]string, 0, len(arguments)+len(added))
	result = append(result, arguments[:insertAt]...)
	result = append(result, added...)
	return append(result, arguments[insertAt:]...)
}

func compilerArguments(arguments []string, invocation compilerInvocation) []string {
	if invocation.explicit {
		return arguments[invocation.compilerIndex:]
	}
	result := make([]string, 1, len(arguments)-invocation.optionsStart+1)
	result[0] = invocation.compiler
	return append(result, arguments[invocation.optionsStart:]...)
}

func trackedPathJoin(base, child string) string {
	windowsContext := runtime.GOOS == "windows" || isExplicitWindowsPath(base) || isExplicitWindowsPath(child)
	return trackedPathJoinContext(base, child, windowsContext)
}

func trackedPathJoinContext(base, child string, windowsContext bool) string {
	if windowsContext {
		base = ConvertPath(base)
		child = ConvertPath(child)
	}
	if windowsContext && len(child) >= 2 && isASCIIAlpha(child[0]) && child[1] == ':' && (len(child) == 2 || child[2] != '/') {
		if len(base) >= 2 && strings.EqualFold(base[:2], child[:2]) {
			return trackedPathJoinContext(base, child[2:], true)
		}
		return cleanTrackedPathContext(child, true)
	}
	if strings.HasPrefix(child, "/") || windowsContext && isExplicitWindowsPath(child) {
		return cleanTrackedPathContext(child, windowsContext)
	}
	joined := path.Join(base, child)
	if strings.HasPrefix(base, "//") && !strings.HasPrefix(joined, "//") {
		joined = "/" + joined
	}
	return joined
}

func cleanTrackedPath(value string) string {
	return cleanTrackedPathContext(value, runtime.GOOS == "windows" || isExplicitWindowsPath(value))
}

func cleanTrackedPathContext(value string, windowsContext bool) string {
	if windowsContext {
		value = ConvertPath(value)
	}
	cleaned := path.Clean(value)
	if strings.HasPrefix(value, "//") && !strings.HasPrefix(cleaned, "//") {
		cleaned = "/" + cleaned
	}
	return cleaned
}

func isASCIIAlpha(value byte) bool {
	return value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z'
}

func isMakeExecutable(argument string) bool {
	switch executableBase(argument) {
	case "make", "gmake", "mingw32-make":
		return true
	default:
		return false
	}
}

func isMakeExecutableFromArguments(arguments []string) bool {
	return len(arguments) > 0 && isMakeExecutable(arguments[0])
}

func splitMakeCommand(line string) ([]string, bool) {
	arguments := []string{}
	var token strings.Builder
	var quote byte
	escaped := false
	tokenStarted := false
	flush := func() {
		if tokenStarted {
			arguments = append(arguments, token.String())
			token.Reset()
			tokenStarted = false
		}
	}

	for i := 0; i < len(line); i++ {
		character := line[i]
		if escaped {
			token.WriteByte(character)
			tokenStarted = true
			escaped = false
			continue
		}
		if quote == '\'' {
			if character == quote {
				quote = 0
			} else {
				token.WriteByte(character)
			}
			continue
		}
		if quote == '"' {
			if character == quote {
				quote = 0
				continue
			}
			if character == '\\' {
				if i+1 < len(line) && line[i+1] == '\\' && (token.Len() == 0 || isAttachedPathOptionStart(token.String())) {
					token.WriteString(`\\`)
					i++
				} else if i+1 < len(line) && strings.ContainsRune(`$\"`, rune(line[i+1])) {
					escaped = true
				} else {
					token.WriteByte(character)
				}
				continue
			}
			token.WriteByte(character)
			continue
		}

		switch character {
		case '\'', '"':
			quote = character
			tokenStarted = true
		case '\\':
			if i+1 < len(line) && line[i+1] == '\\' && (token.Len() == 0 || isAttachedPathOptionStart(token.String())) {
				token.WriteString(`\\`)
				tokenStarted = true
				i++
			} else if i+1 < len(line) && strings.ContainsRune(" \t\r\n'\"\\$`;|&", rune(line[i+1])) {
				escaped = true
				tokenStarted = true
			} else {
				token.WriteByte(character)
				tokenStarted = true
			}
		case ' ', '\t', '\r', '\n':
			flush()
		default:
			token.WriteByte(character)
			tokenStarted = true
		}
	}
	if escaped || quote != 0 {
		return nil, false
	}
	flush()
	return arguments, true
}

func makeCommandDirectory(line, workingDir string) (string, bool) {
	arguments, ok := splitMakeCommand(line)
	if !ok || len(arguments) == 0 || !isMakeExecutable(arguments[0]) {
		return "", false
	}

	directory := workingDir
	windowsContext := runtime.GOOS == "windows" || executableBase(arguments[0]) == "mingw32-make" || isExplicitWindowsPath(workingDir)
	found := false
	for i := 1; i < len(arguments); i++ {
		argument := arguments[i]
		if argument == "--" {
			break
		}
		var value string
		switch {
		case argument == "-C" || argument == "--directory":
			if i+1 >= len(arguments) {
				return "", false
			}
			i++
			value = arguments[i]
		case strings.HasPrefix(argument, "-C") && len(argument) > 2:
			value = argument[2:]
		case strings.HasPrefix(argument, "--directory="):
			value = strings.TrimPrefix(argument, "--directory=")
		default:
			continue
		}
		windowsContext = windowsContext || isExplicitWindowsPath(value)
		directory = trackedPathJoinContext(directory, value, windowsContext)
		found = true
	}
	return directory, found
}

func makeVirtualDirectories(arguments []string, workingDir string) ([]string, bool) {
	if len(arguments) == 0 || executableBase(arguments[0]) != "mkdir" {
		return nil, false
	}
	parents := false
	directories := []string{}
	options := true
	for _, argument := range arguments[1:] {
		if options && argument == "--" {
			options = false
			continue
		}
		if options && (argument == "-p" || argument == "--parents") {
			parents = true
			continue
		}
		if options && strings.HasPrefix(argument, "-") {
			return nil, false
		}
		directories = append(directories, trackedPathJoin(workingDir, argument))
	}
	return directories, parents && len(directories) > 0
}

func hasVirtualDirectory(directories map[string]struct{}, directory string) bool {
	cleaned := cleanTrackedPath(directory)
	if _, ok := directories[cleaned]; ok {
		return true
	}
	prefix := strings.TrimSuffix(cleaned, "/") + "/"
	for candidate := range directories {
		if strings.HasPrefix(candidate, prefix) {
			return true
		}
	}
	return false
}

func makeDirectoryEvent(line, event string) (string, bool) {
	marker := ": " + event + " directory "
	index := strings.Index(line, marker)
	if index < 0 || !strings.Contains(strings.ToLower(line[:index]), "make") {
		return "", false
	}
	value := strings.TrimSpace(line[index+len(marker):])
	if len(value) < 2 {
		return "", false
	}
	delimiter := value[0]
	closingDelimiter := delimiter
	if delimiter == '`' {
		closingDelimiter = '\''
	}
	if (delimiter != '\'' && delimiter != '"' && delimiter != '`') || value[len(value)-1] != closingDelimiter {
		return "", false
	}
	return value[1 : len(value)-1], true
}

func (t *Tool) expandNestedCommands(line, workingDir string) (string, bool) {
	var result strings.Builder
	var quote byte
	escaped := false
	copyFrom := 0
	for i := 0; i < len(line); i++ {
		character := line[i]
		if escaped {
			escaped = false
			continue
		}
		if quote == '\'' {
			if character == quote {
				quote = 0
			}
			continue
		}
		if character == '\\' {
			escaped = true
			continue
		}
		if character == '\'' {
			quote = character
			continue
		}
		if character == '"' {
			if quote == '"' {
				quote = 0
			} else if quote == 0 {
				quote = character
			}
			continue
		}
		if character != '`' {
			continue
		}
		start := i
		for i++; i < len(line); i++ {
			if line[i] == '\\' {
				i++
				continue
			}
			if line[i] == '`' {
				cmd := exec.CommandContext(t.operationContext(), "sh", "-c", line[start+1:i])
				configureProcessCommand(cmd, t.operationContext())
				cmd.Dir = workingDir
				out, err := outputProcessCommand(cmd, t.operationContext())
				if err != nil {
					t.Logger.Error("Error executing nested command:", err)
					return "", false
				}
				result.WriteString(line[copyFrom:start])
				output := strings.TrimRight(string(out), "\n")
				if quote == '"' {
					result.WriteString(quoteDoubleQuotedSubstitution(output))
				} else {
					fields := strings.Fields(output)
					for fieldIndex, field := range fields {
						if fieldIndex > 0 {
							result.WriteByte(' ')
						}
						result.WriteString(quotePOSIXShellArgument(field))
					}
				}
				copyFrom = i + 1
				break
			}
		}
		if i >= len(line) {
			return "", false
		}
	}
	result.WriteString(line[copyFrom:])
	return result.String(), true
}

func quotePOSIXShellArgument(argument string) string {
	return "'" + strings.ReplaceAll(argument, "'", `'"'"'`) + "'"
}

func quoteDoubleQuotedSubstitution(output string) string {
	var quoted strings.Builder
	for _, character := range output {
		if strings.ContainsRune("$`\"\\", character) {
			quoted.WriteByte('\\')
		}
		quoted.WriteRune(character)
	}
	return quoted.String()
}

func hasCompileOnlyFlag(arguments []string, invocation compilerInvocation) bool {
	options := true
	for i := invocation.optionsStart; i < len(arguments); i++ {
		argument := arguments[i]
		if options && argument == "--" {
			options = false
			continue
		}
		if options && compilerOptionTakesArgument(argument) {
			i++
			continue
		}
		if options && argument == "-c" {
			return true
		}
	}
	return false
}

func windowsPathToken(argument string) bool {
	if len(argument) >= 3 && isASCIIAlpha(argument[0]) &&
		argument[1] == ':' && (argument[2] == '/' || argument[2] == '\\') {
		return true
	}
	return strings.HasPrefix(argument, `\\`) || strings.HasPrefix(argument, "//")
}

func windowsRelativePathToken(argument string) bool {
	if !strings.Contains(argument, `\`) {
		return false
	}
	for i := 0; i+1 < len(argument); i++ {
		if argument[i] == '\\' && strings.ContainsRune(" #$`;&|()<>*?[]{}!", rune(argument[i+1])) {
			return false
		}
	}
	return true
}

var attachedPathOptionPrefixes = []string{
	"-I", "-L", "-F", "-B", "-isystem", "-iquote", "-idirafter", "-include", "-imacros",
	"--sysroot=", "-working-directory=",
}

func pathOptionPrefix(argument string) string {
	for _, prefix := range attachedPathOptionPrefixes {
		if strings.HasPrefix(argument, prefix) && len(argument) > len(prefix) {
			return prefix
		}
	}
	return ""
}

func isAttachedPathOptionStart(argument string) bool {
	for _, prefix := range attachedPathOptionPrefixes {
		if argument == prefix {
			return true
		}
	}
	return false
}

func attachedPathOptionValue(argument string) string {
	if prefix := pathOptionPrefix(argument); prefix != "" {
		return argument[len(prefix):]
	}
	return ""
}

func restoreWindowsArguments(arguments, rawArguments []string, invocation compilerInvocation) {
	if len(arguments) != len(rawArguments) {
		return
	}
	windowsContext := runtime.GOOS == "windows"
	for _, rawArgument := range rawArguments {
		value := rawArgument
		if attached := attachedPathOptionValue(rawArgument); attached != "" {
			value = attached
		}
		if windowsPathToken(value) {
			windowsContext = true
			break
		}
	}
	inputIndexes := scanCompilerArguments(arguments, invocation.optionsStart, invocation.compiler, "").inputIndexes
	inputs := make(map[int]struct{}, len(inputIndexes))
	for _, index := range inputIndexes {
		inputs[index] = struct{}{}
	}
	for i, rawArgument := range rawArguments {
		normalized := ""
		switch {
		case windowsPathToken(rawArgument):
			normalized = ConvertPath(rawArgument)
		case windowsContext && i <= invocation.compilerIndex && windowsRelativePathToken(rawArgument):
			normalized = ConvertPath(rawArgument)
		case attachedPathOptionValue(rawArgument) != "":
			prefix := pathOptionPrefix(rawArgument)
			value := attachedPathOptionValue(rawArgument)
			if windowsPathToken(value) || windowsContext && windowsRelativePathToken(value) {
				normalized = prefix + ConvertPath(value)
			}
		default:
			if windowsContext && i > 0 && compilerPathOptionOperand(rawArguments[i-1]) && windowsRelativePathToken(rawArgument) {
				normalized = ConvertPath(rawArgument)
			} else if windowsContext {
				_, input := inputs[i]
				if input && hasSourceExtension(ConvertPath(rawArgument)) && windowsRelativePathToken(rawArgument) {
					normalized = ConvertPath(rawArgument)
				}
			}
		}
		if normalized != "" && pathWithoutSeparators(arguments[i]) == pathWithoutSeparators(normalized) {
			arguments[i] = normalized
		}
	}
}

func compilerPathOptionOperand(argument string) bool {
	switch argument {
	case "-o", "--output", "-MF", "-MJ", "-include", "--include", "-imacros", "--imacros",
		"-include-pch", "-dependency-file", "--dependency-file", "-T", "--script", "-idirafter",
		"-iprefix", "-ivfsoverlay", "-vfsoverlay", "-working-directory", "-serialize-diagnostics",
		"-iwithprefix", "-iwithprefixbefore", "-iwithsysroot", "-isystem", "-isystem-after", "-iquote", "-F",
		"-stdlib++-isystem", "--config", "-imultilib", "-imultiarch", "-index-store-path", "-fdebug-compilation-dir",
		"-iframework", "-iframeworkwithsysroot", "-I", "-L", "-B", "--sysroot", "-isysroot",
		"--gcc-toolchain", "-gcc-toolchain", "-resource-dir", "--cuda-path":
		return true
	default:
		return false
	}
}

func defaultCompilerStart(arguments []string, workingDir string) defaultCompilerLocation {
	index := 0
	fullPathSafe := true
	launcherWorkingDir := workingDir
	for index < len(arguments) && isShellAssignment(arguments[index]) {
		name, _, _ := strings.Cut(arguments[index], "=")
		if strings.EqualFold(name, "PATH") {
			fullPathSafe = false
		}
		index++
	}
	launcherPrefix := index > 0
	if index >= len(arguments) {
		return defaultCompilerLocation{}
	}

	switch executableBase(arguments[index]) {
	case "env":
		launcherPrefix = true
		index++
		for index < len(arguments) {
			argument := arguments[index]
			switch {
			case isShellAssignment(argument):
				name, _, _ := strings.Cut(argument, "=")
				if strings.EqualFold(name, "PATH") {
					fullPathSafe = false
				}
				index++
			case argument == "-i", argument == "--ignore-environment":
				fullPathSafe = false
				index++
			case argument == "--":
				index++
				if index >= len(arguments) {
					return defaultCompilerLocation{}
				}
				return defaultCompilerLocation{index: index, launcher: launcherPrefix, fullPathSafe: fullPathSafe, workingDir: launcherWorkingDir, valid: true}
			case argument == "-C" || argument == "--chdir":
				if index+1 >= len(arguments) {
					return defaultCompilerLocation{}
				}
				launcherWorkingDir = trackedPathJoin(launcherWorkingDir, arguments[index+1])
				index += 2
			case strings.HasPrefix(argument, "-C") && len(argument) > 2:
				launcherWorkingDir = trackedPathJoin(launcherWorkingDir, argument[2:])
				index++
			case strings.HasPrefix(argument, "--chdir="):
				launcherWorkingDir = trackedPathJoin(launcherWorkingDir, strings.TrimPrefix(argument, "--chdir="))
				index++
			case argument == "-u" || argument == "--unset":
				if index+1 >= len(arguments) {
					return defaultCompilerLocation{}
				}
				if strings.EqualFold(arguments[index+1], "PATH") {
					fullPathSafe = false
				}
				index += 2
			case strings.HasPrefix(argument, "--unset="):
				if strings.EqualFold(strings.TrimPrefix(argument, "--unset="), "PATH") {
					fullPathSafe = false
				}
				index++
			default:
				return defaultCompilerLocation{index: index, launcher: launcherPrefix, fullPathSafe: fullPathSafe, workingDir: launcherWorkingDir, valid: true}
			}
		}
		return defaultCompilerLocation{}
	case "xcrun":
		launcherPrefix = true
		fullPathSafe = false
		index++
		for index < len(arguments) {
			argument := arguments[index]
			switch argument {
			case "--sdk", "-sdk", "--toolchain", "-toolchain":
				if index+1 >= len(arguments) {
					return defaultCompilerLocation{}
				}
				index += 2
			case "--log", "--verbose", "--no-cache", "--kill-cache", "--run", "-r":
				index++
			default:
				return defaultCompilerLocation{index: index, launcher: launcherPrefix, fullPathSafe: fullPathSafe, workingDir: launcherWorkingDir, valid: true}
			}
		}
		return defaultCompilerLocation{}
	case "sh", "bash", "dash", "zsh":
		if index+1 >= len(arguments) || executableBase(arguments[index+1]) != "libtool" {
			return defaultCompilerLocation{}
		}
		return libtoolCompilerStart(arguments, index+2, fullPathSafe, launcherWorkingDir)
	case "libtool":
		return libtoolCompilerStart(arguments, index+1, fullPathSafe, launcherWorkingDir)
	default:
		if arguments[index] == "libtool:" && index+1 < len(arguments) && arguments[index+1] == "compile:" {
			return defaultCompilerLocation{index: index + 2, launcher: true, fullPathSafe: fullPathSafe, workingDir: launcherWorkingDir, valid: index+2 < len(arguments)}
		}
		return defaultCompilerLocation{index: index, launcher: launcherPrefix, fullPathSafe: fullPathSafe, workingDir: launcherWorkingDir, valid: true}
	}
}

func libtoolCompilerStart(arguments []string, index int, fullPathSafe bool, workingDir string) defaultCompilerLocation {
	modeCompile := false
	for index < len(arguments) {
		argument := arguments[index]
		switch {
		case argument == "--mode=compile":
			modeCompile = true
			index++
		case argument == "--mode" && index+1 < len(arguments) && arguments[index+1] == "compile":
			modeCompile = true
			index += 2
		case argument == "--":
			index++
		case strings.HasPrefix(argument, "-"):
			index++
		default:
			if !modeCompile {
				return defaultCompilerLocation{}
			}
			return defaultCompilerLocation{index: index, launcher: true, fullPathSafe: fullPathSafe, workingDir: workingDir, valid: true}
		}
	}
	return defaultCompilerLocation{}
}

func applyCompilerWorkingDirectory(arguments []string, invocation compilerInvocation, workingDir string) string {
	directory := workingDir
	options := true
	for i := invocation.optionsStart; i < len(arguments); i++ {
		argument := arguments[i]
		if options && argument == "--" {
			options = false
			continue
		}
		if !options {
			continue
		}
		if argument == "-working-directory" && i+1 < len(arguments) {
			directory = trackedPathJoin(workingDir, arguments[i+1])
			arguments[i+1] = directory
			i++
			continue
		}
		if value, ok := strings.CutPrefix(argument, "-working-directory="); ok {
			directory = trackedPathJoin(workingDir, value)
			arguments[i] = "-working-directory=" + directory
			continue
		}
		if compilerOptionTakesArgument(argument) {
			i++
		}
	}
	return directory
}

func (t *Tool) processCompileCommand(command string, workingDir string, patterns parserPatterns) []parsedCompileCommand {
	arguments := t.splitArgs(command)
	if len(arguments) == 0 {
		return nil
	}

	rawArguments, _ := splitMakeCommand(command)
	findCompile := false
	compilerStartsCommand := false
	launcherPrefix := false
	fullPathSafe := true
	compilerWordIndex := -1
	compilerWorkingDir := workingDir
	if patterns.defaultCompile {
		if location := defaultCompilerStart(arguments, workingDir); location.valid {
			candidate := arguments[location.index:]
			invocation := parseCompilerInvocation(candidate)
			if invocation.valid && patterns.compile.MatchString(invocation.compiler) {
				findCompile = true
				compilerStartsCommand = true
				launcherPrefix = location.launcher
				fullPathSafe = location.fullPathSafe
				compilerWordIndex = location.index
				compilerWorkingDir = location.workingDir
				arguments = candidate
			}
		}
	} else {
		for i, word := range arguments {
			if patterns.compile.MatchString(word) {
				findCompile = true
				compilerStartsCommand = true
				compilerWordIndex = i
				launcherPrefix = i > 0
				fullPathSafe = i == 0
				arguments = arguments[i:]
				break
			}
		}
	}
	if !findCompile || len(arguments) == 0 {
		return nil
	}
	if compilerWordIndex >= 0 && compilerWordIndex < len(rawArguments) {
		rawArguments = rawArguments[compilerWordIndex:]
	}
	if invocation := parseCompilerInvocation(arguments); invocation.valid {
		restoreWindowsArguments(arguments, rawArguments, invocation)
	}

	arguments = normalizeCompilerArgs(arguments)
	invocation := parseCompilerInvocation(arguments)
	if !invocation.valid {
		return nil
	}
	if launcherPrefix {
		invocation.probeDisabled = "compiler command uses environment assignments or a launcher"
	}
	files := sourceFilesFromArguments(arguments, invocation)
	multipleSources := len(files) > 1
	if patterns.defaultFile {
		if !hasCompileOnlyFlag(arguments, invocation) {
			if compilerStartsCommand && len(files) > 0 {
				t.Logger.Error("source files found without -c; command ignored")
			}
			t.Logger.Debugf("found compile:%s, but not compile-only command, ignore command", arguments[0])
			return nil
		}
		if len(files) == 0 {
			t.Logger.Debugf("found compile:%s, but not found file, ignore command", arguments[0])
			return nil
		}
	} else {
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
			if compilerStartsCommand && len(files) > 0 && !hasCompileOnlyFlag(arguments, invocation) {
				t.Logger.Error("source files found without -c; command ignored")
			}
			t.Logger.Debugf("found compile:%s, but not found file, ignore command", arguments[0])
			return nil
		}
		files = []string{ConvertPath(filePath)}
	}
	arguments = insertCompilerArguments(arguments, invocation, t.Config.AddArgs)
	entryDirectory := applyCompilerWorkingDirectory(arguments, invocation, compilerWorkingDir)

	filteredFiles := make([]string, 0, len(files))
	for _, sourceFile := range files {
		if patterns.exclude != nil && patterns.exclude.MatchString(sourceFile) {
			t.Logger.Infof("file %s exclude", sourceFile)
			continue
		}
		if !t.Config.NoStrict {
			fileFullPath := sourceFile
			windowsContext := runtime.GOOS == "windows" || isExplicitWindowsPath(entryDirectory) || isExplicitWindowsPath(sourceFile)
			normalizedSource := sourceFile
			if windowsContext {
				normalizedSource = ConvertPath(normalizedSource)
			}
			if !strings.HasPrefix(normalizedSource, "/") && !(windowsContext && isExplicitWindowsPath(normalizedSource)) {
				fileFullPath = trackedPathJoinContext(entryDirectory, sourceFile, windowsContext)
			}
			info, err := os.Stat(fileFullPath)
			if err != nil || info.IsDir() {
				t.Logger.Warnf("file %s not exist", fileFullPath)
				continue
			}
		}
		filteredFiles = append(filteredFiles, sourceFile)
	}
	files = filteredFiles
	if len(files) == 0 {
		return nil
	}

	if t.Config.Macros && len(files) == 1 && !multipleSources && invocation.probeDisabled == "" {
		macros := t.getPredefinedMacros(compilerArguments(arguments, invocation), files[0], entryDirectory)
		withMacros := make([]string, 0, len(arguments)+len(macros))
		withMacros = append(withMacros, arguments[:invocation.optionsStart]...)
		withMacros = append(withMacros, macros...)
		arguments = append(withMacros, arguments[invocation.optionsStart:]...)
	} else if t.Config.Macros && (multipleSources || invocation.probeDisabled != "") {
		reason := invocation.probeDisabled
		if reason == "" {
			reason = "multiple source inputs are not supported"
		}
		t.Logger.Errorf("failed to get predefined macros from %s: %s", invocation.compiler, reason)
	}

	if t.Config.FullPath && invocation.explicit && fullPathSafe {
		compileFullPath := compilerFullPath(arguments[invocation.compilerIndex], compilerWorkingDir)
		if compileFullPath != "" {
			arguments[invocation.compilerIndex] = ConvertPath(compileFullPath)
		}
	}

	result := make([]parsedCompileCommand, 0, len(files))
	for _, sourceFile := range files {
		result = append(result, parsedCompileCommand{
			arguments: append([]string(nil), arguments...),
			filePath:  sourceFile,
			directory: entryDirectory,
		})
	}
	return result
}

func (t *Tool) Parse(buildLog []string) {
	type directoryFrame struct {
		path        string
		provisional bool
	}
	var (
		workingDir string
		cmdCnt     int
		result     []Command
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

	dirStack := []directoryFrame{{path: workingDir}}
	virtualDirectories := make(map[string]struct{})

	for _, line := range mergeLogicalLines(buildLog) {
		if t.operationContext().Err() != nil {
			t.StatusCode = contextExitCode(t.operationContext())
			return
		}
		t.Logger.Debug("New command:", line)

		// Track make-reported directory changes {{{
		if directory, ok := makeDirectoryEvent(line, "Entering"); ok {
			enterDir := cleanTrackedPath(directory)
			if len(dirStack) > 0 && dirStack[0].provisional {
				dirStack[0] = directoryFrame{path: enterDir}
			} else {
				dirStack = append([]directoryFrame{{path: enterDir}}, dirStack...)
			}
			workingDir = dirStack[0].path
			t.Logger.Infof("entering change workingDir: %s", workingDir)
			continue
		} else if directory, ok := makeDirectoryEvent(line, "Leaving"); ok {
			leaveDir := cleanTrackedPath(directory)
			for i := 0; i < len(dirStack)-1; i++ {
				if cleanTrackedPath(dirStack[i].path) != leaveDir {
					continue
				}
				dirStack = append(dirStack[:i], dirStack[i+1:]...)
				workingDir = dirStack[0].path
				t.Logger.Infof("leaving change workingDir: %s", workingDir)
				break
			}
			continue
		}

		if checkingMake.MatchString(line) {
			continue
		}

		lineWorkingDir := workingDir
		pendingMakeDir := ""
		pendingMakeSafe := false
		previousStatus := shellStatusSuccess
		for _, shellCommand := range splitShellCommands(line) {
			execute, known := shellCommandExecution(shellCommand.separator, previousStatus)
			if !known {
				previousStatus = shellStatusUnknown
				pendingMakeSafe = false
				continue
			}
			if !execute {
				continue
			}
			commandText := shellCommand.text
			if hasUnsupportedShellSyntax(commandText) {
				previousStatus = shellStatusUnknown
				continue
			}
			rawArguments, rawOK := splitMakeCommand(commandText)
			needsExpansion := patterns.compile.MatchString(commandText)
			if rawOK && len(rawArguments) > 0 {
				needsExpansion = needsExpansion || rawArguments[0] == "cd" || isMakeExecutableFromArguments(rawArguments) ||
					t.makeDirectoryMarkers && executableBase(rawArguments[0]) == "mkdir"
			}
			if needsExpansion && strings.Contains(commandText, "`") {
				var ok bool
				commandText, ok = t.expandNestedCommands(commandText, lineWorkingDir)
				if !ok {
					previousStatus = shellStatusUnknown
					continue
				}
			}
			arguments, ok := splitMakeCommand(commandText)
			if t.makeDirectoryMarkers {
				if directories, recognized := makeVirtualDirectories(arguments, lineWorkingDir); recognized {
					for _, directory := range directories {
						virtualDirectories[cleanTrackedPath(directory)] = struct{}{}
					}
					previousStatus = shellStatusSuccess
					continue
				}
			}
			if ok && len(arguments) > 0 && arguments[0] == "cd" {
				if len(arguments) != 2 {
					previousStatus = shellStatusUnknown
					continue
				}
				nextDir := trackedPathJoin(lineWorkingDir, arguments[1])
				info, err := os.Stat(nextDir)
				virtual := hasVirtualDirectory(virtualDirectories, nextDir)
				if (err != nil || !info.IsDir()) && !(t.makeDirectoryMarkers && virtual) {
					previousStatus = shellStatusFailure
					continue
				}
				lineWorkingDir = nextDir
				previousStatus = shellStatusSuccess
				t.Logger.Infof("Temporarily change workingDir: %s", lineWorkingDir)
				continue
			}

			if enterDir, ok := makeCommandDirectory(commandText, lineWorkingDir); ok {
				pendingMakeDir = enterDir
				pendingMakeSafe = true
				previousStatus = shellStatusUnknown
			}

			if !patterns.compile.MatchString(commandText) {
				if isMakeExecutableFromArguments(arguments) {
					previousStatus = shellStatusUnknown
					continue
				}
				if len(arguments) == 1 && (arguments[0] == "true" || arguments[0] == ":") {
					previousStatus = shellStatusSuccess
				} else if len(arguments) == 1 && arguments[0] == "false" {
					previousStatus = shellStatusFailure
				} else {
					previousStatus = shellStatusUnknown
				}
				continue
			}
			for _, parsed := range t.processCompileCommand(commandText, lineWorkingDir, patterns) {
				command := ShellJoinArgs(parsed.arguments)
				if t.Config.CommandStyle {
					result = append(result, Command{Directory: parsed.directory, Command: command, File: parsed.filePath})
				} else {
					result = append(result, Command{Directory: parsed.directory, Arguments: parsed.arguments, File: parsed.filePath})
				}
				t.Logger.Infof("Adding command %d: %s", cmdCnt, command)
				cmdCnt++
			}
			previousStatus = shellStatusUnknown
		}
		if pendingMakeSafe && pendingMakeDir != "" && !t.makeDirectoryMarkers {
			dirStack = append([]directoryFrame{{path: pendingMakeDir, provisional: true}}, dirStack...)
			workingDir = pendingMakeDir
			t.Logger.Infof("make cmd change workingDir: %s", workingDir)
		}
	}
	if t.operationContext().Err() != nil {
		t.StatusCode = contextExitCode(t.operationContext())
		return
	}

	t.WriteJSON(t.Config.OutputFile, cmdCnt, &result)
	if t.operationContext().Err() != nil {
		t.StatusCode = contextExitCode(t.operationContext())
	}
}
