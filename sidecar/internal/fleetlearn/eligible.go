package fleetlearn

// SkipNoPeer is CycleResult.Skipped when no database can have a
// look-alike: a database is never its own peer, and peers must share a
// boundary, so fingerprinting would read and write for nothing.
const SkipNoPeer = "no peer in boundary"

// Eligible reports whether some boundary holds two or more connected
// databases, the least a look-alike needs. It runs no query.
func (s *Service) Eligible() bool {
	return eligible(s.sources())
}

// eligible is true when some boundary holds two or more distinct, named
// sources with a pool.
func eligible(srcs []DatabaseSource) bool {
	seen := map[string]map[string]bool{}
	for _, src := range srcs {
		if src.Pool == nil || src.Name == "" {
			continue
		}
		names := seen[src.Boundary]
		if names == nil {
			names = map[string]bool{}
			seen[src.Boundary] = names
		}
		names[src.Name] = true
		if len(names) >= 2 {
			return true
		}
	}
	return false
}

// noteEligible records whether this cycle may run and returns ok. Entering
// the skip state logs once; staying in it is silent.
func (s *Service) noteEligible(ok bool) bool {
	s.mu.Lock()
	entered := !ok && !s.idle
	s.idle = !ok
	s.mu.Unlock()
	if entered {
		s.logFn("INFO", "fleet learning: skipped, no peer in boundary; it idles "+
			"until two connected databases share a boundary (same tenant tag, or none)")
	}
	return ok
}
