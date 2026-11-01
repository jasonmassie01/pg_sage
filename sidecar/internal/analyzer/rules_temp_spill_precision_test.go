package analyzer

import (
	"encoding/json"
	"strconv"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/collector"
)

func TestTempSpillQueryIDJSONPrecision(t *testing.T) {
	for _, id := range []int64{-5892612755791055559, 8379109690210330486, -9223372036854775808, 9223372036854775807} {
		t.Run(strconv.FormatInt(id, 10), func(t *testing.T) {
			now := time.Now()
			q := collector.QueryStats{QueryID: id, Query: "SELECT payload FROM public.orders ORDER BY payload", Calls: 1}
			prev := &collector.Snapshot{CollectedAt: now.Add(-time.Minute), StatsEpoch: now.Add(-time.Hour), Queries: []collector.QueryStats{q}}
			q.Calls++
			q.TempBlksWritten = 256
			cur := &collector.Snapshot{CollectedAt: now, StatsEpoch: prev.StatsEpoch, Queries: []collector.QueryStats{q}}
			findings := ruleTempSpills(cur, prev, nil, nil)
			if len(findings) != 1 {
				t.Fatalf("findings=%v", findings)
			}
			raw, err := json.Marshal(findings[0].Detail)
			if err != nil {
				t.Fatal(err)
			}
			var browser map[string]any
			if err := json.Unmarshal(raw, &browser); err != nil || browser["queryid"] != strconv.FormatInt(id, 10) {
				t.Fatalf("queryid lost signed64 precision: %s (%v)", raw, err)
			}
		})
	}
}
