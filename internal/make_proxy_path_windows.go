package internal

import (
	"fmt"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

func makeProxyTempDirectoryCandidates(tempDir string, fallbackParents []string) []string {
	directories := append([]string{tempDir}, fallbackParents...)
	if windowsDir, err := windows.GetWindowsDirectory(); err == nil {
		directories = append(directories, filepath.Join(windowsDir, "Temp"))
	}
	for _, directory := range append([]string(nil), directories...) {
		volume := filepath.VolumeName(directory)
		if volume != "" {
			directories = append(directories, volume+string(filepath.Separator))
		}
	}
	return directories
}

func makeProxyCommandPath(proxyPath string) (string, error) {
	commandPath := strings.ReplaceAll(proxyPath, `\`, "/")
	if shellSafeMakeProxyPath(commandPath) {
		return commandPath, nil
	}
	longPath, err := windows.UTF16PtrFromString(proxyPath)
	if err != nil {
		return "", err
	}
	size, err := windows.GetShortPathName(longPath, nil, 0)
	if err != nil {
		return "", err
	}
	shortPath := make([]uint16, size)
	length, err := windows.GetShortPathName(longPath, &shortPath[0], uint32(len(shortPath)))
	if err != nil {
		return "", err
	}
	commandPath = strings.ReplaceAll(windows.UTF16ToString(shortPath[:length]), `\`, "/")
	if !shellSafeMakeProxyPath(commandPath) {
		return "", fmt.Errorf("Make proxy path %q has no shell-safe short form", proxyPath)
	}
	return commandPath, nil
}

func makeProxyIdentityPath(proxyPath string) (string, error) {
	longPath, err := windows.UTF16PtrFromString(proxyPath)
	if err != nil {
		return "", err
	}
	size, err := windows.GetLongPathName(longPath, nil, 0)
	if err != nil {
		return "", err
	}
	identityPath := make([]uint16, size)
	length, err := windows.GetLongPathName(longPath, &identityPath[0], uint32(len(identityPath)))
	if err != nil {
		return "", err
	}
	return windows.UTF16ToString(identityPath[:length]), nil
}
