package trigger

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// Hub is where request-driven sources (the upgrade readiness route, the
// migration lint route) publish: each database runtime registers its
// queue while its review loop runs. A publish to a database without one
// is ErrUnknownDatabase. Every failure is counted in FireErrors and
// returned; the hook's caller logs it and answers its own request anyway.
type Hub struct {
	mu  sync.RWMutex
	dbs map[string]hubEntry
}

type hubEntry struct {
	store      *Store
	largeBytes int64
}

// Default is the process's hub.
var Default = NewHub()

// NewHub returns an empty hub.
func NewHub() *Hub { return &Hub{dbs: map[string]hubEntry{}} }

// Register makes database's queue the target of its publishes; tables
// larger than largeTableBytes make a migration large.
func (h *Hub) Register(database string, s *Store, largeTableBytes int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.dbs[database] = hubEntry{store: s, largeBytes: largeTableBytes}
}

// Unregister removes database's queue.
func (h *Hub) Unregister(database string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.dbs, database)
}

// resolve is database, or the only registered database when it is "".
func (h *Hub) resolve(database string) (string, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if database == "" && len(h.dbs) == 1 {
		for name := range h.dbs {
			return name, true
		}
	}
	_, ok := h.dbs[database]
	return database, ok
}

func (h *Hub) entry(database string, kind Kind) (string, hubEntry, error) {
	name, ok := h.resolve(database)
	if !ok {
		countFireError(kind)
		return "", hubEntry{}, fmt.Errorf("%w: %s trigger for %q", ErrUnknownDatabase, kind,
			database)
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	return name, h.dbs[name], nil
}

// PreUpgrade fires a pre_upgrade review for database's readiness request
// toward targetMajor, at most once per target and UTC day.
func (h *Hub) PreUpgrade(ctx context.Context, database string, targetMajor int,
	method string, now time.Time) error {
	name, e, err := h.entry(database, KindPreUpgrade)
	if err != nil {
		return err
	}
	_, _, err = e.store.Fire(ctx, Trigger{Database: name, Kind: KindPreUpgrade,
		Key: fmt.Sprintf("major:%d:%s", targetMajor, now.UTC().Format(time.DateOnly)),
		Detail: map[string]any{"target_major": targetMajor, "method": method,
			"requested_at": now.UTC().Format(time.RFC3339)}})
	return err
}

// LargeMigration fires a large_migration review when a reviewed migration
// touches a table larger than review.large_table_bytes; a migration of
// smaller tables fires nothing.
func (h *Hub) LargeMigration(ctx context.Context, database string, tables []TableSize,
	now time.Time) error {
	name, e, err := h.entry(database, KindLargeMigration)
	if err != nil {
		return err
	}
	large := LargeTables(tables, e.largeBytes)
	if len(large) == 0 {
		return nil
	}
	_, _, err = e.store.Fire(ctx, Trigger{Database: name, Kind: KindLargeMigration,
		Key: migrationKey(large, now), Detail: map[string]any{"tables": large,
			"large_table_bytes": e.largeBytes}})
	return err
}
