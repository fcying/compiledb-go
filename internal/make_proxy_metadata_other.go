//go:build !aix && !android && !darwin && !dragonfly && !freebsd && !illumos && !linux && !netbsd && !openbsd && !solaris

package internal

import "os"

func openMakeProxyMetadata(filename string) (*os.File, error) {
	return os.Open(filename)
}
