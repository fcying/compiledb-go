//go:build !aix && !android && !darwin && !dragonfly && !freebsd && !illumos && !linux && !netbsd && !openbsd && !solaris && !windows

package internal

func makeProxyTempDirectoryCandidates(tempDir string, fallbackParents []string) []string {
	return append([]string{tempDir}, fallbackParents...)
}

func makeProxyCommandPath(proxyPath string) (string, error) {
	return makeProxyCommandPathOther(proxyPath)
}

func makeProxyIdentityPath(proxyPath string) (string, error) {
	return proxyPath, nil
}
