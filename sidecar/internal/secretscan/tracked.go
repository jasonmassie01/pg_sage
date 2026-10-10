package secretscan

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// AU10 lists the local QA passwords the AgentDB spec's AU-10 row found
// committed in public files (G0-06). Only their lengths and SHA-256 hashes
// are kept; the git history still holds the plaintext, so the values must be
// treated as burned and rotated wherever they were reused.
var AU10 = []Target{
	{Length: 15, SHA256: "d158bea8c2a185ef400a234c0aa2aa2cd4d621a14d506ab6d25ec6fc6038a252"},
	{Length: 13, SHA256: "9e660b8412dd0777c34162af263b250ad61aba8712694aa6a907077902974edd"},
}

// skippedDirs are never walked when root is not a usable git checkout.
var skippedDirs = map[string]bool{".git": true, "node_modules": true, ".gocache": true}

// TrackedFiles lists the files git tracks under root, slash-separated and
// relative to root. When git cannot read the checkout (no git binary, or a
// worktree mounted without its main repository), it walks root instead,
// which scans untracked files too and so is stricter.
func TrackedFiles(root string) ([]string, error) {
	cmd := exec.Command("git", "-C", root, "ls-files", "-z")
	out, err := cmd.Output()
	if err == nil {
		var files []string
		for _, name := range bytes.Split(out, []byte{0}) {
			if len(name) > 0 {
				files = append(files, string(name))
			}
		}
		return files, nil
	}
	return walkFiles(root)
}

func walkFiles(root string) ([]string, error) {
	var files []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != root && skippedDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		files = append(files, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("secretscan: walking %s: %w", root, err)
	}
	return files, nil
}

// ScanFiles scans each file (relative to root) for targets. A listed file
// that no longer exists, such as a tracked file deleted in the working tree,
// is skipped; any other read failure is an error.
func ScanFiles(root string, files []string, targets []Target) ([]Hit, error) {
	m, err := NewMatcher(targets)
	if err != nil {
		return nil, err
	}
	var hits []Hit
	for _, name := range files {
		f, err := os.Open(filepath.Join(root, filepath.FromSlash(name)))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("secretscan: opening %s: %w", name, err)
		}
		found, err := m.Scan(strings.TrimPrefix(name, "./"), f)
		_ = f.Close()
		if err != nil {
			return nil, err
		}
		hits = append(hits, found...)
	}
	return hits, nil
}
