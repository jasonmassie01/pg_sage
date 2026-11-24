package config

import (
	"crypto/sha256"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
)

// fileFingerprint identifies the config file a reload would read. The
// resolved path follows the ConfigMap ..data symlink; mtime is included so
// that touching a rejected file retries it.
type fileFingerprint struct {
	resolved string
	size     int64
	modTime  int64
	sum      [sha256.Size]byte
}

func fingerprintFile(path string) (fileFingerprint, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return fileFingerprint{}, fmt.Errorf("resolve config path %s: %w", path, err)
	}
	file, err := os.Open(resolved)
	if err != nil {
		return fileFingerprint{}, fmt.Errorf("open config %s: %w", resolved, err)
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return fileFingerprint{}, fmt.Errorf("stat config %s: %w", resolved, err)
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return fileFingerprint{}, fmt.Errorf("read config %s: %w", resolved, err)
	}
	fingerprint := fileFingerprint{
		resolved: resolved,
		size:     info.Size(),
		modTime:  info.ModTime().UnixNano(),
	}
	copy(fingerprint.sum[:], hash.Sum(nil))
	return fingerprint, nil
}

// startFingerprint records the file the sidecar started from. When it cannot
// be read, the zero fingerprint makes the first readable state reload.
func (w *Watcher) startFingerprint() fileFingerprint {
	fingerprint, err := fingerprintFile(w.path)
	if err != nil {
		log.Printf("[DEBUG] [config-watcher] %v; the first readable "+
			"version will be reloaded", err)
	}
	return fingerprint
}
