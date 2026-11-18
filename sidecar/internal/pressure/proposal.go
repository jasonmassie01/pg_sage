package pressure

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/executor"
)

// The approval item of an enforcement proposal: an open sage.findings row
// (category pressure_job) anchors one sage.action_queue item, keyed by the
// job and the backend incarnation, in the existing approval flow. The
// finding has no recommended SQL, so the manual "take action" path can
// never run it; only an approval of the item does, through the Runner.

const (
	// JobIdentityPrefix starts the identity key of an enforcement item
	// (pressure_job:<job>:<pid>:<backend_start unix microseconds>).
	JobIdentityPrefix = executor.PressureJobIdentityPrefix
	// RunnerName names the enforcement runner among the executor's
	// approved-action runners.
	RunnerName = "pressure_jobs"
	// FindingCategory is the enforcement finding's category.
	FindingCategory = "pressure_job"
)

// JobIdentityKey is the approval item key of job's cancel of one backend
// incarnation.
func JobIdentityKey(jobID int64, pid int32, backendStart time.Time) string {
	return fmt.Sprintf("%s%d:%d:%d", JobIdentityPrefix, jobID, pid, backendStart.UnixMicro())
}

// ParseJobIdentityKey reads a key JobIdentityKey made.
func ParseJobIdentityKey(key string) (jobID int64, pid int32, start time.Time, ok bool) {
	rest, found := strings.CutPrefix(key, JobIdentityPrefix)
	parts := strings.Split(rest, ":")
	if !found || len(parts) != 3 {
		return 0, 0, time.Time{}, false
	}
	id, err1 := strconv.ParseInt(parts[0], 10, 64)
	p, err2 := strconv.ParseInt(parts[1], 10, 32)
	us, err3 := strconv.ParseInt(parts[2], 10, 64)
	if err1 != nil || err2 != nil || err3 != nil || id <= 0 || p <= 0 || us <= 0 {
		return 0, 0, time.Time{}, false
	}
	return id, int32(p), time.UnixMicro(us).UTC(), true
}

// cancelSQL is the item's SQL, as the approval card and the executor's
// action log show it.
func cancelSQL(pid int32) string { return fmt.Sprintf("SELECT pg_cancel_backend(%d)", pid) }

// PGProposer queues proposals in one monitored database's approval flow.
type PGProposer struct {
	pool       *pgxpool.Pool
	databaseID *int
}

// NewPGProposer binds the queue to a monitored database pool; databaseID
// is the control-plane id (nil outside meta-db mode).
func NewPGProposer(pool *pgxpool.Pool, databaseID *int) *PGProposer {
	return &PGProposer{pool: pool, databaseID: databaseID}
}

// Propose creates the proposal's finding and approval item once; a second
// call (or a concurrent one) returns the same item.
func (q *PGProposer) Propose(ctx context.Context, p Proposal) (int, bool, error) {
	if p.Job.ID <= 0 || p.Backend.PID <= 0 || p.Backend.BackendStart.IsZero() {
		return 0, false, fmt.Errorf("%w: a proposal needs its job and backend identity",
			ErrInvalidRequest)
	}
	key := JobIdentityKey(p.Job.ID, p.Backend.PID, p.Backend.BackendStart)
	detail, err := json.Marshal(proposalDetail(p))
	if err != nil {
		return 0, false, err
	}
	var queueID int
	created := false
	err = pgx.BeginFunc(ctx, q.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `/* pg_sage */ SELECT pg_advisory_xact_lock(
			hashtextextended($1, 0))`, key); err != nil {
			return err
		}
		err := tx.QueryRow(ctx, `/* pg_sage */ SELECT id FROM sage.action_queue
			WHERE identity_key = $1 ORDER BY id LIMIT 1`, key).Scan(&queueID)
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		created = true
		queueID, err = q.insert(ctx, tx, key, p, detail)
		return err
	})
	if err != nil {
		return 0, false, fmt.Errorf("queueing the cancel of pid %d for job %q: %w",
			p.Backend.PID, p.Job.ApplicationName, err)
	}
	return queueID, created, nil
}

