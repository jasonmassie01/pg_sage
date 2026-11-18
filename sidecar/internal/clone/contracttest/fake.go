// Package contracttest is the contract every clone provider meets
// (lab-05a): run against the fakes here and, behind their env flags,
// against the live providers of lab-05b to lab-05g.
package contracttest

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/pg-sage/sidecar/internal/clone"
)

// Fake is an in-memory clone provider of one kind that names resources
// by CloneSpec.ResourceName, so it is a clone.Finder too.
type Fake struct {
	kind Kind
	host string

	mu        sync.Mutex
	n         int
	resources map[string]string // ref -> resource name
}

// Kind is a clone kind (re-exported for callers of NewFake).
type Kind = clone.Kind

// NewFake is a fake of kind whose clones' DSNs point at host.
func NewFake(kind Kind, host string) *Fake {
	return &Fake{kind: kind, host: host, resources: map[string]string{}}
}

// Create makes a resource named spec.ResourceName.
func (f *Fake) Create(ctx context.Context, spec clone.CloneSpec) (clone.Clone, error) {
	if err := ctx.Err(); err != nil {
		return clone.Clone{}, err
	}
	if spec.ResourceName == "" {
		return clone.Clone{}, errors.New("fake clone provider: a resource name is required")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.n++
	ref := fmt.Sprintf("fake-%d", f.n)
	f.resources[ref] = spec.ResourceName
	return clone.Clone{ID: ref, DSN: fmt.Sprintf("postgres://%s:5432/%s", f.host,
		spec.ResourceName), CreatedFrom: time.Now()}, nil
}

// Destroy removes a resource; a missing one is already destroyed.
func (f *Fake) Destroy(_ context.Context, c clone.Clone) error {
	if c.ID == "" {
		return errors.New("fake clone provider: clone ID is required")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.resources, c.ID)
	return nil
}

// SnapshotAge is zero: a fake clones the present.
func (f *Fake) SnapshotAge(context.Context) (time.Duration, error) { return 0, nil }

// Describe is the fake's kind.
func (f *Fake) Describe(context.Context) (clone.Capability, error) {
	return clone.Capability{Kind: f.kind, TypicalReady: time.Second, MaxTTL: time.Hour}, nil
}

// FindByName is the ref of the resource named name.
func (f *Fake) FindByName(_ context.Context, name string) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for ref, n := range f.resources {
		if n == name {
			return ref, true, nil
		}
	}
	return "", false, nil
}

// Exists reports whether ref is a live resource.
func (f *Fake) Exists(_ context.Context, ref string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.resources[ref]
	return ok, nil
}
