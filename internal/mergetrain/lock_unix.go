//go:build unix

package mergetrain

import (
	"errors"
	"os"
	"syscall"
)

var errWouldBlock = syscall.EWOULDBLOCK

// tryLock takes an exclusive flock on f without blocking. flock locks belong
// to the open file description, so two opens of the same path conflict even
// within one process — which is what makes two drivers in one daemon refuse.
func tryLock(f *os.File) error {
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if !errors.Is(err, syscall.EINTR) {
			return err
		}
	}
}
