//go:build unix

package store

import (
	"errors"
	"os"
	"syscall"
)

// processAlive reports whether pid is a running process. Signal 0 checks
// for existence without delivering anything; EPERM means it exists but
// belongs to someone else.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// lockFile takes an exclusive, non-blocking lock on f. The kernel drops it
// when the process exits, however it exits, so a killed run can never
// leave its project locked.
func lockFile(f *os.File) error {
	err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return ErrLocked
	}
	return err
}
