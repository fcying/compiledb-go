package internal

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/bits"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"
)

const (
	maxResponseFileSize      = 8 * 1024 * 1024
	maxResponseFileBytes     = 32 * 1024 * 1024
	maxResponseFileDepth     = 32
	maxResponseFiles         = 256
	maxResponseFileArguments = 100000
	maxResponseFileOutput    = 64 * 1024 * 1024
)

var defaultResponseFileCompiler = regexp.MustCompile(RegexCompile)

type responseFileError struct {
	filename string
	reason   string
	offset   int
}

func (e *responseFileError) Error() string {
	if e.filename == "" {
		return "response file expansion: " + e.reason
	}
	if e.offset >= 0 {
		return fmt.Sprintf("response file %q: %s at byte %d", e.filename, e.reason, e.offset)
	}
	return fmt.Sprintf("response file %q: %s", e.filename, e.reason)
}

type responseFileExpansion struct {
	tool        *Tool
	workingDir  string
	active      []os.FileInfo
	totalBytes  int64
	fileCount   int
	argumentCnt int
	tokenCnt    int
	outputBytes int64
}

func responseFilePath(argument string) (string, bool) {
	if len(argument) <= 1 || argument[0] != '@' {
		return "", false
	}
	return argument[1:], true
}

func supportsGNUResponseFiles(arguments []string, invocation compilerInvocation) bool {
	if !invocation.valid || !defaultResponseFileCompiler.MatchString(invocation.compiler) {
		return false
	}

	compiler := executableBase(invocation.compiler)
	if strings.Contains(compiler, "clang-cl") {
		return false
	}
	if !strings.Contains(compiler, "clang") {
		return true
	}

	driverMode := ""
	rspQuoting := ""
	for _, argument := range arguments[invocation.optionsStart:] {
		if value, ok := strings.CutPrefix(argument, "--driver-mode="); ok {
			driverMode = value
		}
		if value, ok := strings.CutPrefix(argument, "--rsp-quoting="); ok {
			rspQuoting = value
		}
	}
	return driverMode != "cl" && (rspQuoting == "" || rspQuoting == "posix")
}

func hasGNUResponseFiles(arguments []string, invocation compilerInvocation) bool {
	if !supportsGNUResponseFiles(arguments, invocation) {
		return false
	}
	for _, argument := range arguments[invocation.optionsStart:] {
		if _, ok := responseFilePath(argument); ok {
			return true
		}
	}
	return false
}

func (t *Tool) expandCompilerResponseFiles(arguments []string, invocation compilerInvocation, workingDir string) ([]string, error) {
	if !hasGNUResponseFiles(arguments, invocation) {
		return arguments, nil
	}

	expansion := responseFileExpansion{tool: t, workingDir: workingDir}
	result := make([]string, 0, len(arguments))
	for _, argument := range arguments[:invocation.optionsStart] {
		if err := expansion.appendArgument(&result, argument, ""); err != nil {
			return nil, err
		}
	}
	for _, argument := range arguments[invocation.optionsStart:] {
		if err := expansion.expandArgument(&result, argument, 0, ""); err != nil {
			return nil, err
		}
	}
	if clangCLDriverMode(result, invocation) {
		return nil, &responseFileError{reason: "Clang CL driver mode is not supported", offset: -1}
	}
	return result, nil
}

func responseFileEntriesLimit(arguments []string, entries int) error {
	if entries <= 0 {
		return nil
	}
	// Each entry copies the string headers, while JSON output repeats the argument bytes.
	entryBytes := int64(len(arguments) * (bits.UintSize / 4))
	for _, argument := range arguments {
		entryBytes += int64(len(argument) + 1)
	}
	if entryBytes > maxResponseFileOutput/int64(entries) {
		return &responseFileError{
			reason: fmt.Sprintf("expanded entries exceed output limit %d bytes", maxResponseFileOutput),
			offset: -1,
		}
	}
	return nil
}

