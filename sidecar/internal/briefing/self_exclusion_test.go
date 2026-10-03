package briefing

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/testsupport/selfload"
)

// The briefing's system section reports the application's active
// sessions; pg_sage's own are not workload (perf v1.8.3, perf-selfexcl).
// The count is server-wide, so the test compares minimums of several
// samples. (Connections stay every slot in use: capacity, not workload.)
func TestGatherSystemActiveLeavesOutPgSage(t *testing.T) {
	pool, ctx := requireDB(t)
	w := selfload.New(t, testDSN())
	w.StartApp(t)
	worker := New(pool, &config.Config{}, nil, func(string, string, ...any) {})
	active := func() (float64, error) {
		raw, err := worker.gatherSystem(ctx)
		if err != nil {
			return 0, err
		}
		var doc map[string]any
		if err := json.Unmarshal([]byte(raw), &doc); err != nil {
			return 0, err
		}
		n, ok := doc["active"].(float64)
		if !ok {
			return 0, fmt.Errorf("briefing system active = %v", doc["active"])
		}
		return n, nil
	}
	before := selfload.MinOver(t, 8, active)
	w.StartSage(t, 6) // 8 active pg_sage sessions
	after := selfload.MinOver(t, 8, active)
	if before < 2 || after >= before+4 {
		t.Fatalf("briefing active %.0f -> %.0f after 8 active pg_sage sessions started, "+
			"want them left out", before, after)
	}
}
