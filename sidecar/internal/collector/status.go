package collector

import (
	"sync"
	"sync/atomic"
	"time"
)

// BreakerState is the collector circuit breaker's state.
type BreakerState string

const (
	// BreakerClosed: cycles run.
	BreakerClosed BreakerState = "closed"
	// BreakerSkipping: recent cycles were skipped, not yet dormant.
	BreakerSkipping BreakerState = "skipping"
	// BreakerDormant: enough consecutive skips to slow to the dormant
	// interval until three cycles succeed.
	BreakerDormant BreakerState = "dormant"
)

// Code is the state as the pg_sage_collector_breaker_state gauge value
// (0 closed, 1 skipping, 2 dormant; -1 for an unknown state).
func (s BreakerState) Code() int {
	switch s {
	case BreakerClosed:
		return 0
	case BreakerSkipping:
		return 1
	case BreakerDormant:
		return 2
	}
	return -1
}

// StatusCapabilities is what the collector currently reads from the
// database (re-derived each cycle).
type StatusCapabilities struct {
	PGVersionNum       int  `json:"pg_version_num"`
	StatStatements     bool `json:"pg_stat_statements"`
	HasWALColumns      bool `json:"has_wal_columns"`
	HasPlanTimeColumns bool `json:"has_plan_time_columns"`
}

// Status is the collector's health: what used to reach only the logs.
type Status struct {
	BreakerState BreakerState `json:"breaker_state"`
	// BreakerReason is why the breaker skipped (SkipReasonLoad or
	// SkipReasonLoadQueryError); empty while closed.
	BreakerReason    string           `json:"breaker_reason,omitempty"`
	ConsecutiveSkips int              `json:"consecutive_skips"`
	SkipsTotal       map[string]int64 `json:"skips_total"`
	// LastLoadRatio is active client sessions / max_connections at the
	// last successful load query; LoadCeiling is cpu_ceiling_pct / 100.
	LastLoadRatio float64 `json:"last_load_ratio"`
	LoadCeiling   float64 `json:"load_ceiling"`
	// LastSuccess is when a snapshot was last collected.
	LastSuccess time.Time `json:"last_success"`
	// LastErrorAt and LastErrorText are the latest failure of the load
	// query, a collection, a persist or a query_store write.
	LastErrorAt   time.Time `json:"last_error_at"`
	LastErrorText string    `json:"last_error,omitempty"`
	// Unavailable is the latest snapshot's unreadable categories.
	Unavailable        map[string]string  `json:"unavailable"`
	PersistFailures    int64              `json:"persist_failures"`
	QueryStoreFailures int64              `json:"query_store_failures"`
	Capabilities       StatusCapabilities `json:"capabilities"`
}

// health holds the collector's outcome record; the times and the error
// text are guarded by mu, the counters are atomics.
type health struct {
	mu                 sync.Mutex
	lastSuccess        time.Time
	lastErrorAt        time.Time
	lastError          string
	persistFailures    atomic.Int64
	queryStoreFailures atomic.Int64
}

// noteSuccess records a collected snapshot.
func (c *Collector) noteSuccess() {
	c.health.mu.Lock()
	c.health.lastSuccess = c.clock()
	c.health.mu.Unlock()
}

// noteError records err as the latest failure of what; nil is ignored.
func (c *Collector) noteError(what string, err error) {
	if err == nil {
		return
	}
	c.health.mu.Lock()
	c.health.lastErrorAt = c.clock()
	c.health.lastError = what + ": " + err.Error()
	c.health.mu.Unlock()
}

func (c *Collector) notePersistFailure(err error) {
	c.health.persistFailures.Add(1)
	c.noteError("persist", err)
}

func (c *Collector) noteQueryStoreFailure(err error) {
	c.health.queryStoreFailures.Add(1)
	c.noteError("query_store", err)
}

// Status reports the collector's health. It is safe to call from any
// goroutine, before the first cycle too, and returns copies.
func (c *Collector) Status() Status {
	b := c.breaker.view()
	caps := c.Capabilities()
	st := Status{
		BreakerState:     b.state,
		BreakerReason:    b.reason,
		ConsecutiveSkips: b.consecutiveSkips,
		SkipsTotal:       b.skipsTotal,
		LastLoadRatio:    b.lastRatio,
		LoadCeiling:      b.ceiling,
		Unavailable:      map[string]string{},
		Capabilities: StatusCapabilities{
			PGVersionNum:       caps.PGVersionNum,
			StatStatements:     caps.StatStatementsView,
			HasWALColumns:      caps.WALColumns,
			HasPlanTimeColumns: caps.PlanTimeColumns,
		},
		PersistFailures:    c.health.persistFailures.Load(),
		QueryStoreFailures: c.health.queryStoreFailures.Load(),
	}
	c.health.mu.Lock()
	st.LastSuccess = c.health.lastSuccess
	st.LastErrorAt, st.LastErrorText = c.health.lastErrorAt, c.health.lastError
	c.health.mu.Unlock()
	c.mu.RLock()
	if c.latest != nil {
		for k, v := range c.latest.Unavailable {
			st.Unavailable[k] = v
		}
	}
	c.mu.RUnlock()
	return st
}