func clangCLDriverMode(arguments []string, invocation compilerInvocation) bool {
	if !strings.Contains(executableBase(invocation.compiler), "clang") {
		return false
	}
	driverMode := ""
	for _, argument := range arguments[invocation.optionsStart:] {
		if value, ok := strings.CutPrefix(argument, "--driver-mode="); ok {
			driverMode = value
		}
	}
	return driverMode == "cl"
}

func (e *responseFileExpansion) expandArgument(result *[]string, argument string, depth int, sourceFilename string) error {
	filename, response := responseFilePath(argument)
	if !response {
		return e.appendArgument(result, argument, sourceFilename)
	}
	return e.expandFile(result, filename, depth+1)
}

func (e *responseFileExpansion) expandFile(result *[]string, filename string, depth int) error {
	if err := e.tool.operationContext().Err(); err != nil {
		return context.Cause(e.tool.operationContext())
	}
	trackedFilename := joinTrackedPath(e.workingDir, filename)
	fail := func(reason string, offset int) error {
		return &responseFileError{filename: trackedFilename, reason: reason, offset: offset}
	}
	if depth > maxResponseFileDepth {
		return fail(fmt.Sprintf("expansion exceeds depth limit %d", maxResponseFileDepth), -1)
	}
	if e.fileCount >= maxResponseFiles {
		return fail(fmt.Sprintf("expansion exceeds file limit %d", maxResponseFiles), -1)
	}
	if len(trackedFilename) >= 3 && isASCIIAlpha(trackedFilename[0]) && trackedFilename[1] == ':' && trackedFilename[2] == '/' && filepath.Separator != '\\' {
		return fail("path is not accessible on this host", -1)
	}

	hostFilename := filepath.FromSlash(trackedFilename)
	file, err := openResponseFile(hostFilename)
	if err != nil {
		return fail("cannot open file: "+responseFileIOError(err), -1)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return fail("cannot stat file: "+responseFileIOError(err), -1)
	}
	if !info.Mode().IsRegular() {
		return fail("not a regular file", -1)
	}
	if info.Size() > maxResponseFileSize {
		return fail(fmt.Sprintf("file exceeds size limit %d bytes", maxResponseFileSize), -1)
	}
	for _, active := range e.active {
		if os.SameFile(active, info) {
			return fail("recursive expansion", -1)
		}
	}

	e.fileCount++
	e.active = append(e.active, info)
	defer func() { e.active = e.active[:len(e.active)-1] }()

	data, err := readFileWithContextLimit(e.tool.operationContext(), file, maxResponseFileSize)
	if err != nil {
		if e.tool.operationContext().Err() != nil {
			return context.Cause(e.tool.operationContext())
		}
		if errors.Is(err, errReadLimitExceeded) {
			return fail(fmt.Sprintf("file exceeds size limit %d bytes", maxResponseFileSize), -1)
		}
		return fail("cannot read file: "+responseFileIOError(err), -1)
	}
	e.totalBytes += int64(len(data))
	if e.totalBytes > maxResponseFileBytes {
		return fail(fmt.Sprintf("expansion exceeds total size limit %d bytes", maxResponseFileBytes), -1)
	}

	remainingTokens := maxResponseFileArguments - e.tokenCnt
	arguments, parseErr := splitGNUResponseFile(data, remainingTokens)
	if parseErr != nil {
		return fail(parseErr.reason, parseErr.offset)
	}
	e.tokenCnt += len(arguments)
	for _, argument := range arguments {
		if err := e.expandArgument(result, argument, depth, trackedFilename); err != nil {
			return err
		}
	}
	return nil
}

