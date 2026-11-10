package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/auditchain"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/siem"
)

// No concurrency or Postgres tests: sinks are built once at start and the
// change does not touch the cursor store.

// siemPEMFiles writes a CA bundle and a matching certificate and key (the
// httptest server's) plus a garbage file, and returns their paths.
func siemPEMFiles(t *testing.T) (ca, cert, key, garbage string) {
	t.Helper()
	ts := httptest.NewTLSServer(http.NotFoundHandler())
	defer ts.Close()
	pair := ts.TLS.Certificates[0]
	keyDER, err := x509.MarshalPKCS8PrivateKey(pair.PrivateKey)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	dir := t.TempDir()
	write := func(name string, data []byte) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, data, 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		return p
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: pair.Certificate[0]})
	ca = write("ca.pem", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE",
		Bytes: ts.Certificate().Raw}))
	cert = write("client.pem", certPEM)
	key = write("client.key", pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY",
		Bytes: keyDER}))
	garbage = write("garbage.pem", []byte("this is not PEM\n"))
	return ca, cert, key, garbage
}

// A TLS file that is missing or not PEM stops the build, and the error
// names the sink and the path, for every sink type.
func TestBuildSIEMSinksTLSFileErrors(t *testing.T) {
	ca, cert, _, garbage := siemPEMFiles(t)
	missing := filepath.Join(t.TempDir(), "no-such-ca.pem")
	kinds := []config.AuditSIEMSink{
		{Name: "soc", Type: "http", URL: "https://siem.example.com/x"},
		{Name: "otel", Type: "otlp", URL: "https://collector:4318"},
		{Name: "rsys", Type: "syslog", Address: "127.0.0.1:6514", Network: "tls"},
	}
	for _, k := range kinds {
		cases := map[string]config.AuditSIEMSink{}
		c := k
		c.CAFile = missing
		cases[missing] = c
		c = k
		c.CAFile = garbage
		cases[garbage] = c
		c = k
		c.CAFile, c.ClientCertFile, c.ClientKeyFile = ca, cert, missing
		cases[missing+" (key)"] = c
		for path, s := range cases {
			_, err := buildSIEMSinks([]config.AuditSIEMSink{s})
			want := strings.TrimSuffix(path, " (key)")
			if err == nil || !strings.Contains(err.Error(), "sink "+k.Name) ||
				!strings.Contains(err.Error(), want) {
				t.Fatalf("%s with %s: err = %v", k.Name, path, err)
			}
		}
	}
}

