package tlsutil

import (
	"crypto/x509"
	"path/filepath"
	"testing"
)

func TestLoadOrCreate(t *testing.T) {
	dir := t.TempDir()
	cert, key := filepath.Join(dir, "c.pem"), filepath.Join(dir, "k.pem")
	c1, created, err := LoadOrCreate(cert, key, []string{"localhost", "127.0.0.1", "myhost"})
	if err != nil || !created {
		t.Fatalf("create: %v, created=%v", err, created)
	}
	leaf, err := x509.ParseCertificate(c1.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := leaf.VerifyHostname("myhost"); err != nil {
		t.Error(err)
	}
	if err := leaf.VerifyHostname("127.0.0.1"); err != nil {
		t.Error(err)
	}

	c2, created, err := LoadOrCreate(cert, key, nil)
	if err != nil || created {
		t.Fatalf("reload: %v, created=%v", err, created)
	}
	if string(c2.Certificate[0]) != string(c1.Certificate[0]) {
		t.Error("reloaded certificate differs")
	}
}
