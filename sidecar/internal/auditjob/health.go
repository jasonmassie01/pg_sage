package auditjob

import (
	"sort"
	"sync"
	"time"

	"github.com/pg-sage/sidecar/internal/auditchain"
)

// audit-12: the health registry keeps the last scheduled verification of
// every (database, chain) in this process, for /metrics and
// GET /api/v1/audit/status. It holds results only: kinds and counts, never
// a problem's detail or any row content.

// ChainHealth is the last verification result of one chain of one
// database. Problems counts the problems reported, by kind (the report
// lists at most a bounded number; ProblemsTruncated says there were more).
type ChainHealth struct {
	Database          string         `json:"database"`
	Chain             string         `json:"chain"`
	OK                bool           `json:"ok"`
	Mode              string         `json:"mode"`
	Problems          map[string]int `json:"problems"`
	ProblemsTruncated bool           `json:"problems_truncated"`
	HeadSeq           int64          `json:"head_seq"`
	VerifiedAt        time.Time      `json:"verified_at"`
}

// VerifyErrorCount is how many verification runs of one database failed
// before they finished, and the latest failure.
type VerifyErrorCount struct {
	Database  string    `json:"database"`
	Count     int64     `json:"count"`
	LastError string    `json:"last_error,omitempty"`
	LastAt    time.Time `json:"last_at"`
}

// HealthSnapshot is a copy of the registry, sorted by database and chain.
// VerifiedAt is the newest chain's verification time, nil before any.
type HealthSnapshot struct {
	VerifiedAt *time.Time
	Chains     []ChainHealth
	Errors     []VerifyErrorCount
}

type chainKey struct{ database, chain string }

// HealthRegistry is safe for concurrent use; a nil registry records
// nothing and snapshots empty.
type HealthRegistry struct {
	mu     sync.Mutex
	chains map[chainKey]ChainHealth
	errs   map[string]VerifyErrorCount
}

// NewHealthRegistry returns an empty registry.
func NewHealthRegistry() *HealthRegistry {
	return &HealthRegistry{chains: map[chainKey]ChainHealth{},
		errs: map[string]VerifyErrorCount{}}
}

var defaultHealth = NewHealthRegistry()

// DefaultHealth is the process-wide registry the scheduled verification
// records into.
func DefaultHealth() *HealthRegistry { return defaultHealth }

// Record stores one run of Run on database: each report replaces its
// chain's result (a report without a time takes at). A run that failed
// (err) keeps the previous result of every chain it did not report and
// counts the failure; a clean run replaces the database's chains, so a
// chain no longer installed drops out.
func (h *HealthRegistry) Record(database string, reports []auditchain.Report, err error,
	at time.Time) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if err == nil {
		for k := range h.chains {
			if k.database == database {
				delete(h.chains, k)
			}
		}
	}
	for _, rep := range reports {
		if rep.Chain == "" {
			continue
		}
		h.chains[chainKey{database, rep.Chain}] = chainHealth(database, rep, at)
	}
	if err != nil {
		e := h.errs[database]
		e.Database, e.LastError, e.LastAt = database, err.Error(), at
		e.Count++
		h.errs[database] = e
	}
}

func chainHealth(database string, rep auditchain.Report, at time.Time) ChainHealth {
	kinds := make(map[string]int, len(rep.Problems))
	for _, p := range rep.Problems {
		kinds[p.Kind]++
	}
	when := rep.VerifiedAt
	if when.IsZero() {
		when = at
	}
	return ChainHealth{Database: database, Chain: rep.Chain, OK: rep.OK(), Mode: rep.Mode,
		Problems: kinds, ProblemsTruncated: rep.ProblemsTruncated, HeadSeq: rep.HeadSeq,
		VerifiedAt: when}
}

// Retain drops every database not in databases: one removed from the
// fleet stops exporting its chains.
func (h *HealthRegistry) Retain(databases []string) {
	if h == nil {
		return
	}
	keep := make(map[string]bool, len(databases))
	for _, d := range databases {
		keep[d] = true
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for k := range h.chains {
		if !keep[k.database] {
			delete(h.chains, k)
		}
	}
	for d := range h.errs {
		if !keep[d] {
			delete(h.errs, d)
		}
	}
}

// Reset drops everything: a sidecar that is not the fleet leader does
// not verify, so it must not export a result that is going stale.
func (h *HealthRegistry) Reset() {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	clear(h.chains)
	clear(h.errs)
}

// Snapshot copies the registry, sorted by database then chain.
func (h *HealthRegistry) Snapshot() HealthSnapshot {
	var s HealthSnapshot
	if h == nil {
		return s
	}
	h.mu.Lock()
	for _, c := range h.chains {
		problems := make(map[string]int, len(c.Problems))
		for k, n := range c.Problems {
			problems[k] = n
		}
		c.Problems = problems
		s.Chains = append(s.Chains, c)
		if s.VerifiedAt == nil || c.VerifiedAt.After(*s.VerifiedAt) {
			at := c.VerifiedAt
			s.VerifiedAt = &at
		}
	}
	for _, e := range h.errs {
		s.Errors = append(s.Errors, e)
	}
	h.mu.Unlock()
	sort.Slice(s.Chains, func(i, j int) bool {
		a, b := s.Chains[i], s.Chains[j]
		return a.Database < b.Database || (a.Database == b.Database && a.Chain < b.Chain)
	})
	sort.Slice(s.Errors, func(i, j int) bool { return s.Errors[i].Database < s.Errors[j].Database })
	return s
}
