package changeplan

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/pg-sage/sidecar/internal/evidence/workpack"
	"github.com/pg-sage/sidecar/internal/histstore"
)

// lookupTimeout bounds each resolver read of the sage schema.
const lookupTimeout = 10 * time.Second

// DBResolver resolves a plan's citations against the database: findings
// in the monitored database's sage.findings, evidence ids in a stored
// workload pack (evid-02a, workpack.Resolve) and target calls in the
// hourly rollup, both where the database's history lives (histstore).
// Decoded packs are cached per hash; a stored pack never changes.
type DBResolver struct {
	monitored querier
	history   any
	mu        sync.Mutex
	packs     map[string]*workpack.Pack
}

// NewDBResolver resolves findings on monitored and packs and rollups on
// history (the monitored pool, or what histstore.Resolve serves for it).
func NewDBResolver(monitored querier, history any) *DBResolver {
	return &DBResolver{monitored: monitored, history: history,
		packs: map[string]*workpack.Pack{}}
}

// FindingExists reports whether sage.findings holds id.
func (r *DBResolver) FindingExists(ctx context.Context, id int64) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, lookupTimeout)
	defer cancel()
	var ok bool
	err := r.monitored.QueryRow(ctx, `/* pg_sage */ SELECT EXISTS (SELECT 1 FROM sage.findings
		WHERE id = $1)`, id).Scan(&ok)
	if err != nil {
		return false, fmt.Errorf("look up finding %d: %w", id, err)
	}
	return ok, nil
}

// EvidenceExists reports whether stored pack packHash holds id; a pack
// that is not stored, or a malformed id, is false with no error.
func (r *DBResolver) EvidenceExists(ctx context.Context, packHash, id string) (bool, error) {
	if !workpack.ValidSHA(packHash) {
		return false, nil
	}
	p, err := r.pack(ctx, packHash)
	if errors.Is(err, workpack.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if _, err := workpack.Resolve(p, id); err != nil {
		if errors.Is(err, workpack.ErrUnknownEvidence) {
			return false, nil
		}
		return false, fmt.Errorf("resolve %q in pack %s: %w", id, packHash, err)
	}
	return true, nil
}

func (r *DBResolver) pack(ctx context.Context, sha string) (*workpack.Pack, error) {
	r.mu.Lock()
	p, ok := r.packs[sha]
	r.mu.Unlock()
	if ok {
		return p, nil
	}
	ctx, cancel := context.WithTimeout(ctx, lookupTimeout)
	defer cancel()
	_, p, err := workpack.Load(ctx, r.history, sha)
	if err != nil {
		if errors.Is(err, workpack.ErrNotFound) {
			return nil, err
		}
		return nil, fmt.Errorf("load pack %s: %w", sha, err)
	}
	r.mu.Lock()
	r.packs[sha] = p
	r.mu.Unlock()
	return p, nil
}

// TargetCalls sums the calls of queryids in sage.query_rollup_hourly over
// the last window (none recorded is 0, which fails closed: the step must
// declare itself unverifiable).
func (r *DBResolver) TargetCalls(ctx context.Context, queryids []int64,
	window time.Duration) (int64, error) {
	if len(queryids) == 0 {
		return 0, nil
	}
	ctx, cancel := context.WithTimeout(ctx, lookupTimeout)
	defer cancel()
	st := histstore.Resolve(r.history)
	var calls int64
	err := st.QueryRow(ctx, `/* pg_sage */ SELECT COALESCE(sum(h.calls), 0)::bigint
		  FROM sage.query_rollup_hourly h
		 WHERE {db:h} AND h.queryid = ANY ($1)
		   AND h.hour >= now() - make_interval(secs => $2)`, queryids,
		window.Seconds()).Scan(&calls)
	if err != nil {
		return 0, fmt.Errorf("read the target calls of %v: %w", queryids, err)
	}
	return calls, nil
}

// SettingExists reports whether pg_settings on the monitored database has
// the setting name (an operator step's landing check, plan-02d).
func (r *DBResolver) SettingExists(ctx context.Context, name string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, lookupTimeout)
	defer cancel()
	var ok bool
	err := r.monitored.QueryRow(ctx, `/* pg_sage */ SELECT EXISTS (SELECT 1
		FROM pg_catalog.pg_settings WHERE name = $1)`, name).Scan(&ok)
	if err != nil {
		return false, fmt.Errorf("look up setting %s: %w", name, err)
	}
	return ok, nil
}

// ManagedChangeExists reports whether sage.managed_change_proposals holds
// id (an operator step linked to a managed change, plan-02d).
func (r *DBResolver) ManagedChangeExists(ctx context.Context, id int64) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, lookupTimeout)
	defer cancel()
	var ok bool
	err := r.monitored.QueryRow(ctx, `/* pg_sage */ SELECT EXISTS (SELECT 1
		FROM sage.managed_change_proposals WHERE id = $1)`, id).Scan(&ok)
	if err != nil {
		return false, fmt.Errorf("look up managed change %d: %w", id, err)
	}
	return ok, nil
}
