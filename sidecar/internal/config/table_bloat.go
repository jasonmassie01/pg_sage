package config

import "fmt"

// DefaultTableBloatMinBytes: the freeze custodian plans an online bloat
// rebuild only for heaps of at least 1 GiB; below that the analyzer's
// VACUUM findings cover the table (vacuum-04).
const DefaultTableBloatMinBytes int64 = 1 << 30

// BloatPlanMinBytes is the least heap size for a bloat rebuild plan; 0
// (unset) is the default.
func (c *AnalyzerConfig) BloatPlanMinBytes() int64 {
	if c.TableBloatMinBytes <= 0 {
		return DefaultTableBloatMinBytes
	}
	return c.TableBloatMinBytes
}

func (c *AnalyzerConfig) validateTableBloat() error {
	if c.TableBloatMinBytes < 0 {
		return fmt.Errorf("analyzer.table_bloat_min_bytes must be 0 (default) or positive, "+
			"got %d", c.TableBloatMinBytes)
	}
	return nil
}
