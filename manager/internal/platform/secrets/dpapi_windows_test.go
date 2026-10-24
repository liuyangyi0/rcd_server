//go:build windows

package secrets

import (
	"bytes"
	"context"
	"testing"
)

func TestDPAPIRoundTrip(t *testing.T) {
	protector := NewMachineProtector()
	want := []byte("user1:pwd")
	ciphertext, err := protector.Protect(context.Background(), want)
	if err != nil {
		t.Fatalf("Protect() error = %v", err)
	}
	if bytes.Equal(ciphertext, want) {
		t.Fatal("Protect() returned plaintext")
	}
	got, err := protector.Unprotect(context.Background(), ciphertext)
	if err != nil {
		t.Fatalf("Unprotect() error = %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("round trip = %q, want %q", got, want)
	}
}
