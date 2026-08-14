//go:build aix || android || darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd || solaris

package internal

import (
	"os"
	"syscall"
)

func executeMakeProxy(realMake, proxyName string, arguments []string) error {
	return syscall.Exec(realMake, append([]string{proxyName}, arguments...), os.Environ())
}
