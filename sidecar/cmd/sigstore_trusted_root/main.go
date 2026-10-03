// Command sigstore_trusted_root writes the current Sigstore public-good
// trusted root, fetched and verified through Sigstore's TUF repository,
// to the file that internal/benchsig embeds. The release workflow runs it
// before building so a release verifies its bench report signatures
// against the keys Sigstore used to sign them.
//
//	go run ./cmd/sigstore_trusted_root -o internal/benchsig/trusted_root.json
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/pg-sage/sidecar/internal/benchsig"
	"github.com/sigstore/sigstore-go/pkg/tuf"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stderr, fetchTrustedRoot))
}

// fetchTrustedRoot reads trusted_root.json through a TUF client rooted
// in the TUF root embedded in sigstore-go, with a throwaway cache.
func fetchTrustedRoot() ([]byte, error) {
	cache, err := os.MkdirTemp("", "sigstore-tuf-")
	if err != nil {
		return nil, fmt.Errorf("TUF cache: %w", err)
	}
	defer func() { _ = os.RemoveAll(cache) }()
	client, err := tuf.New(tuf.DefaultOptions().WithCachePath(cache))
	if err != nil {
		return nil, fmt.Errorf("TUF client: %w", err)
	}
	raw, err := client.GetTarget("trusted_root.json")
	if err != nil {
		return nil, fmt.Errorf("TUF target trusted_root.json: %w", err)
	}
	return raw, nil
}

func run(args []string, stderr io.Writer, fetch func() ([]byte, error)) int {
	fs := flag.NewFlagSet("sigstore_trusted_root", flag.ContinueOnError)
	fs.SetOutput(stderr)
	out := fs.String("o", "", "file to write the trusted root to")
	if err := fs.Parse(args); err != nil || *out == "" || fs.NArg() != 0 {
		_, _ = fmt.Fprintln(stderr, "usage: sigstore_trusted_root -o <file>")
		return 2
	}
	raw, err := fetch()
	if err == nil {
		err = writeTrustedRoot(*out, raw)
	}
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "sigstore_trusted_root: %v\n", err)
		return 1
	}
	return 0
}

// writeTrustedRoot replaces path with raw once raw is a usable root.
func writeTrustedRoot(path string, raw []byte) error {
	if _, err := benchsig.ParseTrustedRoot(raw); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".trusted_root-*")
	if err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("replace %s: %w", path, err)
	}
	return nil
}