func (e *responseFileExpansion) appendArgument(result *[]string, argument, filename string) error {
	e.argumentCnt++
	e.outputBytes += int64(len(argument) + 1)
	if e.argumentCnt > maxResponseFileArguments {
		return &responseFileError{filename: filename, reason: fmt.Sprintf("expansion exceeds argument limit %d", maxResponseFileArguments), offset: -1}
	}
	if e.outputBytes > maxResponseFileOutput {
		return &responseFileError{filename: filename, reason: fmt.Sprintf("expanded arguments exceed output limit %d bytes", maxResponseFileOutput), offset: -1}
	}
	*result = append(*result, argument)
	return nil
}

func responseFileIOError(err error) string {
	var pathErr *os.PathError
	if errors.As(err, &pathErr) {
		return pathErr.Err.Error()
	}
	return err.Error()
}

func splitGNUResponseFile(data []byte, maxArguments int) ([]string, *shellTokenizationError) {
	if bytes.HasPrefix(data, []byte{0xef, 0xbb, 0xbf}) || bytes.HasPrefix(data, []byte{0xff, 0xfe}) || bytes.HasPrefix(data, []byte{0xfe, 0xff}) {
		return nil, &shellTokenizationError{offset: 0, reason: "byte order mark is not supported"}
	}
	if offset := bytes.IndexByte(data, 0); offset >= 0 {
		return nil, &shellTokenizationError{offset: offset, reason: "NUL byte is not supported"}
	}
	if !utf8.Valid(data) {
		return nil, &shellTokenizationError{offset: firstInvalidUTF8(data), reason: "invalid UTF-8"}
	}

	arguments := []string{}
	var token strings.Builder
	var quote byte
	quoteStart := 0
	tokenStarted := false
	tokenStart := 0
	flush := func(offset int) *shellTokenizationError {
		if !tokenStarted {
			return nil
		}
		if len(arguments) >= maxArguments {
			return &shellTokenizationError{offset: tokenStart, reason: fmt.Sprintf("expansion exceeds argument limit %d", maxResponseFileArguments)}
		}
		arguments = append(arguments, token.String())
		token.Reset()
		tokenStarted = false
		return nil
	}

	for i := 0; i < len(data); i++ {
		character := data[i]
		if quote != 0 {
			if character == quote {
				quote = 0
				continue
			}
			if character == '\\' {
				if i+1 >= len(data) {
					return nil, &shellTokenizationError{offset: i, reason: "trailing escape"}
				}
				i++
				token.WriteByte(data[i])
				continue
			}
			token.WriteByte(character)
			continue
		}

		switch character {
		case '\'', '"':
			if !tokenStarted {
				tokenStart = i
			}
			tokenStarted = true
			quote = character
			quoteStart = i
		case '\\':
			if i+1 >= len(data) {
				return nil, &shellTokenizationError{offset: i, reason: "trailing escape"}
			}
			if data[i+1] == '\n' || data[i+1] == '\r' {
				return nil, &shellTokenizationError{offset: i, reason: "escaped newline is not portable"}
			}
			if !tokenStarted {
				tokenStart = i
			}
			tokenStarted = true
			i++
			token.WriteByte(data[i])
		case ' ', '\t', '\r', '\n':
			if parseErr := flush(i); parseErr != nil {
				return nil, parseErr
			}
		default:
			if !tokenStarted {
				tokenStart = i
			}
			tokenStarted = true
			token.WriteByte(character)
		}
	}
	if quote != 0 {
		return nil, &shellTokenizationError{offset: quoteStart, reason: "unterminated quote"}
	}
	if parseErr := flush(len(data)); parseErr != nil {
		return nil, parseErr
	}
	return arguments, nil
}

func firstInvalidUTF8(data []byte) int {
	for offset := 0; offset < len(data); {
		_, size := utf8.DecodeRune(data[offset:])
		if size == 1 && data[offset] >= utf8.RuneSelf {
			return offset
		}
		offset += size
	}
	return 0
}

func (t *Tool) logResponseFileFailure(line int, workingDir string, err error) {
	t.Logger.Errorf("skip compiler command at build log line %d (cwd %q): %v", line, workingDir, err)
}
