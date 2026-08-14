//go:build !aix && !android && !darwin && !dragonfly && !freebsd && !illumos && !linux && !netbsd && !openbsd && !solaris

package internal

func executeMakeProxy(realMake, proxyName string, arguments []string) error {
	return makeProxyCommand(realMake, proxyName, arguments...).Run()
}
