package main

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/pg-sage/sidecar/internal/config"
)

// security-08: without encryption_key, pg_sage generates a key into
// <data_dir>/encryption.key so secrets are sealed at rest by default.

const encryptionKeyFileName = "encryption.key"

// keySource says where encryption_key comes from.
type keySource string

const (
	keySourceConfigured keySource = "configured" // YAML, flag or SAGE_ENCRYPTION_KEY(_FILE)
	keySourceGenerated  keySource = "generated"  // read from data_dir/encryption.key
	keySourcePending    keySource = "pending"    // data_dir is writable; no file yet
	keySourceNone       keySource = "none"       // no key: secrets stay plaintext
)

// errGeneratedKeyMissing means a database records a generated key id that
// no key file provides: the file was lost or replaced.
var errGeneratedKeyMissing = errors.New("the generated encryption key is missing")

// keyResolution is the startup decision about encryption_key.
type keyResolution struct {
	Source keySource
	Path   string // data_dir/encryption.key; "" without data_dir
	key    string // the generated key, for reload candidates
}

// resolveEncryptionKey runs after config load and before any database is
// opened. A configured key always wins (with a warning when a key file
// also exists). Otherwise an existing key file is read and becomes
// encryption_key; a writable data_dir without one is pending until the
// first control database confirms no generated key was lost (settle).
func resolveEncryptionKey(
	c *config.Config, warn func(component, msg string, args ...any),
) (keyResolution, error) {
	res := keyResolution{Source: keySourceNone}
	if c.DataDir != "" {
		res.Path = filepath.Join(c.DataDir, encryptionKeyFileName)
	}
	if c.EncryptionKey != "" {
		res.Source = keySourceConfigured
		if _, err := os.Lstat(res.Path); res.Path != "" && err == nil {
			warn("startup", "encryption_key is configured, so the key file %s is "+
				"ignored", res.Path)
		}
		return res, nil
	}
	if res.Path == "" {
		warn("startup", "secrets set through the API are stored in plaintext: set "+
			"encryption_key (SAGE_ENCRYPTION_KEY or SAGE_ENCRYPTION_KEY_FILE) or "+
			"data_dir (SAGE_DATA_DIR) to seal them at rest")
		return res, nil
	}
	if _, err := os.Lstat(res.Path); err == nil {
		key, err := readEncryptionKeyFile(res.Path)
		if err != nil {
			return res, err
		}
		c.EncryptionKey, res.Source, res.key = key, keySourceGenerated, key
		return res, nil
	}
	if err := ensureWritableDir(c.DataDir); err != nil {
		warn("startup", "data_dir %s is not writable (%v): secrets set through the "+
			"API are stored in plaintext", c.DataDir, err)
		return res, nil
	}
	res.Source = keySourcePending
	return res, nil
}

// settle decides a pending or missing key once the control database's
// recorded generated key id is known. recorded == "" means no key was
// ever generated for it, so a pending key file is created now. A recorded
// id without a key file is errGeneratedKeyMissing and nothing is created.
func (r *keyResolution) settle(recorded string, c *config.Config) error {
	if r.Source != keySourcePending && r.Source != keySourceNone {
		return nil
	}
	if recorded != "" {
		where := r.Path
		if where == "" {
			where = "data_dir/encryption.key (data_dir is not set)"
		}
		return fmt.Errorf("%w: secrets in this database were sealed with generated "+
			"key id %s, but %s does not exist; restore it from backup (pg_sage "+
			"generated nothing and leaves secrets unsealed)",
			errGeneratedKeyMissing, recorded, where)
	}
	if r.Source == keySourceNone {
		return nil
	}
	key, err := createEncryptionKeyFile(r.Path)
	if err != nil {
		return err
	}
	c.EncryptionKey, r.Source, r.key = key, keySourceGenerated, key
	return nil
}

// checkGeneratedKeyID compares a generated key's id for one database with
// the recorded id. It reports whether the id must be recorded (first
// start), or errGeneratedKeyMissing when the file holds another key.
func checkGeneratedKeyID(r keyResolution, recorded, active string) (bool, error) {
	if r.Source != keySourceGenerated {
		return false, nil
	}
	if recorded == "" {
		return true, nil
	}
	if recorded != active {
		return false, fmt.Errorf("%w: secrets in this database were sealed with "+
			"generated key id %s, but %s holds key id %s; restore the original "+
			"file from backup", errGeneratedKeyMissing, recorded, r.Path, active)
	}
	return false, nil
}

// applyTo gives a reloaded config candidate the generated key, so a
// reload never reads as an encryption_key change.
func (r keyResolution) applyTo(candidate *config.Config) {
	if r.Source == keySourceGenerated && candidate.EncryptionKey == "" {
		candidate.EncryptionKey = r.key
	}
}

// readEncryptionKeyFile reads a generated key: an owner-only file of at
// most 4 KiB holding a base64 32-byte key.
func readEncryptionKeyFile(path string) (string, error) {
	text, err := readOwnerOnlyFile(path, "encryption key file "+path)
	if err != nil {
		return "", err
	}
	text = strings.TrimSpace(text)
	if raw, err := base64.StdEncoding.DecodeString(text); err != nil || len(raw) != 32 {
		return "", fmt.Errorf("encryption key file %s must hold a base64 key of "+
			"32 bytes", path)
	}
	return text, nil
}

// createEncryptionKeyFile writes 32 random bytes as base64 to a private
// temporary file (O_CREATE|O_EXCL, mode 0600, fsync) and links it into
// place, which fails if path exists. A concurrent starter that lost the
// race reads the winner's complete file.
func createEncryptionKeyFile(path string) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generating an encryption key: %w", err)
	}
	key := base64.StdEncoding.EncodeToString(raw)
	tmp, err := writePrivateTemp(filepath.Dir(path), key+"\n")
	if err != nil {
		return "", fmt.Errorf("writing encryption key file %s: %w", path, err)
	}
	defer func() { _ = os.Remove(tmp) }()
	if err := os.Link(tmp, path); errors.Is(err, fs.ErrExist) {
		return readEncryptionKeyFile(path)
	} else if err != nil {
		return "", fmt.Errorf("installing encryption key file %s: %w", path, err)
	}
	syncDir(filepath.Dir(path))
	return key, nil
}

func writePrivateTemp(dir, content string) (string, error) {
	raw := make([]byte, 8)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	tmp := filepath.Join(dir, ".encryption.key."+base64.RawURLEncoding.EncodeToString(raw))
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", err
	}
	_, werr := f.WriteString(content)
	serr := f.Sync()
	cerr := f.Close()
	if err := errors.Join(werr, serr, cerr); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	return tmp, nil
}

// syncDir makes the new directory entry durable where the platform allows
// it; Windows cannot fsync a directory, so a failure is ignored.
func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
}

// ensureWritableDir creates dir (mode 0700) when missing and proves a file
// can be created in it.
func ensureWritableDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	probe, err := os.CreateTemp(dir, ".write-probe-*")
	if err != nil {
		return err
	}
	name := probe.Name()
	_ = probe.Close()
	return os.Remove(name)
}
