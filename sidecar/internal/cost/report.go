package cost

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TopQueries is how many queries the cost section lists.
const TopQueries = 20

// ReasonNotConfigured is the section's reason without rates.
const ReasonNotConfigured = "rates not configured"

// maxConcurrentReads bounds the per-database reads of one section.
const maxConcurrentReads = 8

// Member is one monitored database and the provider resource it runs on
// ("" when no cloud telemetry names it).
type Member struct {
	Database string
	Resource string
	// Identity names the physical database (cluster and database; "":
	// unknown), so two fleet entries for one database count once.
	Identity string
	Pool     *pgxpool.Pool
	aliases  []string // later fleet names of the same physical database
}

// dedupe keeps the first member of each physical database, as the value
// ledger does; the later names still find its rates and its resource.
func dedupe(members []Member) []Member {
	out := make([]Member, 0, len(members))
	at := map[string]int{}
	for _, m := range members {
		i, seen := at[m.Identity]
		if m.Identity == "" || !seen {
			if m.Identity != "" {
				at[m.Identity] = len(out)
			}
			out = append(out, m)
			continue
		}
		out[i].aliases = append(out[i].aliases, m.Database)
		if out[i].Resource == "" {
			out[i].Resource = m.Resource
		}
	}
	return out
}

// key groups the databases of one instance: its resource, else itself.
func (m Member) key() string {
	if m.Resource != "" {
		return m.Resource
	}
	return "db:" + m.Database
}

// Reader reads one member's history.
type Reader interface {
	Deltas(ctx context.Context, m Member, from, to time.Time) ([]QueryDelta, int, error)
	LatestHour(ctx context.Context, m Member) (*HourRow, error)
}

// ResourceCost is one instance's cost over the window, counted once
// however many of its databases pg_sage monitors.
type ResourceCost struct {
	Resource    string   `json:"resource"`
	Databases   []string `json:"databases"`
	Currency    string   `json:"currency"`
	RateSource  string   `json:"rate_source"`
	AsOf        string   `json:"as_of"`
	VCPUs       int      `json:"vcpus"`
	Compute     float64  `json:"compute"`
	Storage     *float64 `json:"storage"`
	IOPS        *float64 `json:"iops"`
	Total       float64  `json:"total"`
	ExecSeconds float64  `json:"exec_seconds"`
	IdleShare   *float64 `json:"idle_share"`
}

// Section is the Value report's cost section. Without rates it is
// unavailable with a reason, and it never shows a figure without its
// rate source and date.
type Section struct {
	Available    bool           `json:"available"`
	Reason       string         `json:"reason,omitempty"`
	RateSource   string         `json:"rate_source,omitempty"`
	AsOf         string         `json:"as_of,omitempty"` // oldest rate date
	Currency     string         `json:"currency,omitempty"`
	WindowStart  time.Time      `json:"window_start"`
	WindowEnd    time.Time      `json:"window_end"`
	WindowHours  float64        `json:"window_hours"`
	Compute      *float64       `json:"compute"`
	Storage      *float64       `json:"storage"`
	IOPS         *float64       `json:"iops"`
	Total        *float64       `json:"total"`
	IdleShare    *float64       `json:"idle_share"`
	TopQueries   []QueryCost    `json:"top_queries"`
	Resources    []ResourceCost `json:"resources"`
	DroppedPairs int            `json:"dropped_pairs"`
	Unpriced     []string       `json:"unpriced,omitempty"`
	Partial      bool           `json:"partial"`
	Unavailable  []string       `json:"unavailable,omitempty"`
}

type emptyRates interface{ empty() bool }

func ratesMissing(rates RateSource) bool {
	if rates == nil {
		return true
	}
	e, ok := rates.(emptyRates)
	return ok && e.empty()
}

// Build prices every member's instance over [from, to) and attributes the
// compute cost to the queries of the instance's databases.
func Build(ctx context.Context, r Reader, members []Member, rates RateSource,
	from, to time.Time) Section {
	s := Section{WindowStart: from, WindowEnd: to, WindowHours: to.Sub(from).Hours(),
		TopQueries: []QueryCost{}, Resources: []ResourceCost{}}
	if ratesMissing(rates) {
		s.Reason = ReasonNotConfigured
		return s
	}
	groups := groupMembers(dedupe(members), rates, &s)
	if len(groups) == 0 {
		s.Reason = "no configured rate matches the monitored databases"
		return s
	}
	readGroups(ctx, r, groups, from, to)
	for _, g := range groups {
		s.add(g, to.Sub(from))
	}
	s.finish()
	return s
}

// group is one priced instance and what was read for it.
type group struct {
	key     string
	rates   Rates
	members []Member
	latest  *HourRow
	deltas  []QueryDelta
	dropped int
	errs    []string
	mu      sync.Mutex
}

func groupMembers(members []Member, rates RateSource, s *Section) []*group {
	byKey := map[string]*group{}
	var out []*group
	for _, m := range members {
		g := byKey[m.key()]
		if g == nil {
			g = &group{key: m.key()}
			byKey[m.key()] = g
			out = append(out, g)
		}
		g.members = append(g.members, m)
	}
	priced := out[:0]
	for _, g := range out {
		if r, ok := lookupRates(g, rates); ok {
			g.rates = r
			priced = append(priced, g)
			continue
		}
		for _, m := range g.members {
			s.Unpriced = append(s.Unpriced, m.Database)
		}
	}
	return priced
}

func lookupRates(g *group, rates RateSource) (Rates, bool) {
	for _, m := range g.members {
		for _, name := range append([]string{m.Database}, m.aliases...) {
			if r, ok := rates.Rates(m.Resource, name); ok {
				return r, true
			}
		}
	}
	return Rates{}, false
}

// readGroups reads every member's deltas and every instance's newest
// hour, at most maxConcurrentReads at a time.
func readGroups(ctx context.Context, r Reader, groups []*group, from, to time.Time) {
	sem := make(chan struct{}, maxConcurrentReads)
	var wg sync.WaitGroup
	for _, g := range groups {
		for i, m := range g.members {
			wg.Add(1)
			go func(g *group, m Member, first bool) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				g.read(ctx, r, m, first, from, to)
			}(g, m, i == 0)
		}
	}
	wg.Wait()
}

func (g *group) read(ctx context.Context, r Reader, m Member, first bool,
	from, to time.Time) {
	deltas, dropped, err := r.Deltas(ctx, m, from, to)
	var latest *HourRow
	var hourErr error
	if first && m.Resource != "" {
		latest, hourErr = r.LatestHour(ctx, m)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if err != nil {
		g.errs = append(g.errs, m.Database+": "+err.Error())
	} else {
		g.deltas = append(g.deltas, deltas...)
		g.dropped += dropped
	}
	if hourErr != nil {
		g.errs = append(g.errs, m.Database+": host usage: "+hourErr.Error())
	} else if latest != nil {
		g.latest = latest
	}
}

// sortedDeltas orders deltas so attribution does not depend on read order.
func (g *group) sortedDeltas() []QueryDelta {
	out := append([]QueryDelta(nil), g.deltas...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Database != out[j].Database {
			return out[i].Database < out[j].Database
		}
		return out[i].QueryID < out[j].QueryID
	})
	return out
}
