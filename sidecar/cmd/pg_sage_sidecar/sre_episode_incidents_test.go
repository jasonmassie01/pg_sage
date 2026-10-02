package main

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/cases"
	"github.com/pg-sage/sidecar/internal/notify"
	"github.com/pg-sage/sidecar/internal/rca"
	"github.com/pg-sage/sidecar/internal/sre"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// The episode sink is the RCA engine of the database: an episode becomes
// (or attaches to) a sage.incidents row through the engine, is persisted
// off the trigger poll and notified through the existing incident
// notification path, and its identity is the one the Cases panel shows.

type eventLog struct {
	mu     sync.Mutex
	events []notify.Event
}

func (l *eventLog) Dispatch(_ context.Context, e notify.Event) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, e)
	return nil
}

func (l *eventLog) ofType(typ string) []notify.Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []notify.Event
	for _, e := range l.events {
		if e.Type == typ {
			out = append(out, e)
		}
	}
	return out
}

func episodeFixture(t *testing.T) (*pgxpool.Pool, context.Context, string, *rca.Engine,
	*eventLog) {
	t.Helper()
	dsn := testdb.SkipUnlessLive(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	pool := openComposedPool(t, ctx, dsn)
	db := fmt.Sprintf("ep_%d", time.Now().UnixNano())
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			"DELETE FROM sage.incidents WHERE database_name = $1", db)
	})
	c := composedConfig("standalone")
	eng := rca.NewEngine(&c.RCA, func(string, string, ...any) {})
	eng.WithDatabaseName(db)
	events := &eventLog{}
	eng.WithDispatcher(events)
	return pool, ctx, db, eng, events
}

func tempEpisode(db string) sre.Episode {
	return sre.Episode{Kind: sre.TriggerTempFiles, Database: db,
		Signal: "sre_temp_file_explosion", Related: []string{"log_temp_file_created"},
		Start: time.Now(), ObservedAt: time.Now(), Severity: "warning",
		Summary:  "Temp-file explosion: temp files grew past the threshold",
		Evidence: "1536 MiB of temp files within 5m0s (threshold 1024 MiB)"}
}

type incidentRow struct {
	severity, source, rootCause string
	signals                     []string
	occurrences                 int
}

func waitIncidentRow(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	id string) incidentRow {
	t.Helper()
	var r incidentRow
	var err error
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); {
		err = pool.QueryRow(ctx, `SELECT severity, source, root_cause, signal_ids,
			occurrence_count FROM sage.incidents WHERE id = $1::uuid`, id).
			Scan(&r.severity, &r.source, &r.rootCause, &r.signals, &r.occurrences)
		if err == nil {
			return r
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("incident %s was not persisted: %v", id, err)
	return r
}

func TestEpisodeIncidents_RecordsPersistsAndNotifies(t *testing.T) {
	pool, ctx, db, eng, events := episodeFixture(t)
	sink := newEpisodeIncidents(eng, pool, func(string, string, ...any) {})
	runCtx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { sink.Run(runCtx); close(done) }()
	t.Cleanup(func() { stop(); <-done })

	ep := tempEpisode(db)
	inc, err := sink.RecordEpisode(ctx, ep)
	if err != nil || inc.ID == "" || inc.Database != db || inc.Source != "deterministic" ||
		strings.Join(inc.SignalIDs, ",") != "sre_temp_file_explosion" {
		t.Fatalf("RecordEpisode = %+v (%v)", inc, err)
	}
	row := waitIncidentRow(t, ctx, pool, inc.ID)
	if row.severity != "warning" || row.source != "deterministic" ||
		row.rootCause != ep.Summary || row.occurrences != 1 {
		t.Fatalf("row = %+v", row)
	}
	for deadline := time.Now().Add(10 * time.Second); len(events.ofType(
		"incident_detected")) == 0 && time.Now().Before(deadline); {
		time.Sleep(50 * time.Millisecond)
	}
	ep.IncidentID = inc.ID
	if again, err := sink.RecordEpisode(ctx, ep); err != nil || again.ID != inc.ID {
		t.Fatalf("continuing observation = %+v (%v), want %s", again, err, inc.ID)
	}
	time.Sleep(300 * time.Millisecond)
	detected := events.ofType("incident_detected")
	if len(detected) != 1 || detected[0].Data["incident_id"] != inc.ID {
		t.Fatalf("incident_detected = %+v, want one for %s", detected, inc.ID)
	}
	want := cases.ProjectIncident(cases.SourceIncident{ID: inc.ID, DatabaseName: db,
		SignalIDs: row.signals, Source: row.source}).ID
	if got := cases.IncidentIdentityKey(cases.SourceIncident{ID: inc.ID,
		DatabaseName: inc.Database, SignalIDs: inc.SignalIDs, Source: inc.Source}); got != want {
		t.Fatalf("episode case id %q, Cases projection %q", got, want)
	}
}

// Concurrent first observations of one episode are one incident.
func TestEpisodeIncidents_ConcurrentFirstObservationsAreOneIncident(t *testing.T) {
	pool, ctx, db, eng, _ := episodeFixture(t)
	sink := newEpisodeIncidents(eng, pool, func(string, string, ...any) {})
	runCtx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { sink.Run(runCtx); close(done) }()
	t.Cleanup(func() { stop(); <-done })
	var wg sync.WaitGroup
	ids := make(chan string, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			inc, err := sink.RecordEpisode(ctx, tempEpisode(db))
			if err != nil {
				t.Errorf("RecordEpisode: %v", err)
			}
			ids <- inc.ID
		}()
	}
	wg.Wait()
	close(ids)
	seen := map[string]bool{}
	for id := range ids {
		seen[id] = true
	}
	if len(seen) != 1 || seen[""] {
		t.Fatalf("incident ids = %v, want one", seen)
	}
	for id := range seen {
		waitIncidentRow(t, ctx, pool, id)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.incidents
		WHERE database_name = $1`, db).Scan(&n); err != nil || n != 1 {
		t.Fatalf("%d incident rows (%v), want 1", n, err)
	}
}

// Without a readable incident store the sink records nothing and says
// why: the detector falls back to its own trigger.
func TestEpisodeIncidents_StoreUnavailable(t *testing.T) {
	dsn := testdb.CreateDatabase(t, "ep_no_schema")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	bare, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(bare.Close)
	c := composedConfig("standalone")
	eng := rca.NewEngine(&c.RCA, func(string, string, ...any) {})
	eng.WithDatabaseName("bare")
	sink := newEpisodeIncidents(eng, bare, func(string, string, ...any) {})
	inc, err := sink.RecordEpisode(ctx, tempEpisode("bare"))
	if err == nil || inc.ID != "" || !strings.Contains(err.Error(), "incident") {
		t.Fatalf("RecordEpisode on a database without the sage schema = %+v (%v)", inc, err)
	}
}
