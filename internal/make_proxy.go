package internal

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

const (
	makeProxyDirectoryPrefix = ".compiledb-make-proxy-"
	makeProxyMetadataName    = ".real-make"
	makeProxyCheckName       = ".check"
	makeProxyCheckArgument   = "--compiledb-internal-make-proxy-check"
)

func init() {
	proxyPath, realMake, ok := makeProxyInvocation()
	if !ok {
		return
	}
	if len(os.Args) == 2 && os.Args[1] == makeProxyCheckArgument && makeProxyCheckInvocation(proxyPath) {
		os.Exit(0)
	}
	err := executeMakeProxy(realMake, proxyPath, recursiveMakeArguments(os.Args[1:]))
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			os.Exit(commandExitCode(exitErr))
		}
		_, _ = fmt.Fprintf(os.Stderr, "compiledb make proxy: %v\n", err)
		if errors.Is(err, os.ErrNotExist) {
			os.Exit(127)
		}
		os.Exit(commandExitCode(err))
	}
	os.Exit(0)
}

func recursiveMakeArguments(arguments []string) []string {
	result := make([]string, 1, len(arguments)+1)
	result[0] = "--print-directory"
	options := true
	for index := 0; index < len(arguments); index++ {
		argument := arguments[index]
		if options && argument == "--" {
			options = false
			result = append(result, argument)
			continue
		}
		if options && !strings.Contains(argument, "=") {
			if name, ok := canonicalMakeLongOption(argument); ok && name == "no-print-directory" {
				continue
			}
		}
		result = append(result, argument)
		if options && makeOptionTakesFollowingArgument(argument, arguments[index+1:]) && index+1 < len(arguments) {
			index++
			result = append(result, arguments[index])
		}
	}
	return result
}

func configureDiscoveryMakeProxy(command *exec.Cmd) (func(), error) {
	if command.Err != nil {
		return func() {}, nil
	}
	workingDir := command.Dir
	if workingDir == "" {
		var err error
		workingDir, err = os.Getwd()
		if err != nil {
			return nil, err
		}
	}
	proxyPath, cleanup, err := createDiscoveryMakeProxy(command.Path, workingDir)
	if err != nil {
		return nil, err
	}
	command.Args[0] = proxyPath
	return cleanup, nil
}

func createDiscoveryMakeProxy(realMake string, fallbackParents ...string) (string, func(), error) {
	executable, err := os.Executable()
	if err != nil {
		return "", nil, err
	}
	parents := makeProxyTempDirectories(fallbackParents...)
	if len(parents) == 0 {
		return "", nil, fmt.Errorf("temporary directory %q is not absolute", os.TempDir())
	}
	return createDiscoveryMakeProxyFromParents(executable, realMake, parents)
}

func createDiscoveryMakeProxyFromParents(executable, realMake string, parents []string) (string, func(), error) {
	var failures []error
	for _, parent := range parents {
		proxyPath, cleanup, err := createDiscoveryMakeProxyIn(parent, executable, realMake)
		if err == nil {
			return proxyPath, cleanup, nil
		}
		failures = append(failures, err)
	}
	return "", nil, errors.Join(failures...)
}

func createDiscoveryMakeProxyIn(parent, executable, realMake string) (string, func(), error) {
	directory, err := os.MkdirTemp(parent, makeProxyDirectoryPrefix)
	if err != nil {
		return "", nil, err
	}
	originalDirectory := directory
	directory, err = filepath.Abs(directory)
	if err != nil {
		_ = os.RemoveAll(originalDirectory)
		return "", nil, err
	}
	cleanup := func() { _ = os.RemoveAll(directory) }
	metadataPath := filepath.Join(directory, makeProxyMetadataName)
	if err := os.WriteFile(metadataPath, []byte(realMake), 0o600); err != nil {
		cleanup()
		return "", nil, err
	}
	proxyName := "make"
	if runtime.GOOS == "windows" {
		proxyName += ".exe"
	}
	proxyPath := filepath.Join(directory, proxyName)
	if strings.ContainsAny(proxyPath, "\r\n") {
		cleanup()
		return "", nil, errors.New("Make proxy path cannot contain newlines")
	}
	if err := installMakeProxyExecutable(executable, proxyPath); err != nil {
		cleanup()
		return "", nil, err
	}
	checkPath := filepath.Join(directory, makeProxyCheckName)
	if err := os.WriteFile(checkPath, nil, 0o600); err != nil {
		cleanup()
		return "", nil, err
	}
	if err := exec.Command(proxyPath, makeProxyCheckArgument).Run(); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("verify Make proxy executable: %w", err)
	}
	if err := os.Remove(checkPath); err != nil {
		cleanup()
		return "", nil, err
	}
	commandPath, err := makeProxyCommandPath(proxyPath)
	if err != nil {
		cleanup()
		return "", nil, err
	}
	return commandPath, cleanup, nil
}

