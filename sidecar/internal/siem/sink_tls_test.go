package siem

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// No concurrency or Postgres tests: sinks are built once at start and the
// change does not touch the cursor store.

// LoadTLSConfig: TLS 1.2 at least; ca_file pins the roots to that bundle,
// none keeps the system roots; the client pair is loaded once.
func TestLoadTLSConfig(t *testing.T) {
	pki := newTestPKI(t)
	cfg, err := LoadTLSConfig(TLSFiles{})
	if err != nil || cfg == nil || cfg.MinVersion != tls.VersionTLS12 || cfg.RootCAs != nil ||
		len(cfg.Certificates) != 0 {
		t.Fatalf("no files: cfg = %+v, err = %v", cfg, err)
	}
	cfg, err = LoadTLSConfig(TLSFiles{CA: pki.caFile, Cert: pki.certFile, Key: pki.keyFile})
	if err != nil || cfg.RootCAs == nil || len(cfg.Certificates) != 1 ||
		cfg.MinVersion != tls.VersionTLS12 || cfg.InsecureSkipVerify {
		t.Fatalf("all files: cfg = %+v, err = %v", cfg, err)
	}
	missing := pki.caFile + ".missing"
	bad := map[string]TLSFiles{
		missing:               {CA: missing},
		pki.garbageFile:       {CA: pki.garbageFile},
		missing + " ":         {Cert: pki.certFile, Key: missing},
		pki.strangerKeyFile:   {Cert: pki.certFile, Key: pki.strangerKeyFile},
		"client_key_file":     {Cert: pki.certFile},
		"client_cert_file":    {Key: pki.keyFile},
		pki.garbageFile + " ": {Cert: pki.garbageFile, Key: pki.keyFile},
	}
	for want, files := range bad {
		cfg, err := LoadTLSConfig(files)
		if err == nil || cfg != nil || !strings.Contains(err.Error(), strings.TrimSpace(want)) {
			t.Fatalf("%+v: cfg = %v, err = %v, want it to name %s", files, cfg, err, want)
		}
	}
}

// tlsKind is one TLS-capable sink type with a receiver to test it against.
type tlsKind struct {
	name  string
	serve func(t *testing.T, cfg *tls.Config) (addr string, got <-chan string)
	sink  func(addr string, cfg *tls.Config) Sink
}

func tlsKinds() []tlsKind {
	return []tlsKind{
		{"http", serveHTTPS, func(addr string, cfg *tls.Config) Sink {
			return NewHTTPSink("soc", addr, "tok", 2*time.Second, cfg)
		}},
		{"otlp", serveHTTPS, func(addr string, cfg *tls.Config) Sink {
			return NewOTLPSink("otel", addr, "tok", 2*time.Second, cfg)
		}},
		{"syslog", serveSyslogTLS, func(addr string, cfg *tls.Config) Sink {
			return NewSyslogSink("rsys", "tls", addr, 2*time.Second, cfg)
		}},
	}
}

func serveHTTPS(t *testing.T, cfg *tls.Config) (string, <-chan string) {
	t.Helper()
	got := make(chan string, 10)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter,
		r *http.Request) {
		got <- r.URL.Path
		_, _ = w.Write([]byte(`{}`))
	}))
	srv.TLS = cfg
	srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv.URL, got
}

func serveSyslogTLS(t *testing.T, cfg *tls.Config) (string, <-chan string) {
	t.Helper()
	ln, err := tls.Listen("tcp", "127.0.0.1:0", cfg)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	got := make(chan string, 10)
	go readFramed(ln, got)
	return ln.Addr().String(), got
}

func receiverTLS(pki testPKI, mutual bool) *tls.Config {
	cfg := &tls.Config{Certificates: []tls.Certificate{pki.server},
		MinVersion: tls.VersionTLS12}
	if mutual {
		cfg.ClientAuth = tls.RequireAndVerifyClientCert
		cfg.ClientCAs = pki.clientCAs
	}
	return cfg
}

// sendWith loads files, sends one event and returns Send's error; it fails
// the test when the receiver's view disagrees with the sender's (a Send
// that succeeded while nothing arrived would advance the cursor past a
// lost event).
func sendWith(t *testing.T, k tlsKind, addr string, got <-chan string, files TLSFiles) error {
	t.Helper()
	cfg, err := LoadTLSConfig(files)
	if err != nil {
		t.Fatalf("%s: load %+v: %v", k.name, files, err)
	}
	s := k.sink(addr, cfg)
	if c, ok := s.(io.Closer); ok {
		defer c.Close()
	}
	err = s.Send(context.Background(), sampleEvents(1))
	select {
	case <-got:
		if err != nil {
			t.Fatalf("%s: delivered, but Send failed: %v", k.name, err)
		}
	case <-time.After(400 * time.Millisecond):
		if err == nil {
			t.Fatalf("%s %+v: Send succeeded but nothing arrived", k.name, files)
		}
	}
	return err
}

