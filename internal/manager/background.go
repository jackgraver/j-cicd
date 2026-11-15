package manager

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"j-cicd/internal/store"
)

const maxLauncherLog = 1 << 20

// Launch starts `exe args...` as a detached background process: a run
// that outlives whatever launched it, so closing the UI doesn't kill a
// build halfway through a push. Its stdout is discarded (the run records
// its own output in its log); stderr goes to <state root>/launcher.log so
// a process that dies before it can record a run still leaves a trace.
func Launch(st *store.Store, exe string, args ...string) error {
	devNull, err := os.Open(os.DevNull)
	if err != nil {
		return err
	}
	defer devNull.Close()
	errLogPath := filepath.Join(st.Root(), "launcher.log")
	flags := os.O_CREATE | os.O_WRONLY | os.O_APPEND
	// Every background run adds a line or two; start over rather than
	// let it grow forever.
	if info, err := os.Stat(errLogPath); err == nil && info.Size() > maxLauncherLog {
		flags |= os.O_TRUNC
	}
	errLog, err := os.OpenFile(errLogPath, flags, 0o644)
	if err != nil {
		return err
	}
	defer errLog.Close()

	cmd := exec.Command(exe, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = devNull, devNull, errLog
	detach(cmd)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting %s: %w", exe, err)
	}
	// Reap it in the background so it doesn't linger as a zombie while
	// the launcher keeps running.
	go cmd.Wait()
	return nil
}

// Cancel asks an in-progress run to stop.
func Cancel(r store.Run) error {
	if r.Status.Done() {
		return fmt.Errorf("run %s already %s", r.ID, r.Status)
	}
	if r.PID <= 0 {
		return fmt.Errorf("run %s has no recorded process", r.ID)
	}
	return signalCancel(r.PID)
}
