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
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// MinVersion is the oldest protocol the API accepts.
const MinVersion = tls.VersionTLS12

// DefaultCheckInterval bounds how often handshakes look for a rotated pair.
const DefaultCheckInterval = 5 * time.Second

// ExpiryWarning is how far ahead of NotAfter the serving certificate is
// logged at warning level and reported as expiring.
const ExpiryWarning = 14 * 24 * time.Hour

// ExpiryCritical is how far ahead of NotAfter it is logged at error level.
const ExpiryCritical = 3 * 24 * time.Hour

// Expiry levels already logged for one certificate.
const (
	expiryLevelNone = iota
	expiryLevelWarn
	expiryLevelError
	expiryLevelExpired
)

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
	// Logf receives informational and warning lines; Errorf receives the
	// critical expiry line and defaults to Logf.
	Logf   func(format string, args ...any)
	Errorf func(format string, args ...any)
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

	failures atomic.Int64
	// expiryMu guards expiryLogged: the highest expiry level logged for
	// each certificate, keyed by its DER digest, so a level logs once.
	expiryMu     sync.Mutex
	expiryLogged map[[sha256.Size]byte]int
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
	if opts.Errorf == nil {
		opts.Errorf = opts.Logf
	}
	r := &Reloader{certFile: certFile, keyFile: keyFile, opts: opts,
		expiryLogged: map[[sha256.Size]byte]int{}}
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

// Reload re-reads the pair now. On failure the current certificate stays
// and, once a certificate is serving, the failure is counted.
func (r *Reloader) Reload() error {
	err := r.reload()
	if err != nil {
		r.mu.RLock()
		serving := r.cert != nil
		r.mu.RUnlock()
		if serving {
			r.failures.Add(1)
		}
	}
	return err
}

func (r *Reloader) reload() error {
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
	r.CheckExpiry()
	return nil
}

// NotAfter is the expiry of the certificate being served.
func (r *Reloader) NotAfter() time.Time {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.cert.Leaf.NotAfter
}

// ReloadFailures counts reloads that failed and left the current
// certificate serving, since the process started.
func (r *Reloader) ReloadFailures() int64 { return r.failures.Load() }

// Expiring reports whether the serving certificate expires within
// ExpiryWarning (or has expired).
func (r *Reloader) Expiring() bool {
	return r.NotAfter().Sub(r.opts.Now()) < ExpiryWarning
}

// CheckExpiry logs the serving certificate's expiry: a warning under
// ExpiryWarning, an error under ExpiryCritical, and an error again once it
// has expired. Each level is logged once per certificate, so an hourly call
// does not repeat it.
func (r *Reloader) CheckExpiry() {
	r.mu.RLock()
	cert := r.cert
	r.mu.RUnlock()
	notAfter := cert.Leaf.NotAfter
	left := notAfter.Sub(r.opts.Now())
	level, logf := expiryLevelNone, r.opts.Logf
	switch {
	case left <= 0:
		level, logf = expiryLevelExpired, r.opts.Errorf
	case left < ExpiryCritical:
		level, logf = expiryLevelError, r.opts.Errorf
	case left < ExpiryWarning:
		level = expiryLevelWarn
	}
	key := sha256.Sum256(cert.Leaf.Raw)
	r.expiryMu.Lock()
	logged := r.expiryLogged[key]
	if level > logged {
		r.expiryLogged[key] = level
	}
	r.expiryMu.Unlock()
	if level <= logged {
		return
	}
	at := notAfter.UTC().Format(time.RFC3339)
	if level == expiryLevelExpired {
		logf("TLS certificate %s expired %s ago (at %s); rotate SAGE_TLS_CERT and "+
			"SAGE_TLS_KEY", r.certFile, (-left).Round(time.Minute), at)
		return
	}
	logf("TLS certificate %s expires in %s (at %s); rotate SAGE_TLS_CERT and "+
		"SAGE_TLS_KEY before then", r.certFile, left.Round(time.Minute), at)
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
		r.failures.Add(1)
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
