package api

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/histstore"
	"github.com/pg-sage/sidecar/internal/snapstore"
	"github.com/pg-sage/sidecar/internal/testsupport/histfixture"
)

// history.store: meta. /snapshots/latest and /snapshots/history read the
// store; the answers must equal monitored mode's, with another database's
// newer snapshots in the store.
func TestSnapshotEndpointsIdenticalInBothPlacements(t *testing.T) {
	p := histfixture.NewPair(t)
	ctx := context.Background()
	start := time.Now().Add(-5 * time.Hour).Truncate(time.Second)
	type answer struct {
		latest  string
		history string
		trunc   bool
	}
	got := map[histstore.Mode]answer{}
	for _, mode := range histfixture.Modes() {
		p.Switch(t, mode)
		w := snapstore.NewWriter()
		for i := 0; i < 5; i++ {
			doc, _ := json.Marshal(map[string]any{"active_backends": i + 1,
				"max_connections": 100})
			if err := w.Persist(ctx, p.Monitored, start.Add(time.Duration(i)*time.Hour),
				[]snapstore.Row{{Category: "system", Data: doc}}); err != nil {
				t.Fatalf("persist: %v", err)
			}
		}
		p.Noise(t, time.Now(), 1) // the newest system snapshot belongs to database 8
		latest, err := querySnapshotLatest(ctx, p.Monitored, "system")
		if err != nil {
			t.Fatalf("%s latest: %v", mode, err)
		}
		points, trunc, err := querySnapshotHistory(ctx, p.Monitored, "system", 24,
			time.Time{}, time.Time{})
		if err != nil {
			t.Fatalf("%s history: %v", mode, err)
		}
		l, _ := json.Marshal(latest)
		h, _ := json.Marshal(points)
		got[mode] = answer{latest: string(l), history: string(h), trunc: trunc}
		if len(points) != 5 {
			t.Fatalf("%s: history has %d points, want this database's 5", mode, len(points))
		}
	}
	mon, meta := got[histstore.ModeMonitored], got[histstore.ModeMeta]
	if mon != meta {
		t.Fatalf("endpoints differ:\nmonitored %+v\nmeta      %+v", mon, meta)
	}
	if mon.latest != `{"active_backends":5,"max_connections":100}` {
		t.Fatalf("latest = %s, want this database's newest snapshot", mon.latest)
	}
}
