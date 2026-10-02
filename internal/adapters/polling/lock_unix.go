//go:build unix

package polling

import (
	"errors"
	"io"
	"os"
	"syscall"
)

// lockFile takes an exclusive lock on path that the kernel releases when the
// process exits, so a crash never leaves a stale lock.
func lockFile(path string) (io.Closer, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrLocked
		}
		return nil, err
	}
	return f, nil
}
