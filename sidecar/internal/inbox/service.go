package inbox

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"
)

// Bounds of a listing.
const (
	DefaultTimeout = 2 * time.Second
	DefaultLimit   = 500
	MaxLimit       = 10000
	defaultWorkers = 16
)

// A source answers these for a database it cannot read; anything else is
// a failed read.
var (
	ErrNotConnected = errors.New("inbox: database not connected")
	ErrUnavailable  = errors.New("inbox: source unavailable on this database")
)

// Source lists one kind's waiting items on one monitored database.
type Source interface {
	List(ctx context.Context, database string) ([]Item, error)
}

// SourceFunc adapts a function to Source.
type SourceFunc func(ctx context.Context, database string) ([]Item, error)

// List calls f.
func (f SourceFunc) List(ctx context.Context, database string) ([]Item, error) {
	return f(ctx, database)
}

// fleetWide marks a source read once per listing, not once per database.
type fleetWide interface {
	Source
	fleetWide()
}

// FleetWide is a source read once per listing for the whole fleet (with
// database ""): for items of databases with no runtime, which no
// per-database source can read. Each item names its own database.
type FleetWide func(ctx context.Context, database string) ([]Item, error)

// List calls f.
func (f FleetWide) List(ctx context.Context, database string) ([]Item, error) {
	return f(ctx, database)
}

func (FleetWide) fleetWide() {}

// Service fans a listing out to one source per kind and database. It
// holds no mutable state, so concurrent List calls share nothing.
type Service struct {
	// Sources maps each kind to its source; a kind missing or mapped to
	// nil is reported as "source not configured", never as empty.
	// A fleet-wide source (FleetWide) is read once per listing, with
	// database ""; its items name their own database and are kept when
	// the request may read it.
	Sources map[Kind]Source
	Timeout time.Duration // per source and database; 0 is DefaultTimeout
	Workers int           // concurrent reads; 0 is 16
}

// Request selects what to list.
type Request struct {
	Databases []string // the databases the caller may read
	Kinds     []Kind   // nil: every kind
	Limit     int      // items returned; <= 0 is DefaultLimit
	// AllDatabases: the caller may read every database, including the
	// ones with no runtime (observe-only members), so a fleet-wide
	// source's items are not narrowed to Databases.
	AllDatabases bool
}

// SourceError names a source (its kind) and database it could not read.
type SourceError struct {
	Source   string `json:"source"`
	Database string `json:"database,omitempty"`
	Error    string `json:"error"`
}

// Result is a listing. Total and CountsByKind count every item found,
// before Limit; with any error they are a lower bound (LowerBound), the
// rule the pending badge uses. Items, CountsByKind and Errors are never
// nil.
type Result struct {
	Items        []Item        `json:"items"`
	CountsByKind map[Kind]int  `json:"counts_by_kind"`
	Total        int           `json:"total"`
	LowerBound   bool          `json:"lower_bound"`
	Truncated    bool          `json:"truncated"`
	Errors       []SourceError `json:"errors"`
}

type job struct {
	kind     Kind
	database string
	src      Source
	fleet    bool // a fleet-wide read: its items are narrowed to the request
}

type outcome struct {
	items []Item
	fail  *SourceError
}

// List reads every requested kind on every database in req, each read
// bounded by the timeout, and sorts the items by risk, then age.
func (s *Service) List(ctx context.Context, req Request) Result {
	kinds := req.Kinds
	if len(kinds) == 0 {
		kinds = Kinds()
	}
	res := Result{Items: []Item{}, CountsByKind: map[Kind]int{}, Errors: []SourceError{}}
	var jobs []job
	for _, k := range kinds {
		res.CountsByKind[k] = 0
		src := s.Sources[k]
		switch _, wide := src.(fleetWide); {
		case src == nil:
			res.Errors = append(res.Errors, SourceError{Source: string(k),
				Error: "source not configured"})
		case wide:
			jobs = append(jobs, job{kind: k, src: src, fleet: true})
		default:
			for _, db := range req.Databases {
				jobs = append(jobs, job{kind: k, database: db, src: src})
			}
		}
	}
	readable := map[string]bool{}
	for _, db := range req.Databases {
		readable[db] = true
	}
	for i, o := range s.run(ctx, jobs) {
		if o.fail != nil {
			res.Errors = append(res.Errors, *o.fail)
			continue
		}
		for _, it := range o.items {
			if !jobs[i].fleet || req.AllDatabases || readable[it.Database] {
				res.Items = append(res.Items, it)
			}
		}
	}
	finish(&res, req.Limit)
	return res
}

