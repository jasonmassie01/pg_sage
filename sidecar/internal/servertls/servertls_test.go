package servertls

import (
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/servertls/servertlstest"
)

// fakeClock is a settable clock for the reload check interval.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

type logSink struct {
	mu    sync.Mutex
	lines []string
}

func (l *logSink) Logf(format string, args ...any) {
	l.mu.Lock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
	l.mu.Unlock()
}

func (l *logSink) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}

func newTestReloader(t *testing.T, pair servertlstest.Pair) (*Reloader, *fakeClock, *logSink) {
	t.Helper()
	clock := &fakeClock{now: time.Now()}
	logs := &logSink{}
	r, err := New(pair.CertPath, pair.KeyPath, Options{
		CheckInterval: time.Second, Now: clock.Now, Logf: logs.Logf})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return r, clock, logs
}

func servedSerial(t *testing.T, r *Reloader) int64 {
	t.Helper()
	cert, err := r.GetCertificate(&tls.ClientHelloInfo{})
	if err != nil {
		t.Fatalf("GetCertificate: %v", err)
	}
	if cert.Leaf == nil {
		t.Fatal("served certificate has no parsed leaf")
	}
	return cert.Leaf.SerialNumber.Int64()
}

func TestNew_ValidPairServesItWithSaneMinimumVersion(t *testing.T) {
	pair := servertlstest.WritePair(t, t.TempDir(), servertlstest.Valid(101))
	r, _, _ := newTestReloader(t, pair)
	if got := servedSerial(t, r); got != 101 {
		t.Fatalf("serial = %d, want 101", got)
	}
	cfg := r.Config()
	if cfg.MinVersion != tls.VersionTLS12 || MinVersion != tls.VersionTLS12 {
		t.Fatalf("MinVersion = %x, want TLS 1.2", cfg.MinVersion)
	}
	if cfg.GetCertificate == nil || len(cfg.Certificates) != 0 {
		t.Fatal("config must serve through GetCertificate so reloads take effect")
	}
}

func TestNew_RejectsBadPairsWithClearErrors(t *testing.T) {
	dir := t.TempDir()
	good := servertlstest.WritePair(t, dir, servertlstest.Valid(1))
	otherDir := t.TempDir()
	other := servertlstest.WritePair(t, otherDir, servertlstest.Valid(2))
	garbage := filepath.Join(dir, "garbage.pem")
	if err := os.WriteFile(garbage, []byte("not pem"), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := map[string]struct {
		cert, key string
		want      []string
	}{
		"mismatched key": {good.CertPath, other.KeyPath,
			[]string{"does not match", good.CertPath, other.KeyPath}},
		"missing cert": {filepath.Join(dir, "nope.pem"), good.KeyPath,
			[]string{"SAGE_TLS_CERT", "nope.pem"}},
		"missing key": {good.CertPath, filepath.Join(dir, "nokey.pem"),
			[]string{"SAGE_TLS_KEY", "nokey.pem"}},
		"garbage cert": {garbage, good.KeyPath, []string{"garbage.pem"}},
		"empty paths":  {"", "", []string{"SAGE_TLS_CERT", "SAGE_TLS_KEY"}},
		"cert only":    {good.CertPath, "", []string{"SAGE_TLS_KEY"}},
	}
	for name, tc := range cases {
		_, err := New(tc.cert, tc.key, Options{})
		if err == nil {
			t.Errorf("%s: New accepted the pair", name)
			continue
		}
		for _, want := range tc.want {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%s: error %q lacks %q", name, err, want)
			}
		}
	}
}

func TestNew_RejectsExpiredAndNotYetValidCertificates(t *testing.T) {
	expired := servertlstest.Options{Serial: 3, NotBefore: time.Now().Add(-48 * time.Hour),
		NotAfter: time.Now().Add(-time.Hour)}
	future := servertlstest.Options{Serial: 4, NotBefore: time.Now().Add(time.Hour),
		NotAfter: time.Now().Add(48 * time.Hour)}
	pair := servertlstest.WritePair(t, t.TempDir(), expired)
	if _, err := New(pair.CertPath, pair.KeyPath, Options{}); !errors.Is(err, ErrExpired) {
		t.Errorf("expired: err = %v, want ErrExpired", err)
	}
	pair = servertlstest.WritePair(t, t.TempDir(), future)
	if _, err := New(pair.CertPath, pair.KeyPath, Options{}); !errors.Is(err, ErrNotYetValid) {
		t.Errorf("not yet valid: err = %v, want ErrNotYetValid", err)
	}
}

// Boundary: a certificate expiring within the warning window loads but warns.
func TestNew_WarnsWhenCertificateExpiresSoon(t *testing.T) {
	soon := servertlstest.Options{Serial: 5, NotBefore: time.Now().Add(-time.Hour),
		NotAfter: time.Now().Add(24 * time.Hour)}
	pair := servertlstest.WritePair(t, t.TempDir(), soon)
	logs := &logSink{}
	if _, err := New(pair.CertPath, pair.KeyPath, Options{Logf: logs.Logf}); err != nil {
		t.Fatalf("New: %v", err)
	}
	if !strings.Contains(logs.String(), "expires") {
		t.Fatalf("no expiry warning: %q", logs.String())
	}
	later := servertlstest.Options{Serial: 6, NotBefore: time.Now().Add(-time.Hour),
		NotAfter: time.Now().Add(90 * 24 * time.Hour)}
	pair = servertlstest.WritePair(t, t.TempDir(), later)
	logs = &logSink{}
	if _, err := New(pair.CertPath, pair.KeyPath, Options{Logf: logs.Logf}); err != nil {
		t.Fatalf("New: %v", err)
	}
	if logs.String() != "" {
		t.Fatalf("90-day certificate warned: %q", logs.String())
	}
}

