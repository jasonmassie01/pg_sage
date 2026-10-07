package fleetlearn

// OutcomeCount is one database's verified outcomes for an action class on
// one table shape; Shape "" counts the class over every table.
type OutcomeCount struct {
	Class     string `json:"class"`
	Shape     string `json:"shape"`
	Improved  int    `json:"improved"`
	Neutral   int    `json:"neutral"`
	Regressed int    `json:"regressed"`
}

func (c OutcomeCount) total() int { return c.Improved + c.Neutral + c.Regressed }

// Prior match levels and the label every prior carries.
const (
	MatchTableShape  = "table_shape"
	MatchActionClass = "action_class"
	SourceLabel      = "from look-alike databases"
)

// Prior is the evidence look-alike databases give for one action shape.
// It is evidence only: it never raises confidence, trust or authority.
type Prior struct {
	Source        string  `json:"source"`
	Match         string  `json:"match"`
	Databases     int     `json:"databases"`
	MinSimilarity float64 `json:"min_similarity"`
	Improved      int     `json:"improved"`
	Neutral       int     `json:"neutral"`
	Regressed     int     `json:"regressed"`
	N             int     `json:"outcomes"`
}

// BuildPrior sums the look-alikes' verified outcomes for class on the
// same table shape; when those are fewer than minN it falls back to the
// class over every table. No prior below minN outcomes (minN < 1 is 1).
func BuildPrior(digests map[string][]OutcomeCount, looks []LookAlike, class,
	shape string, minN int) (Prior, bool) {
	if class == "" || len(looks) == 0 {
		return Prior{}, false
	}
	if minN < 1 {
		minN = 1
	}
	if shape != "" {
		if p := sumPrior(digests, looks, class, shape); p.N >= minN {
			p.Match = MatchTableShape
			return p, true
		}
	}
	p := sumPrior(digests, looks, class, "")
	if p.N < minN {
		return Prior{}, false
	}
	p.Match = MatchActionClass
	return p, true
}

func sumPrior(digests map[string][]OutcomeCount, looks []LookAlike, class,
	shape string) Prior {
	p := Prior{Source: SourceLabel}
	for _, l := range looks {
		contributed := false
		for _, c := range digests[l.Database] {
			if c.Class != class || c.Shape != shape || c.total() == 0 {
				continue
			}
			p.Improved += c.Improved
			p.Neutral += c.Neutral
			p.Regressed += c.Regressed
			contributed = true
		}
		if !contributed {
			continue
		}
		if p.Databases == 0 || l.Similarity < p.MinSimilarity {
			p.MinSimilarity = l.Similarity
		}
		p.Databases++
	}
	p.N = p.Improved + p.Neutral + p.Regressed
	return p
}

// Cautions reports a prior whose look-alikes regressed at least as often
// as they improved: the proposal should then wait for an operator.
func (p Prior) Cautions() bool {
	return p.Regressed > 0 && p.Regressed >= p.Improved
}

// Detail is the prior as a finding detail carries it. It never names the
// look-alike databases.
func (p Prior) Detail() map[string]any {
	return map[string]any{"source": p.Source, "match": p.Match,
		"databases": p.Databases, "min_similarity": p.MinSimilarity,
		"improved": p.Improved, "neutral": p.Neutral, "regressed": p.Regressed,
		"outcomes": p.N}
}
