// Package tlsutil provides the self-signed certificate used when TLS is
// enabled without a user-supplied certificate.
package tlsutil

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"time"
)

// LoadOrCreate loads the key pair at certFile/keyFile, generating and
// saving a self-signed certificate for hosts first if either file is missing.
// It reports whether a new certificate was created.
func LoadOrCreate(certFile, keyFile string, hosts []string) (tls.Certificate, bool, error) {
	_, certErr := os.Stat(certFile)
	_, keyErr := os.Stat(keyFile)
	if certErr == nil && keyErr == nil {
		c, err := tls.LoadX509KeyPair(certFile, keyFile)
		return c, false, err
	}
	if !errors.Is(certErr, os.ErrNotExist) && certErr != nil {
		return tls.Certificate{}, false, certErr
	}
	certPEM, keyPEM, err := Generate(hosts, 2*365*24*time.Hour)
	if err != nil {
		return tls.Certificate{}, false, err
	}
	if err := os.WriteFile(keyFile, keyPEM, 0o600); err != nil {
		return tls.Certificate{}, false, fmt.Errorf("write key: %w", err)
	}
	if err := os.WriteFile(certFile, certPEM, 0o644); err != nil {
		return tls.Certificate{}, false, fmt.Errorf("write certificate: %w", err)
	}
	c, err := tls.X509KeyPair(certPEM, keyPEM)
	return c, true, err
}

// Generate creates a PEM-encoded self-signed ECDSA P-256 certificate.
func Generate(hosts []string, validity time.Duration) (certPEM, keyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return nil, nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "ContainerUI", Organization: []string{"ContainerUI (self-signed)"}},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(validity),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	for _, h := range hosts {
		if ip := net.ParseIP(h); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else if h != "" {
			tmpl.DNSNames = append(tmpl.DNSNames, h)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM, nil
}

// DefaultHosts returns localhost, loopback IPs and the machine's hostname.
func DefaultHosts() []string {
	hosts := []string{"localhost", "127.0.0.1", "::1"}
	if h, err := os.Hostname(); err == nil && h != "" {
		hosts = append(hosts, h)
	}
	return hosts
}