// A receiver signed by a private CA is refused without ca_file and trusted
// with it; ca_file pins the roots, so another CA's bundle is refused too.
func TestTLSSinksTrustTheConfiguredCA(t *testing.T) {
	pki := newTestPKI(t)
	for _, k := range tlsKinds() {
		addr, got := k.serve(t, receiverTLS(pki, false))
		err := sendWith(t, k, addr, got, TLSFiles{})
		if err == nil || !strings.Contains(err.Error(),
			"certificate signed by unknown authority") {
			t.Fatalf("%s without ca_file: err = %v", k.name, err)
		}
		if err := sendWith(t, k, addr, got, TLSFiles{CA: pki.otherCAFile}); err == nil {
			t.Fatalf("%s with another CA's bundle: delivered", k.name)
		}
		if err := sendWith(t, k, addr, got, TLSFiles{CA: pki.caFile}); err != nil {
			t.Fatalf("%s with ca_file: %v", k.name, err)
		}
	}
}

// mTLS: a receiver that requires a client certificate refuses a sink
// without a pair, and one with a pair from another CA, and takes the
// configured pair, for all three sink types.
func TestTLSSinksMutualTLS(t *testing.T) {
	pki := newTestPKI(t)
	for _, k := range tlsKinds() {
		addr, got := k.serve(t, receiverTLS(pki, true))
		if err := sendWith(t, k, addr, got, TLSFiles{CA: pki.caFile}); err == nil {
			t.Fatalf("%s without a client pair: delivered", k.name)
		}
		stranger := TLSFiles{CA: pki.caFile, Cert: pki.strangerCertFile,
			Key: pki.strangerKeyFile}
		if err := sendWith(t, k, addr, got, stranger); err == nil {
			t.Fatalf("%s with an untrusted client pair: delivered", k.name)
		}
		good := TLSFiles{CA: pki.caFile, Cert: pki.certFile, Key: pki.keyFile}
		if err := sendWith(t, k, addr, got, good); err != nil {
			t.Fatalf("%s with the client pair: %v", k.name, err)
		}
	}
}

// Status reports each sink's delivery guarantee (udp is best effort) and
// the warnings it was configured with, detached from the caller's slice.
func TestSinkStatusDeliveryAndWarnings(t *testing.T) {
	warn := []string{"token_over_http"}
	e := NewExporter(nil, []SinkConfig{
		{Sink: NewSyslogSink("dgram", "udp", "127.0.0.1:514", time.Second, nil)},
		{Sink: NewSyslogSink("stream", "", "127.0.0.1:514", time.Second, nil)},
		{Sink: NewSyslogSink("sealed", "tls", "127.0.0.1:6514", time.Second, nil)},
		{Sink: NewHTTPSink("soc", "http://siem/x", "t", time.Second, nil), Warnings: warn},
		{Sink: NewOTLPSink("otel", "https://otel", "", time.Second, nil)},
	}, nil, Options{}, nil, nil)
	want := []string{DeliveryBestEffort, DeliveryAtLeastOnce, DeliveryAtLeastOnce,
		DeliveryAtLeastOnce, DeliveryAtLeastOnce}
	if DeliveryBestEffort != "best_effort" || DeliveryAtLeastOnce != "at_least_once" {
		t.Fatalf("delivery names = %q, %q", DeliveryBestEffort, DeliveryAtLeastOnce)
	}
	warn[0] = "changed by the caller"
	st := e.Status()
	for i, s := range st {
		if s.Delivery != want[i] {
			t.Fatalf("%s: delivery = %q, want %q", s.Name, s.Delivery, want[i])
		}
		if (i == 3) != (len(s.Warnings) == 1) {
			t.Fatalf("%s: warnings = %v", s.Name, s.Warnings)
		}
	}
	if st[3].Warnings[0] != "token_over_http" {
		t.Fatalf("warnings alias the caller's slice: %v", st[3].Warnings)
	}
	st[3].Warnings[0] = "changed by a reader"
	if again := e.Status()[3].Warnings[0]; again != "token_over_http" {
		t.Fatalf("status aliases the exporter's warnings: %q", again)
	}
	body, err := json.Marshal(e.Status()[0])
	if err != nil || !strings.Contains(string(body), `"delivery":"best_effort"`) ||
		strings.Contains(string(body), `"warnings"`) {
		t.Fatalf("udp status JSON = %s (%v)", body, err)
	}
}

// Under TLS 1.2 the receiver checks the client certificate inside the
// handshake, so a refusal fails the dial without the TLS 1.3 confirm
// wait; mTLS still refuses a missing pair and takes the configured one.
func TestTLSSinksMutualTLS12(t *testing.T) {
	pki := newTestPKI(t)
	for _, k := range tlsKinds() {
		cfg := receiverTLS(pki, true)
		cfg.MaxVersion = tls.VersionTLS12
		addr, got := k.serve(t, cfg)
		if err := sendWith(t, k, addr, got, TLSFiles{CA: pki.caFile}); err == nil {
			t.Fatalf("%s over TLS 1.2 without a client pair: delivered", k.name)
		}
		good := TLSFiles{CA: pki.caFile, Cert: pki.certFile, Key: pki.keyFile}
		if err := sendWith(t, k, addr, got, good); err != nil {
			t.Fatalf("%s over TLS 1.2 with the client pair: %v", k.name, err)
		}
	}
}
