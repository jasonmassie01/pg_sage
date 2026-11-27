package config

import "fmt"

// DefaultIndexBloatMinBytes: the collector estimates the bloat of btree
// indexes of at least 100 MiB (rules-13); a smaller index costs little to
// carry and its rebuild buys little.
const DefaultIndexBloatMinBytes int64 = 100 << 20

// IndexBloatThreshold is the estimated bloat percentage at which the
// index_bloat rule reports an index; 0 (unset) or negative is the
// default.
func (c *AnalyzerConfig) IndexBloatThreshold() int {
	if c.IndexBloatThresholdPct <= 0 {
		return DefaultIndexBloatThresholdPct
	}
	return c.IndexBloatThresholdPct
}

// IndexBloatFloorBytes is the least index size the collector estimates;
// 0 (unset) is the default.
func (c *AnalyzerConfig) IndexBloatFloorBytes() int64 {
	if c.IndexBloatMinBytes <= 0 {
		return DefaultIndexBloatMinBytes
	}
	return c.IndexBloatMinBytes
}

func (c *AnalyzerConfig) validateIndexBloat() error {
	if c.IndexBloatMinBytes < 0 {
		return fmt.Errorf("analyzer.index_bloat_min_bytes must be 0 (default) or positive, "+
			"got %d", c.IndexBloatMinBytes)
	}
	return nil
}
