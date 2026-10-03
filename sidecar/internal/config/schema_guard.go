package config

import (
	"fmt"
	"time"
)

// DefaultSchemaGuardDDLDebounceSeconds: after pg_sage's own DDL the schema
// guard re-scans at most once a minute (dogfood lifeos: a DDL-heavy app
// must not be able to drive continuous catalog scans).
const DefaultSchemaGuardDDLDebounceSeconds = 60

// maxSchemaGuardDDLDebounceSeconds keeps a DDL-requested scan within an
// hour; the periodic scan (analyzer interval) covers the rest.
const maxSchemaGuardDDLDebounceSeconds = 3600

// SchemaGuardDDLDebounce is the least time between a schema guard scan and
// a DDL-requested one; 0 (unset) is the default.
func (c *AnalyzerConfig) SchemaGuardDDLDebounce() time.Duration {
	seconds := c.SchemaGuardDDLDebounceSeconds
	if seconds <= 0 {
		seconds = DefaultSchemaGuardDDLDebounceSeconds
	}
	return time.Duration(seconds) * time.Second
}

func (c *AnalyzerConfig) validateSchemaGuard() error {
	seconds := c.SchemaGuardDDLDebounceSeconds
	if seconds < 0 || seconds > maxSchemaGuardDDLDebounceSeconds {
		return fmt.Errorf("analyzer.schema_guard_ddl_debounce_seconds must be 0-%d, got %d",
			maxSchemaGuardDDLDebounceSeconds, seconds)
	}
	return nil
}
