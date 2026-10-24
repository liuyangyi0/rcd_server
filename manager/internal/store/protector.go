package store

import "context"

// SecretProtector keeps platform-specific encryption outside the repository.
// The Windows process wires a DPAPI machine-scope implementation.
type SecretProtector interface {
	Protect(context.Context, []byte) ([]byte, error)
	Unprotect(context.Context, []byte) ([]byte, error)
}

// PassthroughProtector is intentionally only suitable for tests and local
// fixtures. Production wiring must supply a real protector.
type PassthroughProtector struct{}

func (PassthroughProtector) Protect(_ context.Context, value []byte) ([]byte, error) {
	return append([]byte(nil), value...), nil
}

func (PassthroughProtector) Unprotect(_ context.Context, value []byte) ([]byte, error) {
	return append([]byte(nil), value...), nil
}
