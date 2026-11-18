package rubric

// The grade arithmetic's thresholds and the check sources, exported so
// cmd/gen_rubric writes docs/rubric.md from the code (pub-01).
const (
	// MinCoveragePct is the coverage below which a dimension's letter is
	// unknown.
	MinCoveragePct = minCoverage
	// MinKnownDimensions is the known dimensions an overall letter needs.
	MinKnownDimensions = minKnown
	// WarnWeightFraction is the share of its weight a warn earns.
	WarnWeightFraction = warnFraction
	// CriticalCap is the best letter a dimension with a critical check
	// that did not pass can reach.
	CriticalCap = criticalLimit
)

// Sources lists every check source, in the order docs/rubric.md shows them.
func Sources() []Source {
	return []Source{SourceFinding, SourceFirstLook, SourcePosture, SourceLint,
		SourceRunway, SourceDrill, SourceSetting, SourceUpgrade}
}

// Letters lists the grade letters, best first.
func Letters() []Letter { return append([]Letter(nil), letters...) }
