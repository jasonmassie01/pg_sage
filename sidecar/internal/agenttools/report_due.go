package agenttools

import (
	"context"
	"fmt"

	"github.com/pg-sage/sidecar/internal/verify"
)

// dueReportsSQL lists undecided deploys whose verify window has closed,
// oldest first.
const dueReportsSQL = `/* pg_sage */ SELECT finding_id FROM sage.source_fix
WHERE stage = 'deployed' AND decided_at IS NULL AND deployed_at <= $1
ORDER BY deployed_at, finding_id LIMIT $2`

// DecideDue decides up to limit due source-fix reports: deploys whose
// verify window closed with no verdict yet, oldest first. It is the
// background half of the status report (reportStatus), so a verdict is
// stored even when no agent comes back to ask; the conditional update stays
// the only writer, so a verdict an agent's call stored first is kept.
// decided counts the due reports that now carry a verdict. One report's
// failure is logged and skipped; the pass stops at the context's end.
func (t *Tools) DecideDue(ctx context.Context, limit int) (int, error) {
	if err := t.ready(); err != nil {
		return 0, err
	}
	if limit <= 0 {
		return 0, invalid("limit %d is not positive", limit)
	}
	ids, err := t.dueReports(ctx, limit)
	if err != nil {
		return 0, err
	}
	decided := 0
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return decided, err
		}
		rep, err := t.reportStatus(ctx, id)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return decided, ctxErr
			}
			t.opts.Log("WARN", "decide source-fix verdict of finding %d: %v "+
				"(retried on the next pass)", id, err)
			continue
		}
		if rep.Verdict != verify.OutcomePending {
			decided++
		}
	}
	return decided, nil
}

func (t *Tools) dueReports(ctx context.Context, limit int) ([]int64, error) {
	cutoff := t.now().Add(-t.opts.VerifyWindow)
	rows, err := t.pool.Query(ctx, dueReportsSQL, cutoff, limit)
	if err != nil {
		return nil, fmt.Errorf("read due source-fix reports: %w", err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan due source-fix report: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read due source-fix reports: %w", err)
	}
	return ids, nil
}

// SourceFixReport is the finding's stored source-fix report, read as is
// for operators: it never decides a verdict. ErrNotFound when no agent
// has reported on the finding.
func (t *Tools) SourceFixReport(ctx context.Context, findingID int64) (Report, error) {
	if err := t.ready(); err != nil {
		return Report{}, err
	}
	if findingID <= 0 {
		return Report{}, invalid("finding_id %d is not positive", findingID)
	}
	row, err := t.readReport(ctx, findingID)
	if err != nil {
		return Report{}, err
	}
	return t.toReport(row)
}
