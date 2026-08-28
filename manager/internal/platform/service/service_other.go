//go:build !windows

package service

func IsService() (bool, error) { return false, nil }
func Run(Runner) error         { return ErrUnsupported }
func Install(...string) error  { return ErrUnsupported }
func Uninstall() error         { return ErrUnsupported }
func Start() error             { return ErrUnsupported }
func Stop() error              { return ErrUnsupported }
