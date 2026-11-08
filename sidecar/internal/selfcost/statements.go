package selfcost

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// MaxClassifiedKeys bounds a sampler's cache of pg_stat_statements keys
// already told apart as pg_sage's or not. Past it, application keys are
// not remembered and make every sample read the query texts again, which
// is what each sample cost before the cache.
const MaxClassifiedKeys = 2000

// versionSQL is the installed pg_stat_statements version of this database.
const versionSQL = `/* pg_sage */ SELECT extversion FROM pg_extension
WHERE extname = 'pg_stat_statements'`

// The counters of this database's entries, without their texts: reading
// the texts loads pg_stat_statements' whole query-text file. The 1.8 forms
// have no toplevel column and no pg_stat_statements_info (added in 1.9).
const (
	countersSQL = `/* pg_sage */ SELECT userid, queryid, toplevel,
  (total_exec_time + total_plan_time)::float8, calls,
  (shared_blks_hit + shared_blks_read)::int8,
  (SELECT stats_reset FROM pg_stat_statements_info)
FROM pg_stat_statements(false)
WHERE dbid = (SELECT oid FROM pg_database WHERE datname = current_database())
  AND queryid IS NOT NULL`
	countersSQL18 = `/* pg_sage */ SELECT userid, queryid, false,
  (total_exec_time + total_plan_time)::float8, calls,
  (shared_blks_hit + shared_blks_read)::int8, NULL::timestamptz
FROM pg_stat_statements(false)
WHERE dbid = (SELECT oid FROM pg_database WHERE datname = current_database())
  AND queryid IS NOT NULL`
)

// The tagged entries with their texts, read only when the counters show
// keys not yet classified. pg_sage's statements carry the /* pg_sage */
// tag: that is how its own entries are found.
const (
	textsSQL = `/* pg_sage */ SELECT userid, queryid, toplevel,
  (total_exec_time + total_plan_time)::float8, calls,
  (shared_blks_hit + shared_blks_read)::int8, left(query, 2000)
FROM pg_stat_statements(true)
WHERE dbid = (SELECT oid FROM pg_database WHERE datname = current_database())
  AND queryid IS NOT NULL AND strpos(query, '/* pg_sage') > 0`
	textsSQL18 = `/* pg_sage */ SELECT userid, queryid, false,
  (total_exec_time + total_plan_time)::float8, calls,
  (shared_blks_hit + shared_blks_read)::int8, left(query, 2000)
FROM pg_stat_statements(true)
WHERE dbid = (SELECT oid FROM pg_database WHERE datname = current_database())
  AND queryid IS NOT NULL AND strpos(query, '/* pg_sage') > 0`
)

// extension is what the installed pg_stat_statements version allows.
type extension struct {
	version  string
	readable bool // 1.8+: total_exec_time and total_plan_time
	topLevel bool // 1.9+: toplevel and pg_stat_statements_info
}

// classified is one cached key: pg_sage's (with its text) or not, and its
// calls at the last sample (a decrease means the entry was re-created).
type classified struct {
	sage  bool
	calls int64
	text  string
}

// Sampler reads pg_sage's pg_stat_statements entries of one database. It
// keeps the extension version (read once) and a bounded cache of keys
// already classified, so a sample reads the query texts only when new
// keys appear, an entry was re-created or the statistics were reset.
// Safe for concurrent use; each runtime (database) has its own.
type Sampler struct {
	mu       sync.Mutex
	logFn    func(level, msg string, args ...any)
	ext      *extension
	keys     map[StatementKey]classified
	reset    time.Time
	notified bool
}

// NewSampler returns an empty sampler; logFn (may be nil) receives the
// one notice that pg_stat_statements needs an update.
func NewSampler(logFn func(level, msg string, args ...any)) *Sampler {
	return &Sampler{logFn: logFn, keys: map[StatementKey]classified{}}
}

// Statements samples pg_sage's entries. known is false, with no error,
// when pg_stat_statements is not installed, not preloaded or older than
// 1.8; any other failure is an error. A nil sampler samples without a
// cache.
func (s *Sampler) Statements(ctx context.Context, q Querier) (
	map[StatementKey]StatementCounters, bool, error) {
	if s == nil {
		s = NewSampler(nil)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out, err := s.sample(ctx, q)
	switch {
	case err == nil:
		return out, true, nil
	case extensionOutdated(err):
		s.forget()
		s.notice(err)
		return nil, false, nil
	case errors.Is(err, errNotReadable):
		s.notice(err)
		return nil, false, nil
	case errors.Is(err, pgx.ErrNoRows), statementsUnavailable(err):
		s.forget()
		return nil, false, nil
	}
	return nil, false, fmt.Errorf("pg_stat_statements: %w", err)
}

var errNotReadable = errors.New("pg_stat_statements is older than 1.8")

func (s *Sampler) sample(ctx context.Context, q Querier) (
	map[StatementKey]StatementCounters, error) {
	ext, err := s.extension(ctx, q)
	if err != nil {
		return nil, err
	}
	if !ext.readable {
		return nil, fmt.Errorf("%w (installed %s)", errNotReadable, ext.version)
	}
	counters, reset, err := readCounters(ctx, q, ext.topLevel)
	if err != nil {
		return nil, fmt.Errorf("read counters: %w", err)
	}
	s.revalidate(counters, reset)
	var texts map[StatementKey]StatementCounters
	if s.unclassified(counters) {
		if texts, err = readTexts(ctx, q, ext.topLevel); err != nil {
			return nil, fmt.Errorf("read tagged texts: %w", err)
		}
	}
	return s.classify(counters, texts), nil
}

// extension reads the installed version once; forget drops it.
func (s *Sampler) extension(ctx context.Context, q Querier) (extension, error) {
	if s.ext != nil {
		return *s.ext, nil
	}
	var v string
	if err := q.QueryRow(ctx, versionSQL).Scan(&v); err != nil {
		return extension{}, err
	}
	ext := extension{version: v, readable: atLeast(v, 1, 8), topLevel: atLeast(v, 1, 9)}
	if ext.readable { // an older one is read again: the advised update shows at once
		s.ext = &ext
	}
	return ext, nil
}

// forget drops the version and the cache: the extension went away or is
// not the version it was.
func (s *Sampler) forget() {
	s.ext = nil
	s.keys = map[StatementKey]classified{}
	s.reset = time.Time{}
}

func (s *Sampler) notice(cause error) {
	if s.notified || s.logFn == nil {
		return
	}
	s.notified = true
	s.logFn("INFO", "selfcost: pg_sage's own statement cost is unknown on this database "+
		"(%v); run ALTER EXTENSION pg_stat_statements UPDATE to measure it", cause)
}

// atLeast compares a "major.minor" extension version numerically (1.10
// is newer than 1.9).
func atLeast(version string, major, minor int) bool {
	maj, min, _ := strings.Cut(version, ".")
	ma, err := strconv.Atoi(maj)
	if err != nil {
		return false
	}
	mi, _ := strconv.Atoi(min)
	return ma > major || (ma == major && mi >= minor)
}

// extensionOutdated is 42703 (undefined column): the installed version
// lacks a column this version of pg_sage reads.
func extensionOutdated(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "42703"
}
