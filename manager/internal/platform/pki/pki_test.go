package pki

import (
	"crypto/x509"
	"encoding/pem"
	"os"
	"testing"
)

func TestEnsureCreatesReusablePair(t *testing.T) {
	dir := t.TempDir()
	pair, err := Ensure(dir, "manager.local")
	if err != nil {
		t.Fatalf("Ensure() error = %v", err)
	}
	certificatePEM, err := os.ReadFile(pair.CertificatePath)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	block, _ := pem.Decode(certificatePEM)
	if block == nil {
		t.Fatal("certificate PEM could not be decoded")
	}
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("ParseCertificate() error = %v", err)
	}
	if len(certificate.URIs) != 1 || certificate.URIs[0].String() != ApplicationURI {
		t.Fatalf("certificate URIs = %v", certificate.URIs)
	}
	if len(certificate.DNSNames) < 1 {
		t.Fatal("certificate has no DNS names")
	}
	second, err := Ensure(dir, "different.local")
	if err != nil {
		t.Fatalf("second Ensure() error = %v", err)
	}
	if second != pair {
		t.Fatalf("second pair = %+v, want %+v", second, pair)
	}
}
