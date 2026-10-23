//go:build !linux

package app

func (r *Runtime) startReleaseControl() (func(), error) { return func() {}, nil }
