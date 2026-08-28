package server

import (
	"bytes"
	"crypto/rsa"
	"math/big"
	"strconv"
	"testing"
)

func TestRSAUserNameCiphertextBlocksUseModulusWhenExponentIsShort(t *testing.T) {
	for _, test := range []struct {
		modulusBits int
		blockBytes  int
	}{
		{1024, 128},
		{2048, 256},
		{2049, 257}, // Size rounds a non-byte-aligned modulus upwards.
		{3072, 384},
		{4096, 512},
	} {
		t.Run(strconv.Itoa(test.modulusBits), func(t *testing.T) {
			// Deterministic sizing-only fixture, not a key used for encryption.
			// It reproduces the observed 256-byte N / 255-byte D without costly
			// or probabilistic key generation. Real decryption is exercised by
			// the server's username interoperability tests.
			key := &rsa.PrivateKey{
				PublicKey: rsa.PublicKey{
					N: new(big.Int).Add(new(big.Int).Lsh(big.NewInt(1), uint(test.modulusBits-1)), big.NewInt(1)),
					E: 65537,
				},
				D: new(big.Int).Add(new(big.Int).Lsh(big.NewInt(1), uint(test.modulusBits-9)), big.NewInt(1)),
			}
			if len(key.D.Bytes()) != test.blockBytes-1 {
				t.Fatalf("fixture private exponent is not one byte shorter: D=%d N=%d", len(key.D.Bytes()), key.Size())
			}
			block := make([]byte, rsaCiphertextBlockSize(key))
			if len(block) != test.blockBytes {
				t.Fatalf("username cipher block=%d, want modulus bytes=%d (D bytes=%d)", len(block), test.blockBytes, len(key.D.Bytes()))
			}
			first := bytes.Repeat([]byte{0x31}, test.blockBytes)
			second := bytes.Repeat([]byte{0xa7}, test.blockBytes)
			ciphertext := bytes.NewBuffer(append(append([]byte(nil), first...), second...))
			for index, want := range [][]byte{first, second} {
				n, err := ciphertext.Read(block)
				if err != nil || n != test.blockBytes || !bytes.Equal(block, want) {
					t.Fatalf("ciphertext block %d was split at the private exponent length: read=%d want=%d err=%v", index, n, test.blockBytes, err)
				}
			}
			if ciphertext.Len() != 0 {
				t.Fatalf("RSA block split left %d stray bytes", ciphertext.Len())
			}
		})
	}
}
