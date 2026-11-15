//go:build !unix

package store

import "os"

// processAlive can't check other processes portably here, so it assumes
// they're alive: a run is never marked interrupted when it might not be.
func processAlive(pid int) bool { return pid > 0 }

// lockFile is a no-op without flock; the in-progress run check in the
// manager is then the only guard against concurrent runs.
func lockFile(f *os.File) error { return nil }
