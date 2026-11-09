package siem

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
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

// testPKI is a throwaway PKI: a server CA and its 127.0.0.1 leaf, a
// client CA and its leaf, and an unrelated CA with a leaf of its own.
type testPKI struct {
	caFile, otherCAFile, garbageFile  string
	certFile, keyFile                 string // client pair signed by the client CA
	strangerCertFile, strangerKeyFile string // client pair signed by the other CA
	server                            tls.Certificate
	clientCAs                         *x509.CertPool
}

type testLeaf struct {
	certPEM, keyPEM []byte
}

func newTestPKI(t *testing.T) testPKI {
	t.Helper()
	dir := t.TempDir()
	write := func(name string, data []byte) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, data, 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		return p
	}
	serverCA, serverKey := newTestCA(t, "sage test server CA")
	clientCA, clientKey := newTestCA(t, "sage test client CA")
	otherCA, otherKey := newTestCA(t, "sage test other CA")
	srv := newTestLeaf(t, serverCA, serverKey, "127.0.0.1", true)
	cli := newTestLeaf(t, clientCA, clientKey, "pg-sage-sidecar", false)
	stranger := newTestLeaf(t, otherCA, otherKey, "stranger", false)
	pair, err := tls.X509KeyPair(srv.certPEM, srv.keyPEM)
	if err != nil {
		t.Fatalf("server pair: %v", err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(clientCA)
	return testPKI{
		caFile:           write("ca.pem", certPEM(serverCA)),
		otherCAFile:      write("other-ca.pem", certPEM(otherCA)),
		garbageFile:      write("garbage.pem", []byte("-----BEGIN NOTHING-----\nxx\n")),
		certFile:         write("client.pem", cli.certPEM),
		keyFile:          write("client.key", cli.keyPEM),
		strangerCertFile: write("stranger.pem", stranger.certPEM),
		strangerKeyFile:  write("stranger.key", stranger.keyPEM),
		server:           pair,
		clientCAs:        pool,
	}
}

func certPEM(c *x509.Certificate) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.Raw})
}

func testSerial(t *testing.T) *big.Int {
	t.Helper()
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 62))
	if err != nil {
		t.Fatalf("serial: %v", err)
	}
	return n
}

func newTestCA(t *testing.T, cn string) (*x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("ca key: %v", err)
	}
	tmpl := &x509.Certificate{SerialNumber: testSerial(t), Subject: pkix.Name{CommonName: cn},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("ca cert: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse ca: %v", err)
	}
	return cert, key
}

func newTestLeaf(t *testing.T, ca *x509.Certificate, caKey *ecdsa.PrivateKey, cn string,
	server bool) testLeaf {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("leaf key: %v", err)
	}
	tmpl := &x509.Certificate{SerialNumber: testSerial(t), Subject: pkix.Name{CommonName: cn},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}
	if server {
		tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
		tmpl.IPAddresses = []net.IP{net.ParseIP("127.0.0.1")}
		tmpl.DNSNames = []string{"localhost"}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatalf("leaf cert: %v", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("leaf key: %v", err)
	}
	return testLeaf{
		certPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		keyPEM:  pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}),
	}
}
