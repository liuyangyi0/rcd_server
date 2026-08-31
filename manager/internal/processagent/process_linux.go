//go:build linux

package processagent

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

func platformSupported() error { return nil }

func acquireLock(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o640)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("another process agent owns this data directory: %w", err)
	}
	return f, nil
}

func prepareChild(cmd *exec.Cmd) {
	// startOwned keeps the creating OS thread alive until Wait returns. If the
	// agent crashes, Linux kills this child rather than leaving an unmanaged
	// lower that a replacement agent might accidentally duplicate or adopt.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
}

func requestStop(process *os.Process) error { return process.Signal(syscall.SIGTERM) }
