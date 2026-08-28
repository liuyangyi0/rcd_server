//go:build !windows

package secrets

import (
	"context"
	"errors"
)

type unsupportedProtector struct{}

func NewMachineProtector() Protector { return unsupportedProtector{} }

func (unsupportedProtector) Protect(context.Context, []byte) ([]byte, error) {
	return nil, errors.New("machine credential protection is only supported on Windows")
}

func (unsupportedProtector) Unprotect(context.Context, []byte) ([]byte, error) {
	return nil, errors.New("machine credential protection is only supported on Windows")
}