func makeProxyInvocation() (string, string, bool) {
	if len(os.Args) == 0 {
		return "", "", false
	}
	proxyPath, err := filepath.Abs(os.Args[0])
	if err != nil || executableBase(proxyPath) != "make" {
		return "", "", false
	}
	identityPath, err := makeProxyIdentityPath(proxyPath)
	if err != nil || !strings.HasPrefix(filepath.Base(filepath.Dir(identityPath)), makeProxyDirectoryPrefix) {
		return "", "", false
	}
	metadata, ok := readMakeProxyMetadata(filepath.Join(filepath.Dir(identityPath), makeProxyMetadataName))
	if !ok {
		return "", "", false
	}
	return os.Args[0], metadata, true
}

func makeProxyCheckInvocation(proxyPath string) bool {
	identityPath, err := makeProxyIdentityPath(proxyPath)
	if err != nil {
		return false
	}
	info, err := os.Stat(filepath.Join(filepath.Dir(identityPath), makeProxyCheckName))
	return err == nil && info.Mode().IsRegular()
}

func shellSafeMakeProxyPath(filename string) bool {
	if filename == "" {
		return false
	}
	if runtime.GOOS == "windows" {
		filename = strings.ReplaceAll(filename, `\`, "/")
	}
	if !filepath.IsAbs(filename) {
		return false
	}
	const safe = "_@%+=:,./-~"
	for index := 0; index < len(filename); index++ {
		character := filename[index]
		if character > 0x7f ||
			(character >= 'a' && character <= 'z') ||
			(character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') ||
			strings.IndexByte(safe, character) >= 0 {
			continue
		}
		return false
	}
	return true
}

func makeProxyTempDirectories(fallbackParents ...string) []string {
	tempDir := os.TempDir()
	candidates := makeProxyTempDirectoryCandidates(tempDir, fallbackParents)
	directories := make([]string, 0, len(candidates))
	seen := make(map[string]struct{}, cap(directories))
	appendDirectory := func(directory string) {
		if !filepath.IsAbs(directory) {
			return
		}
		directory = filepath.Clean(directory)
		key := directory
		if runtime.GOOS == "windows" {
			key = strings.ToLower(key)
		}
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		directories = append(directories, directory)
	}
	for _, directory := range candidates {
		appendDirectory(directory)
	}
	return directories
}

func makeProxyCommandPathOther(proxyPath string) (string, error) {
	if shellSafeMakeProxyPath(proxyPath) {
		return proxyPath, nil
	}
	return "", fmt.Errorf("Make proxy path %q is not safe for recursive recipes", proxyPath)
}

func readMakeProxyMetadata(filename string) (string, bool) {
	metadataFile, err := openMakeProxyMetadata(filename)
	if err != nil {
		return "", false
	}
	info, statErr := metadataFile.Stat()
	if statErr != nil || !info.Mode().IsRegular() {
		_ = metadataFile.Close()
		return "", false
	}
	metadata, readErr := io.ReadAll(io.LimitReader(metadataFile, 64*1024+1))
	closeErr := metadataFile.Close()
	if readErr != nil || closeErr != nil || len(metadata) == 0 || len(metadata) > 64*1024 ||
		strings.IndexByte(string(metadata), 0) >= 0 {
		return "", false
	}
	return string(metadata), true
}

func makeProxyCommand(realMake, proxyName string, arguments ...string) *exec.Cmd {
	command := exec.Command(realMake, arguments...)
	command.Args[0] = proxyName
	command.Stdin = os.Stdin
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	command.Env = os.Environ()
	return command
}

func installMakeProxyExecutable(executable, proxyPath string) error {
	absoluteExecutable, err := filepath.Abs(executable)
	if err != nil {
		return err
	}
	resolvedExecutable, err := filepath.EvalSymlinks(absoluteExecutable)
	if err != nil {
		return err
	}
	executable = resolvedExecutable
	if err := os.Symlink(executable, proxyPath); err == nil {
		return nil
	}
	if err := os.Link(executable, proxyPath); err == nil {
		return nil
	}
	source, err := os.Open(executable)
	if err != nil {
		return err
	}
	defer source.Close()
	destination, err := os.OpenFile(proxyPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o700)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(destination, source)
	closeErr := destination.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}
