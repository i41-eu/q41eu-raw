package main

import (
	"bytes"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

func TestSelfSignedCertIsCreatedOnceAndReused(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "sub", "key.pem")
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))

	first, err := loadOrCreateCert(certFile, keyFile, quiet)
	if err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(keyFile); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("key file: %v, %v", fi, err)
	}
	if first.Leaf == nil || len(first.Leaf.IPAddresses) == 0 || first.Leaf.DNSNames[0] != "localhost" {
		t.Fatalf("certificate names: %+v", first.Leaf)
	}
	second, err := loadOrCreateCert(certFile, keyFile, quiet)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first.Certificate[0], second.Certificate[0]) {
		t.Fatal("a second start made a new certificate; phones would have to accept it again")
	}
}
