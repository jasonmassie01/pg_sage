package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/cases"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/store"
)

var caseTicketSyncOnce sync.Once

// startCaseTicketSync runs the case ticket sync worker once per process,
// on the router shutdown context (market-14).
func startCaseTicketSync(
	mgr *fleet.DatabaseManager, pool *pgxpool.Pool, ns *store.NotificationStore,
	deps notificationRouteDeps,
) {
	caseTicketSyncOnce.Do(func() {
		syncer := &cases.TicketSyncer{
			Store:     cases.NewTicketStore(pool),
			Tracker:   jiraTrackerFor(ns, deps.policy),
			CaseState: newCaseStateCache(mgr, time.Minute).lookup,
			Log: func(format string, args ...any) {
				slog.Warn("case ticket sync", "detail", fmt.Sprintf(format, args...))
			},
		}
		go syncer.Run(shutdownContext(), cases.TicketSyncInterval)
	})
}

// caseStateCache reads case states for the sync worker, keeping each
// answer for ttl so one cycle over many tickets reads a case once.
type caseStateCache struct {
	mgr  *fleet.DatabaseManager
	ttl  time.Duration
	mu   sync.Mutex
	seen map[string]cachedCaseState
	now  func() time.Time
	read func(ctx context.Context, database, id string) (cases.State, error)
}

// caseStateCacheMax bounds the cache; past it the cache starts over.
const caseStateCacheMax = 1024

var errNoFleet = errors.New("no databases are monitored")

type cachedCaseState struct {
	state cases.State
	at    time.Time
}

func newCaseStateCache(mgr *fleet.DatabaseManager, ttl time.Duration) *caseStateCache {
	c := &caseStateCache{mgr: mgr, ttl: ttl, seen: map[string]cachedCaseState{},
		now: time.Now}
	c.read = func(ctx context.Context, database, id string) (cases.State, error) {
		if c.mgr == nil {
			return "", errNoFleet
		}
		state, _, err := readCaseState(ctx, c.mgr, database, id)
		return state, err
	}
	return c
}

// lookup is a cases.CaseStateFunc: known is false when the database could
// not be read (no comment is made on an unknown state).
func (c *caseStateCache) lookup(
	ctx context.Context, database, id string,
) (cases.State, bool) {
	key := database + "\x00" + id
	c.mu.Lock()
	hit, ok := c.seen[key]
	c.mu.Unlock()
	if ok && c.now().Sub(hit.at) < c.ttl {
		return hit.state, true
	}
	state, err := c.read(ctx, database, id)
	if err != nil {
		return "", false
	}
	c.mu.Lock()
	if len(c.seen) >= caseStateCacheMax {
		c.seen = map[string]cachedCaseState{}
	}
	c.seen[key] = cachedCaseState{state: state, at: c.now()}
	c.mu.Unlock()
	return state, true
}
