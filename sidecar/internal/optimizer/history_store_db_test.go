package optimizer

import (
	"context"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/histstore"
	"github.com/pg-sage/sidecar/internal/testsupport/histfixture"
)

// history.store: meta. The cold-start check counts this database's
// snapshots only: another database's history in the store must not make a
// new database look warm.
func TestColdStartIsPerDatabaseInBothPlacements(t *testing.T) {
	p := histfixture.NewPair(t)
	ctx := context.Background()
	for _, mode := range histfixture.Modes() {
		p.Switch(t, mode)
		p.Noise(t, time.Now(), 1, 2, 3)
		p.Noise(t, time.Now().Add(-time.Minute), 1, 2, 3)
		cold, err := CheckColdStart(ctx, p.Monitored, 3)
		if err != nil {
			t.Fatalf("%s: %v", mode, err)
		}
		if !cold {
			t.Fatalf("%s: a database with no snapshots of its own must be cold", mode)
		}
		st := histstore.Resolve(p.Monitored)
		for i := 0; i < 3; i++ {
			if _, err := st.Exec(ctx, `INSERT INTO sage.snapshots (collected_at, category,
				data{dbcol}) VALUES (now(), 'system', '{}'{dbval})`); err != nil {
				t.Fatal(err)
			}
		}
		cold, err = CheckColdStart(ctx, p.Monitored, 3)
		if err != nil || cold {
			t.Fatalf("%s: three snapshots of its own make it warm (cold=%v, err=%v)",
				mode, cold, err)
		}
	}
}
