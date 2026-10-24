//go:build !linux

package processagent

import (
	"errors"
	"os"
	"os/exec"
)

var errUnsupported = errors.New("the process-agent command currently supports Linux only; manager/OPC functionality is unchanged")

func platformSupported() error             { return errUnsupported }
func acquireLock(string) (*os.File, error) { return nil, errUnsupported }
func prepareChild(*exec.Cmd)               {}
func requestStop(*os.Process) error        { return errUnsupported }
