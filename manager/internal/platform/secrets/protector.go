package secrets

import "context"

// Protector encrypts credentials before they are written to the configuration store.
// Implementations must never return an input slice as the output slice.
type Protector interface {
	Protect(context.Context, []byte) ([]byte, error)
	Unprotect(context.Context, []byte) ([]byte, error)
}
