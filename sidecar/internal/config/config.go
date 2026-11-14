package config

import (
	"fmt"
	"strings"
	"sync"
)

// hotReloadMu serializes hot-reload writes and any reads that need a
// self-consistent view of multiple fields. Kept at package scope (not
// as a Config method or field) so Config remains copy-safe for watcher
// snapshots and executor per-cycle overrides — adding Lock/Unlock
// methods would make Config look like sync.Locker and trigger
// "assignment copies lock value" from go vet.
var hotReloadMu sync.RWMutex

// LockForHotReload acquires the package-level config write lock. Call
// from any code path that mutates a live Config struct concurrently
// with background readers.
func LockForHotReload() { hotReloadMu.Lock() }

// UnlockForHotReload releases the package-level config write lock.
func UnlockForHotReload() { hotReloadMu.Unlock() }

// RLockForHotReload acquires the package-level config read lock.
func RLockForHotReload() { hotReloadMu.RLock() }

// RUnlockForHotReload releases the package-level config read lock.
func RUnlockForHotReload() { hotReloadMu.RUnlock() }

// DSN builds a libpq connection string from individual fields.
func (p *PostgresConfig) DSN() string {
	if p.DatabaseURL != "" {
		return p.DatabaseURL
	}
	return fmt.Sprintf(
		"host=%s port=%d user=%s password=%s dbname=%s sslmode=%s",
		conninfoValue(p.Host), p.Port, conninfoValue(p.User),
		conninfoValue(p.Password), conninfoValue(p.Database),
		conninfoValue(p.SSLMode),
	)
}

// conninfoValue quotes a libpq key/value conninfo value so spaces, quotes
// and embedded "key=value" text cannot break or inject parameters
// (G5-B22). libpq escapes backslash and single quote with a backslash.
func conninfoValue(value string) string {
	escaped := strings.NewReplacer(`\`, `\\`, "'", `\'`).Replace(value)
	return "'" + escaped + "'"
}

// IsStandalone returns true if running in standalone mode.
func (c *Config) IsStandalone() bool {
	return c.Mode == "standalone"
}

// IsFleet returns true if running in fleet mode.
func (c *Config) IsFleet() bool {
	return c.Mode == "fleet"
}

// HasMetaDB returns true if a metadata database is configured.
func (c *Config) HasMetaDB() bool {
	return c.MetaDB != ""
}

// HasEncryptionKey returns true if an encryption passphrase is set.
func (c *Config) HasEncryptionKey() bool {
	return c.EncryptionKey != ""
}
