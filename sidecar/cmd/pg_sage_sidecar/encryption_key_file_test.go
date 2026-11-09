package main

import (
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
)

// security-08: without encryption_key, a writable data_dir gets a
// generated key file.

type warnLog struct {
	mu    sync.Mutex
	lines []string
}

func (w *warnLog) logf(_, msg string, args ...any) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.lines = append(w.lines, fmt.Sprintf(msg, args...))
}

func (w *warnLog) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return strings.Join(w.lines, "\n")
}

// requirePOSIXModes skips tests that read a key file back: Windows reports
// mode 0666 for every writable file, so the owner-only check cannot pass.
func requirePOSIXModes(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX file modes are not enforced on Windows (every writable " +
			"file reads as 0666); the owner-only key file check runs on Linux CI")
	}
}

func keyConfig(dataDir string) *config.Config {
	c := config.DefaultConfig()
	c.DataDir = dataDir
	return c
}

func TestResolveEncryptionKey_EmptyDataDirIsPendingThenGenerated(t *testing.T) {
	dir := t.TempDir()
	c := keyConfig(dir)
	res, err := resolveEncryptionKey(c, (&warnLog{}).logf)
	if err != nil || res.Source != keySourcePending {
		t.Fatalf("resolve = %+v, %v; want pending", res, err)
	}
	if res.Path != filepath.Join(dir, "encryption.key") {
		t.Fatalf("path = %q", res.Path)
	}
	if c.EncryptionKey != "" {
		t.Fatal("resolve set a key before the database check")
	}
	if _, err := os.Stat(res.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("resolve created the file before the database check: %v", err)
	}
	if err := res.settle("", c); err != nil {
		t.Fatalf("settle: %v", err)
	}
	raw, err := base64.StdEncoding.DecodeString(c.EncryptionKey)
	if err != nil || len(raw) != 32 || res.Source != keySourceGenerated {
		t.Fatalf("generated key = %d bytes, %v; source %s", len(raw), err, res.Source)
	}
	onDisk, err := os.ReadFile(res.Path)
	if err != nil || strings.TrimSpace(string(onDisk)) != c.EncryptionKey {
		t.Fatalf("file holds %q, %v; want the generated key", onDisk, err)
	}
}

func TestResolveEncryptionKey_FileIsOwnerOnlyAndReused(t *testing.T) {
	requirePOSIXModes(t)
	dir := t.TempDir()
	first := keyConfig(dir)
	res, err := resolveEncryptionKey(first, (&warnLog{}).logf)
	if err != nil {
		t.Fatal(err)
	}
	if err := res.settle("", first); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(res.Path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("key file mode = %v, %v; want 0600", info.Mode().Perm(), err)
	}
	second := keyConfig(dir)
	again, err := resolveEncryptionKey(second, (&warnLog{}).logf)
	if err != nil || again.Source != keySourceGenerated {
		t.Fatalf("second start = %+v, %v; want generated", again, err)
	}
	if second.EncryptionKey != first.EncryptionKey {
		t.Fatal("second start did not reuse the generated key")
	}
}