func (q *PGProposer) insert(ctx context.Context, tx pgx.Tx, key string, p Proposal,
	detail []byte) (int, error) {
	title := fmt.Sprintf("Backfill %q kept writing through %d stop verdicts: cancel pid %d",
		p.Job.ApplicationName, p.Stops, p.Backend.PID)
	var findingID, queueID int
	err := tx.QueryRow(ctx, `/* pg_sage */ INSERT INTO sage.findings
		(category, severity, object_type, object_identifier, title, detail, recommendation)
		VALUES ($1, 'warning', 'backend', $2, $3, $4::jsonb, $5)
		ON CONFLICT (category, object_identifier) WHERE status = 'open'
		DO UPDATE SET last_seen = now() RETURNING id`, FindingCategory, key, title,
		string(detail), "Approve to cancel the job's current statement (its transaction "+
			"rolls back), or deny and let the job continue; the job's own check_pressure "+
			"calls say stop either way.").Scan(&findingID)
	if err != nil {
		return 0, err
	}
	err = tx.QueryRow(ctx, `/* pg_sage */ INSERT INTO sage.action_queue
		(database_id, finding_id, proposed_sql, action_risk, action_type, identity_key,
		 policy_decision, guardrails, expires_at)
		VALUES ($1, $2, $3, 'moderate', 'cancel_backend', $4, 'queue_approval',
		        '["approval_required", "pressure_recheck", "identity_recheck",
		          "evidence_age_5s"]'::jsonb, $5)
		RETURNING id`, q.databaseID, findingID, cancelSQL(p.Backend.PID), key,
		p.ExpiresAt).Scan(&queueID)
	return queueID, err
}

// settleSQL resolves the open enforcement findings whose approval item is
// no longer live (run, failed, denied or expired): nothing else resolves
// them, and an open warning would otherwise stay on the findings list.
const settleSQL = `/* pg_sage */ UPDATE sage.findings f
	SET status = 'resolved', resolved_at = COALESCE(f.resolved_at, now())
	WHERE f.category = $1 AND f.status = 'open'
	  AND NOT EXISTS (SELECT 1 FROM sage.action_queue q
	        WHERE q.finding_id = f.id
	          AND (q.status IN ('approved', 'scheduled')
	               OR (q.status = 'pending' AND q.expires_at > now())))`

// Settle resolves the enforcement findings whose approval item ended and
// returns how many it resolved.
func (q *PGProposer) Settle(ctx context.Context) (int64, error) {
	tag, err := q.pool.Exec(ctx, settleSQL, FindingCategory)
	if err != nil {
		return 0, fmt.Errorf("resolving the settled backfill cancel findings: %w", err)
	}
	return tag.RowsAffected(), nil
}

// proposalDetail is the finding detail the approval card cites: the job,
// the target incarnation and every stopping or slowing input as one line.
func proposalDetail(p Proposal) map[string]any {
	d := map[string]any{"source": FindingCategory, "job_id": p.Job.ID,
		"application_name": p.Job.ApplicationName, "database": p.Job.Database,
		"verdict": string(p.Verdict.Verdict), "consecutive_stops": p.Stops,
		"reasons": p.Verdict.Reasons, "checked_at": p.Verdict.CheckedAt,
		"signal_target": map[string]any{"pid": p.Backend.PID,
			"backend_start": p.Backend.BackendStart.UTC().Format(time.RFC3339Nano)}}
	if p.Job.Table != "" {
		d["target_table"] = p.Job.Table
	}
	for _, in := range p.Verdict.Inputs {
		if line, ok := inputLine(in); ok {
			d["input_"+in.Name] = line
		}
	}
	return d
}

// inputLine is one input as the card shows it; inputs in their ok band
// and inputs that do not apply are left out.
func inputLine(in Input) (string, bool) {
	switch {
	case in.State == StateUnknown:
		return "unknown: " + in.Reason, in.Required
	case in.State != StateKnown || in.Value == nil || in.Band == BandOK || in.Band == "":
		return "", false
	}
	line := num(*in.Value) + " " + in.Unit
	if in.SlowLimit != nil {
		line += ", slow at " + num(*in.SlowLimit)
	}
	if in.StopLimit != nil {
		line += ", stop at " + num(*in.StopLimit)
	}
	return line + ": " + string(in.Band), true
}

func num(v float64) string { return strconv.FormatFloat(v, 'g', 6, 64) }
