package internal

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
)

var makePath = "make"

const makePipeWaitDelay = time.Second

func commandExitCode(err error) int {
	var exitErr *exec.ExitError
	if err == nil {
		return 0
	}
	var execErr *exec.Error
	if errors.Is(err, exec.ErrNotFound) || (errors.As(err, &execErr) && errors.Is(execErr.Err, os.ErrNotExist)) {
		return 127
	}
	if errors.As(err, &exitErr) {
		if code, ok := signaledExitCode(exitErr); ok {
			return code
		}
		if code := exitErr.ExitCode(); code >= 0 {
			return code
		}
		return 1
	}
	return 1
}

func checkMakeExecutable(filename string) error {
	info, err := os.Stat(filename)
	if err != nil {
		return err
	}
	if info.IsDir() || (runtime.GOOS != "windows" && info.Mode()&0o111 == 0) {
		return fs.ErrPermission
	}
	return nil
}

func makeExecutableCandidates(filename string, searchPath bool) []string {
	candidates := executableCandidates(filename, runtime.GOOS, os.Getenv("PATHEXT"))
	if runtime.GOOS != "windows" || !searchPath || filepath.Ext(filename) == "" || len(candidates) != 1 {
		return candidates
	}
	extensions := windowsExecutableExtensions(os.Getenv("PATHEXT"))
	for _, extension := range extensions {
		if strings.EqualFold(filepath.Ext(filename), extension) {
			candidates = make([]string, 1, len(extensions)+1)
			candidates[0] = filename
			for _, suffix := range extensions {
				candidates = append(candidates, filename+suffix)
			}
			break
		}
	}
	return candidates
}

func findMakeExecutable(filename string, searchPath bool) (string, error) {
	var firstError error
	for _, candidate := range makeExecutableCandidates(filename, searchPath) {
		if err := checkMakeExecutable(candidate); err != nil {
			if !errors.Is(err, os.ErrNotExist) && firstError == nil {
				firstError = err
			}
			continue
		}
		absolute, err := filepath.Abs(candidate)
		if err != nil {
			return "", err
		}
		return absolute, nil
	}
	if firstError != nil {
		return "", firstError
	}
	return "", exec.ErrNotFound
}

