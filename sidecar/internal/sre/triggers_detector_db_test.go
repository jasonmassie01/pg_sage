package sre

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

// A detector episode's incident is a committed RCA incident like any
// other: the RCA trigger source reads it back (after a restart, too) as
// exactly the trigger the detector emits, so both coalesce into one
// investigation linked to the incident (CHECK-13), whichever runs first
// and however many polls race.

// rowSink links every episode to one stored incident row.
type rowSink struct {
	id, db string
}

func (s rowSink) RecordEpisode(_ context.Context, ep Episode) (EpisodeIncident, error) {
	return EpisodeIncident{ID: s.id, Database: s.db, SignalIDs: []string{ep.Signal},
		Source: "deterministic"}, nil
}

func TestDetectorIncident_TriggerEqualsTheRCATriggerAndCoalesces(t *testing.T) {
	st, pool, ctx := liveStore(t, DefaultLimits())
	db := fmt.Sprintf("det_%d", time.Now().UnixNano())
	id := insertIncident(t, ctx, pool, db, false, "sre_temp_file_explosion")

	fromRCA, err := NewPGTriggerSource(pool, db).Triggers(ctx)
	if err != nil || len(fromRCA) != 1 {
		t.Fatalf("RCA triggers = %+v (%v), want the detector incident", fromRCA, err)
	}
	r := newSeqRunner().add(tempPoll(0, 0), tempPoll(time.Minute, 2<<30))
	d, err := NewReactiveDetector(r, db, DefaultDetectorConfig(), nil)
	if err != nil {
		t.Fatalf("detector: %v", err)
	}
	d.WithIncidents(rowSink{id: id, db: db})
	if _, err := d.Triggers(ctx); err != nil {
		t.Fatalf("first poll: %v", err)
	}
	fromDetector, err := d.Triggers(ctx)
	if err != nil || len(fromDetector) != 1 {
		t.Fatalf("detector triggers = %+v (%v)", fromDetector, err)
	}
	if fromRCA[0] != fromDetector[0] {
		t.Fatalf("RCA trigger %+v\ndetector     %+v: they must be identical", fromRCA[0],
			fromDetector[0])
	}
	if fromRCA[0].Kind != TriggerTempFiles || fromRCA[0].IncidentID != id {
		t.Fatalf("trigger = %+v, want temp_file_explosion of %s", fromRCA[0], id)
	}

	c, _ := testCoordinator(t, ctx, st, newSeqRunner(), nil)
	var wg sync.WaitGroup
	results := make(chan Investigation, 16)
	created := make(chan bool, 16)
	for i := 0; i < 16; i++ {
		tr := fromRCA[0]
		if i%2 == 1 {
			tr = fromDetector[0]
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			inv, isNew, err := c.Start(ctx, tr)
			if err != nil {
				t.Errorf("start: %v", err)
				return
			}
			results <- inv
			created <- isNew
		}()
	}
	wg.Wait()
	close(results)
	close(created)
	ids := map[UUID]bool{}
	for inv := range results {
		ids[inv.ID] = true
		if inv.IncidentID != id || inv.CaseID != fromRCA[0].CaseID {
			t.Errorf("investigation = %+v, want it linked to incident %s", inv, id)
		}
	}
	news := 0
	for isNew := range created {
		if isNew {
			news++
		}
	}
	if len(ids) != 1 || news != 1 {
		t.Fatalf("%d investigations (%d created) for one episode, want exactly 1",
			len(ids), news)
	}
}
