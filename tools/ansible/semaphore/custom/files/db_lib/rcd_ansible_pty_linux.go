//go:build linux

// SPDX-License-Identifier: MIT
package db_lib

import (
	"os"
	"os/exec"
	"strconv"
	"syscall"

	"golang.org/x/sys/unix"
)

// Keep ioctl pointer arguments live through x/sys's pointer-safe syscall
// wrapper. creack/pty v1.1.24 passes a stack pointer as uintptr across Go
// function calls; stack growth can invalidate that pointer and read pts 0.
// No permission relaxation, alternate user, or command retry is involved.
func openAnsiblePTY() (master, slave *os.File, err error) {
	master, err = os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		return nil, nil, err
	}
	defer func() {
		if err != nil {
			master.Close()
		}
	}()
	minor, err := unix.IoctlGetInt(int(master.Fd()), unix.TIOCGPTN)
	if err != nil {
		return master, nil, err
	}
	if err = unix.IoctlSetPointerInt(int(master.Fd()), unix.TIOCSPTLCK, 0); err != nil {
		return master, nil, err
	}
	slave, err = os.OpenFile("/dev/pts/"+strconv.Itoa(minor), os.O_RDWR|syscall.O_NOCTTY, 0)
	return master, slave, err
}

func startAnsiblePTY(cmd *exec.Cmd) (*os.File, error) {
	master, slave, err := openAnsiblePTY()
	if err != nil {
		return nil, err
	}
	defer slave.Close()
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setsid = true
	cmd.SysProcAttr.Setctty = true
	if cmd.Stdin == nil {
		cmd.Stdin = slave
	}
	if cmd.Stdout == nil {
		cmd.Stdout = slave
	}
	if cmd.Stderr == nil {
		cmd.Stderr = slave
	}
	if err = cmd.Start(); err != nil {
		master.Close()
		return nil, err
	}
	return master, nil
}
