package fleetlearn

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Source is one fleet database to read findings from.
type Source struct {
	Name string
	Pool *pgxpool.Pool
}

// SourceError is a database whose findings could not be read. Error is
// safe to show; Cause is for the log.
type SourceError struct {
	Database string `json:"database"`
	Error    string `json:"error"`
	Cause    error  `json:"-"`
}

// FleetFindingsResult is a fleet-wide read of recurring findings.
// Truncated names the databases with more open findings than the
// per-database cap, whose recurrence counts may be low.
type FleetFindingsResult struct {
	Findings         []FleetFinding `json:"findings"`
	MinDatabases     int            `json:"min_databases"`
	DatabasesScanned int            `json:"databases_scanned"`
	Errors           []SourceError  `json:"errors"`
	Truncated        []string       `json:"truncated"`
}

const (
	openFindingsSQL = `/* pg_sage */ SELECT id, category, severity,
	object_identifier, title, COALESCE(recommendation, ''), last_seen
	FROM sage.findings WHERE status = 'open'
	ORDER BY last_seen DESC LIMIT $1`
	defaultFindingsPerDatabase = 500
	findingsReadTimeout        = 10 * time.Second
	findingsRowTTL             = 60 * time.Second
	findingsErrorTTL           = 15 * time.Second
	findingsDeadline           = 15 * time.Second
	findingsParallel           = 8
)

// FindingsReader reads the fleet's open findings with a per-source cache:
// rows live a minute and errors 15 s, concurrent misses for one source
// share one read, at most 8 reads run at once, and a call waits at most
// 15 s. Grouping is per call, so callers may ask for different sources and
// minimums.
type FindingsReader struct {
	rowTTL, errTTL, deadline, readTimeout time.Duration
	sem                                   chan struct{}
	now                                   func() time.Time
	read                                  func(context.Context, Source, int) (
		[]FindingRow, bool, error)

	mu    sync.Mutex
	cache map[string]*sourceRead
}

// sourceRead is one read of a source. Its fields are written once, before
// done is closed, and never after.
type sourceRead struct {
	pool      *pgxpool.Pool
	limit     int
	done      chan struct{}
	at        time.Time
	rows      []FindingRow
	truncated bool
	err       error
}

func (s *sourceRead) finished() bool {
	select {
	case <-s.done:
		return true
	default:
		return false
	}
}

// NewFindingsReader returns a reader with the production limits.
func NewFindingsReader() *FindingsReader {
	return &FindingsReader{rowTTL: findingsRowTTL, errTTL: findingsErrorTTL,
		deadline: findingsDeadline, readTimeout: findingsReadTimeout,
		sem: make(chan struct{}, findingsParallel), now: time.Now,
		read: readOpenFindings, cache: map[string]*sourceRead{}}
}

// sharedFindings is the process-wide reader the API and MCP share.
var sharedFindings = NewFindingsReader()

// CollectFleetFindings reads the open findings of every source (at most
// perDatabaseLimit each) through the process-wide cache and groups the
// recurring ones. A database that cannot be read is reported in Errors
// and never hides the others.
func CollectFleetFindings(ctx context.Context, sources []Source, minDatabases,
	perDatabaseLimit int) FleetFindingsResult {
	return sharedFindings.Collect(ctx, sources, minDatabases, perDatabaseLimit)
}

