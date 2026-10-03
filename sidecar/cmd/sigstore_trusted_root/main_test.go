package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/benchsig"
)

// The release workflow refreshes the embedded Sigstore trusted root
// before building; a fetched root that cannot verify keyless signatures
// must never replace the embedded one. (The TUF fetch itself needs the
// network and is exercised by the release workflow.)

func TestRunWritesAUsableTrustedRoot(t *testing.T) {
	out := filepath.Join(t.TempDir(), "trusted_root.json")
	good := benchsig.EmbeddedTrustedRoot()
	var stderr bytes.Buffer
	if code := run([]string{"-o", out}, &stderr,
		func() ([]byte, error) { return good, nil }); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	got, err := os.ReadFile(out)
	if err != nil || !bytes.Equal(got, good) {
		t.Fatalf("written root differs (%v)", err)
	}
}

func TestRunKeepsTheOldRootOnFailure(t *testing.T) {
	out := filepath.Join(t.TempDir(), "trusted_root.json")
	if err := os.WriteFile(out, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := map[string]func() ([]byte, error){
		"fetch fails":   func() ([]byte, error) { return nil, errors.New("TUF unreachable") },
		"not a root":    func() ([]byte, error) { return []byte("{}"), nil },
		"empty payload": func() ([]byte, error) { return nil, nil },
	}
	for name, fetch := range cases {
		var stderr bytes.Buffer
		if code := run([]string{"-o", out}, &stderr, fetch); code != 1 ||
			!strings.Contains(stderr.String(), "sigstore_trusted_root") {
			t.Errorf("%s: exit %d %q", name, code, stderr.String())
		}
		if got, _ := os.ReadFile(out); string(got) != "old" {
			t.Errorf("%s replaced the root with %q", name, got)
		}
	}
}

func TestRunUsage(t *testing.T) {
	for _, args := range [][]string{nil, {"-o"}, {"-o", "x", "extra"}, {"-bogus"}} {
		var stderr bytes.Buffer
		if code := run(args, &stderr, func() ([]byte, error) {
			t.Fatal("fetched without a destination")
			return nil, nil
		}); code != 2 {
			t.Errorf("%v: exit %d", args, code)
		}
	}
}
