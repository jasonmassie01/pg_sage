package fleet

import (
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/config"
)

// D3: value readers see every registered instance, sorted by name,
// including a failed instance (nil pool) so it is reported, not dropped.
func TestValueSourcesListsEveryInstanceByName(t *testing.T) {
	if got := ValueSources(nil); got != nil {
		t.Fatalf("nil manager sources = %#v, want nil", got)
	}
	mgr := NewManager(&config.Config{Mode: "fleet"})
	if got := ValueSources(mgr); len(got) != 0 {
		t.Fatalf("empty manager sources = %#v", got)
	}
	pool := &pgxpool.Pool{}
	mgr.RegisterInstance(&DatabaseInstance{Name: "orders", Pool: pool})
	mgr.RegisterInstance(&DatabaseInstance{Name: "billing"})

	got := ValueSources(mgr)
	if len(got) != 2 || got[0].Name != "billing" || got[1].Name != "orders" {
		t.Fatalf("sources = %#v, want billing then orders", got)
	}
	if got[0].Pool != nil || got[1].Pool != pool {
		t.Fatalf("source pools not carried through: %#v", got)
	}
}
