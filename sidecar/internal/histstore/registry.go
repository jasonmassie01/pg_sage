package histstore

import (
	"errors"
	"fmt"
	"reflect"
	"sort"
	"sync"
	"sync/atomic"
)

// The registry maps a monitored database's pool to its history store. A
// runtime registers its pool when history.store is meta (or when the
// monitored database is the meta database itself) and unregisters it when
// it stops; every reader resolves the store from the pool it holds.

type registration struct {
	token uint64
	name  string
	store Store
}

var registry = struct {
	sync.RWMutex
	byDB map[any]registration
	next uint64
}{byDB: map[any]registration{}}

// Registration is one registered monitored database.
type Registration struct {
	Monitored any
	Name      string
	Store     Store
}

// Register makes Resolve(monitored) return s until the returned func runs.
// A later registration of the same pool replaces this one, and this one's
// unregister then leaves the later one alone.
func Register(monitored any, name string, s Store) func() {
	if !hashable(monitored) {
		return func() {}
	}
	registry.Lock()
	registry.next++
	token := registry.next
	registry.byDB[monitored] = registration{token: token, name: name, store: s}
	registry.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			registry.Lock()
			defer registry.Unlock()
			if r, ok := registry.byDB[monitored]; ok && r.token == token {
				delete(registry.byDB, monitored)
			}
		})
	}
}

// ErrNotRegistered: the process keeps history in the meta database and the
// pool a reader holds has no registered store, so its history is unknown.
var ErrNotRegistered = errors.New("histstore: history.store is meta and this pool has no " +
	"registered history store")

// processMeta is the process's history.store: true when it is meta.
var processMeta atomic.Bool

// refused counts the statements refused with ErrNotRegistered.
var refused atomic.Uint64

// SetProcessMode sets where this process keeps history (history.store).
// In meta mode an unregistered pool is refused instead of read as its own
// history. The sidecar calls it once at startup; the default is monitored.
func SetProcessMode(m Mode) error {
	switch m {
	case ModeMonitored, ModeMeta:
		processMeta.Store(m == ModeMeta)
		return nil
	}
	return fmt.Errorf("history process mode %q: want %q or %q", m, ModeMonitored, ModeMeta)
}

// ProcessMode is the mode SetProcessMode set.
func ProcessMode() Mode {
	if processMeta.Load() {
		return ModeMeta
	}
	return ModeMonitored
}

// Refused is how many statements were refused with ErrNotRegistered since
// the process started: each one is a reader wired to a pool no runtime
// registered.
func Refused() uint64 { return refused.Load() }

// Resolve returns the history store of the monitored database db: the
// registered one, else db itself in monitored mode. In meta mode an
// unregistered db resolves to a store that refuses every statement with
// ErrNotRegistered: reading the pool's own tables would read the wrong
// history. A Store resolves to itself, so code given a Store directly runs
// on it; nil resolves to the zero Store.
func Resolve(db any) Store {
	if s, ok := db.(Store); ok {
		return s
	}
	if isNil(db) {
		return Store{}
	}
	if hashable(db) {
		registry.RLock()
		r, ok := registry.byDB[db]
		registry.RUnlock()
		if ok {
			return r.store
		}
	}
	if processMeta.Load() {
		return Store{err: fmt.Errorf("%w for %T: the runtime that owns it must Register "+
			"it (history.store: meta)", ErrNotRegistered, db)}
	}
	return NewMonitored(db)
}

// Registrations lists the registered monitored databases, by name.
func Registrations() []Registration {
	registry.RLock()
	out := make([]Registration, 0, len(registry.byDB))
	for db, r := range registry.byDB {
		out = append(out, Registration{Monitored: db, Name: r.name, Store: r.store})
	}
	registry.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// hashable reports whether v can be a map key (a pool pointer can; a
// struct holding a slice would panic).
func hashable(v any) bool {
	return v != nil && reflect.TypeOf(v).Comparable()
}
