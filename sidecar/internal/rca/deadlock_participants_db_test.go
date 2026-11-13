package rca

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/signalid"
)

// rca-07b against the per-package fixture database: the participants are
// stored in sage.incidents.log_occurrences, survive a restart, a new
// window replaces them, and no statement literal is stored anywhere.

// participantDeadlock queues a deadlock signal as the classifier builds it
// in fingerprint mode: the raw DETAIL (with literals) rides in "detail",
// the participants carry fingerprints only.
func (r *flapRig) participantDeadlock(parts []DeadlockParticipant) {
	r.src.signals = append(r.src.signals, &Signal{
		ID: signalid.LogDeadlockDetected, FiredAt: r.now, Severity: "critical",
		Metrics: map[string]any{"database": r.db, "pid": parts[0].PID,
			"occurrence_count": 1, "affected_pids": []int{parts[0].PID, parts[1].PID},
			"first_seen": r.now.Format(time.RFC3339Nano),
			"last_seen":  r.now.Format(time.RFC3339Nano),
			"message":    "deadlock detected",
			"detail": "Process 4242: UPDATE users SET email = 'alice@example.com' " +
				"WHERE card = 4111111111111111",
			MetricDeadlockParticipants: parts}})
}

type storedParticipants struct {
	Count        int                   `json:"count"`
	Participants []DeadlockParticipant `json:"participants"`
}

func (r *flapRig) storedDeadlock() (row string, occ storedParticipants, raw string) {
	r.t.Helper()
	err := r.pool.QueryRow(r.ctx, `SELECT row_to_json(i)::text,
		COALESCE(i.log_occurrences::text, '')
		FROM sage.incidents i
		WHERE database_name = $1 AND 'log_deadlock_detected' = ANY(signal_ids)
		ORDER BY detected_at DESC LIMIT 1`, r.db).Scan(&row, &raw)
	if err != nil {
		r.t.Fatalf("stored deadlock incident: %v", err)
	}
	if err := json.Unmarshal([]byte(raw), &occ); err != nil {
		r.t.Fatalf("decode log_occurrences %s: %v", raw, err)
	}
	return row, occ, raw
}

func TestDeadlockParticipants_StoredWithoutLiterals(t *testing.T) {
	r := newFlapRig(t)
	r.participantDeadlock(twoParticipants())
	cycle(r.t, r.ctx, r.eng, r.pool, false)
	row, occ, raw := r.storedDeadlock()
	if len(occ.Participants) != 2 || occ.Participants[0].PID != 4242 ||
		occ.Participants[0].WaitMode != "ShareLock" || occ.Participants[1].BlockedBy != 4242 {
		t.Fatalf("stored participants = %s", raw)
	}
	if occ.Participants[0].Fingerprint == occ.Participants[1].Fingerprint {
		t.Errorf("stored fingerprints are not distinct: %s", raw)
	}
	for _, leak := range []string{"alice", "example.com", "4111111111111111", `"statement"`} {
		if strings.Contains(row, leak) {
			t.Errorf("sage.incidents row holds %q: %s", leak, row)
		}
	}
	if !strings.Contains(row, "statement fp=0123456789abcdef") {
		t.Errorf("causal chain lacks the wait-cycle evidence: %s", row)
	}
}

// State transition on real storage: a restart hydrates the set, and a
// recurrence in a new window replaces it and sums the count.
func TestDeadlockParticipants_NewWindowReplacesStoredSet(t *testing.T) {
	r := newFlapRig(t)
	r.participantDeadlock(twoParticipants())
	cycle(r.t, r.ctx, r.eng, r.pool, false)
	fresh := testEngine()
	fresh.WithDatabaseName(r.db)
	if err := fresh.Hydrate(r.ctx, r.pool); err != nil {
		t.Fatalf("hydrate: %v", err)
	}
	var got *LogOccurrences
	for _, inc := range fresh.ActiveIncidents() {
		if len(inc.SignalIDs) == 1 && inc.SignalIDs[0] == signalid.LogDeadlockDetected {
			got = inc.LogOccurrences
		}
	}
	if got == nil || len(got.Participants) != 2 ||
		got.Participants[1].Fingerprint != "fedcba9876543210" {
		t.Fatalf("hydrated occurrences = %+v", got)
	}
	r.now = r.base.Add(5 * time.Minute)
	r.participantDeadlock([]DeadlockParticipant{
		{PID: 9001, WaitMode: "ExclusiveLock", WaitObject: "relation 16384 of database 5",
			BlockedBy: 9002, Fingerprint: "1111111111111111"},
		{PID: 9002, WaitMode: "AccessShareLock", WaitObject: "relation 16390 of database 5",
			BlockedBy: 9001, Fingerprint: "2222222222222222"}})
	cycle(r.t, r.ctx, r.eng, r.pool, false)
	_, occ, raw := r.storedDeadlock()
	if occ.Count != 2 || len(occ.Participants) != 2 || occ.Participants[0].PID != 9001 {
		t.Errorf("after the recurrence log_occurrences = %s; want count 2 and 9001's set", raw)
	}
}
