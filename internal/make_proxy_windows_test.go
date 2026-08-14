package internal

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

func TestMakeProxyShortPathRetainsLongIdentity(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "proxy path")
	if err := os.Mkdir(parent, 0o755); err != nil {
		t.Fatalf("create proxy parent failed: %v", err)
	}
	directory, err := os.MkdirTemp(parent, makeProxyDirectoryPrefix)
	if err != nil {
		t.Fatalf("create proxy directory failed: %v", err)
	}
	proxyPath := filepath.Join(directory, "make.exe")
	if err := os.WriteFile(proxyPath, []byte("proxy"), 0o700); err != nil {
		t.Fatalf("write proxy executable failed: %v", err)
	}
	const realMake = `C:\tools\gmake.exe`
	if err := os.WriteFile(filepath.Join(directory, makeProxyMetadataName), []byte(realMake), 0o600); err != nil {
		t.Fatalf("write proxy metadata failed: %v", err)
	}

	commandPath, err := makeProxyCommandPath(proxyPath)
	if err != nil {
		t.Skipf("short paths are unavailable on this volume: %v", err)
	}
	if !shellSafeMakeProxyPath(commandPath) || strings.ContainsAny(commandPath, ` \`+"\t\r\n") {
		t.Fatalf("proxy command path is not shell-safe: %q", commandPath)
	}
	oldArg0 := os.Args[0]
	os.Args[0] = commandPath
	t.Cleanup(func() { os.Args[0] = oldArg0 })
	invocationPath, selectedMake, ok := makeProxyInvocation()
	if !ok || invocationPath != commandPath || selectedMake != realMake {
		t.Fatalf("short proxy path lost long identity: path=%q make=%q ok=%v", invocationPath, selectedMake, ok)
	}
}

func TestMakeProxyTempDirectoriesIncludeShellSafeFallbacks(t *testing.T) {
	buildDir := `C:\work\build`
	directories := makeProxyTempDirectories(buildDir)
	windowsDir, err := windows.GetWindowsDirectory()
	if err != nil {
		t.Fatalf("get Windows directory failed: %v", err)
	}
	for _, want := range []string{buildDir, filepath.Join(windowsDir, "Temp"), `C:\`} {
		found := false
		for _, directory := range directories {
			if strings.EqualFold(directory, want) {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("missing Windows proxy fallback %q: %#v", want, directories)
		}
	}
	for _, proxyPath := range []string{
		`C:\work\build\compiledb-make-proxy-123\make.exe`,
		`C:\compiledb-make-proxy-123\make.exe`,
	} {
		commandPath, err := makeProxyCommandPath(proxyPath)
		if err != nil || strings.Contains(commandPath, `\`) || !shellSafeMakeProxyPath(commandPath) {
			t.Fatalf("shell-safe fallback depends on short names: path=%q error=%v", commandPath, err)
		}
	}
}

func TestMakeProxyCreatesShellSafeFallback(t *testing.T) {
	workingDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("get working directory failed: %v", err)
	}
	proxyPath, cleanup, err := createDiscoveryMakeProxy(`/tools/gmake`, workingDir)
	if err != nil {
		t.Fatalf("create shell-safe Make proxy failed: %v", err)
	}
	defer cleanup()
	if !shellSafeMakeProxyPath(proxyPath) {
		t.Fatalf("created Make proxy is not shell-safe: %q", proxyPath)
	}
}
