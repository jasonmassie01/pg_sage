package gameday

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/clone"
)

// keyedProvider is a shared target without a cluster lock.
type keyedProvider struct {
	fakeProvider
	key string
}

func (p *keyedProvider) TargetKey() string { return p.key }

var _ clone.SharedTarget = (*keyedProvider)(nil)

func TestLeasesHoldOneRunPerTargetKey(t *testing.T) {
	l := NewLeases()
	ctx := context.Background()
	p := &keyedProvider{key: "scratch:5432"}
	r1, err := l.Acquire(ctx, p, Holder{Kind: "game day", ID: "gd-1", Database: "orders"})
	if err != nil || r1 == nil {
		t.Fatalf("a free target: release %v, err %v", r1 != nil, err)
	}
	same := &keyedProvider{key: "scratch:5432"} // another runtime's provider
	_, err = l.Acquire(ctx, same, Holder{Kind: "local bench", ID: "lb-1", Database: "users"})
	if !errors.Is(err, ErrRunning) || !strings.Contains(err.Error(),
		"target busy: game day gd-1 for orders") {
		t.Fatalf("a held target: err = %v", err)
	}
	other, err := l.Acquire(ctx, &keyedProvider{key: "scratch:5433"}, Holder{Kind: "game day",
		ID: "gd-2", Database: "users"})
	if err != nil {
		t.Fatalf("another target was held: %v", err)
	}
	other()
	r1()
	r2, err := l.Acquire(ctx, same, Holder{Kind: "local bench", ID: "lb-2", Database: "users"})
	if err != nil {
		t.Fatalf("a released target: %v", err)
	}
	r1() // a stale release must not free the new holder's lease
	_, err = l.Acquire(ctx, p, Holder{Kind: "game day", ID: "gd-3", Database: "orders"})
	if !errors.Is(err, ErrRunning) || !strings.Contains(err.Error(), "local bench lb-2") {
		t.Fatalf("a stale release freed the target: err = %v", err)
	}
	r2()
}

// A provider that gives each run its own clone needs no lease.
func TestLeasesDoNotHoldPerRunClones(t *testing.T) {
	l := NewLeases()
	p := &fakeProvider{}
	h := Holder{Kind: "game day", ID: "gd-1", Database: "orders"}
	r1, err1 := l.Acquire(context.Background(), p, h)
	r2, err2 := l.Acquire(context.Background(), p, h)
	if err1 != nil || err2 != nil || r1 == nil || r2 == nil {
		t.Fatalf("per-run clones were leased: %v %v", err1, err2)
	}
	r1()
	r2()
}

// Invalid input fails closed: no provider, or a shared target without a key.
func TestLeasesRefuseUnkeyedTargets(t *testing.T) {
	l := NewLeases()
	h := Holder{Kind: "game day", ID: "gd-1", Database: "orders"}
	if r, err := l.Acquire(context.Background(), nil, h); err == nil || r != nil {
		t.Fatalf("nil provider: release %v, err %v", r != nil, err)
	}
	r, err := l.Acquire(context.Background(), &keyedProvider{key: "  "}, h)
	if err == nil || r != nil || errors.Is(err, ErrRunning) ||
		!strings.Contains(err.Error(), "target key") {
		t.Fatalf("empty key: release %v, err %v", r != nil, err)
	}
}

// A local target is one shared target per cluster; its key carries no
// credentials. Clone providers give each run its own target.
func TestLocalProviderIsASharedTarget(t *testing.T) {
	p, _ := sharedLocal(t, 6010)
	var st clone.SharedTarget = p
	if st.TargetKey() != "scratch.internal:6010" {
		t.Fatalf("target key = %q", st.TargetKey())
	}
	q := mustLocal(t, "postgres://other:pw@SCRATCH.internal:6010/another", nil,
		refusing(prodCluster()))
	if q.TargetKey() != p.TargetKey() || strings.Contains(q.TargetKey(), "pw") {
		t.Fatalf("one cluster, two keys: %q and %q", p.TargetKey(), q.TargetKey())
	}
	if _, ok := any(&fakeProvider{}).(clone.SharedTarget); ok {
		t.Fatal("a per-run clone provider is a shared target")
	}
}
