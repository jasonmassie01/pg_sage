package broker

import (
	"fmt"
	"regexp"
	"time"
)

// Config bounds the brokered read path (agents.query.*, agents.broker.*).
type Config struct {
	MaxRows        int
	MaxRowsCeiling int
	MaxBytes       int
	// StatementTimeout and LockTimeout are set with SET LOCAL in every
	// transaction, so a statement cannot lift them (RO-15).
	StatementTimeout time.Duration
	LockTimeout      time.Duration
	// SearchPath is the profile schemas, between pg_catalog and pg_temp.
	SearchPath []string
	// PoolMaxConns is per (principal, database); MaxTotalConns bounds the
	// broker's connections across every agent.
	PoolMaxConns  int
	MaxTotalConns int
	PoolIdle      time.Duration
}

// DefaultConfig is the spec §9 defaults.
func DefaultConfig() Config {
	return Config{MaxRows: 200, MaxRowsCeiling: 1000, MaxBytes: 1 << 20,
		StatementTimeout: 30 * time.Second, LockTimeout: time.Second,
		SearchPath: []string{"public"}, PoolMaxConns: 2, MaxTotalConns: 20,
		PoolIdle: time.Minute}
}

var schemaName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_$]{0,62}$`)

// Validate checks every bound.
func (c Config) Validate() error {
	switch {
	case c.MaxRows < 1 || c.MaxRowsCeiling < c.MaxRows:
		return invalidf("max_rows must be at least 1 and at most max_rows_ceiling")
	case c.MaxBytes < 1:
		return invalidf("max_bytes must be at least 1")
	case c.StatementTimeout < time.Millisecond || c.LockTimeout < time.Millisecond:
		return invalidf("statement and lock timeouts must be at least 1ms")
	case c.PoolMaxConns < 1 || c.MaxTotalConns < c.PoolMaxConns:
		return invalidf("pool_max_conns must be at least 1 and at most " +
			"max_total_connections")
	case c.PoolIdle <= 0:
		return invalidf("pool_idle must be positive")
	}
	for _, s := range c.SearchPath {
		if !schemaName.MatchString(s) {
			return invalidf("search path entry %q is not a plain schema name", s)
		}
	}
	return nil
}

func invalidf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}
