package internal

import (
	"bytes"
	"os"
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
	RegexCompile string = `(?i)^(?:.*[/\\])?(?:[A-Za-z0-9_.+]+-)*(?:(?:gcc|g\+\+)(?:(?:-?[0-9]+(?:\.[0-9]+)*(?:-(?:posix|win32))?)|-(?:posix|win32)|-mp-[0-9]+(?:\.[0-9]+)*)?|clang(?:\+\+|-cl)?(?:-[0-9]+(?:\.[0-9]+)*)?|(?:cc|c\+\+)(?:-[0-9]+(?:\.[0-9]+)*)?)(?:\.exe)?$`
	RegexFile    string = `^.*\s+-c.*\s(?:(?:"|')(.*?\.(?i:c|cpp|cc|cxx|c\+\+|s|m|mm|cu))(?:"|')|([^\s"']+\.(?i:c|cpp|cc|cxx|c\+\+|s|m|mm|cu)))(\s|$)`

	// We want to skip such lines from configure to avoid spurious MAKE expansion errors.
	checkingMake = regexp.MustCompile(`^checking whether .* sets \$\(\w+\)\.\.\. (yes|no)$`)
)

const maxBuildLogLineSize = 100 * 1024 * 1024

type parserPatterns struct {
	compile        *regexp.Regexp
	file           *regexp.Regexp
	exclude        []*regexp.Regexp
	defaultCompile bool
	defaultFile    bool
}

type logicalLine struct {
	text string
	line int
}

