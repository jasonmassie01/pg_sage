package servertls

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/servertls/servertlstest"
)

// No new concurrency test: CheckExpiry and the failure counter share the
// reloader's locks, which TestReload_ConcurrentHandshakesDuringRotation
// already exercises.

const day = 24 * time.Hour

type expiryHarness struct {
	r          *Reloader
	clock      *fakeClock
	warn, errs *logSink
}

// newExpiryReloader loads a certificate that expires `left` after the fake
// clock's start, logging warnings and errors to separate sinks.
func newExpiryReloader(t *testing.T, dir string, serial int64, left time.Duration) expiryHarness {
	t.Helper()
	start := time.Now()
	pair := servertlstest.WritePair(t, dir, servertlstest.Options{Serial: serial,
		NotBefore: start.Add(-time.Hour), NotAfter: start.Add(left)})
	h := expiryHarness{clock: &fakeClock{now: start}, warn: &logSink{}, errs: &logSink{}}
	r, err := New(pair.CertPath, pair.KeyPath, Options{CheckInterval: time.Second,
		Now: h.clock.Now, Logf: h.warn.Logf, Errorf: h.errs.Logf})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h.r = r
	return h
}

func (h expiryHarness) hourlyChecks(n int) {
	for i := 0; i < n; i++ {
		h.clock.Advance(time.Hour)
		h.r.CheckExpiry()
	}
}

func countLines(l *logSink, sub string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, line := range l.lines {
		if strings.Contains(line, sub) {
			n++
		}
	}
	return n
}

func TestCheckExpiry_ThirteenDaysWarnsOnceOverHourlyChecks(t *testing.T) {
	h := newExpiryReloader(t, t.TempDir(), 1, 13*day)
	h.hourlyChecks(2)
	if got := countLines(h.warn, "expires in"); got != 1 {
		t.Fatalf("warnings = %d, want exactly 1: %q", got, h.warn.String())
	}
	if !strings.Contains(h.warn.String(), "rotate SAGE_TLS_CERT") {
		t.Fatalf("warning is not actionable: %q", h.warn.String())
	}
	if h.errs.String() != "" {
		t.Fatalf("13 days left logged an error: %q", h.errs.String())
	}
	if !h.r.Expiring() {
		t.Fatal("Expiring() = false with 13 days left")
	}
}

func TestCheckExpiry_TwoDaysErrorsOnceAndDoesNotWarn(t *testing.T) {
	h := newExpiryReloader(t, t.TempDir(), 2, 2*day)
	h.hourlyChecks(2)
	if got := countLines(h.errs, "expires in"); got != 1 {
		t.Fatalf("errors = %d, want exactly 1: %q", got, h.errs.String())
	}
	if h.warn.String() != "" {
		t.Fatalf("2 days left also warned: %q", h.warn.String())
	}
}

func TestCheckExpiry_FifteenDaysIsSilent(t *testing.T) {
	h := newExpiryReloader(t, t.TempDir(), 3, 15*day)
	h.hourlyChecks(2)
	if h.warn.String() != "" || h.errs.String() != "" {
		t.Fatalf("15 days left logged: warn %q, err %q", h.warn.String(), h.errs.String())
	}
	if h.r.Expiring() {
		t.Fatal("Expiring() = true with 15 days left")
	}
}

// Boundary: exactly ExpiryWarning left is silent; one second less warns.
func TestCheckExpiry_WarningThresholdIsExclusive(t *testing.T) {
	h := newExpiryReloader(t, t.TempDir(), 4, 30*day)
	h.clock.now = h.r.NotAfter().Add(-ExpiryWarning)
	h.r.CheckExpiry()
	if h.warn.String() != "" || h.r.Expiring() {
		t.Fatalf("exactly 14 days left warned (%q) or is expiring", h.warn.String())
	}
	h.clock.Advance(time.Second)
	h.r.CheckExpiry()
	if got := countLines(h.warn, "expires in"); got != 1 || !h.r.Expiring() {
		t.Fatalf("14 days less 1s: warnings = %d, expiring = %v", got, h.r.Expiring())
	}
}

