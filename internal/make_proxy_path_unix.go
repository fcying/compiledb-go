//go:build aix || android || darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd || solaris

package internal

func makeProxyTempDirectoryCandidates(tempDir string, fallbackParents []string) []string {
	directories := append([]string{tempDir}, fallbackParents...)
	return append(directories, "/tmp")
}

func makeProxyCommandPath(proxyPath string) (string, error) {
	return makeProxyCommandPathOther(proxyPath)
}

func makeProxyIdentityPath(proxyPath string) (string, error) {
	return proxyPath, nil
}
