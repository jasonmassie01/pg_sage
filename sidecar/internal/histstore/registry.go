package histstore

import (
	"reflect"
	"sort"
	"sync"
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

// Resolve returns the history store of the monitored database db: the
// registered one, else db itself (monitored mode). A Store resolves to
// itself, so code given a Store directly runs on it.
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
