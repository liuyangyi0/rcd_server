package pki

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const ApplicationURI = "urn:RcdManager:rcd_manager"

type Pair struct {
	CertificatePath string
	PrivateKeyPath  string
}

func Ensure(dir, advertisedHost string) (Pair, error) {
	pair := Pair{
		CertificatePath: filepath.Join(dir, "rcd-manager-cert.pem"),
		PrivateKeyPath:  filepath.Join(dir, "rcd-manager-key.pem"),
	}
	certExists := fileExists(pair.CertificatePath)
	keyExists := fileExists(pair.PrivateKeyPath)
	if certExists && keyExists {
		return pair, nil
	}
	if certExists != keyExists {
		return Pair{}, fmt.Errorf("incomplete PKI pair in %s; restore or remove both files", dir)
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return Pair{}, fmt.Errorf("create PKI directory: %w", err)
	}
	if err := generate(pair, advertisedHost, time.Now()); err != nil {
		return Pair{}, err
	}
	return pair, nil
}

func generate(pair Pair, advertisedHost string, now time.Time) error {
	key, err := rsa.GenerateKey(rand.Reader, 3072)
	if err != nil {
		return fmt.Errorf("generate RSA key: %w", err)
	}
	serialLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, serialLimit)
	if err != nil {
		return fmt.Errorf("generate certificate serial: %w", err)
	}
	applicationURI, err := url.Parse(ApplicationURI)
	if err != nil {
		return fmt.Errorf("parse application URI: %w", err)
	}
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			Organization: []string{"RCD"},
			CommonName:   "RCD OPC Manager",
		},
		NotBefore:             now.Add(-5 * time.Minute),
		NotAfter:              now.AddDate(5, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment | x509.KeyUsageDataEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		URIs:                  []*url.URL{applicationURI},
	}
	host := strings.TrimSpace(advertisedHost)
	if ip := net.ParseIP(host); ip != nil {
		template.IPAddresses = append(template.IPAddresses, ip)
	} else if host != "" {
		template.DNSNames = append(template.DNSNames, host)
	}
	template.IPAddresses = append(template.IPAddresses, net.ParseIP("127.0.0.1"))
	template.DNSNames = append(template.DNSNames, "localhost")

	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return fmt.Errorf("create certificate: %w", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	if err := writeExclusive(pair.CertificatePath, certPEM, 0o644); err != nil {
		return err
	}
	if err := writeExclusive(pair.PrivateKeyPath, keyPEM, 0o600); err != nil {
		_ = os.Remove(pair.CertificatePath)
		return err
	}
	return nil
}

func writeExclusive(path string, content []byte, mode os.FileMode) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}
	if _, err := file.Write(content); err != nil {
		file.Close()
		_ = os.Remove(path)
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close %s: %w", path, err)
	}
	return nil
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
