// Package servertls serves the API over TLS from SAGE_TLS_CERT and
// SAGE_TLS_KEY (CG-02) and picks up a rotated pair without a restart.
//
// Rotation: every handshake may trigger a stat of both files, at most once
// per check interval. When either modification time changed, the pair is
// re-read; a pair that fails to load or validate is logged and the current
// certificate keeps serving. Stat follows symlinks, so the atomic symlink
// swap Kubernetes uses for secret volumes is seen too.
package servertls

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

// MinVersion is the oldest protocol the API accepts.
const MinVersion = tls.VersionTLS12

// DefaultCheckInterval bounds how often handshakes look for a rotated pair.
const DefaultCheckInterval = 5 * time.Second

// expiryWarning is how far ahead of NotAfter a loaded certificate is
// reported as expiring soon.
const expiryWarning = 14 * 24 * time.Hour

var (
	// ErrExpired reports a certificate past its NotAfter.
	ErrExpired = errors.New("certificate has expired")
	// ErrNotYetValid reports a certificate before its NotBefore.
	ErrNotYetValid = errors.New("certificate is not valid yet")
)

// Options tunes a Reloader; the zero value is production behaviour.
type Options struct {
	CheckInterval time.Duration
	Now           func() time.Time
	Logf          func(format string, args ...any)
}

// Reloader holds the current certificate and reloads it when its files
// change. It is safe for concurrent handshakes.
type Reloader struct {
	certFile, keyFile string
	opts              Options

	mu        sync.RWMutex
	cert      *tls.Certificate
	certMod   time.Time
	keyMod    time.Time
	lastCheck time.Time
	checking  sync.Mutex
}

// New loads and validates the pair, failing with an error that names the
// variable and path at fault.
func New(certFile, keyFile string, opts Options) (*Reloader, error) {
	if certFile == "" || keyFile == "" {
		return nil, errors.New("SAGE_TLS_CERT and SAGE_TLS_KEY must both name files")
	}
	if opts.CheckInterval <= 0 {
		opts.CheckInterval = DefaultCheckInterval
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Logf == nil {
		opts.Logf = func(string, ...any) {}
	}
	r := &Reloader{certFile: certFile, keyFile: keyFile, opts: opts}
	if err := r.Reload(); err != nil {
		return nil, err
	}
	return r, nil
}

// Config returns the server TLS configuration. Certificates come from
// GetCertificate so a reload reaches new connections.
func (r *Reloader) Config() *tls.Config {
	return &tls.Config{MinVersion: MinVersion, GetCertificate: r.GetCertificate}
}

// GetCertificate serves the current certificate, first reloading it when the
// check interval has passed and the files changed.
func (r *Reloader) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	r.maybeReload()
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.cert, nil
}

// Reload re-reads the pair now. On failure the current certificate stays.
func (r *Reloader) Reload() error {
	certMod, keyMod, err := r.modTimes()
	if err != nil {
		return err
	}
	cert, err := r.load()
	if err != nil {
		return err
	}
	r.mu.Lock()
	r.cert, r.certMod, r.keyMod = cert, certMod, keyMod
	r.lastCheck = r.opts.Now()
	r.mu.Unlock()
	if left := cert.Leaf.NotAfter.Sub(r.opts.Now()); left < expiryWarning {
		r.opts.Logf("TLS certificate %s expires in %s (at %s)", r.certFile,
			left.Round(time.Minute), cert.Leaf.NotAfter.UTC().Format(time.RFC3339))
	}
	return nil
}

func (r *Reloader) maybeReload() {
	now := r.opts.Now()
	r.mu.RLock()
	due := now.Sub(r.lastCheck) >= r.opts.CheckInterval
	r.mu.RUnlock()
	if !due || !r.checking.TryLock() {
		return // another handshake is already checking
	}
	defer r.checking.Unlock()
	certMod, keyMod, err := r.modTimes()
	r.mu.Lock()
	r.lastCheck = now
	changed := err == nil && (!certMod.Equal(r.certMod) || !keyMod.Equal(r.keyMod))
	r.mu.Unlock()
	if err != nil {
		r.opts.Logf("TLS reload check failed; keeping the current certificate: %v", err)
		return
	}
	if !changed {
		return
	}
	if err := r.Reload(); err != nil {
		// Remember the bad pair's times: retry when the files change again,
		// not on every interval.
		r.mu.Lock()
		r.certMod, r.keyMod = certMod, keyMod
		r.mu.Unlock()
		r.opts.Logf("TLS reload failed; keeping the current certificate: %v", err)
		return
	}
	r.opts.Logf("TLS certificate reloaded from %s", r.certFile)
}

func (r *Reloader) modTimes() (certMod, keyMod time.Time, err error) {
	certInfo, err := os.Stat(r.certFile)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("SAGE_TLS_CERT: %w", err)
	}
	keyInfo, err := os.Stat(r.keyFile)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("SAGE_TLS_KEY: %w", err)
	}
	return certInfo.ModTime(), keyInfo.ModTime(), nil
}

// load reads, pairs and validates the certificate and key.
func (r *Reloader) load() (*tls.Certificate, error) {
	cert, err := tls.LoadX509KeyPair(r.certFile, r.keyFile)
	if err != nil {
		return nil, r.pairError(err)
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return nil, fmt.Errorf("SAGE_TLS_CERT %s: parsing certificate: %w", r.certFile, err)
	}
	cert.Leaf = leaf
	now := r.opts.Now()
	if now.After(leaf.NotAfter) {
		return nil, fmt.Errorf("SAGE_TLS_CERT %s: %w at %s", r.certFile, ErrExpired,
			leaf.NotAfter.UTC().Format(time.RFC3339))
	}
	if now.Before(leaf.NotBefore) {
		return nil, fmt.Errorf("SAGE_TLS_CERT %s: %w until %s", r.certFile, ErrNotYetValid,
			leaf.NotBefore.UTC().Format(time.RFC3339))
	}
	return &cert, nil
}

// pairError names the file at fault in a tls.LoadX509KeyPair failure.
func (r *Reloader) pairError(err error) error {
	msg := err.Error()
	switch {
	case containsAny(msg, "private key does not match public key"):
		return fmt.Errorf("SAGE_TLS_KEY %s does not match the certificate in "+
			"SAGE_TLS_CERT %s", r.keyFile, r.certFile)
	case containsAny(msg, "failed to find any PEM data in certificate",
		"failed to find certificate PEM data"):
		return fmt.Errorf("SAGE_TLS_CERT %s holds no PEM certificate: %w", r.certFile, err)
	case containsAny(msg, "failed to find any PEM data in key",
		"failed to find PEM block with type ending in \"PRIVATE KEY\""):
		return fmt.Errorf("SAGE_TLS_KEY %s holds no PEM private key: %w", r.keyFile, err)
	}
	return fmt.Errorf("loading TLS pair (SAGE_TLS_CERT %s, SAGE_TLS_KEY %s): %w",
		r.certFile, r.keyFile, err)
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
