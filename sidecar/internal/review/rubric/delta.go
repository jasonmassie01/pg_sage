package rubric

// Coverage bands: a delta reports a band change, not every point.
const (
	BandLow     = "low"     // below 60%: the letter is unknown
	BandPartial = "partial" // 60% to below 90%
	BandHigh    = "high"    // 90% and more
)

// CoverageBand is the band of a coverage percentage.
func CoverageBand(pct float64) string {
	switch {
	case pct >= 90:
		return BandHigh
	case pct >= minCoverage:
		return BandPartial
	}
	return BandLow
}

// CheckMove is one check whose result changed ("" From: not graded before).
type CheckMove struct {
	Check string `json:"check"`
	From  Status `json:"from,omitempty"`
	To    Status `json:"to"`
}

// DimensionChange is one dimension whose letter, coverage band or check
// results changed.
type DimensionChange struct {
	Dimension  string      `json:"dimension"`
	FromLetter Letter      `json:"from_letter"`
	ToLetter   Letter      `json:"to_letter"`
	FromBand   string      `json:"from_band,omitempty"`
	ToBand     string      `json:"to_band"`
	Checks     []CheckMove `json:"checks"`
}

// Changes is what changed since the previous grade. The first run, and
// the first run of a new rubric version, is the whole report.
type Changes struct {
	FirstRun      bool              `json:"first_run"`
	RubricChanged bool              `json:"rubric_changed"`
	FromOverall   Letter            `json:"from_overall"`
	ToOverall     Letter            `json:"to_overall"`
	Dimensions    []DimensionChange `json:"dimensions"`
}

// Empty reports an unchanged run: nothing to notify (silence means
// healthy).
func (c Changes) Empty() bool {
	return !c.FirstRun && !c.RubricChanged && c.FromOverall == c.ToOverall &&
		len(c.Dimensions) == 0
}

// Delta lists what changed from prev to cur. A nil prev is the first run;
// a prev of another rubric version is not compared letter by letter.
func Delta(prev *Grade, cur Grade) Changes {
	out := Changes{ToOverall: cur.Overall, Dimensions: []DimensionChange{}}
	if prev == nil || prev.RubricVersion != cur.RubricVersion {
		out.FirstRun, out.RubricChanged = prev == nil, prev != nil
		for _, d := range cur.Dimensions {
			out.Dimensions = append(out.Dimensions, dimensionChange(nil, d))
		}
		return out
	}
	out.FromOverall = prev.Overall
	before := make(map[string]DimensionGrade, len(prev.Dimensions))
	for _, d := range prev.Dimensions {
		before[d.Dimension] = d
	}
	for _, d := range cur.Dimensions {
		p, ok := before[d.Dimension]
		var pp *DimensionGrade
		if ok {
			pp = &p
		}
		ch := dimensionChange(pp, d)
		if !ok || ch.FromLetter != ch.ToLetter || ch.FromBand != ch.ToBand ||
			len(ch.Checks) > 0 {
			out.Dimensions = append(out.Dimensions, ch)
		}
	}
	return out
}

// dimensionChange compares cur with prev (nil: every check is a move).
func dimensionChange(prev *DimensionGrade, cur DimensionGrade) DimensionChange {
	out := DimensionChange{Dimension: cur.Dimension, ToLetter: cur.Letter,
		ToBand: CoverageBand(cur.Coverage), Checks: []CheckMove{}}
	was := map[string]Status{}
	if prev != nil {
		out.FromLetter, out.FromBand = prev.Letter, CoverageBand(prev.Coverage)
		for _, c := range prev.Checks {
			was[c.Check] = c.Status
		}
	}
	for _, c := range cur.Checks {
		if from := was[c.Check]; prev == nil || from != c.Status {
			out.Checks = append(out.Checks, CheckMove{Check: c.Check, From: from, To: c.Status})
		}
	}
	return out
}
