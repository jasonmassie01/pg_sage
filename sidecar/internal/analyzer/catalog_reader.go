package analyzer

import "github.com/pg-sage/sidecar/internal/catalogread"

// catalog is the bounded reader for the analyzer's read-only checks on
// the monitored database: each statement runs read-only under
// safety.query_timeout_ms (static.md F11), so a slow check is cut off and
// fails closed instead of stalling the cycle. Sage history readers
// (query_history, plan diff) and finding writes stay on the pool.
func (a *Analyzer) catalog() catalogread.Reader {
	t := catalogread.Default()
	if a.cfg != nil {
		t = catalogread.FromSafety(a.cfg.Safety)
	}
	return catalogread.New(a.pool, t)
}
