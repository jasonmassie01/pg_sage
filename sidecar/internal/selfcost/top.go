package selfcost

import (
	"regexp"
	"sort"
	"strings"
)

// TopN is how many of pg_sage's costliest statements a meter keeps.
const TopN = 5

// MaxStatementText bounds a statement's text in a finding or report.
const MaxStatementText = 160

// StatementCost is one statement's work between two readings.
type StatementCost struct {
	Text   string
	TimeMs float64
	Calls  int64
	Blocks int64
}

// TopStatements are the n statements that used the most database time
// between prev and cur, with the same reset rules as the totals: an
// entry whose counters went back counts from zero, a new one in full. A
// statement that did no work is left out. Ties order by text.
func TopStatements(prev, cur map[StatementKey]StatementCounters, n int) []StatementCost {
	if n <= 0 {
		return nil
	}
	var out []StatementCost
	for k, c := range cur {
		p, ok := prev[k]
		if !ok || c.Calls < p.Calls || c.TimeMs < p.TimeMs || c.Blocks < p.Blocks {
			p = StatementCounters{}
		}
		calls := c.Calls - p.Calls
		if calls <= 0 && c.TimeMs <= p.TimeMs {
			continue
		}
		out = append(out, StatementCost{Text: StatementText(c.Text),
			TimeMs: c.TimeMs - p.TimeMs, Calls: calls, Blocks: c.Blocks - p.Blocks})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].TimeMs != out[j].TimeMs {
			return out[i].TimeMs > out[j].TimeMs
		}
		return out[i].Text < out[j].Text
	})
	if len(out) > n {
		out = out[:n]
	}
	return out
}

// tagComment matches pg_sage's statement tags (/* pg_sage */, /* pg_sage
// sre:probe v1 */, any case).
var tagComment = regexp.MustCompile(`(?i)/\*\s*pg_sage[^*]*\*/`)

// StatementText is a statement as a finding shows it: pg_sage's tag
// removed, whitespace collapsed, cut to MaxStatementText characters.
func StatementText(q string) string {
	s := strings.Join(strings.Fields(tagComment.ReplaceAllString(q, " ")), " ")
	if len(s) > MaxStatementText {
		s = strings.TrimSpace(s[:MaxStatementText-3]) + "..."
	}
	return s
}

// Top is a copy of the costliest statements of the latest window (none
// before a usable window or without pg_stat_statements).
func (m *Meter) Top() []StatementCost {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]StatementCost(nil), m.top...)
}