func resolveMakeExecutable(executable, workingDir string) (string, error) {
	baseDir := filepath.FromSlash(workingDir)
	if !filepath.IsAbs(baseDir) {
		absolute, err := filepath.Abs(baseDir)
		if err != nil {
			return "", err
		}
		baseDir = absolute
	}

	candidate := filepath.FromSlash(executable)
	driveRelative := runtime.GOOS == "windows" && filepath.VolumeName(candidate) != "" && !filepath.IsAbs(candidate)
	if driveRelative {
		return findMakeExecutable(candidate, false)
	}
	if strings.ContainsAny(executable, `/\`) {
		if !filepath.IsAbs(candidate) {
			candidate = filepath.Join(baseDir, candidate)
		}
		return findMakeExecutable(candidate, false)
	}

	for _, directory := range filepath.SplitList(os.Getenv("PATH")) {
		if directory == "" {
			if runtime.GOOS == "windows" {
				continue
			}
			directory = baseDir
		} else if !filepath.IsAbs(directory) {
			directory = filepath.Join(baseDir, directory)
		}
		if fullPath, err := findMakeExecutable(filepath.Join(directory, executable), true); err == nil {
			return fullPath, nil
		}
	}
	return "", exec.ErrNotFound
}

func (t *Tool) makeCommand(arguments ...string) *exec.Cmd {
	ctx := t.operationContext()
	executable := t.Config.MakeCommand
	if executable == "" {
		executable = makePath
	}
	fullPath, resolveErr := resolveMakeExecutable(executable, t.Config.BuildDir)
	var cmd *exec.Cmd
	if resolveErr == nil {
		cmd = exec.CommandContext(ctx, fullPath, arguments...)
	} else {
		cmd = exec.CommandContext(ctx, executable, arguments...)
		cmd.Err = resolveErr
	}
	configureMakeCommand(cmd, ctx)
	cmd.Dir = t.Config.BuildDir
	return cmd
}

func watchExitedProcessTree(ctx context.Context, process *os.Process) func() {
	stopped := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		select {
		case <-ctx.Done():
			_ = terminateProcessTree(process, ctx)
		case <-stopped:
			if ctx.Err() != nil {
				_ = terminateProcessTree(process, ctx)
			}
		}
	}()
	return func() {
		close(stopped)
		<-done
		releaseProcessTree(process)
	}
}

func runMakeCommand(ctx context.Context, cmd *exec.Cmd, stdout, stderr *os.File, encoding string) (error, func()) {
	stdoutRelay, err := startOutputRelay(stdout, encoding)
	if err != nil {
		return err, func() {}
	}
	stderrRelay := stdoutRelay
	if stderr != stdout {
		stderrRelay, err = startOutputRelay(stderr, encoding)
		if err != nil {
			stdoutRelay.stop()
			return err, func() {}
		}
	}
	cmd.Stdout = stdoutRelay.input
	cmd.Stderr = stderrRelay.input
	if err = startProcessCommand(cmd); err != nil {
		stdoutRelay.stop()
		if stderrRelay != stdoutRelay {
			stderrRelay.stop()
		}
		return err, func() {}
	}
	stopWatching := watchExitedProcessTree(ctx, cmd.Process)
	stdoutRelay.closeInput()
	if stderrRelay != stdoutRelay {
		stderrRelay.closeInput()
	}
	makeErr := waitStartedProcessCommand(cmd, ctx)
	stdoutErr, stderrErr := waitOutputRelays(ctx, stdoutRelay, stderrRelay)
	if stdoutErr != nil || stderrErr != nil {
		_ = cleanupExitedProcessTree(cmd.Process)
	}
	if makeErr != nil {
		return makeErr, stopWatching
	}
	if stdoutErr != nil || stderrErr != nil {
		return fmt.Errorf("make output incomplete: %w", errors.Join(stdoutErr, stderrErr)), stopWatching
	}
	return nil, stopWatching
}

func waitOutputRelays(ctx context.Context, stdoutRelay, stderrRelay *outputRelay) (error, error) {
	if stderrRelay == stdoutRelay {
		err := stdoutRelay.wait(ctx, makePipeWaitDelay)
		return err, err
	}
	type relayResult struct {
		stdout bool
		err    error
	}
	results := make(chan relayResult, 2)
	go func() { results <- relayResult{stdout: true, err: stdoutRelay.wait(ctx, makePipeWaitDelay)} }()
	go func() { results <- relayResult{err: stderrRelay.wait(ctx, makePipeWaitDelay)} }()
	var stdoutErr, stderrErr error
	for range 2 {
		result := <-results
		if result.stdout {
			stdoutErr = result.err
		} else {
			stderrErr = result.err
		}
	}
	return stdoutErr, stderrErr
}

func reportMakeOutputError(logger *logrus.Logger, err error) bool {
	if !errors.Is(err, exec.ErrWaitDelay) && !errors.Is(err, errProcessOutputIncomplete) {
		return false
	}
	logger.Errorf("make output incomplete: %v", err)
	return true
}

func loggerAtLevel(logger *logrus.Logger, level logrus.Level) *logrus.Logger {
	clone := logrus.New()
	clone.SetOutput(logger.Out)
	clone.SetFormatter(logger.Formatter)
	clone.ReplaceHooks(logger.Hooks)
	clone.SetReportCaller(logger.ReportCaller)
	clone.ExitFunc = logger.ExitFunc
	clone.SetBufferPool(logger.BufferPool)
	clone.SetLevel(level)
	return clone
}

type discoveryResult struct {
	status       int
	parserStatus int
	stopWatching func()
}

func (t *Tool) runDiscoveryMake(cmd *exec.Cmd) discoveryResult {
	var stdoutBuf, stderrBuf bytes.Buffer
	cmd.Stdout = &stdoutBuf
	cmd.Stderr = &stderrBuf
	err := waitProcessCommand(cmd, t.operationContext())
	result := discoveryResult{}
	if cmd.Process != nil {
		result.stopWatching = watchExitedProcessTree(t.operationContext(), cmd.Process)
		cleanupErr := cleanupExitedProcessTree(cmd.Process)
		if cleanupErr != nil && !errors.Is(cleanupErr, os.ErrProcessDone) && err == nil {
			err = cleanupErr
		}
	}
	if stderrBuf.Len() > 0 {
		if writeErr := writeFileWithContext(t.operationContext(), os.Stderr, stderrBuf.Bytes()); writeErr != nil && err == nil {
			err = writeErr
		}
	}
	if err != nil && !reportMakeOutputError(t.Logger, err) {
		if t.operationContext().Err() != nil {
			result.status = contextExitCode(t.operationContext())
		} else {
			result.status = commandExitCode(err)
		}
		t.Logger.Errorf("dry-run make failed: %v", err)
		return result
	}

	buildLog, scanErr := scanBuildLog(stdoutBuf.Bytes())
	if scanErr != nil {
		result.status = 1
		t.Logger.Errorf("read dry-run output failed: %v", scanErr)
		return result
	}
	clone := *t
	clone.makeDirectoryMarkers = true
	if !t.Config.NoBuild {
		clone.Logger = loggerAtLevel(t.Logger, logrus.ErrorLevel)
	}
	clone.Parse(buildLog)
	result.parserStatus = clone.StatusCode
	return result
}

func (t *Tool) MakeWrap(args []string) {
	buildArgs := append([]string(nil), args...)
	dryRunArgs := discoveryMakeArguments(args)
	usesStdin := makeUsesStdinMakefile(buildArgs) || makeUsesStdinMakefile(dryRunArgs) ||
		makeEnvironmentUsesStdinMakefile(os.Environ())
	var stdinData []byte
	if usesStdin {
		var err error
		stdinData, err = readFileWithContext(t.operationContext(), os.Stdin)
		if err != nil {
			t.Logger.Errorf("failed to read Make stdin: %v", err)
			if t.operationContext().Err() != nil {
				t.StatusCode = contextExitCode(t.operationContext())
			} else {
				t.StatusCode = 1
			}
			return
		}
	}

	var stopWatchingReal func()
	if !t.Config.NoBuild {
		cmd := t.makeCommand(buildArgs...)
		if usesStdin {
			cmd.Stdin = bytes.NewReader(stdinData)
		}
		makeStdout := os.Stdout
		if t.Config.OutputFile == "-" {
			makeStdout = os.Stderr
		}
		buildErr, watcher := runMakeCommand(t.operationContext(), cmd, makeStdout, os.Stderr, t.Config.Encoding)
		stopWatchingReal = watcher
		if buildErr != nil {
			if t.operationContext().Err() != nil {
				t.StatusCode = contextExitCode(t.operationContext())
				if stopWatchingReal != nil {
					stopWatchingReal()
				}
				return
			}
			if !errors.Is(buildErr, exec.ErrWaitDelay) && !errors.Is(buildErr, errProcessOutputIncomplete) {
				t.StatusCode = commandExitCode(buildErr)
				t.Logger.Errorf("make failed with status %d: %v", t.StatusCode, buildErr)
				if stopWatchingReal != nil {
					stopWatchingReal()
				}
				return
			}
			_ = reportMakeOutputError(t.Logger, buildErr)
		}
	}
	if stopWatchingReal != nil {
		defer stopWatchingReal()
	}

	dryRunCmd := t.makeCommand(dryRunArgs...)
	dryRunCmd.WaitDelay = makePipeWaitDelay
	cleanupProxy, proxyErr := configureDiscoveryMakeProxy(dryRunCmd)
	if proxyErr != nil {
		t.StatusCode = 1
		t.Logger.Errorf("configure recursive Make discovery failed: %v", proxyErr)
		return
	}
	defer cleanupProxy()
	dryRunCmd.Env = discoveryMakeEnvironment(dryRunCmd.Environ())
	if usesStdin {
		dryRunCmd.Stdin = bytes.NewReader(stdinData)
	}
	discovery := t.runDiscoveryMake(dryRunCmd)
	if discovery.stopWatching != nil {
		defer discovery.stopWatching()
	}
	if discovery.status != 0 {
		t.StatusCode = discovery.status
		return
	}
	if discovery.parserStatus != 0 {
		t.StatusCode = discovery.parserStatus
	}
}

func discoveryMakeArguments(arguments []string) []string {
	sanitized := sanitizeMakeArguments(arguments)
	return insertMakeOptions(sanitized, "-Bnkw", "-j1", "--print-directory")
}

func sanitizeMakeArguments(arguments []string) []string {
	sanitized := make([]string, 0, len(arguments)+6)
	options := true
	for i := 0; i < len(arguments); i++ {
		argument := arguments[i]
		if options && argument == "--" {
			options = false
			sanitized = append(sanitized, argument)
			continue
		}
		if options {
			if isConflictingLongMakeOption(argument) {
				continue
			}
			if makeLongOptionTakesArgument(argument) && !strings.Contains(argument, "=") {
				sanitized = append(sanitized, argument)
				if i+1 < len(arguments) {
					i++
					sanitized = append(sanitized, arguments[i])
				}
				continue
			}
			var takesNext bool
			argument, takesNext = removeMakeModeFlags(argument)
			if argument != "" {
				sanitized = append(sanitized, argument)
			}
			if takesNext && i+1 < len(arguments) {
				i++
				sanitized = append(sanitized, arguments[i])
			}
			if argument == "" || takesNext {
				continue
			}
		}
		if !options {
			sanitized = append(sanitized, argument)
		}
	}
	return sanitized
}

func removeMakeModeFlags(argument string) (string, bool) {
	if len(argument) < 2 || argument[0] != '-' || argument[1] == '-' {
		return argument, false
	}
	var result strings.Builder
	result.WriteByte('-')
	for i := 1; i < len(argument); i++ {
		option := argument[i]
		if option == 'q' || option == 't' || option == 'S' || option == 'p' || option == 'h' || option == 'v' {
			continue
		}
		result.WriteByte(option)
		if strings.ContainsRune("CEfIoW", rune(option)) {
			result.WriteString(argument[i+1:])
			return result.String(), i+1 == len(argument)
		}
		if strings.ContainsRune("jlO", rune(option)) && i+1 < len(argument) {
			result.WriteString(argument[i+1:])
			return result.String(), false
		}
	}
	if result.Len() == 1 {
		return "", false
	}
	return result.String(), false
}

func isConflictingLongMakeOption(argument string) bool {
	name, ok := canonicalMakeLongOption(argument)
	if !ok {
		return false
	}
	return name == "question" || name == "touch" || name == "no-keep-going" || name == "stop" ||
		name == "no-print-directory" || name == "print-data-base" || name == "help" || name == "version"
}

type makeOptionArgumentMode uint8

const (
	makeOptionArgumentNone makeOptionArgumentMode = iota
	makeOptionArgumentRequired
	makeOptionArgumentOptionalAttached
)

func makeLongOptionArgumentMode(argument string) makeOptionArgumentMode {
	name, ok := canonicalMakeLongOption(argument)
	if !ok {
		return makeOptionArgumentNone
	}
	switch name {
	case "directory", "file", "makefile", "include-dir", "eval", "old-file", "assume-old",
		"new-file", "assume-new", "what-if", "jobserver-auth", "jobserver-fds", "jobserver-style":
		return makeOptionArgumentRequired
	case "debug", "jobs", "load-average", "max-load", "output-sync", "shuffle":
		return makeOptionArgumentOptionalAttached
	default:
		return makeOptionArgumentNone
	}
}

func makeLongOptionTakesArgument(argument string) bool {
	return makeLongOptionArgumentMode(argument) == makeOptionArgumentRequired
}

func makeShortOptionArgumentMode(option byte) (makeOptionArgumentMode, bool) {
	switch {
	case strings.ContainsRune("CEfIoW", rune(option)):
		return makeOptionArgumentRequired, true
	case strings.ContainsRune("jlO", rune(option)):
		return makeOptionArgumentOptionalAttached, true
	case strings.ContainsRune("bmBdehikLnpqrRsStvw", rune(option)):
		return makeOptionArgumentNone, true
	default:
		return makeOptionArgumentNone, false
	}
}

func canonicalMakeLongOption(argument string) (string, bool) {
	if !strings.HasPrefix(argument, "--") {
		return "", false
	}
	name, _, _ := strings.Cut(strings.TrimPrefix(argument, "--"), "=")
	options := []string{
		"always-make", "assume-new", "assume-old", "check-symlink-times", "debug", "directory",
		"dry-run", "environment-overrides", "eval", "file", "help", "ignore-errors", "include-dir",
		"jobserver-auth", "jobserver-fds", "jobs", "just-print", "keep-going", "load-average", "makefile",
		"jobserver-style",
		"max-load", "new-file", "no-builtin-rules", "no-builtin-variables", "no-keep-going",
		"no-print-directory", "no-silent", "old-file", "output-sync", "print-data-base", "print-directory",
		"question", "quiet", "recon", "shuffle", "silent", "stop", "touch", "trace", "version",
		"warn-undefined-variables", "what-if",
	}
	for _, option := range options {
		if option == name {
			return option, true
		}
	}
	match := ""
	for _, option := range options {
		if strings.HasPrefix(option, name) {
			if match != "" {
				return "", false
			}
			match = option
		}
	}
	return match, match != ""
}

func makeUsesStdinMakefile(arguments []string) bool {
	options := true
	for i := 0; i < len(arguments); i++ {
		argument := arguments[i]
		if options && argument == "--" {
			options = false
			continue
		}
		if !options {
			continue
		}
		if name, ok := canonicalMakeLongOption(argument); ok && (name == "file" || name == "makefile") {
			_, value, attached := strings.Cut(argument, "=")
			if attached {
				return value == "-"
			}
			if i+1 < len(arguments) {
				i++
				if arguments[i] == "-" {
					return true
				}
			}
			continue
		}
		if len(argument) < 2 || argument[0] != '-' || argument[1] == '-' {
			continue
		}
		for position := 1; position < len(argument); position++ {
			option := argument[position]
			if option == 'f' {
				if position+1 < len(argument) {
					return argument[position+1:] == "-"
				}
				if i+1 < len(arguments) {
					i++
					if arguments[i] == "-" {
						return true
					}
				}
				break
			}
			if strings.ContainsRune("CEIoW", rune(option)) {
				if position+1 == len(argument) {
					i++
				}
				break
			}
			if strings.ContainsRune("jlO", rune(option)) && position+1 < len(argument) {
				break
			}
		}
	}
	return false
}

func makeEnvironmentUsesStdinMakefile(environment []string) bool {
	for _, value := range environment {
		name, flags, ok := strings.Cut(value, "=")
		if !ok || (name != "MAKEFLAGS" && name != "GNUMAKEFLAGS") {
			continue
		}
		arguments := splitMakeFlagArguments(flags)
		if len(arguments) > 0 && makeFlagHasImplicitCluster(arguments[0]) {
			arguments[0] = "-" + arguments[0]
		}
		if makeUsesStdinMakefile(arguments) {
			return true
		}
	}
	return false
}

func discoveryMakeEnvironment(environment []string) []string {
	result := make([]string, 0, len(environment))
	for _, value := range environment {
		name, flags, ok := strings.Cut(value, "=")
		if !ok || (name != "MAKEFLAGS" && name != "GNUMAKEFLAGS") {
			result = append(result, value)
			continue
		}
		arguments := splitMakeFlagArguments(flags)
		if len(arguments) > 0 && makeFlagHasImplicitCluster(arguments[0]) {
			arguments[0] = "-" + arguments[0]
		}
		arguments = sanitizeMakeArguments(arguments)
		arguments = reorderMakeFlagArguments(arguments)
		result = append(result, name+"="+joinMakeFlagArguments(arguments))
	}
	return result
}

func splitMakeFlagArguments(value string) []string {
	arguments := make([]string, 0)
	var current strings.Builder
	flush := func() {
		if current.Len() == 0 {
			return
		}
		arguments = append(arguments, current.String())
		current.Reset()
	}
	for index := 0; index < len(value); index++ {
		character := value[index]
		if character == '\\' && index+1 < len(value) {
			current.WriteByte(value[index+1])
			index++
			continue
		}
		if character == ' ' || character == '\t' || character == '\r' || character == '\n' {
			flush()
			continue
		}
		current.WriteByte(character)
	}
	flush()
	return arguments
}

func makeFlagHasImplicitCluster(argument string) bool {
	return argument != "" && !strings.HasPrefix(argument, "-") && !strings.Contains(argument, "=")
}

func reorderMakeFlagArguments(arguments []string) []string {
	options := make([]string, 0, len(arguments))
	assignments := make([]string, 0, len(arguments))
	goals := make([]string, 0, len(arguments))
	parseOptions := true
	for index := 0; index < len(arguments); index++ {
		argument := arguments[index]
		if parseOptions && argument == "--" {
			parseOptions = false
			continue
		}
		if parseOptions && strings.HasPrefix(argument, "-") && argument != "-" {
			options = append(options, argument)
			if makeOptionTakesFollowingArgument(argument, arguments[index+1:]) {
				index++
				options = append(options, arguments[index])
			}
			continue
		}
		if parseOptions && strings.Contains(argument, "=") {
			assignments = append(assignments, argument)
			continue
		}
		goals = append(goals, argument)
	}
	result := make([]string, 0, len(arguments)+1)
	result = append(result, options...)
	result = append(result, assignments...)
	if len(goals) > 0 {
		result = append(result, "--")
		result = append(result, goals...)
	}
	return result
}

func makeOptionTakesFollowingArgument(argument string, remaining []string) bool {
	if strings.HasPrefix(argument, "--") {
		name, ok := canonicalMakeLongOption(argument)
		if !ok || strings.Contains(argument, "=") {
			return false
		}
		if name == "jobs" {
			return len(remaining) > 0 && makeJobsOptionValue(remaining[0])
		}
		if name == "load-average" || name == "max-load" {
			return len(remaining) > 0 && makeLoadOptionValue(remaining[0])
		}
		return makeLongOptionTakesArgument(argument)
	}
	if len(argument) < 2 || argument == "-" {
		return false
	}
	for index := 1; index < len(argument); index++ {
		if strings.ContainsRune("CEfIoW", rune(argument[index])) {
			return index+1 == len(argument)
		}
		if strings.ContainsRune("jlO", rune(argument[index])) {
			if index+1 != len(argument) || argument[index] == 'O' || len(remaining) == 0 {
				return false
			}
			if argument[index] == 'j' {
				return makeJobsOptionValue(remaining[0])
			}
			return makeLoadOptionValue(remaining[0])
		}
	}
	return false
}

func makeJobsOptionValue(value string) bool {
	if value == "" {
		return false
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}

func makeLoadOptionValue(value string) bool {
	if value == "" || value[0] == '+' || value[0] == '-' {
		return false
	}
	lower := strings.ToLower(value)
	if strings.Contains(lower, "inf") || strings.Contains(lower, "nan") {
		return false
	}
	_, err := strconv.ParseFloat(value, 64)
	return err == nil || errors.Is(err, strconv.ErrRange)
}

func joinMakeFlagArguments(arguments []string) string {
	quoted := make([]string, 0, len(arguments))
	for _, argument := range arguments {
		var value strings.Builder
		for _, character := range argument {
			switch character {
			case '\\', ' ', '\t', '\r', '\n':
				value.WriteByte('\\')
				value.WriteRune(character)
			default:
				value.WriteRune(character)
			}
		}
		quoted = append(quoted, value.String())
	}
	return strings.Join(quoted, " ")
}

func insertMakeOptions(arguments []string, options ...string) []string {
	insertAt := len(arguments)
	for i, argument := range arguments {
		if argument == "--" {
			insertAt = i
			break
		}
	}
	result := make([]string, 0, len(arguments)+len(options))
	result = append(result, arguments[:insertAt]...)
	result = append(result, options...)
	return append(result, arguments[insertAt:]...)
}