// Collect reads sources (cached when fresh) and groups the findings open
// on at least minDatabases of them. Sources still pending at the deadline
// are reported in Errors as timed out; their reads finish in the
// background and fill the cache.
func (r *FindingsReader) Collect(ctx context.Context, sources []Source, minDatabases,
	perDatabaseLimit int) FleetFindingsResult {
	if minDatabases < MinFleetDatabases {
		minDatabases = MinFleetDatabases
	}
	if perDatabaseLimit <= 0 {
		perDatabaseLimit = defaultFindingsPerDatabase
	}
	reads := r.begin(ctx, sources, perDatabaseLimit)
	wait, cancel := context.WithTimeout(ctx, r.deadline)
	defer cancel()
	res := FleetFindingsResult{MinDatabases: minDatabases, Errors: []SourceError{},
		Truncated: []string{}}
	var rows []FindingRow
	for i, sr := range reads {
		name := sources[i].Name
		select {
		case <-sr.done:
		case <-wait.Done():
		}
		switch {
		case !sr.finished():
			res.Errors = append(res.Errors, SourceError{Database: name, Error: "timed out",
				Cause: fmt.Errorf("fleet findings: read %q still pending after %s: %w",
					name, r.deadline, wait.Err())})
		case sr.err != nil:
			res.Errors = append(res.Errors, SourceError{Database: name,
				Error: "read open findings failed", Cause: sr.err})
		default:
			res.DatabasesScanned++
			if sr.truncated {
				res.Truncated = append(res.Truncated, name)
			}
			rows = append(rows, sr.rows...)
		}
	}
	res.Findings = GroupFleetFindings(rows, minDatabases)
	return res
}

// begin returns each source's current read, starting one for every source
// with no fresh or in-flight read, and drops stale reads no longer asked.
func (r *FindingsReader) begin(ctx context.Context, sources []Source,
	limit int) []*sourceRead {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	for name, sr := range r.cache {
		if sr.finished() && now.Sub(sr.at) >= r.rowTTL {
			delete(r.cache, name)
		}
	}
	out := make([]*sourceRead, len(sources))
	for i, s := range sources {
		sr := r.cache[s.Name]
		if sr == nil || !r.usable(sr, s, limit, now) {
			sr = &sourceRead{pool: s.Pool, limit: limit, done: make(chan struct{})}
			r.cache[s.Name] = sr
			go r.fetch(context.WithoutCancel(ctx), sr, s)
		}
		out[i] = sr
	}
	return out
}

// usable reports whether sr answers for s: the same pool and limit, and
// still in flight or younger than its TTL.
func (r *FindingsReader) usable(sr *sourceRead, s Source, limit int, now time.Time) bool {
	if sr.pool != s.Pool || sr.limit != limit {
		return false
	}
	if !sr.finished() {
		return true
	}
	ttl := r.rowTTL
	if sr.err != nil {
		ttl = r.errTTL
	}
	return now.Sub(sr.at) < ttl
}

// fetch runs one read under the reader's semaphore and read timeout. It is
// detached from the caller so a caller that leaves cannot fail the read
// for the others waiting on it.
func (r *FindingsReader) fetch(ctx context.Context, sr *sourceRead, s Source) {
	r.sem <- struct{}{}
	defer func() { <-r.sem }()
	ctx, cancel := context.WithTimeout(ctx, r.readTimeout)
	defer cancel()
	rows, truncated, err := r.read(ctx, s, sr.limit)
	r.mu.Lock()
	sr.rows, sr.truncated, sr.err, sr.at = rows, truncated, err, r.now()
	close(sr.done)
	r.mu.Unlock()
}

// readOpenFindings reads at most limit open findings of s, newest first,
// and reports whether more were open.
func readOpenFindings(ctx context.Context, s Source, limit int) ([]FindingRow, bool,
	error) {
	if s.Pool == nil {
		return nil, false, fmt.Errorf("fleet findings: database %q has no connection", s.Name)
	}
	rows, err := s.Pool.Query(ctx, openFindingsSQL, limit+1)
	if err != nil {
		return nil, false, fmt.Errorf("fleet findings: read %q: %w", s.Name, err)
	}
	defer rows.Close()
	var out []FindingRow
	for rows.Next() {
		r := FindingRow{Database: s.Name}
		if err := rows.Scan(&r.ID, &r.Category, &r.Severity, &r.ObjectIdentifier,
			&r.Title, &r.Recommendation, &r.LastSeen); err != nil {
			return nil, false, fmt.Errorf("fleet findings: scan %q: %w", s.Name, err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("fleet findings: read %q: %w", s.Name, err)
	}
	if len(out) > limit {
		return out[:limit], true, nil
	}
	return out, false, nil
}
