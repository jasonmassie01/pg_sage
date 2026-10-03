package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/pg-sage/sidecar/internal/verify"
)

// withheldUpsertSQL records one withheld admission per finding and reason;
// (xmax = 0) is true for a new row. INSERT ... ON CONFLICT DO UPDATE is
// atomic under concurrency: every concurrent withhold either inserts or
// increments.
const withheldUpsertSQL = `INSERT INTO sage.admission_withheld
	(database_name, finding_key, reason, mode, detail, evidence, decision_id)
	VALUES ($1, $2, $3, $4, $5, $6::jsonb, NULLIF($7, 0))
	ON CONFLICT (database_name, finding_key, reason) DO UPDATE SET
		last_seen_at = now(), occurrences = sage.admission_withheld.occurrences + 1,
		mode = EXCLUDED.mode, detail = EXCLUDED.detail, evidence = EXCLUDED.evidence,
		decision_id = COALESCE(EXCLUDED.decision_id, sage.admission_withheld.decision_id)
	RETURNING (xmax = 0)`

// recordWithheldAdmission upserts the withheld record and reports whether
// it is new, so the log line is written once per finding and reason. A
// withhold went uncounted when this upsert failed and the error was
// logged and reported as a new record (perf-selfexcl): the failure seen
// under load was a pooled connection another session had terminated. On
// a lost connection (the statement did not run) the upsert is retried:
// each failure discards that connection, so at most the pool's size of
// retries reaches a live one. Any other failure is returned with what was
// being recorded.
func (e *Executor) recordWithheldAdmission(
	ctx context.Context, findingKey string, decisionID int64, admission verify.Admission,
) (bool, error) {
	if e.pool == nil {
		return true, nil
	}
	evidence, err := json.Marshal(admission.Evidence)
	if err != nil {
		evidence = []byte("{}")
	}
	var inserted bool
	retries := int(e.pool.Config().MaxConns)
	for attempt := 0; ; attempt++ {
		err = e.pool.QueryRow(ctx, withheldUpsertSQL, e.databaseName, findingKey,
			admission.Reason, admission.Mode, admission.Detail, string(evidence),
			decisionID).Scan(&inserted)
		if err == nil || attempt >= retries || !connectionLost(ctx, err) {
			break
		}
	}
	if err != nil {
		return false, fmt.Errorf("record withheld admission %s (reason %s, database %s): %w",
			findingKey, admission.Reason, e.databaseName, err)
	}
	return inserted, nil
}

// connectionLost reports a failure of the connection rather than of the
// statement: the server terminated the session (SQLSTATE 57P01-57P03,
// class 08) or the socket closed, while the caller's context is live.
func connectionLost(ctx context.Context, err error) bool {
	if ctx.Err() != nil {
		return false
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return strings.HasPrefix(pgErr.Code, "57P0") || strings.HasPrefix(pgErr.Code, "08")
	}
	return pgconn.SafeToRetry(err) || errors.Is(err, io.EOF) ||
		errors.Is(err, io.ErrUnexpectedEOF) || strings.Contains(err.Error(), "conn closed")
}
