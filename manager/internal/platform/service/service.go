package service

import (
	"context"
	"errors"
)

const (
	Name        = "RcdManager"
	DisplayName = "RCD OPC Manager"
)

type Runner func(context.Context) error

var ErrUnsupported = errors.New("Windows service management is unsupported on this platform")