type shellTokenizationError struct {
	offset int
	reason string
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

func (t *Tool) splitArgs(input string, line int, workingDir string) []string {
	args, parseErr := splitShellArguments(input)
	if parseErr != nil {
		t.logTokenizationFailure(line, workingDir, parseErr)
		return nil
	}

	return args
}

func splitShellArguments(line string) ([]string, *shellTokenizationError) {
	arguments := []string{}
	var token strings.Builder
	var quote byte
	quoteStart := 0
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
			quoteStart = i
			tokenStarted = true
		case '\\':
			if i+1 >= len(line) {
				return nil, &shellTokenizationError{offset: i, reason: "trailing escape"}
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
		flush()
		return nil, &shellTokenizationError{offset: quoteStart, reason: "unterminated quote"}
	}
	flush()
	return arguments, nil
}

func (t *Tool) logTokenizationFailure(line int, workingDir string, parseErr *shellTokenizationError) {
	t.Logger.Errorf(
		"skip malformed command at build log line %d (cwd %q): %s at byte %d",
		line,
		workingDir,
		parseErr.reason,
		parseErr.offset,
	)
}

func shellLexicalError(line string) *shellTokenizationError {
	var quote byte
	quoteStart := 0
	backtickStart := -1
	escaped := false
	for i := 0; i < len(line); i++ {
		character := line[i]
		if escaped {
			escaped = false
			continue
		}
		if backtickStart >= 0 {
			if character == '\\' {
				escaped = true
			} else if character == '`' {
				backtickStart = -1
			}
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
		if character == '`' {
			backtickStart = i
			continue
		}
		if character == '\'' {
			if quote == 0 {
				quote = character
				quoteStart = i
			}
			continue
		}
		if character == '"' {
			if quote == '"' {
				quote = 0
			} else if quote == 0 {
				quote = character
				quoteStart = i
			}
		}
	}
	if backtickStart >= 0 {
		return &shellTokenizationError{offset: backtickStart, reason: "unterminated backtick"}
	}
	if quote != 0 {
		return &shellTokenizationError{offset: quoteStart, reason: "unterminated quote"}
	}
	if escaped {
		return &shellTokenizationError{offset: len(line) - 1, reason: "trailing escape"}
	}
	return nil
}

type logicalLineIssue struct {
	line   int
	reason string
}

func lineContinuation(line string, initialQuote byte, initialWordStarted bool) (string, bool, byte, bool) {
	quote := initialQuote
	escaped := false
	wordStarted := initialWordStarted || initialQuote != 0
	for i := 0; i < len(line); i++ {
		character := line[i]
		if quote == '\'' {
			if character == quote {
				quote = 0
			}
			continue
		}
		if escaped {
			escaped = false
			wordStarted = true
			continue
		}
		if character == '\\' {
			escaped = true
			continue
		}
		if quote != 0 {
			if character == quote {
				quote = 0
			}
			continue
		}
		if character == '#' && !wordStarted {
			return line, false, 0, false
		}
		if character == '\'' || character == '"' || character == '`' {
			quote = character
			wordStarted = true
			continue
		}
		switch character {
		case ' ', '\t', '\r', '\n', ';', '|', '&', '<', '>', '(', ')':
			wordStarted = false
		default:
			wordStarted = true
		}
	}
	if escaped && quote != '\'' {
		return line[:len(line)-1], true, quote, wordStarted
	}
	return line, false, 0, false
}

func mergeLogicalLines(lines []string) ([]logicalLine, []logicalLineIssue) {
	merged := make([]logicalLine, 0, len(lines))
	issues := []logicalLineIssue{}
	var builder strings.Builder
	var quote byte
	wordStarted := false
	continuationStart := 0

	for index, line := range lines {
		line = strings.TrimSuffix(line, "\r")
		if len(line) > maxBuildLogLineSize {
			start := index + 1
			if continuationStart != 0 {
				start = continuationStart
			}
			issues = append(issues, logicalLineIssue{line: start, reason: "physical line exceeds 100 MiB limit"})
			builder.Reset()
			quote = 0
			wordStarted = false
			continuationStart = 0
			continue
		}

		content, continued, nextQuote, nextWordStarted := lineContinuation(line, quote, wordStarted)
		if continued {
			if continuationStart == 0 {
				continuationStart = index + 1
			}
			builder.WriteString(content)
			quote = nextQuote
			wordStarted = nextWordStarted
			continue
		}

		builder.WriteString(content)
		if text := strings.TrimSpace(builder.String()); text != "" {
			start := index + 1
			if continuationStart != 0 {
				start = continuationStart
			}
			merged = append(merged, logicalLine{text: text, line: start})
		}
		builder.Reset()
		quote = 0
		wordStarted = false
		continuationStart = 0
	}

	if continuationStart != 0 {
		issues = append(issues, logicalLineIssue{line: continuationStart, reason: "unterminated line continuation"})
	}

	return merged, issues
}

func compilePatterns(cfg Config) (parserPatterns, error) {
	patterns := parserPatterns{}

	for _, pattern := range cfg.Exclude {
		excludeRegex, err := regexp.Compile(pattern)
		if err != nil {
			return patterns, err
		}
		patterns.exclude = append(patterns.exclude, excludeRegex)
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

func joinTrackedPath(base, child string) string {
	windowsContext := runtime.GOOS == "windows" || isExplicitWindowsAbsolutePath(base) || isExplicitWindowsAbsolutePath(child)
	return joinTrackedPathWithWindowsMode(base, child, windowsContext)
}

func joinTrackedPathWithWindowsMode(base, child string, windowsContext bool) string {
	if windowsContext {
		base = trackedPathToSlash(base)
		child = trackedPathToSlash(child)
	}
	if windowsContext && len(child) >= 2 && isASCIIAlpha(child[0]) && child[1] == ':' && (len(child) == 2 || child[2] != '/') {
		if len(base) >= 2 && strings.EqualFold(base[:2], child[:2]) {
			return joinTrackedPathWithWindowsMode(base, child[2:], true)
		}
		return cleanTrackedPathWithWindowsMode(child, true)
	}
	if strings.HasPrefix(child, "/") || windowsContext && isExplicitWindowsAbsolutePath(child) {
		return cleanTrackedPathWithWindowsMode(child, windowsContext)
	}
	joined := path.Join(base, child)
	if strings.HasPrefix(base, "//") && !strings.HasPrefix(joined, "//") {
		joined = "/" + joined
	}
	return joined
}

func cleanTrackedPath(value string) string {
	return cleanTrackedPathWithWindowsMode(value, runtime.GOOS == "windows" || isExplicitWindowsAbsolutePath(value))
}

func cleanTrackedPathWithWindowsMode(value string, windowsContext bool) string {
	if windowsContext {
		value = trackedPathToSlash(value)
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
	index := shellExecutableIndex(arguments)
	return index < len(arguments) && isMakeExecutable(arguments[index])
}

func shellExecutableIndex(arguments []string) int {
	index := 0
	for index < len(arguments) && isShellAssignment(arguments[index]) {
		index++
	}
	return index
}

type makeArgumentSpec struct {
	option      bool
	stop        bool
	valid       bool
	valueMode   makeOptionArgumentMode
	directory   bool
	valueInline bool
	inlineValue string
}

func classifyMakeArgument(argument string) makeArgumentSpec {
	if argument == "--" {
		return makeArgumentSpec{option: true, stop: true, valid: true}
	}
	if strings.HasPrefix(argument, "--") {
		name, ok := canonicalMakeLongOption(argument)
		if !ok {
			return makeArgumentSpec{}
		}
		_, value, attached := strings.Cut(argument, "=")
		valueMode := makeLongOptionArgumentMode(argument)
		if attached && valueMode == makeOptionArgumentNone {
			return makeArgumentSpec{}
		}
		return makeArgumentSpec{
			option:      true,
			valid:       true,
			valueMode:   valueMode,
			directory:   name == "directory",
			valueInline: attached,
			inlineValue: value,
		}
	}
	if len(argument) < 2 || argument[0] != '-' {
		return makeArgumentSpec{valid: true}
	}
	for index := 1; index < len(argument); index++ {
		option := argument[index]
		valueMode, known := makeShortOptionArgumentMode(option)
		if !known {
			return makeArgumentSpec{}
		}
		if valueMode == makeOptionArgumentNone {
			continue
		}
		return makeArgumentSpec{
			option:      true,
			valid:       true,
			valueMode:   valueMode,
			directory:   option == 'C',
			valueInline: index+1 < len(argument),
			inlineValue: argument[index+1:],
		}
	}
	return makeArgumentSpec{option: true, valid: true}
}

func malformedShellLineRelevant(line, workingDir string, patterns parserPatterns) bool {
	for _, segment := range shellDiagnosticSegments(line) {
		arguments, _ := splitMakeCommand(segment)
		commandIndex := shellExecutableIndex(arguments)
		if commandIndex < len(arguments) && arguments[commandIndex] == "cd" ||
			isMakeExecutableFromArguments(arguments) ||
			commandContainsCompiler(segment, arguments, workingDir, patterns) ||
			compilerBacktickCandidate(segment, workingDir) {
			return true
		}
	}
	return false
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
		flush()
		return arguments, false
	}
	flush()
	return arguments, true
}

func makeCommandDirectory(line, workingDir string) (string, bool) {
	arguments, ok := splitMakeCommand(line)
	if !ok {
		return "", false
	}
	return makeCommandDirectoryFromArguments(arguments, workingDir)
}

func makeCommandDirectoryFromArguments(arguments []string, workingDir string) (string, bool) {
	commandIndex := shellExecutableIndex(arguments)
	if commandIndex >= len(arguments) || !isMakeExecutable(arguments[commandIndex]) {
		return "", false
	}

	directory := workingDir
	windowsContext := runtime.GOOS == "windows" || executableBase(arguments[commandIndex]) == "mingw32-make" ||
		isExplicitWindowsAbsolutePath(workingDir)
	found := false
	for i := commandIndex + 1; i < len(arguments); i++ {
		spec := classifyMakeArgument(arguments[i])
		if !spec.valid {
			return "", false
		}
		if spec.stop {
			break
		}
		if spec.valueMode == makeOptionArgumentNone ||
			spec.valueMode == makeOptionArgumentOptionalAttached && !spec.valueInline {
			continue
		}
		value := spec.inlineValue
		if !spec.valueInline {
			if i+1 >= len(arguments) {
				return "", false
			}
			i++
			value = arguments[i]
		}
		if !spec.directory {
			continue
		}
		if value == "" {
			return "", false
		}
		windowsContext = windowsContext || isExplicitWindowsAbsolutePath(value)
		directory = joinTrackedPathWithWindowsMode(directory, value, windowsContext)
		found = true
	}
	return directory, found
}

func makeVirtualDirectories(arguments []string, workingDir string) ([]string, bool) {
	commandIndex := shellExecutableIndex(arguments)
	if commandIndex >= len(arguments) || executableBase(arguments[commandIndex]) != "mkdir" {
		return nil, false
	}
	parents := false
	directories := []string{}
	options := true
	for _, argument := range arguments[commandIndex+1:] {
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
		directories = append(directories, joinTrackedPath(workingDir, argument))
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

func makeDirectoryEvent(line, event, makeCommand string) (string, bool) {
	marker := ": " + event + " directory "
	index := strings.Index(line, marker)
	if index < 0 || !isMakeDirectoryMarkerPrefix(strings.TrimSpace(line[:index]), makeCommand) {
		return "", false
	}
	value := strings.TrimSpace(line[index+len(marker):])
	return makeDirectoryMarkerValue(value)
}

func makeDirectoryMarkerValue(value string) (string, bool) {
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

func isMakeDirectoryMarkerPrefix(prefix, makeCommand string) bool {
	if strings.HasSuffix(prefix, "]") {
		open := strings.LastIndexByte(prefix, '[')
		if open < 0 || open == len(prefix)-2 {
			return false
		}
		for _, character := range prefix[open+1 : len(prefix)-1] {
			if character < '0' || character > '9' {
				return false
			}
		}
		prefix = prefix[:open]
	}
	if strings.Contains(prefix, `\`) && !isRawWindowsAbsolutePathToken(prefix) {
		return false
	}
	base := executableBase(prefix)
	return isMakeExecutable(prefix) || strings.HasSuffix(base, "-make") ||
		makeCommand != "" && base == executableBase(makeCommand)
}

func (t *Tool) expandNestedCommands(line, workingDir string) (string, bool, *shellTokenizationError) {
	for {
		expanded, found, ok, parseErr := t.expandNextNestedCommand(line, workingDir)
		if !ok {
			return "", false, parseErr
		}
		if !found {
			return line, true, nil
		}
		line = expanded
	}
}

func (t *Tool) expandNextNestedCommand(line, workingDir string) (string, bool, bool, *shellTokenizationError) {
	var quote byte
	escaped := false
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
				var stdout bytes.Buffer
				var stderr bytes.Buffer
				err := runShellProgram(t.operationContext(), line[start+1:i], workingDir, &stdout, &stderr)
				if err != nil {
					t.Logger.Error("Error executing nested command:", err)
					return "", true, false, nil
				}
				output := strings.TrimRight(stdout.String(), "\n")
				replacement := ""
				if quote == '"' {
					replacement = quoteDoubleQuotedSubstitution(output)
				} else {
					var result strings.Builder
					fields := strings.Fields(output)
					for fieldIndex, field := range fields {
						if fieldIndex > 0 {
							result.WriteByte(' ')
						}
						result.WriteString(quotePOSIXShellArgument(field))
					}
					replacement = result.String()
				}
				return line[:start] + replacement + line[i+1:], true, true, nil
			}
		}
		if i >= len(line) {
			return "", true, false, &shellTokenizationError{offset: start, reason: "unterminated backtick"}
		}
	}
	return line, false, true, nil
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

func isRawWindowsAbsolutePathToken(argument string) bool {
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
		if isRawWindowsAbsolutePathToken(value) {
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
		case isRawWindowsAbsolutePathToken(rawArgument):
			normalized = windowsPathToSlash(rawArgument)
		case windowsContext && i <= invocation.compilerIndex && windowsRelativePathToken(rawArgument):
			normalized = windowsPathToSlash(rawArgument)
		case attachedPathOptionValue(rawArgument) != "":
			prefix := pathOptionPrefix(rawArgument)
			value := attachedPathOptionValue(rawArgument)
			if isRawWindowsAbsolutePathToken(value) || windowsContext && windowsRelativePathToken(value) {
				normalized = prefix + windowsPathToSlash(value)
			}
		default:
			if windowsContext && i > 0 && compilerPathOptionOperand(rawArguments[i-1]) && windowsRelativePathToken(rawArgument) {
				normalized = windowsPathToSlash(rawArgument)
			} else if windowsContext {
				_, input := inputs[i]
				if input && hasSourceExtension(windowsPathToSlash(rawArgument)) && windowsRelativePathToken(rawArgument) {
					normalized = windowsPathToSlash(rawArgument)
				}
			}
		}
		if normalized != "" && separatorlessPathRestorationKey(arguments[i]) == separatorlessPathRestorationKey(normalized) {
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
	case "time":
		return commandAfterTime(arguments, index+1, fullPathSafe, launcherWorkingDir)
	case "nice":
		return commandAfterNice(arguments, index+1, fullPathSafe, launcherWorkingDir)
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
				return nestedCompilerStart(arguments, index, fullPathSafe, launcherWorkingDir)
			case argument == "-C" || argument == "--chdir":
				if index+1 >= len(arguments) {
					return defaultCompilerLocation{}
				}
				launcherWorkingDir = joinTrackedPath(launcherWorkingDir, arguments[index+1])
				index += 2
			case strings.HasPrefix(argument, "-C") && len(argument) > 2:
				launcherWorkingDir = joinTrackedPath(launcherWorkingDir, argument[2:])
				index++
			case strings.HasPrefix(argument, "--chdir="):
				launcherWorkingDir = joinTrackedPath(launcherWorkingDir, strings.TrimPrefix(argument, "--chdir="))
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
				return nestedCompilerStart(arguments, index, fullPathSafe, launcherWorkingDir)
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

func commandAfterTime(arguments []string, index int, fullPathSafe bool, workingDir string) defaultCompilerLocation {
	for index < len(arguments) {
		argument := arguments[index]
		switch {
		case argument == "--":
			return nestedCompilerStart(arguments, index+1, fullPathSafe, workingDir)
		case argument == "-p" || argument == "--portability" || argument == "-a" || argument == "--append" ||
			argument == "-v" || argument == "--verbose" || argument == "-q" || argument == "--quiet":
			index++
		case argument == "-f" || argument == "--format" || argument == "-o" || argument == "--output":
			if index+1 >= len(arguments) {
				return defaultCompilerLocation{}
			}
			index += 2
		case strings.HasPrefix(argument, "-f") && len(argument) > 2 || strings.HasPrefix(argument, "--format=") ||
			strings.HasPrefix(argument, "-o") && len(argument) > 2 || strings.HasPrefix(argument, "--output="):
			index++
		case strings.HasPrefix(argument, "-"):
			return defaultCompilerLocation{}
		default:
			return nestedCompilerStart(arguments, index, fullPathSafe, workingDir)
		}
	}
	return defaultCompilerLocation{}
}

func commandAfterNice(arguments []string, index int, fullPathSafe bool, workingDir string) defaultCompilerLocation {
	for index < len(arguments) {
		argument := arguments[index]
		switch {
		case argument == "--":
			return nestedCompilerStart(arguments, index+1, fullPathSafe, workingDir)
		case argument == "-n" || argument == "--adjustment":
			if index+1 >= len(arguments) {
				return defaultCompilerLocation{}
			}
			index += 2
		case strings.HasPrefix(argument, "--adjustment=") || isNiceAdjustment(argument):
			index++
		case strings.HasPrefix(argument, "-"):
			return defaultCompilerLocation{}
		default:
			return nestedCompilerStart(arguments, index, fullPathSafe, workingDir)
		}
	}
	return defaultCompilerLocation{}
}

func isNiceAdjustment(argument string) bool {
	if len(argument) < 2 || argument[0] != '-' {
		return false
	}
	for _, character := range argument[1:] {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}

func nestedCompilerStart(arguments []string, index int, fullPathSafe bool, workingDir string) defaultCompilerLocation {
	if index >= len(arguments) {
		return defaultCompilerLocation{}
	}
	location := defaultCompilerStart(arguments[index:], workingDir)
	if !location.valid {
		return defaultCompilerLocation{}
	}
	location.index += index
	location.launcher = true
	location.fullPathSafe = fullPathSafe && location.fullPathSafe
	return location
}

func compilerMatchesPatterns(compiler string, patterns parserPatterns) bool {
	return patterns.compile.MatchString(compiler)
}

func commandContainsCompiler(command string, arguments []string, workingDir string, patterns parserPatterns) bool {
	if patterns.defaultCompile {
		location := defaultCompilerStart(arguments, workingDir)
		if !location.valid {
			return false
		}
		invocation := parseCompilerInvocation(arguments[location.index:])
		return invocation.valid && compilerMatchesPatterns(invocation.compiler, patterns)
	}
	return patterns.compile.MatchString(command)
}

func compilerBacktickCandidate(command, workingDir string) bool {
	start := strings.Index(command, "`")
	if start < 0 {
		return false
	}
	prefix := command[:start] + "__compiledb_compiler_candidate__"
	arguments, ok := splitMakeCommand(prefix)
	if !ok {
		return false
	}
	location := defaultCompilerStart(arguments, workingDir)
	if !location.valid || location.index >= len(arguments) {
		return false
	}
	invocation := parseCompilerInvocation(arguments[location.index:])
	return invocation.valid && invocation.compiler == "__compiledb_compiler_candidate__"
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
			directory = joinTrackedPath(workingDir, arguments[i+1])
			arguments[i+1] = directory
			i++
			continue
		}
		if value, ok := strings.CutPrefix(argument, "-working-directory="); ok {
			directory = joinTrackedPath(workingDir, value)
			arguments[i] = "-working-directory=" + directory
			continue
		}
		if compilerOptionTakesArgument(argument) {
			i++
		}
	}
	return directory
}

func (t *Tool) processCompileCommand(command string, workingDir string, line int, patterns parserPatterns) []parsedCompileCommand {
	arguments := t.splitArgs(command, line, workingDir)
	if len(arguments) == 0 {
		return nil
	}

	rawArguments, _ := splitMakeCommand(command)
	findCompile := false
	launcherPrefix := false
	fullPathSafe := true
	compilerWordIndex := -1
	compilerWorkingDir := workingDir
	if patterns.defaultCompile {
		if location := defaultCompilerStart(arguments, workingDir); location.valid {
			candidate := arguments[location.index:]
			invocation := parseCompilerInvocation(candidate)
			if location.index < len(rawArguments) {
				restoreWindowsArguments(candidate, rawArguments[location.index:], invocation)
				invocation = parseCompilerInvocation(candidate)
			}
			if invocation.valid && compilerMatchesPatterns(invocation.compiler, patterns) {
				findCompile = true
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
			t.Logger.Debugf("found compile:%s, but not found file, ignore command", arguments[0])
			return nil
		}
		files = []string{slashPath(filePath)}
	}
	arguments = insertCompilerArguments(arguments, invocation, t.Config.AddArgs)
	entryDirectory := applyCompilerWorkingDirectory(arguments, invocation, compilerWorkingDir)

	filteredFiles := make([]string, 0, len(files))
	for _, sourceFile := range files {
		excluded := false
		for _, exclude := range patterns.exclude {
			location := exclude.FindStringIndex(sourceFile)
			if location != nil && location[0] == 0 {
				excluded = true
				break
			}
		}
		if excluded {
			t.Logger.Infof("file %s exclude", sourceFile)
			continue
		}
		if !t.Config.NoStrict {
			fileFullPath := sourceFile
			windowsContext := runtime.GOOS == "windows" || isExplicitWindowsAbsolutePath(entryDirectory) || isExplicitWindowsAbsolutePath(sourceFile)
			normalizedSource := sourceFile
			if windowsContext {
				normalizedSource = windowsPathToSlash(normalizedSource)
			}
			if !strings.HasPrefix(normalizedSource, "/") && !(windowsContext && isExplicitWindowsAbsolutePath(normalizedSource)) {
				fileFullPath = joinTrackedPathWithWindowsMode(entryDirectory, sourceFile, windowsContext)
			}
			if err := strictSourceFile(fileFullPath); err != nil {
				t.Logger.Warnf("skip source %s: %v", fileFullPath, err)
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
			arguments[invocation.compilerIndex] = hostPathToDatabasePath(compileFullPath)
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
	} else if !isStdinInput(t.Config.InputFile) {
		absPath, _ := filepath.Abs(t.Config.InputFile)
		workingDir = filepath.Dir(absPath)
	} else {
		workingDir, _ = os.Getwd()
	}
	workingDir = trackedPathToSlash(workingDir)
	t.Logger.Infof("workingDir: %s", workingDir)

	// Compile parser regexes {{{
	patterns, err := compilePatterns(t.Config)
	if err != nil {
		t.Logger.Fatalln("invalid parser regex:", err)
		return
	}

	dirStack := []directoryFrame{{path: workingDir}}
	virtualDirectories := make(map[string]struct{})

	logicalLines, lineIssues := mergeLogicalLines(buildLog)
	for _, issue := range lineIssues {
		t.Logger.Errorf("skip build log line %d: %s", issue.line, issue.reason)
	}
	for _, logicalLine := range logicalLines {
		line := logicalLine.text
		lineNumber := logicalLine.line
		if t.operationContext().Err() != nil {
			t.StatusCode = contextExitCode(t.operationContext())
			return
		}
		t.Logger.Debug("New command:", line)

		commandList, commandListErr := parseShellCommandList(line)
		makeCommand := t.Config.MakeCommand
		if makeCommand == "" {
			makeCommand = makePath
		}

		// Track make-reported directory changes {{{
		markerLine := compatibleMakeDirectoryMarker(commandList, commandListErr, line, makeCommand)
		if directory, ok := makeDirectoryEvent(markerLine, "Entering", makeCommand); ok {
			enterDir := cleanTrackedPath(directory)
			if len(dirStack) > 0 && dirStack[0].provisional {
				dirStack[0] = directoryFrame{path: enterDir}
			} else {
				dirStack = append([]directoryFrame{{path: enterDir}}, dirStack...)
			}
			workingDir = dirStack[0].path
			t.Logger.Infof("entering change workingDir: %s", workingDir)
			continue
		} else if directory, ok := makeDirectoryEvent(markerLine, "Leaving", makeCommand); ok {
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
		if commandListErr != nil {
			if malformedShellLineRelevant(line, workingDir, patterns) {
				parseErr := shellLexicalError(line)
				if parseErr == nil {
					parseErr = shellParseError(commandListErr)
				}
				t.logTokenizationFailure(lineNumber, workingDir, parseErr)
			}
			continue
		}
		if !supportedShellCommandList(commandList, makeCommand) {
			t.Logger.Debugf("skip unsupported shell structure: %s", line)
			continue
		}

		lineWorkingDir := workingDir
		pendingMakeDir := ""
		pendingMakeSafe := false
		pendingMakeGeneration := 0
		summarizeCommand := func(commandText, commandName string, staticName bool) shellCommandSummary {
			arguments, ok := splitMakeCommand(commandText)
			if !ok {
				return shellCommandSummary{statuses: shellStatusEither, changesState: true}
			}
			commandIndex := shellExecutableIndex(arguments)
			if commandIndex >= len(arguments) {
				return shellCommandSummary{statuses: shellStatusEither, changesState: true}
			}
			status := shellStatusEither
			if staticName && (commandName == "true" || commandName == ":") {
				status = shellStatusMaySucceed
			} else if staticName && commandName == "false" {
				status = shellStatusMayFail
			}
			changesState := !staticName || commandName == "cd"
			if !changesState && isMakeExecutable(commandName) {
				arguments[commandIndex] = commandName
				_, changesState = makeCommandDirectoryFromArguments(arguments, lineWorkingDir)
			}
			if !changesState && t.makeDirectoryMarkers && executableBase(commandName) == "mkdir" {
				arguments[commandIndex] = commandName
				_, changesState = makeVirtualDirectories(arguments, lineWorkingDir)
			}
			return shellCommandSummary{statuses: status, changesState: changesState}
		}
		processCommand := func(commandText, commandName string, staticName bool) shellCommandResult {
			originalCommandText := commandText
			failureResult := func() shellCommandResult {
				summary := summarizeCommand(originalCommandText, commandName, staticName)
				return shellCommandResult{status: shellStatusUnknown, safe: staticName && !summary.changesState}
			}
			rawArguments, rawOK := splitMakeCommand(commandText)
			needsExpansion := false
			compilerCandidateExpansion := false
			if patterns.defaultCompile {
				needsExpansion = rawOK && commandContainsCompiler(commandText, rawArguments, lineWorkingDir, patterns)
				compilerCandidateExpansion = !needsExpansion && compilerBacktickCandidate(commandText, lineWorkingDir)
			} else {
				needsExpansion = patterns.compile.MatchString(commandText)
			}
			if rawOK {
				commandIndex := shellExecutableIndex(rawArguments)
				if commandIndex < len(rawArguments) {
					needsExpansion = needsExpansion || staticName && (commandName == "cd" ||
						isMakeExecutable(commandName) ||
						t.makeDirectoryMarkers && executableBase(commandName) == "mkdir")
				}
			}
			if compilerCandidateExpansion {
				var found bool
				var ok bool
				var parseErr *shellTokenizationError
				commandText, found, ok, parseErr = t.expandNextNestedCommand(commandText, lineWorkingDir)
				if !ok {
					if parseErr != nil {
						t.logTokenizationFailure(lineNumber, lineWorkingDir, parseErr)
					}
					return shellCommandResult{status: shellStatusUnknown, safe: true}
				}
				if !found {
					return failureResult()
				}
				candidateArguments, parsed := splitMakeCommand(commandText)
				if !parsed {
					if commandContainsCompiler(commandText, candidateArguments, lineWorkingDir, patterns) {
						parseErr := shellLexicalError(originalCommandText)
						if parseErr != nil {
							t.logTokenizationFailure(lineNumber, lineWorkingDir, parseErr)
						} else {
							t.splitArgs(commandText, lineNumber, lineWorkingDir)
						}
					}
					return failureResult()
				}
				if !commandContainsCompiler(commandText, candidateArguments, lineWorkingDir, patterns) {
					return failureResult()
				}
				candidateIndex := shellExecutableIndex(candidateArguments)
				if candidateIndex >= len(candidateArguments) {
					return failureResult()
				}
				commandName = candidateArguments[candidateIndex]
				staticName = true
				needsExpansion = true
			}
			if needsExpansion && strings.Contains(commandText, "`") {
				var ok bool
				var parseErr *shellTokenizationError
				commandText, ok, parseErr = t.expandNestedCommands(commandText, lineWorkingDir)
				if !ok {
					if parseErr != nil {
						t.logTokenizationFailure(lineNumber, lineWorkingDir, parseErr)
					}
					return failureResult()
				}
			}
			arguments, ok := splitMakeCommand(commandText)
			if !ok {
				commandIndex := shellExecutableIndex(arguments)
				if commandIndex < len(arguments) && arguments[commandIndex] == "cd" ||
					isMakeExecutableFromArguments(arguments) ||
					commandContainsCompiler(commandText, arguments, lineWorkingDir, patterns) {
					t.splitArgs(commandText, lineNumber, lineWorkingDir)
				}
				return failureResult()
			}
			commandIndex := shellExecutableIndex(arguments)
			if commandIndex >= len(arguments) {
				return failureResult()
			}
			if staticName {
				arguments[commandIndex] = commandName
			}
			commandArguments := arguments[commandIndex:]
			if t.makeDirectoryMarkers && staticName && executableBase(commandName) == "mkdir" {
				if directories, recognized := makeVirtualDirectories(arguments, lineWorkingDir); recognized {
					for _, directory := range directories {
						virtualDirectories[cleanTrackedPath(directory)] = struct{}{}
					}
					return shellCommandResult{status: shellStatusSuccess, safe: true}
				}
			}
			if staticName && commandName == "cd" {
				if len(commandArguments) != 2 {
					return shellCommandResult{status: shellStatusUnknown, safe: false}
				}
				nextDir := joinTrackedPath(lineWorkingDir, commandArguments[1])
				if t.Config.NoStrict {
					lineWorkingDir = nextDir
					return shellCommandResult{status: shellStatusSuccess, safe: true}
				}
				info, err := os.Stat(nextDir)
				virtual := hasVirtualDirectory(virtualDirectories, nextDir)
				if (err != nil || !info.IsDir()) && !(t.makeDirectoryMarkers && virtual) {
					return shellCommandResult{status: shellStatusFailure, safe: true}
				}
				lineWorkingDir = nextDir
				t.Logger.Infof("Temporarily change workingDir: %s", lineWorkingDir)
				return shellCommandResult{status: shellStatusSuccess, safe: true}
			}

			if enterDir, ok := makeCommandDirectoryFromArguments(arguments, lineWorkingDir); staticName &&
				isMakeExecutable(commandName) && ok {
				pendingMakeDir = enterDir
				pendingMakeSafe = true
				pendingMakeGeneration++
			}

			if !commandContainsCompiler(commandText, arguments, lineWorkingDir, patterns) {
				if staticName && isMakeExecutable(commandName) {
					return shellCommandResult{status: shellStatusUnknown, safe: true}
				}
				if staticName && (commandName == "true" || commandName == ":") {
					return shellCommandResult{status: shellStatusSuccess, safe: true}
				}
				if staticName && commandName == "false" {
					return shellCommandResult{status: shellStatusFailure, safe: true}
				}
				return shellCommandResult{status: shellStatusUnknown, safe: staticName}
			}
			for _, parsed := range t.processCompileCommand(commandText, lineWorkingDir, lineNumber, patterns) {
				command := ShellJoinArgs(parsed.arguments)
				if t.Config.CommandStyle {
					result = append(result, Command{Directory: parsed.directory, Command: command, File: parsed.filePath})
				} else {
					result = append(result, Command{Directory: parsed.directory, Arguments: parsed.arguments, File: parsed.filePath})
				}
				t.Logger.Infof("Adding command %d: %s", cmdCnt, command)
				cmdCnt++
			}
			return shellCommandResult{status: shellStatusUnknown, safe: true}
		}
		statementPendingGeneration := pendingMakeGeneration
		lineSafe := evaluateShellCommandList(commandList, line, makeCommand, processCommand, summarizeCommand, func() {
			if pendingMakeGeneration != statementPendingGeneration {
				pendingMakeSafe = false
			}
		}, func(shellEvaluation) {
			statementPendingGeneration = pendingMakeGeneration
		})
		if !lineSafe {
			pendingMakeSafe = false
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
