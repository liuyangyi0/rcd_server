//go:build windows

package secrets

import (
	"context"
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

var machineEntropy = []byte("RcdManager/credentials/v1")

// DPAPIProtector uses Windows DPAPI machine scope so the service account can
// decrypt credentials after restart without storing a separate key on disk.
type DPAPIProtector struct{}

func NewMachineProtector() Protector { return DPAPIProtector{} }

func (DPAPIProtector) Protect(_ context.Context, plaintext []byte) ([]byte, error) {
	input := blobFromBytes(plaintext)
	entropy := blobFromBytes(machineEntropy)
	var output windows.DataBlob
	if err := windows.CryptProtectData(
		&input,
		nil,
		&entropy,
		0,
		nil,
		windows.CRYPTPROTECT_LOCAL_MACHINE,
		&output,
	); err != nil {
		return nil, fmt.Errorf("DPAPI protect: %w", err)
	}
	return copyAndFree(&output)
}

func (DPAPIProtector) Unprotect(_ context.Context, ciphertext []byte) ([]byte, error) {
	input := blobFromBytes(ciphertext)
	entropy := blobFromBytes(machineEntropy)
	var output windows.DataBlob
	if err := windows.CryptUnprotectData(&input, nil, &entropy, 0, nil, 0, &output); err != nil {
		return nil, fmt.Errorf("DPAPI unprotect: %w", err)
	}
	return copyAndFree(&output)
}

func blobFromBytes(value []byte) windows.DataBlob {
	blob := windows.DataBlob{Size: uint32(len(value))}
	if len(value) > 0 {
		blob.Data = &value[0]
	}
	return blob
}

func copyAndFree(blob *windows.DataBlob) ([]byte, error) {
	if blob.Data == nil || blob.Size == 0 {
		return []byte{}, nil
	}
	defer windows.LocalFree(windows.Handle(uintptr(unsafe.Pointer(blob.Data)))) //nolint:errcheck
	result := make([]byte, int(blob.Size))
	copy(result, unsafe.Slice(blob.Data, int(blob.Size)))
	return result, nil
}
