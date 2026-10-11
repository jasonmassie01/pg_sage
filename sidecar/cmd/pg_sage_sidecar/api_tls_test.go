package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/servertls/servertlstest"
)

func TestNewAPITLS_DisabledWithoutAPair(t *testing.T) {
	for _, c := range []*config.Config{nil, {}, {TLSCert: "only-cert.pem"}} {
		r, err := newAPITLS(c)
		if r != nil || err != nil {
			t.Errorf("config %+v: reloader %v, err %v; want plain HTTP", c, r, err)
		}
	}
	if got := apiListenLog("127.0.0.1:8080", nil); got != "listening on 127.0.0.1:8080" {
		t.Fatalf("plain log = %q", got)
	}
}

func TestNewAPITLS_LoadsPairAndMarksListener(t *testing.T) {
	pair := servertlstest.WritePair(t, t.TempDir(), servertlstest.Valid(9))
	r, err := newAPITLS(&config.Config{TLSCert: pair.CertPath, TLSKey: pair.KeyPath})
	if err != nil || r == nil {
		t.Fatalf("reloader %v, err %v", r, err)
	}
	if got := apiListenLog(":8443", r); got != "listening on :8443 (TLS)" {
		t.Fatalf("TLS log = %q", got)
	}
}

func TestNewAPITLS_BadPairIsAnError(t *testing.T) {
	pair := servertlstest.WritePair(t, t.TempDir(), servertlstest.Valid(9))
	other := servertlstest.WritePair(t, t.TempDir(), servertlstest.Valid(10))
	_, err := newAPITLS(&config.Config{TLSCert: pair.CertPath, TLSKey: other.KeyPath})
	if err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("err = %v", err)
	}
}

func TestSecretGetenv_ResolvesFilesAndFallsBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dsn")
	if err := os.WriteFile(path, []byte("postgres://from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SAGE_META_DB", "")
	t.Setenv("SAGE_META_DB_FILE", path)
	t.Setenv("SAGE_HISTORY_TEST_OTHER", "plain")
	getenv, err := secretGetenv("SAGE_META_DB")
	if err != nil {
		t.Fatalf("secretGetenv: %v", err)
	}
	if got := getenv("SAGE_META_DB"); got != "postgres://from-file" {
		t.Fatalf("SAGE_META_DB = %q", got)
	}
	if got := getenv("SAGE_HISTORY_TEST_OTHER"); got != "plain" {
		t.Fatalf("fallback = %q", got)
	}
	t.Setenv("SAGE_META_DB", "postgres://env")
	if _, err := secretGetenv("SAGE_META_DB"); err == nil ||
		!strings.Contains(err.Error(), "SAGE_META_DB_FILE") {
		t.Fatalf("both set: err = %v", err)
	}
}
