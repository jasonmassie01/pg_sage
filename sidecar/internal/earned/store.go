package earned

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Ledger errors. Every API error wraps exactly one of them.
var (
	ErrInvalidRequest        = errors.New("invalid autonomy request")
	ErrInvalidReport         = errors.New("invalid PGIncidentBench report")
	ErrNotFound              = errors.New("autonomy proposal not found")
	ErrNotPending            = errors.New("autonomy proposal is not pending")
	ErrProposalExpired       = errors.New("autonomy proposal expired")
	ErrEvidenceNotMet        = errors.New("promotion evidence is not met")
	ErrHumanApprovalRequired = errors.New("a promotion needs a human approver")
	ErrConflict              = errors.New("autonomy ledger changed concurrently")
	ErrNotADowngrade         = errors.New("the target level is not below the current level")
	ErrUnavailable           = errors.New("autonomy ledger unavailable")
)

// EvidenceNotMetError names the unmet checks of a promotion.
type EvidenceNotMetError struct{ Assessment Assessment }

func (e *EvidenceNotMetError) Error() string {
	var unmet []string
	for _, c := range e.Assessment.Checks {
		if !c.Met {
			unmet = append(unmet, fmt.Sprintf("%s (observed %s, required %s)", c.Name,
				c.Observed, c.Required))
		}
	}
	return fmt.Sprintf("%v for %s: %s", ErrEvidenceNotMet, e.Assessment.Target,
		strings.Join(unmet, "; "))
}

// Unwrap makes errors.Is(err, ErrEvidenceNotMet) hold.
func (e *EvidenceNotMetError) Unwrap() error { return ErrEvidenceNotMet }

// ActorPgSage is the actor of every change pg_sage makes on its own.
const ActorPgSage = "pg_sage"

// State is the durable level of one family x class pair.
type State struct {
	Family    Family          `json:"family"`
	Class     ActionClass     `json:"class"`
	Level     Level           `json:"level"`
	Version   int64           `json:"version"`
	Evidence  json.RawMessage `json:"evidence,omitempty"`
	ChangedBy string          `json:"changed_by,omitempty"`
	Reason    string          `json:"reason,omitempty"`
	ChangedAt time.Time       `json:"changed_at"`
	// Stored is false for a default level nobody has changed.
	Stored bool `json:"stored"`
}

// PostgresStore keeps one deployment's ledger in the sage schema.
type PostgresStore struct {
	pool       *pgxpool.Pool
	deployment string
}

var uuidPattern = regexp.MustCompile(
	`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// NewPostgresStore binds the ledger of deploymentID (a UUID).
func NewPostgresStore(pool *pgxpool.Pool, deploymentID string) (*PostgresStore, error) {
	if pool == nil {
		return nil, fmt.Errorf("%w: no database pool", ErrUnavailable)
	}
	if !uuidPattern.MatchString(deploymentID) {
		return nil, fmt.Errorf("%w: deployment id %q is not a UUID", ErrInvalidRequest,
			deploymentID)
	}
	return &PostgresStore{pool: pool, deployment: deploymentID}, nil
}

// DeploymentID is the ledger's deployment.
func (s *PostgresStore) DeploymentID() string { return s.deployment }

// EnsureDeployment returns this database's Sage SRE deployment UUID,
// creating it once (the investigator shares the same row).
func EnsureDeployment(ctx context.Context, pool *pgxpool.Pool) (string, error) {
	if pool == nil {
		return "", fmt.Errorf("%w: no database pool", ErrUnavailable)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO sage.sre_deployments (deployment_id)
		VALUES ($1) ON CONFLICT (singleton) DO NOTHING`, newID()); err != nil {
		return "", fmt.Errorf("%w: ensure deployment: %v", ErrUnavailable, err)
	}
	var id string
	err := pool.QueryRow(ctx, "SELECT deployment_id::text FROM sage.sre_deployments").Scan(&id)
	if err != nil {
		return "", fmt.Errorf("%w: read deployment: %v", ErrUnavailable, err)
	}
	return id, nil
}

// newID is a random version-4 UUID.
func newID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("crypto/rand failed: %v", err))
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

type querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// withTx runs fn in one transaction.
func (s *PostgresStore) withTx(ctx context.Context, fn func(pgx.Tx) error) error {
	return pgx.BeginFunc(ctx, s.pool, fn)
}

// storeErr wraps a database failure as ErrUnavailable with what failed.
func storeErr(what string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%s: %w", what, err)
	}
	return fmt.Errorf("%w: %s: %v", ErrUnavailable, what, err)
}

const levelColumns = `family, action_class, level, version, evidence, changed_by,
	change_reason, changed_at`

func scanState(row pgx.Row) (State, error) {
	var st State
	var family, class string
	var level int16
	err := row.Scan(&family, &class, &level, &st.Version, &st.Evidence, &st.ChangedBy,
		&st.Reason, &st.ChangedAt)
	st.Family, st.Class, st.Level, st.Stored = Family(family), ActionClass(class),
		Level(level), err == nil
	return st, err
}

// readLevel is the stored state of a pair; found is false without a row.
func (s *PostgresStore) readLevel(ctx context.Context, q querier, f Family,
	c ActionClass) (State, bool, error) {
	st, err := scanState(q.QueryRow(ctx, `SELECT `+levelColumns+`
		FROM sage.sre_family_autonomy
		WHERE deployment_id = $1 AND family = $2 AND action_class = $3`,
		s.deployment, string(f), string(c)))
	if errors.Is(err, pgx.ErrNoRows) {
		return State{}, false, nil
	}
	return st, err == nil, storeErr("read autonomy level", err)
}

// Levels lists every stored pair of the deployment.
func (s *PostgresStore) Levels(ctx context.Context) ([]State, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+levelColumns+`
		FROM sage.sre_family_autonomy WHERE deployment_id = $1
		ORDER BY family, action_class`, s.deployment)
	if err != nil {
		return nil, storeErr("list autonomy levels", err)
	}
	defer rows.Close()
	var out []State
	for rows.Next() {
		st, err := scanState(rows)
		if err != nil {
			return nil, storeErr("scan autonomy level", err)
		}
		out = append(out, st)
	}
	return out, storeErr("list autonomy levels", rows.Err())
}

// levelChange is one compare-and-set of a pair's level.
type levelChange struct {
	Family        Family
	Class         ActionClass
	To            Level
	ExpectVersion int64 // 0: no row yet
	Actor, Reason string
	Evidence      json.RawMessage
	At            time.Time
}

// writeLevel sets a pair's level if its version is still ExpectVersion.
func (s *PostgresStore) writeLevel(ctx context.Context, q querier, ch levelChange) (State,
	error) {
	if len(ch.Evidence) == 0 {
		ch.Evidence = json.RawMessage(`{}`)
	}
	var row pgx.Row
	if ch.ExpectVersion == 0 {
		row = q.QueryRow(ctx, `INSERT INTO sage.sre_family_autonomy
			(deployment_id, family, action_class, level, evidence, changed_by,
			 change_reason, changed_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
			ON CONFLICT (deployment_id, family, action_class) DO NOTHING
			RETURNING `+levelColumns, s.deployment, string(ch.Family), string(ch.Class),
			int16(ch.To), ch.Evidence, ch.Actor, ch.Reason, ch.At)
	} else {
		row = q.QueryRow(ctx, `UPDATE sage.sre_family_autonomy
			SET level = $4, version = version + 1, evidence = $5, changed_by = $6,
			    change_reason = $7, changed_at = $8
			WHERE deployment_id = $1 AND family = $2 AND action_class = $3
			  AND version = $9
			RETURNING `+levelColumns, s.deployment, string(ch.Family), string(ch.Class),
			int16(ch.To), ch.Evidence, ch.Actor, ch.Reason, ch.At, ch.ExpectVersion)
	}
	st, err := scanState(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return State{}, fmt.Errorf("%w: %s/%s", ErrConflict, ch.Family, ch.Class)
	}
	return st, storeErr("write autonomy level", err)
}

// Pool is the control database the ledger lives in.
func (s *PostgresStore) Pool() *pgxpool.Pool { return s.pool }
