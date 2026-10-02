package sre

import "testing"

// M6: incidents from the RCA engine's log trees (checkpoints occurring
// too frequently, temp files, replication conflicts) and its replication
// lag signal start the new families. Replication lag moved from the WAL
// retention family: the lag signal reads pg_stat_replication replay lag,
// which the WAL family (slots, archiver, WAL volume) never examines.
func TestTriggers_MapsM6IncidentSignals(t *testing.T) {
	_, pool, ctx := liveStore(t, DefaultLimits())
	want := map[string]TriggerKind{
		insertIncident(t, ctx, pool, "m6db", false, "log_checkpoint_too_frequent"): TriggerCheckpoint,
		insertIncident(t, ctx, pool, "m6db", false, "log_temp_file_created"):       TriggerTempFiles,
		insertIncident(t, ctx, pool, "m6db", false, "replication_lag_increasing"):  TriggerReplicationLag,
		insertIncident(t, ctx, pool, "m6db", false, "log_replication_conflict"):    TriggerReplicationLag,
		insertIncident(t, ctx, pool, "m6db", false, "wal_growth_spike"):            TriggerWAL,
	}
	got, err := NewPGTriggerSource(pool, "m6db").Triggers(ctx)
	if err != nil {
		t.Fatalf("triggers: %v", err)
	}
	m := byKey(got)
	for id, kind := range want {
		tr, ok := m["incident:"+id]
		if !ok || tr.Kind != kind || tr.IncidentID != id {
			t.Errorf("incident %s = %+v (found %v), want %s", id, tr, ok, kind)
		}
	}
}
