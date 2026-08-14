//go:build aix || android || darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd || solaris

package internal

import (
	"os"

	"golang.org/x/sys/unix"
)

func openMakeProxyMetadata(filename string) (*os.File, error) {
	fd, err := unix.Open(filename, unix.O_RDONLY|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	unix.CloseOnExec(fd)
	return os.NewFile(uintptr(fd), filename), nil
}
