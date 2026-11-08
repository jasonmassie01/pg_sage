package lakebaseobs

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

func TestDBHistoricalQueryFilterPrecedesInboxLimit(t *testing.T) {
	for _, tc := range []struct {
		stream Stream
		id     probes.ID
	}{{Plans, probes.LakebasePlans}, {ASH, probes.LakebaseSessions}} {
		t.Run(string(tc.stream), func(t *testing.T) {
			s, scope, now := evidenceFixture(t, time.Hour)
			matching := []Event{
				scopedEvent(t, scope, tc.stream, now.Add(-10*time.Minute), now, map[string]string{"queryid": "17"}),
				scopedEvent(t, scope, tc.stream, now.Add(-11*time.Minute), now, map[string]string{"queryid": "-1"}),
				scopedEvent(t, scope, tc.stream, now.Add(-12*time.Minute), now, map[string]string{"queryid": "18446744073709551615"}),
			}
			events := append([]Event(nil), matching...)
			// Expired data is still physically present; filtering must preserve
			// the direct retention predicate rather than relying on cleanup.
			events = append(events, scopedEvent(t, scope, tc.stream, now.Add(-2*time.Hour), now, map[string]string{"queryid": "17"}))
			for i := range evidenceRows + 30 {
				events = append(events, scopedEvent(t, scope, tc.stream, now.Add(-time.Minute-time.Duration(i)*time.Millisecond), now, map[string]string{"queryid": "18", "row_position": fmt.Sprint(i)}))
			}
			seedEvidence(t, s, tc.stream, now, events...)
			for _, q := range []struct {
				id   int64
				want []Event
			}{{17, matching[:1]}, {-1, matching[1:]}} {
				policy := &probes.ReadPolicy{Version: 1, Scope: "database", Database: scope.DatabaseName, Binding: probes.PoolBinding(s.monitored), QueryID: q.id}
				ctx := probes.WithReadPolicy(context.Background(), policy)
				r := s.readAt(ctx, tc.id, probes.Args{QueryID: q.id}, now)
				if r.Status != probes.StatusOK || len(r.Rows) != len(q.want) || r.Truncated || r.History == nil || r.History.Complete || r.History.ExactMultiplicity {
					t.Fatalf("newer unrelated rows hid query %d: %+v", q.id, r)
				}
				for i, row := range r.Rows {
					if row["queryid"] != q.id || row["event_id"] != q.want[i].ID || row["scope_hash"] != scope.Hash() {
						t.Fatalf("query %d lost identity/provenance: %+v", q.id, row)
					}
				}
				if r := s.readAt(ctx, tc.id, probes.Args{QueryID: 18}, now); r.Reason != probes.ReasonPolicyDenied || len(r.Rows) != 0 {
					t.Fatalf("query filter widened pinned policy: %+v", r)
				}
			}
			for _, query := range []int64{0, 18} {
				r := s.readAt(context.Background(), tc.id, probes.Args{QueryID: query}, now)
				if r.Status != probes.StatusOK || len(r.Rows) != evidenceRows || !r.Truncated {
					t.Fatalf("query %d changed unfiltered/filtered row cap: %+v", query, r)
				}
				for _, row := range r.Rows {
					if row["queryid"] != int64(18) {
						t.Fatalf("query %d returned a row outside newest bounded matches: %+v", query, row)
					}
				}
			}
			if r := s.readAt(context.Background(), tc.id, probes.Args{QueryID: 19}, now); r.Status != probes.StatusUnsupported || r.Reason != "historical_no_observations_completeness_unknown" || len(r.Rows) != 0 || r.Truncated {
				t.Fatalf("no matching query became complete/truncated evidence: %+v", r)
			}
			for _, invalid := range []probes.Args{{QueryID: 17, PID: 1}, {QueryID: 17, Window: -time.Second}} {
				if r := s.readAt(context.Background(), tc.id, invalid, now); r.Reason != "invalid_args" || len(r.Rows) != 0 {
					t.Fatalf("query filter admitted invalid args: %+v", r)
				}
			}
			if r := s.readAt(context.Background(), probes.LakebaseLogs, probes.Args{QueryID: 17}, now); r.Reason != "invalid_args" || len(r.Rows) != 0 {
				t.Fatalf("query filter admitted an unscoped source: %+v", r)
			}
			if err := s.store.ActivateOwner(context.Background(), scope, false); err != nil {
				t.Fatal(err)
			}
			if r := s.readAt(context.Background(), tc.id, probes.Args{QueryID: -1}, now); r.Reason != "history_policy_revoked" || len(r.Rows) != 0 {
				t.Fatalf("query filter bypassed revoked payload policy: %+v", r)
			}
		})
	}
}
