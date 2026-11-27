package runway

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// playbooks-03b: the runway writer names a sequence with quote_ident on
// each part, so a mixed-case sequence is public."Name" while the
// analyzer's sequence_exhaustion finding says public.Name. The cases API
// matches both spellings; this pins the runway's half on a real server.
func TestMonitorTick_SequenceIdentifierIsQuoteIdentSpelling(t *testing.T) {
	pool, ctx := livePool(t)
	id := time.Now().UnixNano()
	tbl, seq := fmt.Sprintf("mon_ident_%d", id), fmt.Sprintf("Mon_Ident_%d_Seq", id)
	liveSequence(t, ctx, pool, fmt.Sprintf(`CREATE TABLE %[1]s (id int);
		CREATE SEQUENCE "%[2]s" OWNED BY %[1]s.id;
		SELECT setval('"%[2]s"', 2100000000);
		SELECT nextval('"%[2]s"') FROM generate_series(1, 5);`, tbl, seq))
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), "DROP TABLE "+tbl) })
	quoted := `public."` + seq + `"`
	seedSeries(t, ctx, pool, quoted, 2100000000, 55, 2147483647)
	m := newTestMonitor(t, pool, probes.NewRunner(pool, probes.Catalog(),
		probes.NewLimiter(1)), &fakeStarter{})
	if _, err := m.Tick(ctx); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if n, detail := findingDetail(t, ctx, pool, quoted); n != 1 || detail == nil {
		t.Fatalf("%d open findings for %s (runway detail %v), want the runway "+
			"finding under the quote_ident spelling", n, quoted, detail)
	}
	if n, _ := findingDetail(t, ctx, pool, "public."+seq); n != 0 {
		t.Fatalf("%d open findings under the unquoted spelling, want 0", n)
	}
}
