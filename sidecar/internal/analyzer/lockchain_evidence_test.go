package analyzer

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
)

// Lock-chain findings carry the backend identity the executor re-checks
// before an approved cancel/terminate (executor identity recheck).
func TestLockChainDetail_BackendEvidence(t *testing.T) {
	started := time.Date(2026, 9, 26, 8, 0, 0, 123456000, time.UTC)
	since := started.Add(time.Minute)
	c := LockChain{
		RootBlockerPID: 4242, RootBlockerQuery: strings.Repeat("é", 250),
		RootBlockerApp: "app", RootBlockerSince: since,
		RootBlockerBackendStart: started, RootBlockerQueryID: 77,
	}
	d := lockChainDetail(c)
	if d["pid"] != 4242 || d["query_id"] != int64(77) || d["app_name"] != "app" {
		t.Fatalf("identity fields = %v %v %v", d["pid"], d["query_id"], d["app_name"])
	}
	if d["backend_start"] != started || d["query_start"] != since {
		t.Fatalf("times = %v %v", d["backend_start"], d["query_start"])
	}
	if q, _ := d["query"].(string); len([]rune(q)) != 200 || strings.HasSuffix(q, "...") {
		t.Fatalf("query evidence must be LEFT(query,200) without ellipsis, got %d runes",
			len([]rune(q)))
	}
	if _, ok := lockChainDetail(LockChain{})["backend_start"]; ok {
		t.Fatal("zero backend_start must be omitted so the executor rejects it")
	}
}

// The lock-chain query (with backend_start/query_id) runs on the fixture.
func TestDetectLockChains_QueryRuns(t *testing.T) {
	pool := phase2Pool(t)
	cfg := &config.Config{}
	cfg.Analyzer.LockChain.Enabled = true
	chains, err := DetectLockChains(context.Background(), pool, cfg)
	if err != nil {
		t.Fatalf("DetectLockChains: %v", err)
	}
	for _, c := range chains {
		if c.RootBlockerBackendStart.IsZero() {
			t.Fatalf("chain without backend_start: %+v", c)
		}
	}
}
