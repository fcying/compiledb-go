//go:build !aix && !darwin && !dragonfly && !freebsd && !illumos && !linux && !netbsd && !openbsd && !solaris

package internal

import "os"

func openResponseFile(filename string) (*os.File, error) {
	return os.Open(filename)
}
