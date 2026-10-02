package sre

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/cases"
)

// PGTriggerSource is the RCA trigger adapter (AI-SRE-SPEC §5): it reads
// committed triggers from a monitored database's sage schema. Open
// incidents whose signals map to an investigation family, and open
// plan_regression findings, each start at most one investigation (their
// idempotency key is the incident or finding id). Case ids match the
// Cases projection, so the Cases panel links the investigation.
type PGTriggerSource struct {
	pool     *pgxpool.Pool
	database string
}

// triggerBatch bounds the triggers read per poll and kind.
const triggerBatch = 50

// signalKinds maps RCA signals to investigation families.
// replication_lag_increasing (pg_stat_replication replay lag) starts the
// M6 replication lag family: the WAL family reads slots, the archiver
// and WAL volume, never the replicas' lag. The reactive detector's own
// incident signals map back to their families, so its incidents resume
// after a restart under the same idempotency key.
var signalKinds = map[string]TriggerKind{
	"lock_contention": TriggerLock, "idle_in_tx_elevated": TriggerLock,
	"orphaned_prepared_tx": TriggerLock, "connections_high": TriggerConnections,
	"wal_growth_spike": TriggerWAL, "replication_lag_increasing": TriggerReplicationLag,
	"log_replication_conflict":    TriggerReplicationLag,
	"log_checkpoint_too_frequent": TriggerCheckpoint,
	"log_temp_file_created":       TriggerTempFiles,
	"sre_checkpoint_storm":        TriggerCheckpoint,
	"sre_temp_file_explosion":     TriggerTempFiles,
	"sre_lwlock_contention":       TriggerLWLock,
}

// NewPGTriggerSource reads the sage schema of pool; database is the
// instance name incidents and the Cases projection use.
func NewPGTriggerSource(pool *pgxpool.Pool, database string) *PGTriggerSource {
	return &PGTriggerSource{pool: pool, database: database}
}

// Triggers lists the current triggers, incidents first.
func (s *PGTriggerSource) Triggers(ctx context.Context) ([]Trigger, error) {
	if s == nil || s.pool == nil {
		return nil, fmt.Errorf("%w: trigger source has no database", ErrInvalidRequest)
	}
	out, err := s.incidentTriggers(ctx)
	if err != nil {
		return nil, fmt.Errorf("read incident triggers: %w", err)
	}
	plans, err := s.planTriggers(ctx)
	if err != nil {
		return nil, fmt.Errorf("read plan regression triggers: %w", err)
	}
	return append(out, plans...), nil
}

func (s *PGTriggerSource) incidentTriggers(ctx context.Context) ([]Trigger, error) {
	rows, err := s.pool.Query(ctx, `SELECT id::text, signal_ids, source,
		    COALESCE(database_name, '')
		FROM sage.incidents
		WHERE resolved_at IS NULL AND (database_name IS NULL OR database_name = $1)
		ORDER BY detected_at, id LIMIT $2`, s.database, triggerBatch)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Trigger
	for rows.Next() {
		var inc cases.SourceIncident
		if err := rows.Scan(&inc.ID, &inc.SignalIDs, &inc.Source,
			&inc.DatabaseName); err != nil {
			return nil, err
		}
		if kind, ok := incidentKind(inc.SignalIDs); ok {
			out = append(out, incidentTrigger(inc, kind))
		}
	}
	return out, rows.Err()
}

func incidentKind(signals []string) (TriggerKind, bool) {
	for _, sig := range signals {
		if kind, ok := signalKinds[sig]; ok {
			return kind, true
		}
	}
	return "", false
}

func (s *PGTriggerSource) planTriggers(ctx context.Context) ([]Trigger, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, COALESCE(object_type, ''),
		    COALESCE(object_identifier, ''), detail::text
		FROM sage.findings
		WHERE status = 'open' AND category = 'plan_regression'
		ORDER BY last_seen DESC, id LIMIT $1`, triggerBatch)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Trigger
	for rows.Next() {
		f := cases.SourceFinding{DatabaseName: s.database, Category: "plan_regression"}
		var id int64
		var detail string
		if err := rows.Scan(&id, &f.ObjectType, &f.ObjectIdentifier, &detail); err != nil {
			return nil, err
		}
		qid, ok := findingQueryID(detail, &f)
		if !ok {
			continue // no query identity: nothing a plan probe can check
		}
		out = append(out, Trigger{CaseID: clip(cases.IdentityKeyForFinding(f)),
			Kind: TriggerPlan, Subject: "queryid " + strconv.FormatInt(qid, 10),
			IdempotencyKey: "finding:" + strconv.FormatInt(id, 10)})
	}
	return out, rows.Err()
}

// findingQueryID reads detail.queryid exactly (as json.Number).
func findingQueryID(detail string, f *cases.SourceFinding) (int64, bool) {
	var d map[string]any
	dec := json.NewDecoder(strings.NewReader(detail))
	dec.UseNumber()
	if dec.Decode(&d) != nil {
		return 0, false
	}
	f.Detail = d
	n, ok := d["queryid"].(json.Number)
	if !ok {
		return 0, false
	}
	qid, err := n.Int64()
	return qid, err == nil
}

// clip bounds a case id to the store's 256 characters.
func clip(s string) string { return truncateRunes(s, 256) }

// truncateRunes keeps at most n runes of s.
func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}
