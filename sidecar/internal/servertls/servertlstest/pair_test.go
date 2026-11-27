package servertlstest

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// No concurrent access tests: WritePair and BumpMtime share no state; each
// call touches only the files it is given.

// fatalTB records a helper's fatal failure and ends the calling goroutine
// the way testing.T does, so the helpers' own error paths can be checked.
type fatalTB struct {
	testing.TB
	mu     sync.Mutex
	fatals []string
}

func (f *fatalTB) Helper() {}

func (f *fatalTB) Fatal(args ...any) { f.fail(fmt.Sprint(args...)) }

func (f *fatalTB) Fatalf(format string, args ...any) { f.fail(fmt.Sprintf(format, args...)) }

func (f *fatalTB) fail(msg string) {
	f.mu.Lock()
	f.fatals = append(f.fatals, msg)
	f.mu.Unlock()
	runtime.Goexit()
}

// runFatal calls fn on its own goroutine and returns the fatal messages.
func runFatal(fn func(tb testing.TB)) []string {
	tb := &fatalTB{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn(tb)
	}()
	<-done
	tb.mu.Lock()
	defer tb.mu.Unlock()
	return append([]string(nil), tb.fatals...)
}

// loadPair loads the pair as the TLS listener does (the key must match the
// certificate) and returns the parsed leaf.
func loadPair(t *testing.T, pair Pair) *x509.Certificate {
	t.Helper()
	kp, err := tls.LoadX509KeyPair(pair.CertPath, pair.KeyPath)
	if err != nil {
		t.Fatalf("load the written pair: %v", err)
	}
	if len(kp.Certificate) != 1 {
		t.Fatalf("certificate chain length = %d, want 1", len(kp.Certificate))
	}
	leaf, err := x509.ParseCertificate(kp.Certificate[0])
	if err != nil {
		t.Fatalf("parse the leaf: %v", err)
	}
	return leaf
}

func selfRoots(cert *x509.Certificate) *x509.CertPool {
	roots := x509.NewCertPool()
	roots.AddCert(cert)
	return roots
}

func pemType(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	block, rest := pem.Decode(data)
	if block == nil {
		t.Fatalf("%s holds no PEM block", path)
	}
	if len(rest) != 0 {
		t.Fatalf("%s has %d bytes after its PEM block", path, len(rest))
	}
	return block.Type
}

func TestWritePair_FilesParseAndChainVerifies(t *testing.T) {
	dir := t.TempDir()
	pair := WritePair(t, dir, Valid(42))
	if pair.CertPath != filepath.Join(dir, "cert.pem") ||
		pair.KeyPath != filepath.Join(dir, "key.pem") || pair.Serial != 42 {
		t.Fatalf("pair = %+v, want cert.pem and key.pem in %s with serial 42", pair, dir)
	}
	if got := pemType(t, pair.CertPath); got != "CERTIFICATE" {
		t.Errorf("certificate PEM type = %q", got)
	}
	if got := pemType(t, pair.KeyPath); got != "EC PRIVATE KEY" {
		t.Errorf("key PEM type = %q", got)
	}
	cert := loadPair(t, pair)
	if cert.SerialNumber.Int64() != 42 || cert.Subject.CommonName != "pg-sage.test" {
		t.Errorf("serial %v, CN %q; want 42, pg-sage.test",
			cert.SerialNumber, cert.Subject.CommonName)
	}
	for _, name := range []string{"localhost", "127.0.0.1"} {
		_, err := cert.Verify(x509.VerifyOptions{Roots: selfRoots(cert), DNSName: name,
			KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}})
		if err != nil {
			t.Errorf("verify for %s: %v", name, err)
		}
	}
	// A name the certificate does not carry must fail, or the loop above
	// would pass for a certificate that names everything.
	_, err := cert.Verify(x509.VerifyOptions{Roots: selfRoots(cert), DNSName: "pg-sage.invalid"})
	var hostErr x509.HostnameError
	if !errors.As(err, &hostErr) {
		t.Errorf("verify for an unlisted name: err = %v, want a HostnameError", err)
	}
	if runtime.GOOS != "windows" { // Windows reports 0666 for any writable file
		for _, path := range []string{pair.CertPath, pair.KeyPath} {
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() != 0o600 {
				t.Errorf("%s mode = %v, want 0600", path, info.Mode().Perm())
			}
		}
	}
}

func TestValid_WindowContainsNow(t *testing.T) {
	before := time.Now()
	opts := Valid(7)
	after := time.Now()
	if opts.Serial != 7 {
		t.Errorf("serial = %d, want 7", opts.Serial)
	}
	if opts.NotBefore.Before(before.Add(-time.Hour)) ||
		opts.NotBefore.After(after.Add(-time.Hour)) {
		t.Errorf("NotBefore = %v, want an hour before %v", opts.NotBefore, before)
	}
	if opts.NotAfter.Before(before.Add(24*time.Hour)) ||
		opts.NotAfter.After(after.Add(24*time.Hour)) {
		t.Errorf("NotAfter = %v, want a day after %v", opts.NotAfter, before)
	}
}