func TestReload_PicksUpRotatedCertificateAfterInterval(t *testing.T) {
	dir := t.TempDir()
	pair := servertlstest.WritePair(t, dir, servertlstest.Valid(10))
	r, clock, logs := newTestReloader(t, pair)
	rotated := servertlstest.WritePair(t, dir, servertlstest.Valid(11))
	servertlstest.BumpMtime(t, rotated, time.Minute)

	// Within the check interval the old certificate keeps serving.
	clock.Advance(500 * time.Millisecond)
	if got := servedSerial(t, r); got != 10 {
		t.Fatalf("before interval: serial = %d, want 10", got)
	}
	clock.Advance(time.Second)
	if got := servedSerial(t, r); got != 11 {
		t.Fatalf("after interval: serial = %d, want 11", got)
	}
	if !strings.Contains(logs.String(), "reloaded") {
		t.Fatalf("reload not logged: %q", logs.String())
	}
}

func TestReload_UnchangedFilesAreNotReparsed(t *testing.T) {
	pair := servertlstest.WritePair(t, t.TempDir(), servertlstest.Valid(20))
	r, clock, logs := newTestReloader(t, pair)
	for i := 0; i < 5; i++ {
		clock.Advance(2 * time.Second)
		if got := servedSerial(t, r); got != 20 {
			t.Fatalf("serial = %d", got)
		}
	}
	if strings.Contains(logs.String(), "reloaded") {
		t.Fatalf("unchanged files reloaded: %q", logs.String())
	}
}

func TestReload_BadReplacementKeepsServingTheOldCertificate(t *testing.T) {
	dir := t.TempDir()
	pair := servertlstest.WritePair(t, dir, servertlstest.Valid(30))
	r, clock, logs := newTestReloader(t, pair)
	// Half-written rotation: a new certificate with the old key.
	other := servertlstest.WritePair(t, t.TempDir(), servertlstest.Valid(31))
	data, err := os.ReadFile(other.CertPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pair.CertPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	servertlstest.BumpMtime(t, pair, time.Minute)
	clock.Advance(2 * time.Second)
	if got := servedSerial(t, r); got != 30 {
		t.Fatalf("serial = %d, want the old 30 after a failed reload", got)
	}
	if !strings.Contains(logs.String(), "keeping the current certificate") {
		t.Fatalf("failed reload not logged: %q", logs.String())
	}
	if err := r.Reload(); err == nil {
		t.Fatal("forced Reload of a mismatched pair succeeded")
	}
}

func TestReload_ForcedReloadStateTransitions(t *testing.T) {
	dir := t.TempDir()
	pair := servertlstest.WritePair(t, dir, servertlstest.Valid(40))
	r, _, _ := newTestReloader(t, pair)
	servertlstest.WritePair(t, dir, servertlstest.Valid(41))
	if err := r.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if got := servedSerial(t, r); got != 41 {
		t.Fatalf("serial = %d, want 41", got)
	}
	if err := os.Remove(pair.KeyPath); err != nil {
		t.Fatal(err)
	}
	if err := r.Reload(); err == nil || !strings.Contains(err.Error(), "SAGE_TLS_KEY") {
		t.Fatalf("Reload without key: err = %v", err)
	}
	if got := servedSerial(t, r); got != 41 {
		t.Fatalf("serial after failed reload = %d, want 41", got)
	}
}

func TestReload_ConcurrentHandshakesDuringRotation(t *testing.T) {
	dir := t.TempDir()
	pair := servertlstest.WritePair(t, dir, servertlstest.Valid(50))
	r, clock, _ := newTestReloader(t, pair)
	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cert, err := r.GetCertificate(&tls.ClientHelloInfo{})
			if err != nil || cert == nil || cert.Leaf == nil {
				errs <- fmt.Errorf("handshake cert: %v", err)
				return
			}
			if s := cert.Leaf.SerialNumber.Int64(); s != 50 && s != 51 {
				errs <- fmt.Errorf("serial %d", s)
			}
		}()
		if i == 10 {
			rotated := servertlstest.WritePair(t, dir, servertlstest.Valid(51))
			servertlstest.BumpMtime(t, rotated, time.Minute)
			clock.Advance(2 * time.Second)
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func TestServe_RealHandshakeHonoursMinimumVersion(t *testing.T) {
	pair := servertlstest.WritePair(t, t.TempDir(), servertlstest.Valid(60))
	r, _, _ := newTestReloader(t, pair)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") }))
	srv.TLS = r.Config()
	srv.StartTLS()
	defer srv.Close()

	client := func(maxVersion uint16) *http.Client {
		return &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true, //nolint:gosec // test
				MaxVersion: maxVersion}}}
	}
	resp, err := client(tls.VersionTLS13).Get(srv.URL)
	if err != nil {
		t.Fatalf("TLS 1.3 request: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if string(body) != "ok" || resp.TLS == nil ||
		resp.TLS.PeerCertificates[0].SerialNumber.Int64() != 60 {
		t.Fatalf("body %q, tls %+v", body, resp.TLS)
	}
	if _, err := client(tls.VersionTLS11).Get(srv.URL); err == nil {
		t.Fatal("a TLS 1.1 client completed a handshake")
	}
}