// run reads every job with at most Workers at once; outcomes keep the
// jobs' order.
func (s *Service) run(ctx context.Context, jobs []job) []outcome {
	out := make([]outcome, len(jobs))
	workers := s.Workers
	if workers <= 0 {
		workers = defaultWorkers
	}
	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup
	for i := range jobs {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			out[i] = s.read(ctx, jobs[i])
		}(i)
	}
	wg.Wait()
	return out
}

func (s *Service) timeout() time.Duration {
	if s.Timeout > 0 {
		return s.Timeout
	}
	return DefaultTimeout
}

// read calls one source under the timeout. A source that ignores its
// context still times out: its late answer is dropped.
func (s *Service) read(ctx context.Context, j job) outcome {
	tctx, cancel := context.WithTimeout(ctx, s.timeout())
	defer cancel()
	done := make(chan outcome, 1)
	go func() {
		items, err := j.src.List(tctx, j.database)
		if err != nil {
			done <- outcome{fail: s.failure(j, err)}
			return
		}
		done <- outcome{items: stamp(items, j)}
	}()
	select {
	case o := <-done:
		return o
	case <-tctx.Done():
		return outcome{fail: s.failure(j, tctx.Err())}
	}
}

// failure words err for the caller; a database error itself is logged,
// never returned, since viewers read the inbox.
func (s *Service) failure(j job, err error) *SourceError {
	f := &SourceError{Source: string(j.kind), Database: j.database}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		f.Error = fmt.Sprintf("timed out after %s", s.timeout())
	case errors.Is(err, context.Canceled):
		f.Error = "cancelled"
	case errors.Is(err, ErrNotConnected):
		f.Error = "not connected"
	case errors.Is(err, ErrUnavailable):
		f.Error = "unavailable on this database"
	default:
		slog.Error("inbox source read failed", "source", j.kind,
			"database", j.database, "error", err)
		f.Error = "read failed; the sidecar log has the cause"
	}
	return f
}

// stamp fills the kind and database a source left empty.
func stamp(items []Item, j job) []Item {
	for i := range items {
		if items[i].Kind == "" {
			items[i].Kind = j.kind
		}
		if items[i].Database == "" {
			items[i].Database = j.database
		}
		items[i].Risk = NormalizeRisk(items[i].Risk)
	}
	return items
}

// finish sorts, counts and truncates res.
func finish(res *Result, limit int) {
	if limit <= 0 {
		limit = DefaultLimit
	}
	order := map[Kind]int{}
	for i, k := range Kinds() {
		order[k] = i
	}
	sort.SliceStable(res.Items, func(a, b int) bool {
		x, y := res.Items[a], res.Items[b]
		if rx, ry := riskRank(x.Risk), riskRank(y.Risk); rx != ry {
			return rx < ry
		}
		if !x.CreatedAt.Equal(y.CreatedAt) {
			return x.CreatedAt.Before(y.CreatedAt)
		}
		if x.Kind != y.Kind {
			return order[x.Kind] < order[y.Kind]
		}
		if x.Database != y.Database {
			return x.Database < y.Database
		}
		return x.ID < y.ID
	})
	for _, it := range res.Items {
		res.CountsByKind[it.Kind]++
	}
	res.Total = len(res.Items)
	res.LowerBound = len(res.Errors) > 0
	if len(res.Items) > limit {
		res.Items, res.Truncated = res.Items[:limit], true
	}
}