// Valid files build every sink type, with or without a client pair.
func TestBuildSIEMSinksLoadsTLSFiles(t *testing.T) {
	ca, cert, key, _ := siemPEMFiles(t)
	sinks, err := buildSIEMSinks([]config.AuditSIEMSink{
		{Name: "soc", Type: "http", URL: "https://siem.example.com/x", CAFile: ca},
		{Name: "otel", Type: "otlp", URL: "https://collector:4318", CAFile: ca,
			ClientCertFile: cert, ClientKeyFile: key},
		{Name: "rsys", Type: "syslog", Address: "127.0.0.1:6514", Network: "tls",
			ClientCertFile: cert, ClientKeyFile: key},
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if len(sinks) != 3 || sinks[1].Sink.Name() != "otel" || sinks[2].Sink.Name() != "rsys" {
		t.Fatalf("sinks = %+v", sinks)
	}
}

// A token over plain http and a udp syslog sink each log exactly one
// warning at start; the API status carries token_over_http and the
// delivery guarantee.
func TestSIEMStartupWarningsAndStatus(t *testing.T) {
	t.Setenv("SAGE_TEST_SIEM_TOKEN", "s3cret-token")
	cfgs := []config.AuditSIEMSink{
		{Name: "plain", Type: "http", URL: "http://siem.internal/x",
			TokenEnv: "SAGE_TEST_SIEM_TOKEN"},
		{Name: "lab", Type: "otlp", URL: "http://lab:4318", TokenEnv: "SAGE_TEST_SIEM_TOKEN",
			AllowInsecureHTTP: true},
		{Name: "secure", Type: "http", URL: "https://siem.example.com/x",
			TokenEnv: "SAGE_TEST_SIEM_TOKEN"},
		{Name: "dgram", Type: "syslog", Address: "127.0.0.1:514", Network: "udp"},
		{Name: "stream", Type: "syslog", Address: "127.0.0.1:514", Network: "tcp"},
	}
	warns := siemStartupWarnings(cfgs)
	if len(warns) != 2 {
		t.Fatalf("startup warnings = %q, want one for plain and one for dgram", warns)
	}
	if !strings.Contains(warns[0], "plain") || !strings.Contains(warns[0], "token_over_http") ||
		strings.Contains(warns[0], "s3cret-token") {
		t.Fatalf("token warning = %q", warns[0])
	}
	if !strings.Contains(warns[1], "dgram") || !strings.Contains(warns[1], "best effort") {
		t.Fatalf("udp warning = %q", warns[1])
	}
	if got := siemStartupWarnings(nil); len(got) != 0 {
		t.Fatalf("no sinks: warnings = %q", got)
	}
	sinks, err := buildSIEMSinks(cfgs)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	st := siem.NewExporter(nil, sinks, nil, siem.Options{}, nil, nil).Status()
	wantDelivery := []string{"at_least_once", "at_least_once", "at_least_once",
		"best_effort", "at_least_once"}
	for i, s := range st {
		if s.Delivery != wantDelivery[i] {
			t.Fatalf("%s: delivery = %q, want %q", s.Name, s.Delivery, wantDelivery[i])
		}
		if wantWarn := i == 0; wantWarn != (len(s.Warnings) == 1) {
			t.Fatalf("%s: warnings = %v", s.Name, s.Warnings)
		}
	}
	body, err := json.Marshal(st[0])
	if err != nil || !strings.Contains(string(body), `"warnings":["token_over_http"]`) ||
		!strings.Contains(string(body), `"delivery":"at_least_once"`) {
		t.Fatalf("status JSON = %s (%v)", body, err)
	}
}

// buildSIEMSinks hands each sink the TLS configuration its files describe:
// a receiver signed by the test CA is refused without ca_file and trusted
// with it, for http, otlp and syslog alike (a sink built with nil TLS
// would fall back to the system roots and refuse it either way).
func TestBuildSIEMSinksWireTheCAIntoEverySink(t *testing.T) {
	ca, _, _, _ := siemPEMFiles(t)
	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter,
		_ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	ts.Config.ErrorLog = log.New(io.Discard, "", 0)
	ts.StartTLS()
	defer ts.Close()
	ln, err := tls.Listen("tcp", "127.0.0.1:0",
		&tls.Config{Certificates: ts.TLS.Certificates, MinVersion: tls.VersionTLS12})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go discardConns(ln)
	kinds := []config.AuditSIEMSink{
		{Name: "soc", Type: "http", URL: ts.URL, TimeoutSeconds: 2},
		{Name: "otel", Type: "otlp", URL: ts.URL, TimeoutSeconds: 2},
		{Name: "rsys", Type: "syslog", Address: ln.Addr().String(), Network: "tls",
			TimeoutSeconds: 2},
	}
	events := []siem.Event{siem.Map(siem.Record{Source: "control", Chain: "action_log",
		Link: auditchain.Link{Seq: 1, Op: "I", V: 1, At: time.Now()}})}
	for _, k := range kinds {
		for _, pinned := range []bool{false, true} {
			s := k
			if pinned {
				s.CAFile = ca
			}
			err := sendOnce(t, s, events)
			if pinned && err != nil {
				t.Fatalf("%s with ca_file: %v", k.Name, err)
			}
			if !pinned && (err == nil ||
				!strings.Contains(err.Error(), "certificate signed by unknown authority")) {
				t.Fatalf("%s without ca_file: err = %v", k.Name, err)
			}
		}
	}
}

// sendOnce builds one sink from its config and sends events through it.
func sendOnce(t *testing.T, s config.AuditSIEMSink, events []siem.Event) error {
	t.Helper()
	sinks, err := buildSIEMSinks([]config.AuditSIEMSink{s})
	if err != nil || len(sinks) != 1 {
		t.Fatalf("%s: build: %v (%d sinks)", s.Name, err, len(sinks))
	}
	if c, ok := sinks[0].Sink.(io.Closer); ok {
		defer c.Close()
	}
	return sinks[0].Sink.Send(context.Background(), events)
}

// discardConns accepts TLS connections and drains them until ln closes.
func discardConns(ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go func() {
			defer c.Close()
			_, _ = io.Copy(io.Discard, c)
		}()
	}
}
