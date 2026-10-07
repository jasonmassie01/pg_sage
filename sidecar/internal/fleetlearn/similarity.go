package fleetlearn

import (
	"sort"
	"time"
)

// Fingerprint is one database's privacy-preserving shape: sorted, unique
// shape hashes of its tables, indexes and top statements. Labels map a
// table shape hash to a table name only when the operator opted in.
type Fingerprint struct {
	Database   string            `json:"database"`
	Boundary   string            `json:"boundary"`
	Tables     []string          `json:"tables"`
	Indexes    []string          `json:"indexes"`
	Queries    []string          `json:"queries"`
	Labels     map[string]string `json:"labels,omitempty"`
	ComputedAt time.Time         `json:"computed_at"`
}

func (f Fingerprint) empty() bool {
	return len(f.Tables) == 0 && len(f.Indexes) == 0 && len(f.Queries) == 0
}

// Similarity weights: the schema dominates, the workload refines.
const (
	WeightTables  = 0.5
	WeightIndexes = 0.2
	WeightQueries = 0.3
)

// Similarity is the weighted Jaccard overlap of two fingerprints in [0,1].
// A dimension empty on both sides (no pg_stat_statements, no indexes) is
// left out and its weight redistributed; two empty fingerprints score 0.
func Similarity(a, b Fingerprint) float64 {
	dims := []struct {
		x, y []string
		w    float64
	}{{a.Tables, b.Tables, WeightTables}, {a.Indexes, b.Indexes, WeightIndexes},
		{a.Queries, b.Queries, WeightQueries}}
	score, weight := 0.0, 0.0
	for _, d := range dims {
		if len(d.x) == 0 && len(d.y) == 0 {
			continue
		}
		score += d.w * jaccard(d.x, d.y)
		weight += d.w
	}
	if weight == 0 {
		return 0
	}
	return score / weight
}

func jaccard(x, y []string) float64 {
	set := make(map[string]int, len(x)+len(y))
	for _, v := range x {
		set[v] |= 1
	}
	for _, v := range y {
		set[v] |= 2
	}
	both := 0
	for _, bits := range set {
		if bits == 3 {
			both++
		}
	}
	if len(set) == 0 {
		return 0
	}
	return float64(both) / float64(len(set))
}

// LookAlike is a peer database and how similar it is.
type LookAlike struct {
	Database   string  `json:"database"`
	Similarity float64 `json:"similarity"`
}

// LookAlikes are the peers of target in the same boundary whose
// similarity is at least min (and above zero), most similar first. A
// database is never its own look-alike; an isolated or empty target has
// none.
func LookAlikes(target Fingerprint, peers []Fingerprint, min float64) []LookAlike {
	out := []LookAlike{}
	if Isolated(target.Boundary) || target.empty() {
		return out
	}
	for _, p := range peers {
		if p.Database == target.Database || p.Boundary != target.Boundary {
			continue
		}
		if s := Similarity(target, p); s > 0 && s >= min {
			out = append(out, LookAlike{Database: p.Database, Similarity: s})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Similarity != out[j].Similarity {
			return out[i].Similarity > out[j].Similarity
		}
		return out[i].Database < out[j].Database
	})
	return out
}

// sortedUnique sorts values and drops duplicates and empty strings.
func sortedUnique(values []string) []string {
	seen := make(map[string]bool, len(values))
	out := make([]string, 0, len(values))
	for _, v := range values {
		if v != "" && !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out
}
