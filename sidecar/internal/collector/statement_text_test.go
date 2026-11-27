package collector

import (
	"fmt"
	"sync"
	"testing"
)

// StatementText (sre-07) serves the investigator's EXPLAIN the statement
// text the collector already holds in memory, so the explainer never
// loads pg_stat_statements' whole query-text file.

func TestStatementText_FindsTheLatestSnapshotsText(t *testing.T) {
	c := &Collector{latest: &Snapshot{Queries: []QueryStats{
		{QueryID: 11, Query: "SELECT 1"},
		{QueryID: -42, Query: "SELECT v FROM t WHERE id = $1"},
	}}}
	got, ok := c.StatementText(-42)
	if !ok || got != "SELECT v FROM t WHERE id = $1" {
		t.Fatalf("StatementText(-42) = %q, %v", got, ok)
	}
	if got, ok := c.StatementText(11); !ok || got != "SELECT 1" {
		t.Fatalf("StatementText(11) = %q, %v", got, ok)
	}
}

func TestStatementText_MissingTextIsNotCached(t *testing.T) {
	cases := map[string]*Collector{
		"nil collector": nil,
		"no snapshot":   {},
		"other ids": {latest: &Snapshot{
			Queries: []QueryStats{{QueryID: 1, Query: "SELECT 1"}}}},
		"empty text": {latest: &Snapshot{Queries: []QueryStats{{QueryID: 7, Query: ""}}}},
		"no queries": {latest: &Snapshot{}},
	}
	for name, c := range cases {
		if got, ok := c.StatementText(7); ok || got != "" {
			t.Errorf("%s: StatementText(7) = %q, %v; want not cached", name, got, ok)
		}
	}
	c := &Collector{latest: &Snapshot{Queries: []QueryStats{{QueryID: 0, Query: "x"}}}}
	if _, ok := c.StatementText(0); ok {
		t.Fatal("queryid 0 (no queryid) served a text")
	}
}

func TestStatementText_ReadsTheNewestSnapshotUnderConcurrentCollection(t *testing.T) {
	c := &Collector{}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			snap := &Snapshot{Queries: []QueryStats{{QueryID: 5, Query: fmt.Sprintf("q%d", i)}}}
			c.mu.Lock()
			c.previous, c.latest = c.latest, snap
			c.mu.Unlock()
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			if got, ok := c.StatementText(5); ok && got == "" {
				t.Error("served an empty text")
			}
		}
	}()
	wg.Wait()
	if got, ok := c.StatementText(5); !ok || got != "q199" {
		t.Fatalf("after collection StatementText(5) = %q, %v; want q199", got, ok)
	}
}
