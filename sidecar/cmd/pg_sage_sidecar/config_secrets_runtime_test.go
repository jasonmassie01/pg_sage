package main

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/crypto"
	"github.com/pg-sage/sidecar/internal/store"
)

var fixtureSalt = bytes.Repeat([]byte{7}, 16)

func TestBuildConfigKeyring_NoPassphraseMeansNoKeyring(t *testing.T) {
	kr, err := buildConfigKeyring("", "", fixtureSalt)
	if err != nil || kr != nil {
		t.Fatalf("= (%v, %v), want (nil, nil)", kr, err)
	}
}

func TestBuildConfigKeyring_ActiveAndPrevious(t *testing.T) {
	kr, err := buildConfigKeyring("new-pass", "old-pass", fixtureSalt)
	if err != nil || kr == nil {
		t.Fatalf("= (%v, %v)", kr, err)
	}
	newOnly, _ := buildConfigKeyring("new-pass", "", fixtureSalt)
	oldOnly, _ := buildConfigKeyring("old-pass", "", fixtureSalt)
	if kr.ActiveKeyID() != newOnly.ActiveKeyID() {
		t.Fatal("active key id is not derived from encryption_key")
	}
	sealed, _ := oldOnly.Seal("v", "aad")
	if plain, kid, err := kr.Open(sealed, "aad"); err != nil || plain != "v" ||
		kid != oldOnly.ActiveKeyID() {
		t.Fatalf("previous key cannot open old ciphertext: (%q, %q, %v)", plain, kid, err)
	}
}

func TestBuildConfigKeyring_SaltSeparatesDeployments(t *testing.T) {
	a, _ := buildConfigKeyring("same-pass", "", fixtureSalt)
	b, _ := buildConfigKeyring("same-pass", "", bytes.Repeat([]byte{8}, 16))
	if a.ActiveKeyID() == b.ActiveKeyID() {
		t.Fatal("two deployments with one passphrase share a key")
	}
}

func TestBuildConfigKeyring_PreviousWithoutActiveIsAnError(t *testing.T) {
	if _, err := buildConfigKeyring("", "old-pass", fixtureSalt); err == nil {
		t.Fatal("previous key without an active key accepted")
	}
}

func TestBuildConfigKeyring_ShortSaltIsAnError(t *testing.T) {
	_, err := buildConfigKeyring("pass", "", []byte{1, 2})
	if err == nil || !strings.Contains(err.Error(), "salt") {
		t.Fatalf("short salt: err = %v", err)
	}
}

func TestReportConfigSecretSeal(t *testing.T) {
	cases := map[string]struct {
		keyring bool
		report  store.SecretSealReport
		want    []string // substrings of the single expected line, nil = silent
		level   string
	}{
		"no key with plaintext secrets warns": {false,
			store.SecretSealReport{Plaintext: 2}, []string{"plaintext", "encryption_key",
				"SAGE_ENCRYPTION_KEY_FILE"}, "WARN"},
		"no key and nothing stored is silent": {false, store.SecretSealReport{}, nil, ""},
		"sealed and rotated rows reported": {true,
			store.SecretSealReport{Sealed: 1, Rotated: 2}, []string{"sealed 1", "rotated 2"},
			"INFO"},
		"unreadable rows are an error": {true, store.SecretSealReport{Unreadable: 1},
			[]string{"cannot be decrypted", "encryption_key_previous"}, "ERROR"},
		"nothing to do is silent": {true, store.SecretSealReport{}, nil, ""},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var lines []string
			logf := func(level, _ string, format string, args ...any) {
				lines = append(lines, level+" "+fmt.Sprintf(format, args...))
			}
			reportConfigSecretSeal(tc.keyring, tc.report, logf)
			if tc.want == nil {
				if len(lines) != 0 {
					t.Fatalf("expected silence, got %v", lines)
				}
				return
			}
			if len(lines) != 1 || !strings.HasPrefix(lines[0], tc.level) {
				t.Fatalf("lines = %v, want one %s line", lines, tc.level)
			}
			for _, w := range tc.want {
				if !strings.Contains(lines[0], w) {
					t.Fatalf("line %q lacks %q", lines[0], w)
				}
			}
		})
	}
}

func TestReportConfigSecretSeal_NeverLogsKeyMaterial(t *testing.T) {
	kr, _ := buildConfigKeyring("very-secret-passphrase", "", fixtureSalt)
	var out []string
	reportConfigSecretSeal(kr != nil, store.SecretSealReport{Sealed: 1},
		func(level, _ string, format string, args ...any) {
			out = append(out, fmt.Sprintf(format, args...))
		})
	for _, l := range out {
		if strings.Contains(l, "very-secret-passphrase") || strings.Contains(l, crypto.SealedPrefix) {
			t.Fatalf("log line leaks secret material: %q", l)
		}
	}
}
