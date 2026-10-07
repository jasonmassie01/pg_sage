package slo

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/histstore"
	"github.com/pg-sage/sidecar/internal/testsupport/histfixture"
)

// history.store: meta. The latency proxy slices this database's
// query_store captures in the store; another database's newer captures
// (same query ids, 10 s means) must not change the slice.
func TestLatencyProxyIdenticalInBothPlacements(t *testing.T) {
	p := histfixture.NewPair(t)
	ctx := context.Background()
	cfg := DefaultProxyConfig()
	cfg.LatencyThresholdMs = 5
	got := map[histstore.Mode]string{}
	for _, mode := range histfixture.Modes() {
		p.Switch(t, mode)
		st := histstore.Resolve(p.Monitored)
		for i, age := range []int{240, 120} {
			for q := 0; q < 3; q++ {
				if _, err := st.Exec(ctx, `INSERT INTO sage.query_store (captured_at, queryid,
					calls, total_exec_time, mean_exec_time{dbcol})
					VALUES (date_trunc('second', now()) - $1 * interval '1 second', $2, $3, $4,
					0{dbval})`, age, 9001+q, int64(100*(i+1)*(q+1)),
					float64(300*(i+1)*(q+1)+50*i)); err != nil {
					t.Fatal(err)
				}
			}
		}
		p.Noise(t, time.Now().Add(-10*time.Second), 9001, 9002, 9003)
		s := NewLatencyProxy(p.Monitored, cfg).Slice(ctx, time.Now(), fakeHistory{})
		if s.Value == nil {
			t.Fatalf("%s: no value (%s)", mode, s.Reason)
		}
		got[mode] = fmt.Sprintf("%.6f/%v/%v/%s", *s.Value, s.Eligible, s.Bad, s.Reason)
	}
	if got[histstore.ModeMonitored] != got[histstore.ModeMeta] {
		t.Fatalf("latency slices differ: monitored %s, meta %s",
			got[histstore.ModeMonitored], got[histstore.ModeMeta])
	}
}