// Boundary: the written certificate is valid inside its window and invalid
// a second outside either edge (x509 stores whole seconds).
func TestValid_CertificateWindowEdges(t *testing.T) {
	opts := Valid(9)
	cert := loadPair(t, WritePair(t, t.TempDir(), opts))
	cases := []struct {
		name string
		at   time.Time
		want x509.InvalidReason
		ok   bool
	}{
		{"now", time.Now(), 0, true},
		{"before NotBefore", opts.NotBefore.Add(-2 * time.Second), x509.Expired, false},
		{"after NotAfter", opts.NotAfter.Add(2 * time.Second), x509.Expired, false},
	}
	for _, tc := range cases {
		_, err := cert.Verify(x509.VerifyOptions{Roots: selfRoots(cert),
			DNSName: "localhost", CurrentTime: tc.at})
		if tc.ok {
			if err != nil {
				t.Errorf("%s: %v", tc.name, err)
			}
			continue
		}
		var invalid x509.CertificateInvalidError
		if !errors.As(err, &invalid) || invalid.Reason != tc.want {
			t.Errorf("%s: err = %v, want reason %v", tc.name, err, tc.want)
		}
	}
}

func TestWritePair_ExpiredOptionsWriteAnExpiredCertificate(t *testing.T) {
	now := time.Now()
	opts := Options{Serial: 3, NotBefore: now.Add(-48 * time.Hour),
		NotAfter: now.Add(-24 * time.Hour)}
	cert := loadPair(t, WritePair(t, t.TempDir(), opts))
	_, err := cert.Verify(x509.VerifyOptions{Roots: selfRoots(cert), DNSName: "localhost"})
	var invalid x509.CertificateInvalidError
	if !errors.As(err, &invalid) || invalid.Reason != x509.Expired {
		t.Fatalf("verify an expired pair: err = %v, want Expired", err)
	}
}

// Zero options are accepted: serial 0 and a window at the zero time, so the
// certificate is already expired.
func TestWritePair_ZeroOptions(t *testing.T) {
	pair := WritePair(t, t.TempDir(), Options{})
	cert := loadPair(t, pair)
	if pair.Serial != 0 || cert.SerialNumber.Sign() != 0 {
		t.Errorf("serial = %d / %v, want 0", pair.Serial, cert.SerialNumber)
	}
	_, err := cert.Verify(x509.VerifyOptions{Roots: selfRoots(cert), DNSName: "localhost"})
	var invalid x509.CertificateInvalidError
	if !errors.As(err, &invalid) || invalid.Reason != x509.Expired {
		t.Errorf("verify a zero-window pair: err = %v, want Expired", err)
	}
}

// State transition: a second write replaces both files, so the key still
// matches the certificate and the new serial is served.
func TestWritePair_ReplacesTheExistingPair(t *testing.T) {
	dir := t.TempDir()
	first := loadPair(t, WritePair(t, dir, Valid(1)))
	second := loadPair(t, WritePair(t, dir, Valid(2)))
	if first.SerialNumber.Int64() != 1 || second.SerialNumber.Int64() != 2 {
		t.Fatalf("serials = %v then %v, want 1 then 2", first.SerialNumber, second.SerialNumber)
	}
	if first.Equal(second) {
		t.Error("the second write left the first certificate in place")
	}
}

func TestWritePair_NegativeSerialFailsTheTest(t *testing.T) {
	dir := t.TempDir()
	fatals := runFatal(func(tb testing.TB) {
		WritePair(tb, dir, Options{Serial: -1, NotBefore: time.Now(),
			NotAfter: time.Now().Add(time.Hour)})
	})
	if len(fatals) != 1 || !strings.Contains(fatals[0], "serial number must be positive") {
		t.Fatalf("fatals = %q, want one about the serial", fatals)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Errorf("dir holds %d entries (err %v), want none", len(entries), err)
	}
}

func TestWritePair_MissingDirFailsTheTest(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "missing")
	fatals := runFatal(func(tb testing.TB) { WritePair(tb, dir, Valid(1)) })
	if len(fatals) != 1 || !strings.Contains(fatals[0], "cert.pem") {
		t.Fatalf("fatals = %q, want one naming cert.pem", fatals)
	}
}

func TestBumpMtime_MovesBothFilesByTheDuration(t *testing.T) {
	pair := WritePair(t, t.TempDir(), Valid(5))
	// Boundary: forward, backward and zero.
	for _, by := range []time.Duration{2 * time.Hour, -3 * time.Hour, 0} {
		before := time.Now()
		BumpMtime(t, pair, by)
		after := time.Now()
		for _, path := range []string{pair.CertPath, pair.KeyPath} {
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			// Allow two seconds for file systems with coarse timestamps.
			lo, hi := before.Add(by-2*time.Second), after.Add(by+2*time.Second)
			if mt := info.ModTime(); mt.Before(lo) || mt.After(hi) {
				t.Errorf("by %v: %s mtime = %v, want between %v and %v",
					by, filepath.Base(path), mt, lo, hi)
			}
		}
	}
	if got := loadPair(t, pair).SerialNumber.Int64(); got != 5 {
		t.Errorf("serial after bumps = %d, want 5 (contents unchanged)", got)
	}
}

func TestBumpMtime_MissingFileFailsTheTest(t *testing.T) {
	dir := t.TempDir()
	pair := Pair{CertPath: filepath.Join(dir, "cert.pem"), KeyPath: filepath.Join(dir, "key.pem")}
	fatals := runFatal(func(tb testing.TB) { BumpMtime(tb, pair, time.Minute) })
	if len(fatals) != 1 || !strings.Contains(fatals[0], "cert.pem") {
		t.Fatalf("fatals = %q, want one naming cert.pem", fatals)
	}
}
