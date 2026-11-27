package main

import (
	"crypto/ed25519"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/pg-sage/sidecar/internal/evidence"
)

// pg_sage_sidecar evidence verify (audit-10) checks an evidence pack
// offline: every file against SHA256SUMS and the manifest and, with
// --public-key, the manifest's Ed25519 signature by that key. It reads
// only the pack and the key file; it needs no database.

// evidenceSubcommand is the first argument that runs the command.
const evidenceSubcommand = "evidence"

const evidenceUsage = `usage: pg_sage_sidecar evidence verify <pack.tar.gz> [--public-key <file>]
--public-key is the trusted Ed25519 public key: the public_key_pem of
GET /api/v1/audit/evidence/key saved to a file, or its 64 hex characters.
Exit codes: 0 the pack verifies, 1 it was altered or the signature does not
verify, 2 usage or an unusable key, 3 the pack is unsigned but a key was given.`

// Exit codes of the evidence command.
const (
	evidenceExitOK       = 0
	evidenceExitInvalid  = 1
	evidenceExitUsage    = 2
	evidenceExitUnsigned = 3
)

type evidenceArgs struct {
	pack, keyFile string
}

// parseEvidenceArgs accepts the flag before or after the pack path.
func parseEvidenceArgs(args []string) (evidenceArgs, error) {
	if len(args) == 0 || args[0] != "verify" {
		return evidenceArgs{}, errors.New("a subcommand is required: verify")
	}
	var a evidenceArgs
	fs := flag.NewFlagSet("evidence verify", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&a.keyFile, "public-key", "", "trusted Ed25519 public key file")
	var positional []string
	rest := args[1:]
	for {
		if err := fs.Parse(rest); err != nil {
			return a, fmt.Errorf("%w (--public-key needs a file)", err)
		}
		if fs.NArg() == 0 {
			break
		}
		positional = append(positional, fs.Arg(0))
		rest = fs.Args()[1:]
	}
	switch len(positional) {
	case 0:
		return a, errors.New("name the pack to verify")
	case 1:
		a.pack = positional[0]
		return a, nil
	}
	return a, fmt.Errorf("verify takes one pack, got %d", len(positional))
}

// runEvidenceCommand runs pg_sage_sidecar evidence and returns its exit code.
func runEvidenceCommand(args []string, stdout, stderr io.Writer) int {
	a, err := parseEvidenceArgs(args)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "evidence: %v\n%s\n", err, evidenceUsage)
		return evidenceExitUsage
	}
	var trusted ed25519.PublicKey
	if a.keyFile != "" {
		raw, err := os.ReadFile(a.keyFile)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "evidence: read the public key: %v\n", err)
			return evidenceExitUsage
		}
		if trusted, err = evidence.ParsePublicKey(raw); err != nil {
			_, _ = fmt.Fprintf(stderr, "evidence: %s: %v\n", a.keyFile, err)
			return evidenceExitUsage
		}
	}
	f, err := os.Open(a.pack)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "evidence: open the pack: %v\n", err)
		return evidenceExitUsage
	}
	defer func() { _ = f.Close() }()
	m, err := evidence.Verify(f, trusted)
	switch {
	case errors.Is(err, evidence.ErrUnsigned):
		_, _ = fmt.Fprintf(stderr, "evidence: %s: %v; its hashes guard against "+
			"accidental corruption only\n", a.pack, err)
		return evidenceExitUnsigned
	case err != nil:
		_, _ = fmt.Fprintf(stderr, "evidence: %s does not verify: %v\n", a.pack, err)
		return evidenceExitInvalid
	}
	reportVerifiedPack(stdout, a.pack, m, trusted)
	return evidenceExitOK
}

func reportVerifiedPack(w io.Writer, pack string, m evidence.Manifest,
	trusted ed25519.PublicKey) {
	_, _ = fmt.Fprintf(w, "%s: %d files match SHA256SUMS and the manifest (%s)\n", pack,
		len(m.Files), m.Format)
	switch {
	case trusted != nil:
		_, _ = fmt.Fprintf(w, "signature: verified with key %s\n", evidence.KeyID(trusted))
	case m.Signer != nil:
		_, _ = fmt.Fprintf(w, "signature: not checked; the pack names key %s. Pass "+
			"--public-key with the key from GET /api/v1/audit/evidence/key to check it\n",
			m.Signer.KeyID)
	default:
		_, _ = fmt.Fprintf(w, "signature: none; integrity is %s (anyone who edits a "+
			"file can regenerate the hashes)\n", evidence.IntegrityHashOnly)
	}
}
