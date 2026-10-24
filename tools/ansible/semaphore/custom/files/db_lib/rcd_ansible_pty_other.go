//go:build !linux

// SPDX-License-Identifier: MIT
package db_lib

import (
	"github.com/creack/pty"
	"os"
	"os/exec"
)

func startAnsiblePTY(cmd *exec.Cmd) (*os.File, error) { return pty.Start(cmd) }