// State transitions on one never-rotated certificate: silent, warn, error,
// expired, each exactly once.
func TestCheckExpiry_NeverRotatedCertificateEscalatesOncePerLevel(t *testing.T) {
	h := newExpiryReloader(t, t.TempDir(), 5, 15*day)
	notAfter := h.r.NotAfter()
	if d := notAfter.Sub(h.clock.Now()); d < 15*day-time.Second || d > 15*day {
		t.Fatalf("NotAfter() is %s from now, want 15 days", d)
	}
	steps := []struct {
		at         time.Duration // before NotAfter
		warn, errs int
	}{
		{13 * day, 1, 0}, {12 * day, 1, 0}, {2 * day, 1, 1}, {day, 1, 1},
		{-time.Hour, 1, 2}, {-2 * day, 1, 2},
	}
	for _, s := range steps {
		h.clock.now = notAfter.Add(-s.at)
		h.r.CheckExpiry()
		gotWarn := countLines(h.warn, "expires in")
		gotErr := countLines(h.errs, "expire")
		if gotWarn != s.warn || gotErr != s.errs {
			t.Fatalf("at NotAfter-%s: warn %d err %d, want %d/%d; err log %q",
				s.at, gotWarn, gotErr, s.warn, s.errs, h.errs.String())
		}
	}
	if countLines(h.errs, "expired") != 1 {
		t.Fatalf("expiry itself not reported once: %q", h.errs.String())
	}
}

// A rotated-in certificate is a new certificate: its own levels log again.
func TestCheckExpiry_RotatedCertificateLogsItsOwnLevels(t *testing.T) {
	dir := t.TempDir()
	h := newExpiryReloader(t, dir, 6, 13*day)
	rotated := servertlstest.WritePair(t, dir, servertlstest.Options{Serial: 7,
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(13 * day)})
	servertlstest.BumpMtime(t, rotated, time.Minute)
	if err := h.r.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	h.hourlyChecks(2)
	if got := countLines(h.warn, "expires in"); got != 2 {
		t.Fatalf("warnings = %d, want one per certificate: %q", got, h.warn.String())
	}
}

// Error propagation: each failed reload is counted and the old
// certificate keeps serving.
func TestReloadFailures_CountsBadReloadsAndKeepsServing(t *testing.T) {
	dir := t.TempDir()
	pair := servertlstest.WritePair(t, dir, servertlstest.Valid(50))
	r, clock, _ := newTestReloader(t, pair)
	if got := r.ReloadFailures(); got != 0 {
		t.Fatalf("fresh reloader failures = %d, want 0", got)
	}
	other := servertlstest.WritePair(t, t.TempDir(), servertlstest.Valid(51))
	data, err := os.ReadFile(other.CertPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pair.CertPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := r.Reload(); err == nil {
			t.Fatal("Reload of a mismatched pair succeeded")
		}
	}
	if got := r.ReloadFailures(); got != 3 {
		t.Fatalf("failures after 3 bad reloads = %d, want 3", got)
	}
	// The handshake-driven reload counts its failure too.
	servertlstest.BumpMtime(t, pair, time.Minute)
	clock.Advance(2 * time.Second)
	if got := servedSerial(t, r); got != 50 {
		t.Fatalf("serial = %d, want the old 50", got)
	}
	if got := r.ReloadFailures(); got != 4 {
		t.Fatalf("failures after a bad handshake reload = %d, want 4", got)
	}
	if err := os.Remove(pair.KeyPath); err != nil {
		t.Fatal(err)
	}
	clock.Advance(2 * time.Second)
	servedSerial(t, r)
	if got := r.ReloadFailures(); got != 5 {
		t.Fatalf("failures after a missing key = %d, want 5", got)
	}
}

// A successful reload is not a failure.
func TestReloadFailures_GoodReloadDoesNotCount(t *testing.T) {
	dir := t.TempDir()
	pair := servertlstest.WritePair(t, dir, servertlstest.Valid(60))
	r, _, _ := newTestReloader(t, pair)
	servertlstest.WritePair(t, dir, servertlstest.Valid(61))
	if err := r.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if got := r.ReloadFailures(); got != 0 {
		t.Fatalf("failures = %d after a good reload", got)
	}
}
