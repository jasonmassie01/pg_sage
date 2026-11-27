package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/servertls"
	"github.com/pg-sage/sidecar/internal/servertls/servertlstest"
)

func TestWriteTLSMetrics_AbsentWithoutTLS(t *testing.T) {
	var b strings.Builder
	writeTLSMetrics(&b, nil)
	if b.String() != "" {
		t.Fatalf("plain HTTP wrote TLS metrics: %q", b.String())
	}
	if apiTLSExpiringFunc(nil) != nil {
		t.Fatal("plain HTTP has a tls_expiring probe")
	}
	startTLSExpiryWatch(context.Background(), nil, time.Millisecond) // must not panic
}

func TestWriteTLSMetrics_ExposesExpiryAndFailures(t *testing.T) {
	notAfter := time.Now().Add(20 * 24 * time.Hour).Truncate(time.Second)
	pair := servertlstest.WritePair(t, t.TempDir(), servertlstest.Options{Serial: 70,
		NotBefore: time.Now().Add(-time.Hour), NotAfter: notAfter})
	r, err := servertls.New(pair.CertPath, pair.KeyPath, servertls.Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Two forced reloads of a pair whose key file is gone.
	if err := os.Remove(pair.KeyPath); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := r.Reload(); err == nil {
			t.Fatal("Reload without a key succeeded")
		}
	}
	var b strings.Builder
	writeTLSMetrics(&b, r)
	want := []string{
		"# TYPE pg_sage_api_tls_cert_not_after_seconds gauge\n",
		fmt.Sprintf("pg_sage_api_tls_cert_not_after_seconds %d\n", notAfter.Unix()),
		"# TYPE pg_sage_api_tls_reload_failures_total counter\n",
		"pg_sage_api_tls_reload_failures_total 2\n",
	}
	for _, w := range want {
		if !strings.Contains(b.String(), w) {
			t.Errorf("metrics omit %q:\n%s", w, b.String())
		}
	}
	if probe := apiTLSExpiringFunc(r); probe == nil || probe() {
		t.Fatal("20-day certificate reported as expiring")
	}
}

type lockedLog struct {
	mu    sync.Mutex
	lines []string
}

func (l *lockedLog) logf(format string, args ...any) {
	l.mu.Lock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
	l.mu.Unlock()
}

func (l *lockedLog) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.lines)
}

// The watch re-checks a never-rotated certificate on its own, and stops
// with its context.
func TestStartTLSExpiryWatch_ChecksUntilCancelled(t *testing.T) {
	start := time.Now()
	pair := servertlstest.WritePair(t, t.TempDir(), servertlstest.Options{Serial: 72,
		NotBefore: start.Add(-time.Hour), NotAfter: start.Add(20 * 24 * time.Hour)})
	var mu sync.Mutex
	now := start
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	logs := &lockedLog{}
	r, err := servertls.New(pair.CertPath, pair.KeyPath, servertls.Options{Now: clock,
		Logf: logs.logf, Errorf: logs.logf})
	if err != nil || logs.count() != 0 {
		t.Fatalf("New: %v, logs %v", err, logs.lines)
	}
	mu.Lock()
	now = start.Add(8 * 24 * time.Hour) // 12 days left
	mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startTLSExpiryWatch(ctx, r, 5*time.Millisecond)
	deadline := time.Now().Add(5 * time.Second)
	for logs.count() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if logs.count() != 1 || !strings.Contains(logs.lines[0], "expires in") {
		t.Fatalf("watch did not warn once: %v", logs.lines)
	}
	cancel()
	time.Sleep(20 * time.Millisecond)
	mu.Lock()
	now = start.Add(25 * 24 * time.Hour) // expired
	mu.Unlock()
	time.Sleep(50 * time.Millisecond)
	if logs.count() != 1 {
		t.Fatalf("watch kept checking after cancel: %v", logs.lines)
	}
}

// CHECK-29: /metrics carries the TLS series when the API serves TLS and
// omits them when it serves plain HTTP.
func TestHandleMetrics_TLSSeriesOnlyWithTLS(t *testing.T) {
	preserveMainWiringGlobals(t)
	oldTLS := apiTLS
	t.Cleanup(func() { apiTLS = oldTLS })
	// Fleet mode with no manager: only the process-wide series are written.
	cfg = config.DefaultConfig()
	cfg.Mode = "fleet"
	pool, coll, anal, fleetMgr, fleetLLMBudget = nil, nil, nil, nil, nil
	notAfter := time.Now().Add(30 * 24 * time.Hour).Truncate(time.Second)
	pair := servertlstest.WritePair(t, t.TempDir(), servertlstest.Options{Serial: 73,
		NotBefore: time.Now().Add(-time.Hour), NotAfter: notAfter})
	r, err := servertls.New(pair.CertPath, pair.KeyPath, servertls.Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	apiTLS = nil
	if body := renderMetrics(t); strings.Contains(body, "pg_sage_api_tls_") {
		t.Fatalf("plain HTTP /metrics has TLS series:\n%s", body)
	}
	apiTLS = r
	body := renderMetrics(t)
	for _, want := range []string{
		fmt.Sprintf("pg_sage_api_tls_cert_not_after_seconds %d\n", notAfter.Unix()),
		"pg_sage_api_tls_reload_failures_total 0\n",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("TLS /metrics lacks %q", want)
		}
	}
}