func TestResolveEncryptionKey_RefusesGroupReadableFile(t *testing.T) {
	requirePOSIXModes(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "encryption.key")
	if err := os.WriteFile(path, []byte(validKeyText()), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	c := keyConfig(dir)
	_, err := resolveEncryptionKey(c, (&warnLog{}).logf)
	if err == nil || !strings.Contains(err.Error(), path) ||
		!strings.Contains(err.Error(), "mode 600") {
		t.Fatalf("err = %v, want a named error about mode 600 on %s", err, path)
	}
	if c.EncryptionKey != "" {
		t.Fatal("a refused key file still set encryption_key")
	}
}

func TestResolveEncryptionKey_RefusesOversizedFile(t *testing.T) {
	requirePOSIXModes(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "encryption.key")
	if err := os.WriteFile(path, []byte(strings.Repeat("A", 4097)), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := resolveEncryptionKey(keyConfig(dir), (&warnLog{}).logf)
	if err == nil || !strings.Contains(err.Error(), "exceeds") ||
		!strings.Contains(err.Error(), path) {
		t.Fatalf("err = %v, want a named size error", err)
	}
}

func TestResolveEncryptionKey_RefusesMalformedKey(t *testing.T) {
	requirePOSIXModes(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "encryption.key")
	short := base64.StdEncoding.EncodeToString([]byte("sixteen-byte-key"))
	for _, body := range []string{"", "not base64 !!", short} {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := resolveEncryptionKey(keyConfig(dir), (&warnLog{}).logf)
		if err == nil || !strings.Contains(err.Error(), "32 bytes") {
			t.Fatalf("body %q: err = %v, want a 32-byte key error", body, err)
		}
	}
}

func validKeyText() string {
	return base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
}

func TestResolveEncryptionKey_ConfiguredKeyWinsWithWarning(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "encryption.key")
	if err := os.WriteFile(path, []byte(validKeyText()), 0o600); err != nil {
		t.Fatal(err)
	}
	c := keyConfig(dir)
	c.EncryptionKey = "operator-passphrase"
	warns := &warnLog{}
	res, err := resolveEncryptionKey(c, warns.logf)
	if err != nil || res.Source != keySourceConfigured {
		t.Fatalf("resolve = %+v, %v; want configured", res, err)
	}
	if c.EncryptionKey != "operator-passphrase" {
		t.Fatal("the key file replaced a configured encryption_key")
	}
	if !strings.Contains(warns.String(), path) {
		t.Fatalf("warnings = %q, want one naming the ignored %s", warns, path)
	}
	if strings.Contains(warns.String(), "operator-passphrase") {
		t.Fatal("warning holds the configured key")
	}
}

func TestResolveEncryptionKey_ConfiguredKeyWithoutDataDir(t *testing.T) {
	c := keyConfig("")
	c.EncryptionKey = "operator-passphrase"
	res, err := resolveEncryptionKey(c, (&warnLog{}).logf)
	if err != nil || res.Source != keySourceConfigured || res.Path != "" {
		t.Fatalf("resolve = %+v, %v; want configured, no path", res, err)
	}
}

func TestResolveEncryptionKey_NoDataDirWarnsPlaintext(t *testing.T) {
	c := keyConfig("")
	warns := &warnLog{}
	res, err := resolveEncryptionKey(c, warns.logf)
	if err != nil || res.Source != keySourceNone || c.EncryptionKey != "" {
		t.Fatalf("resolve = %+v, %v, key %q; want none", res, err, c.EncryptionKey)
	}
	for _, want := range []string{"plaintext", "encryption_key", "data_dir"} {
		if !strings.Contains(warns.String(), want) {
			t.Errorf("warning %q does not mention %s", warns, want)
		}
	}
	if err := res.settle("", c); err != nil || c.EncryptionKey != "" {
		t.Fatalf("settle with no data_dir = %v, key %q; want nothing", err, c.EncryptionKey)
	}
}

func TestResolveEncryptionKey_UnwritableDataDirWarns(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "a-file")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := keyConfig(filepath.Join(blocker, "sub"))
	warns := &warnLog{}
	res, err := resolveEncryptionKey(c, warns.logf)
	if err != nil || res.Source != keySourceNone {
		t.Fatalf("resolve = %+v, %v; want none", res, err)
	}
	if !strings.Contains(warns.String(), "not writable") {
		t.Fatalf("warnings = %q, want not writable", warns)
	}
}

func TestResolveEncryptionKey_CreatesMissingDataDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "var", "lib", "pg_sage")
	res, err := resolveEncryptionKey(keyConfig(dir), (&warnLog{}).logf)
	if err != nil || res.Source != keySourcePending {
		t.Fatalf("resolve = %+v, %v; want pending", res, err)
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		t.Fatalf("data_dir not created: %v", err)
	}
}

