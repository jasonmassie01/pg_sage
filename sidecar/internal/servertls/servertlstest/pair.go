// Package servertlstest writes generated certificate and key pairs for tests
// of the API's TLS listener.
package servertlstest

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Pair is a generated self-signed certificate and its key on disk.
type Pair struct {
	CertPath, KeyPath string
	Serial            int64
}

// Options sets the certificate's serial number and validity window.
type Options struct {
	Serial    int64
	NotBefore time.Time
	NotAfter  time.Time
}

// Valid returns options for a certificate valid from an hour ago for a day.
func Valid(serial int64) Options {
	return Options{Serial: serial, NotBefore: time.Now().Add(-time.Hour),
		NotAfter: time.Now().Add(24 * time.Hour)}
}

// WritePair writes a fresh pair into dir as cert.pem and key.pem, replacing
// any pair already there. The certificate names localhost and 127.0.0.1.
func WritePair(t testing.TB, dir string, opts Options) Pair {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(opts.Serial),
		Subject:      pkix.Name{CommonName: "pg-sage.test"},
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore:    opts.NotBefore,
		NotAfter:     opts.NotAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	pair := Pair{CertPath: filepath.Join(dir, "cert.pem"),
		KeyPath: filepath.Join(dir, "key.pem"), Serial: opts.Serial}
	writePEM(t, pair.CertPath, "CERTIFICATE", der)
	writePEM(t, pair.KeyPath, "EC PRIVATE KEY", keyDER)
	return pair
}

func writePEM(t testing.TB, path, kind string, der []byte) {
	t.Helper()
	data := pem.EncodeToMemory(&pem.Block{Type: kind, Bytes: der})
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// BumpMtime moves both files' modification time forward so a reload sees a
// change even on file systems with coarse timestamps.
func BumpMtime(t testing.TB, pair Pair, by time.Duration) {
	t.Helper()
	when := time.Now().Add(by)
	for _, path := range []string{pair.CertPath, pair.KeyPath} {
		if err := os.Chtimes(path, when, when); err != nil {
			t.Fatal(err)
		}
	}
}
