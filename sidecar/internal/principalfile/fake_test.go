package principalfile

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
)

// memBackend is an in-memory Backend standing in for the core
// workstream's principal store.
type memBackend struct {
	mu        sync.Mutex
	byID      map[string]*Principal
	next      int
	failAfter int // fail the mutation after this many succeed (0 = never)
	calls     []string
}

func newMem(ps ...Principal) *memBackend {
	m := &memBackend{byID: map[string]*Principal{}}
	for _, p := range ps {
		cp := p
		if cp.ID == "" {
			m.next++
			cp.ID = fmt.Sprintf("agp_%020d", m.next)
		}
		m.byID[cp.ID] = &cp
	}
	return m
}

var errInjected = errors.New("injected backend failure")

func (m *memBackend) record(call string) error {
	if m.failAfter > 0 && len(m.calls) >= m.failAfter {
		return errInjected
	}
	m.calls = append(m.calls, call)
	return nil
}

func (m *memBackend) List(context.Context) ([]Principal, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Principal, 0, len(m.byID))
	for _, p := range m.byID {
		cp := *p
		cp.Identities = append([]Identity(nil), p.Identities...)
		out = append(out, cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (m *memBackend) Create(_ context.Context, p Principal) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.record("create " + p.Name); err != nil {
		return "", err
	}
	m.next++
	p.ID = fmt.Sprintf("agp_%020d", m.next)
	p.Identities = nil
	m.byID[p.ID] = &p
	return p.ID, nil
}

func (m *memBackend) Update(_ context.Context, id string, c Change) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.byID[id]
	if !ok {
		return fmt.Errorf("no principal %s", id)
	}
	if err := m.record(string(c.Op) + " " + p.Name + " " + c.Field); err != nil {
		return err
	}
	switch c.Field {
	case "profile":
		p.Profile = c.To
	case "env_ceiling":
		p.EnvCeiling = c.To
	case "sponsor":
		p.Sponsor = c.To
	case "tenant":
		p.Tenant = c.To
	case "status":
		p.Status = c.To
	case "managed_by":
		p.ManagedBy = c.To
	}
	return nil
}

func (m *memBackend) Bind(_ context.Context, id string, ident Identity) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.record("bind " + m.byID[id].Name + " " + ident.Subject); err != nil {
		return err
	}
	m.byID[id].Identities = append(m.byID[id].Identities, ident)
	return nil
}

func (m *memBackend) Unbind(_ context.Context, id string, ident Identity) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.record("unbind " + m.byID[id].Name + " " + ident.Subject); err != nil {
		return err
	}
	p := m.byID[id]
	kept := p.Identities[:0]
	for _, i := range p.Identities {
		if i != ident {
			kept = append(kept, i)
		}
	}
	p.Identities = kept
	return nil
}

// memCatalog knows two profiles and two users.
type memCatalog struct{}

func (memCatalog) ProfileClasses(name string) ([]string, bool) {
	switch name {
	case "readonly-analyst", "legacy":
		return []string{"read"}, true
	case "app-writer":
		return []string{"read", "write_insert", "write_update"}, true
	}
	return nil, false
}

func (memCatalog) UserExists(sponsor string) bool {
	return sponsor == "alice@example.com" || sponsor == "bob@example.com"
}