// Error propagation: a recorded key id with no file is a lost key; pg_sage
// must not generate a replacement.
func TestSettle_RecordedIDWithoutFileTripsGuard(t *testing.T) {
	for _, source := range []keySource{keySourcePending, keySourceNone} {
		dir := t.TempDir()
		res := keyResolution{Source: source, Path: filepath.Join(dir, "encryption.key")}
		c := keyConfig(dir)
		err := res.settle("0123456789abcdef", c)
		if !errors.Is(err, errGeneratedKeyMissing) {
			t.Fatalf("%s: err = %v, want errGeneratedKeyMissing", source, err)
		}
		for _, want := range []string{res.Path, "0123456789abcdef"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%s: err %q does not name %s", source, err, want)
			}
		}
		if _, statErr := os.Stat(res.Path); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("%s: a replacement key file was generated", source)
		}
		if c.EncryptionKey != "" {
			t.Fatalf("%s: a key was set", source)
		}
	}
}

func TestCheckGeneratedKeyID(t *testing.T) {
	gen := keyResolution{Source: keySourceGenerated, Path: "/d/encryption.key"}
	conf := keyResolution{Source: keySourceConfigured}
	cases := []struct {
		name             string
		res              keyResolution
		recorded, active string
		wantRecord       bool
		wantErr          bool
	}{
		{"generated, first start", gen, "", "aaaa", true, false},
		{"generated, same id", gen, "aaaa", "aaaa", false, false},
		{"generated, other id", gen, "bbbb", "aaaa", false, true},
		{"configured, other id", conf, "bbbb", "aaaa", false, false},
		{"configured, nothing recorded", conf, "", "aaaa", false, false},
	}
	for _, tc := range cases {
		record, err := checkGeneratedKeyID(tc.res, tc.recorded, tc.active)
		if record != tc.wantRecord || (err != nil) != tc.wantErr {
			t.Errorf("%s: record %v err %v; want %v / err %v",
				tc.name, record, err, tc.wantRecord, tc.wantErr)
		}
		if tc.wantErr && (!errors.Is(err, errGeneratedKeyMissing) ||
			!strings.Contains(err.Error(), tc.recorded)) {
			t.Errorf("%s: err %v must be errGeneratedKeyMissing naming %s",
				tc.name, err, tc.recorded)
		}
	}
}

// Concurrency: two starts racing on an empty data_dir end with one file
// and one key (O_EXCL; the loser reads the winner's file).
func TestCreateEncryptionKeyFile_RaceYieldsOneKey(t *testing.T) {
	requirePOSIXModes(t)
	path := filepath.Join(t.TempDir(), "encryption.key")
	keys := make([]string, 8)
	errs := make([]error, 8)
	var wg sync.WaitGroup
	for i := range keys {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			keys[i], errs[i] = createEncryptionKeyFile(path)
		}(i)
	}
	wg.Wait()
	for i := range keys {
		if errs[i] != nil || keys[i] != keys[0] {
			t.Fatalf("starter %d got %q, %v; want the one key %q", i, keys[i], errs[i], keys[0])
		}
	}
}

func TestApplyGeneratedKey_KeepsReloadCandidatesStable(t *testing.T) {
	gen := keyResolution{Source: keySourceGenerated, key: "generated-key"}
	candidate := keyConfig("/d")
	gen.applyTo(candidate)
	if candidate.EncryptionKey != "generated-key" {
		t.Fatalf("candidate key = %q, want the generated key", candidate.EncryptionKey)
	}
	configured := keyConfig("/d")
	configured.EncryptionKey = "operator"
	gen.applyTo(configured)
	if configured.EncryptionKey != "operator" {
		t.Fatal("a generated key replaced a configured one on reload")
	}
	none := keyConfig("")
	(keyResolution{Source: keySourceNone}).applyTo(none)
	if none.EncryptionKey != "" {
		t.Fatal("no source set a key")
	}
}
