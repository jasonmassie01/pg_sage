package evidence

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"
	"time"
)

// evid-02a: SealFiles seals any set of files the way an audit pack is
// sealed (name order, per-file SHA-256, optional Ed25519 signer) without
// the audit metadata, so the workload pack reuses the pack format and its
// export can sign with the same key. The audit pack is unchanged.
// No DB test: SealFiles reads nothing. No concurrency test: it shares no
// state between calls.

func TestSealFiles_SortsHashesAndLeavesTheAuditFieldsEmpty(t *testing.T) {
	at := time.Date(2026, 11, 2, 9, 30, 0, 0, time.FixedZone("x", 3600))
	p := SealFiles("pg_sage-workload/1", []File{
		{Name: "workload.json", Data: []byte(`{"b":1}`)},
		{Name: "header.json", Data: []byte(`{"a":1}`)},
	}, nil, at)
	m := p.Manifest
	if m.Format != "pg_sage-workload/1" || m.Generator != "pg_sage" ||
		!m.CreatedAt.Equal(at) || m.CreatedAt.Location() != time.UTC {
		t.Fatalf("manifest head %+v, want the given format, pg_sage and the UTC time", m)
	}
	if len(m.Files) != 2 || m.Files[0].Name != "header.json" ||
		m.Files[1].Name != "workload.json" {
		t.Fatalf("files %+v, want header.json then workload.json", m.Files)
	}
	sum := sha256.Sum256([]byte(`{"a":1}`))
	if m.Files[0].SHA256 != hex.EncodeToString(sum[:]) || m.Files[0].Bytes != 7 {
		t.Fatalf("header entry %+v, want its SHA-256 and 7 bytes", m.Files[0])
	}
	if m.Scope != (Scope{}) || m.Truncated || m.Files[0].Rows != nil ||
		m.Files[0].Source != "" {
		t.Fatalf("audit metadata leaked into %+v", m)
	}
	if m.Signed || m.Signer != nil || m.Integrity != IntegrityHashOnly {
		t.Fatalf("unsigned pack %+v, want hash-only integrity", m)
	}
	if p.Files[0].Name != "header.json" {
		t.Fatalf("pack files %v, want name order", p.Files)
	}
}

func TestSealFiles_SignedPackVerifiesAndATamperedOneDoesNot(t *testing.T) {
	_, key, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	p := SealFiles("pg_sage-workload/1", []File{{Name: "header.json",
		Data: []byte(`{"a":1}`)}}, key, time.Unix(1_700_000_000, 0))
	if !p.Manifest.Signed || p.Manifest.Signer == nil || p.Manifest.Signer.Alg != "Ed25519" {
		t.Fatalf("signed manifest %+v, want an Ed25519 signer", p.Manifest)
	}
	var buf bytes.Buffer
	if err := p.WriteTarGz(&buf); err != nil {
		t.Fatalf("write: %v", err)
	}
	m, err := Verify(bytes.NewReader(buf.Bytes()), key.Public().(ed25519.PublicKey))
	if err != nil || m.Format != "pg_sage-workload/1" {
		t.Fatalf("verify: %v (%+v), want the sealed manifest", err, m)
	}
	p.Files[0].Data = []byte(`{"a":2}`)
	buf.Reset()
	if err := p.WriteTarGz(&buf); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(bytes.NewReader(buf.Bytes()), nil); !errors.Is(err, ErrTampered) {
		t.Fatalf("verify of an edited file: %v, want ErrTampered", err)
	}
}

func TestSealFiles_EmptyAndNilFilesGiveAnEmptyManifest(t *testing.T) {
	for _, files := range [][]File{nil, {}} {
		p := SealFiles("f", files, nil, time.Time{})
		if len(p.Manifest.Files) != 0 || len(p.Files) != 0 || p.Manifest.Format != "f" {
			t.Fatalf("SealFiles(%v) = %+v, want no files", files, p.Manifest)
		}
	}
}

func TestSealFiles_DoesNotReorderTheCallersSlice(t *testing.T) {
	in := []File{{Name: "b"}, {Name: "a"}}
	_ = SealFiles("f", in, nil, time.Time{})
	if in[0].Name != "b" || in[1].Name != "a" {
		t.Fatalf("caller's files reordered to %v", in)
	}
}

func TestAuditFormat_IsUnchanged(t *testing.T) {
	if Format != "pg_sage-evidence/2" {
		t.Fatalf("audit pack format %q changed", Format)
	}
}
