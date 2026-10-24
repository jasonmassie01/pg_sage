package impact

import (
	"context"
	"sync"
)

// A review session's workload reads. The reasoner's tool calls of one
// review (queries_touching, table_context, whatif_index, inspect_statement)
// each build a run of their own; without a cache every call would read
// pg_stat_statements' text file under its lock and the query_store
// history again. Within one session the same reads are answered once.
// Outside a session (Analyze, a context without the cache) every read
// goes to the database.

type sessionCacheKey struct{}

// WithSessionCache returns ctx carrying a fresh workload cache for one
// review session; the engine's tool calls under it share their reads.
func WithSessionCache(ctx context.Context) context.Context {
	return context.WithValue(ctx, sessionCacheKey{}, &sessionCache{
		reads: map[string]workloadRead{}, rated: map[int64]bool{},
		rate: map[int64]queryRate{}})
}

// workloadRead is one pg_stat_statements read: the matching statements
// (unparsed) and the count of hidden texts.
type workloadRead struct {
	rows   []workStmt
	hidden int64
}

// sessionCache is safe for concurrent use. A nil cache (no session)
// keeps nothing.
type sessionCache struct {
	mu    sync.Mutex
	reads map[string]workloadRead
	rated map[int64]bool
	rate  map[int64]queryRate
}

func sessionCacheOf(ctx context.Context) *sessionCache {
	c, _ := ctx.Value(sessionCacheKey{}).(*sessionCache)
	return c
}

func (c *sessionCache) workload(key string) (workloadRead, bool) {
	if c == nil {
		return workloadRead{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	w, ok := c.reads[key]
	return w, ok
}

func (c *sessionCache) keepWorkload(key string, w workloadRead) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reads[key] = w
}

// rates is the known rates of ids and the ids not read yet.
func (c *sessionCache) rates(ids []int64) (map[int64]queryRate, []int64) {
	out := map[int64]queryRate{}
	if c == nil {
		return out, ids
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	var missing []int64
	for _, id := range ids {
		if !c.rated[id] {
			missing = append(missing, id)
		} else if q, ok := c.rate[id]; ok {
			out[id] = q
		}
	}
	return out, missing
}

// keepRates records a read of ids (those without history included).
func (c *sessionCache) keepRates(ids []int64, read map[int64]queryRate) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, id := range ids {
		c.rated[id] = true
		if q, ok := read[id]; ok {
			c.rate[id] = q
		}
	}
}
