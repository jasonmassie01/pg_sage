package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Discovered row statuses (market-06). candidate -> enrolling -> enrolled
// -> adopted | expired; candidate -> ignored; any listed row -> gone.
const (
	DiscoveryCandidate = "candidate"
	DiscoveryEnrolling = "enrolling"
	DiscoveryEnrolled  = "enrolled"
	DiscoveryAdopted   = "adopted"
	DiscoveryExpired   = "expired"
	DiscoveryIgnored   = "ignored"
	DiscoveryGone      = "gone"
)

// staleClaim is how long an enrolling claim may stay unresolved before
// another cycle (or sidecar) may claim the row again.
const staleClaim = 10 * time.Minute

// discoveryLock serialises the multi-row writes of every sidecar (Upsert,
// MarkGone and Claim): max_enrolled holds between sidecars, and their row
// locks cannot deadlock (pg_advisory_xact_lock key "SGDS").
const discoveryLock = int64(0x53474453)

// beginLocked opens a transaction holding discoveryLock.
func (s *DiscoveryStore) beginLocked(ctx context.Context, what string) (pgx.Tx, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin %s: %w", what, err)
	}
	if _, err := tx.Exec(ctx, `/* pg_sage */ SELECT pg_advisory_xact_lock($1)`,
		discoveryLock); err != nil {
		_ = tx.Rollback(ctx)
		return nil, fmt.Errorf("lock %s: %w", what, err)
	}
	return tx, nil
}

// DiscoveredCandidate is one database a source reported, with what the
// source knows of it (fleetpol-01b): tags, creator ("agent:..." or
// "person:..."), the provider's creation time, and the policy set the
// runner matched it to ("" when none or policy sets are off).
type DiscoveredCandidate struct {
	ExternalID string
	Host       string
	Port       int
	DBName     string
	Tags       map[string]string
	Creator    string
	CreatedAt  time.Time
	SetName    string
	// Ignored are the source's reasons for attributes it dropped (logged,
	// never stored).
	Ignored []string
}

// DiscoveredRow is a row of sage.discovered_databases, with the member's
// expiry when it was enrolled.
type DiscoveredRow struct {
	ID             int64
	Source         string
	ExternalID     string
	Host           string
	Port           int
	DBName         string
	Owner          string
	Status         string
	DatabaseID     *int
	LastError      string
	FirstSeen      time.Time
	LastSeen       time.Time
	ExpiresAt      *time.Time
	AlreadyManaged bool
	// fleetpol-01b: attributes, the matched set and the milestones.
	Tags             map[string]string
	Creator          string
	CreatedAt        *time.Time
	SetName          string
	EnrolledAt       *time.Time
	GradeRequestedAt *time.Time
	FirstGradedAt    *time.Time
	BaselineAt       *time.Time
}

// DiscoverySourceStatus is a source's last run.
type DiscoverySourceStatus struct {
	Source        string
	LastRunAt     time.Time
	LastSuccessAt *time.Time
	LastError     string
	LastCount     int
}

// DiscoveryStore reads and writes sage.discovered_databases and
// sage.discovery_sources in the meta database. It has no path that writes
// trust_level or execution_mode.
type DiscoveryStore struct{ pool *pgxpool.Pool }

// NewDiscoveryStore returns a DiscoveryStore on pool.
func NewDiscoveryStore(pool *pgxpool.Pool) *DiscoveryStore {
	return &DiscoveryStore{pool: pool}
}

// discoveredColumns are the columns scanDiscovered reads, d being
// sage.discovered_databases.
const discoveredColumns = `d.id, d.source, d.external_id, d.host, d.port, d.dbname,
	d.owner, d.status, d.database_id, d.last_error, d.first_seen, d.last_seen,
	(SELECT m.expires_at FROM sage.databases m WHERE m.id = d.database_id),
	EXISTS (SELECT 1 FROM sage.databases m WHERE m.host = d.host
	        AND m.port = d.port AND m.database_name = d.dbname
	        AND (d.database_id IS NULL OR m.id <> d.database_id)),
	d.tags, d.creator, d.created_at, COALESCE(d.set_name, ''), d.enrolled_at,
	d.grade_requested_at, d.first_graded_at, d.baseline_at`

