package changefeed

import (
	"context"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// guard-02b review: an investigation reads the feed through the
// change_feed probe, so a provider event's attribution must reach that
// evidence, not only the API JSON.
func TestProviderEvent_DBAttributionInProbeEvidence(t *testing.T) {
	f := newPollFixture(t)
	now := time.Now().UTC()
	id := "rds:evidence:" + string(f.scope.DatabaseID)
	r := &fakeReader{events: []ProviderEvent{{ID: id, Kind: KindFailover,
		Summary: "A Multi-AZ failover has completed.", At: now.Add(-time.Minute)}}}
	p, err := NewPoller(PollerDeps{Feed: f.feed, Monitored: f.pool, Control: f.pool,
		Interval: time.Minute, ProviderEventsOnly: true, Managed: true, ProviderAPI: r,
		Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.PollOnce(f.ctx); err != nil {
		t.Fatal(err)
	}
	res := f.feed.Probe(context.Background(), probes.Args{Window: time.Hour})
	if !hasColumn(res.Columns, "attribution") {
		t.Fatalf("columns = %v, want attribution", res.Columns)
	}
	rows, err := probes.ChangeRows(res)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	var found, unattributed bool
	for _, row := range rows {
		switch {
		case row.EventID == id:
			found = true
			if row.Attribution != AttributionProvider || row.Kind != string(KindFailover) ||
				row.Source != SourceProviderAPI {
				t.Fatalf("provider row = %+v, want a provider_api failover by the provider",
					row)
			}
		case row.Kind == string(KindDeploy) && row.Attribution != "":
			unattributed = true
		}
	}
	if !found {
		t.Fatalf("provider event %s not in the probe rows %+v", id, rows)
	}
	if unattributed {
		t.Fatal("a deploy carried an attribution")
	}
}

func hasColumn(cols []string, name string) bool {
	for _, c := range cols {
		if c == name {
			return true
		}
	}
	return false
}
