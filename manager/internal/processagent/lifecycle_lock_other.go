//go:build !linux

package processagent

import (
	"errors"
	"os"
)

func lockLifecycle(string) (*os.File, error) {
	return nil, errors.New("safe lifecycle is supported on Linux only")
}
