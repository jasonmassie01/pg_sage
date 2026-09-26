package collector

import "encoding/json"

// CacheHitRatioUnknown marks SystemStats.CacheHitRatio when
// pg_stat_database has no block accesses yet. Consumers must treat it as
// "no data" (never as a 0% hit ratio). It persists as JSON null.
const CacheHitRatioUnknown = -1.0

// systemStatsFields has SystemStats' fields without its JSON methods.
type systemStatsFields SystemStats

// systemStatsWire shadows cache_hit_ratio with a nullable value.
type systemStatsWire struct {
	systemStatsFields
	CacheHitRatio *float64 `json:"cache_hit_ratio"`
}

// CacheHitRatioKnown reports whether CacheHitRatio holds a real fraction.
func (s SystemStats) CacheHitRatioKnown() bool {
	return s.CacheHitRatio >= 0
}

// MarshalJSON writes an unknown cache hit ratio as null so SQL
// aggregates (e.g. avg over sage.snapshots) ignore it.
func (s SystemStats) MarshalJSON() ([]byte, error) {
	w := systemStatsWire{systemStatsFields: systemStatsFields(s)}
	if s.CacheHitRatioKnown() {
		ratio := s.CacheHitRatio
		w.CacheHitRatio = &ratio
	}
	return json.Marshal(w)
}

// UnmarshalJSON maps a null or missing cache_hit_ratio to
// CacheHitRatioUnknown.
func (s *SystemStats) UnmarshalJSON(data []byte) error {
	var w systemStatsWire
	if err := json.Unmarshal(data, &w); err != nil {
		return err
	}
	*s = SystemStats(w.systemStatsFields)
	s.CacheHitRatio = CacheHitRatioUnknown
	if w.CacheHitRatio != nil {
		s.CacheHitRatio = *w.CacheHitRatio
	}
	return nil
}