func scanDiscovered(rows pgx.Rows) ([]DiscoveredRow, error) {
	defer rows.Close()
	var out []DiscoveredRow
	for rows.Next() {
		var r DiscoveredRow
		if err := rows.Scan(&r.ID, &r.Source, &r.ExternalID, &r.Host, &r.Port,
			&r.DBName, &r.Owner, &r.Status, &r.DatabaseID, &r.LastError,
			&r.FirstSeen, &r.LastSeen, &r.ExpiresAt, &r.AlreadyManaged, &r.Tags,
			&r.Creator, &r.CreatedAt, &r.SetName, &r.EnrolledAt, &r.GradeRequestedAt,
			&r.FirstGradedAt, &r.BaselineAt); err != nil {
			return nil, fmt.Errorf("scanning discovered database: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Upsert records a listing: new candidates are inserted, known ones get
// last_seen, owner and attributes refreshed, and their set while they
// are not yet enrolled. It returns how many rows were new. The
// unique (source, external_id) key keeps concurrent sidecars from making
// duplicates.
func (s *DiscoveryStore) Upsert(ctx context.Context, source, owner string,
	cands []DiscoveredCandidate, now time.Time) (int, error) {
	if len(cands) == 0 {
		return 0, nil
	}
	qctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	// A batch is one implicit transaction, so the lock is held to its end.
	batch := &pgx.Batch{}
	batch.Queue(`/* pg_sage */ SELECT pg_advisory_xact_lock($1)`, discoveryLock)
	for _, c := range cands {
		batch.Queue(upsertDiscoveredSQL, source, c.ExternalID, c.Host, c.Port, c.DBName,
			owner, now, candidateTags(c.Tags), c.Creator, nullTime(c.CreatedAt),
			nullString(c.SetName))
	}
	br := s.pool.SendBatch(qctx, batch)
	defer func() { _ = br.Close() }()
	if _, err := br.Exec(); err != nil {
		return 0, fmt.Errorf("lock discovered databases of %s: %w", source, err)
	}
	added := 0
	for range cands {
		var inserted bool
		if err := br.QueryRow().Scan(&inserted); err != nil {
			return 0, fmt.Errorf("recording discovered databases of %s: %w", source, err)
		}
		if inserted {
			added++
		}
	}
	return added, br.Close()
}

// upsertDiscoveredSQL records one candidate; $1-$7 as before
// fleetpol-01b, $8-$11 its tags, creator, creation time and set.
const upsertDiscoveredSQL = `/* pg_sage */ INSERT INTO sage.discovered_databases AS d
	    (source, external_id, host, port, dbname, owner, first_seen, last_seen,
	     tags, creator, created_at, set_name)
	 VALUES ($1, $2, $3, $4, $5, $6, $7, $7, $8, $9, $10, $11)
	 ON CONFLICT (source, external_id) DO UPDATE SET
	    host = EXCLUDED.host, port = EXCLUDED.port, dbname = EXCLUDED.dbname,
	    owner = EXCLUDED.owner, tags = EXCLUDED.tags, creator = EXCLUDED.creator,
	    created_at = COALESCE(EXCLUDED.created_at, d.created_at),
	    set_name = CASE WHEN d.status IN ('candidate', 'enrolling')
	                    THEN EXCLUDED.set_name ELSE d.set_name END,
	    last_seen = GREATEST(d.last_seen, EXCLUDED.last_seen)
	 RETURNING (xmax = 0)`

// MarkGone marks gone every listed row of source not seen since
// seenBefore, ignored and gone rows aside, and returns them.
func (s *DiscoveryStore) MarkGone(ctx context.Context, source string,
	seenBefore time.Time) ([]DiscoveredRow, error) {
	qctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	tx, err := s.beginLocked(qctx, "marking gone databases of "+source)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(qctx) }()
	rows, err := tx.Query(qctx, `/* pg_sage */ WITH gone AS (
		    UPDATE sage.discovered_databases SET status = 'gone', status_changed_at = now()
		     WHERE source = $1 AND last_seen < $2 AND status NOT IN ('gone', 'ignored')
		    RETURNING *)
		 SELECT `+discoveredColumns+` FROM gone d ORDER BY d.id`, source, seenBefore)
	if err != nil {
		return nil, fmt.Errorf("marking gone databases of %s: %w", source, err)
	}
	gone, err := scanDiscovered(rows)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(qctx); err != nil {
		return nil, fmt.Errorf("commit gone databases of %s: %w", source, err)
	}
	for _, g := range gone {
		if err := s.audit(ctx, "fleet.discovery.gone", g, 0); err != nil {
			return gone, err
		}
	}
	return gone, nil
}

// RecordSourceRun records a source's run: its error (kept until the next
// success) or its success and count.
func (s *DiscoveryStore) RecordSourceRun(ctx context.Context, source string, count int,
	runErr error, now time.Time) error {
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	msg := ""
	if runErr != nil {
		msg = runErr.Error()
		if len(msg) > 1000 {
			msg = msg[:1000]
		}
	}
	_, err := s.pool.Exec(qctx, `/* pg_sage */ INSERT INTO sage.discovery_sources
		    (source, last_run_at, last_success_at, last_error, last_count)
		 VALUES ($1, $2, CASE WHEN $3 = '' THEN $2::timestamptz END, $3, $4)
		 ON CONFLICT (source) DO UPDATE SET last_run_at = EXCLUDED.last_run_at,
		    last_error = EXCLUDED.last_error,
		    last_success_at = COALESCE(EXCLUDED.last_success_at,
		                               sage.discovery_sources.last_success_at),
		    last_count = CASE WHEN EXCLUDED.last_error = '' THEN EXCLUDED.last_count
		                      ELSE sage.discovery_sources.last_count END`,
		source, now, msg, count)
	if err != nil {
		return fmt.Errorf("recording discovery run of %s: %w", source, err)
	}
	return nil
}

// claimDiscoveredSQL moves up to $2 claimable candidates of source $1 to
// enrolling at $3; an enrolling claim older than $4 is stale and claimable.
// $5, when not NULL, keeps the claim to the candidates of that policy set.
const claimDiscoveredSQL = `/* pg_sage */ WITH claimed AS (
		    UPDATE sage.discovered_databases SET status = 'enrolling',
		           status_changed_at = $3
		     WHERE id IN (SELECT c.id FROM sage.discovered_databases c
		                   WHERE c.source = $1 AND (c.status = 'candidate'
		                      OR (c.status = 'enrolling' AND c.status_changed_at < $4))
		                     AND ($5::text IS NULL OR c.set_name = $5)
		                     AND NOT EXISTS (SELECT 1 FROM sage.databases m
		                          WHERE m.host = c.host AND m.port = c.port
		                            AND m.database_name = c.dbname)
		                   ORDER BY (c.last_error <> ''),
		                         CASE WHEN c.last_error <> '' THEN c.status_changed_at END,
		                         c.first_seen, c.id
		                   LIMIT $2)
		    RETURNING *)
		 SELECT ` + discoveredColumns + ` FROM claimed d ORDER BY d.id`

// Claim takes up to limit candidates of source for enrolment, keeping the
// live discovered members (enrolling, enrolled, adopted) at or under
// maxLive across every sidecar. Rows already managed by hand are skipped.
// Rows whose last enrolment failed go after untried ones, longest-waiting
// first, so a database the role cannot connect to never starves the rest.
func (s *DiscoveryStore) Claim(ctx context.Context, source string, limit, maxLive int,
	now time.Time) ([]DiscoveredRow, error) {
	return s.claim(ctx, source, nil, limit, maxLive, now)
}

func (s *DiscoveryStore) claim(ctx context.Context, source string, set *string,
	limit, maxLive int, now time.Time) ([]DiscoveredRow, error) {
	qctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	tx, err := s.beginLocked(qctx, "discovery claim")
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(qctx) }()
	stale := now.Add(-staleClaim)
	var live int
	if err := tx.QueryRow(qctx, `/* pg_sage */ SELECT count(*)
		 FROM sage.discovered_databases WHERE status IN ('enrolled', 'adopted')
		    OR (status = 'enrolling' AND status_changed_at >= $1)`, stale,
	).Scan(&live); err != nil {
		return nil, fmt.Errorf("count live discovered members: %w", err)
	}
	room := min(limit, maxLive-live)
	if room <= 0 {
		return nil, nil
	}
	rows, err := tx.Query(qctx, claimDiscoveredSQL, source, room, now, stale, set)
	if err != nil {
		return nil, fmt.Errorf("claim discovered databases of %s: %w", source, err)
	}
	claimed, err := scanDiscovered(rows)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(qctx); err != nil {
		return nil, fmt.Errorf("commit discovery claim: %w", err)
	}
	return claimed, nil
}

// MarkEnrolled records that claimed row rowID is now member databaseID,
// and audits the enrolment.
func (s *DiscoveryStore) MarkEnrolled(ctx context.Context, rowID int64, databaseID int) error {
	row, err := s.transition(ctx, rowID, DiscoveryEnrolling, DiscoveryEnrolled,
		&databaseID, "")
	if err != nil {
		return err
	}
	return s.audit(ctx, "fleet.discovery.enrol", row, 0)
}

// Release returns a claimed row to candidate with the enrolment error.
func (s *DiscoveryStore) Release(ctx context.Context, rowID int64, reason string) error {
	_, err := s.transition(ctx, rowID, DiscoveryEnrolling, DiscoveryCandidate, nil, reason)
	return err
}

// DueForExpiry returns the enrolled rows whose member no owner adopted and
// whose expires_at is at or before now.
func (s *DiscoveryStore) DueForExpiry(ctx context.Context, now time.Time) (
	[]DiscoveredRow, error) {
	qctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	rows, err := s.pool.Query(qctx, `/* pg_sage */ SELECT `+discoveredColumns+`
		 FROM sage.discovered_databases d JOIN sage.databases m ON m.id = d.database_id
		 WHERE d.status = 'enrolled' AND m.adopted_at IS NULL AND m.expires_at <= $1
		 ORDER BY d.id`, now)
	if err != nil {
		return nil, fmt.Errorf("listing expired discovered members: %w", err)
	}
	return scanDiscovered(rows)
}

// SetStatus moves a row to status (expired by the TTL), with reason, and
// audits it.
func (s *DiscoveryStore) SetStatus(ctx context.Context, rowID int64,
	status, reason string) error {
	row, err := s.transition(ctx, rowID, "", status, nil, reason)
	if err != nil {
		return err
	}
	return s.audit(ctx, "fleet.discovery."+status, row, 0)
}
