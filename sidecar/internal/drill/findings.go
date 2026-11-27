package drill

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/notify"
)

// Finding categories (internal/findingcat).
const (
	// CategoryFailed is a failed or errored drill: critical, paged, and
	// resolved by the next pass.
	CategoryFailed = "restore_drill_failed"
	// CategoryUnknown is a database no drill can run on (no provider).
	CategoryUnknown = "recoverability_unknown"
)

// Pager sends finding alerts (a notify.Dispatcher).
type Pager interface {
	Dispatch(ctx context.Context, event notify.Event) error
}

// findingAlerter is a pager that says whether it takes finding alerts
// (only an outbox dispatcher does, as for the analyzer's findings).
type findingAlerter interface{ AlertsFindings() bool }

func pages(p Pager) bool {
	if p == nil {
		return false
	}
	fa, ok := p.(findingAlerter)
	return !ok || fa.AlertsFindings()
}

func objectID(database string) string { return "restore_drill:" + database }

// ReportResult opens restore_drill_failed for a failed drill, or resolves
// it (and recoverability_unknown) for a passed one, and pages either way
// through pager (nil: no page).
func ReportResult(ctx context.Context, pool *pgxpool.Pool, pager Pager, r Result) error {
	if pool == nil {
		return fmt.Errorf("report restore drill of %s: %w", r.Database, ErrNoStore)
	}
	if r.Passed {
		return errors.Join(resolve(ctx, pool, pager, r.Database, CategoryFailed),
			resolve(ctx, pool, pager, r.Database, CategoryUnknown))
	}
	f := failedFinding(r)
	return open(ctx, pool, pager, r.Database, f)
}

func failedFinding(r Result) analyzer.Finding {
	detail := map[string]any{"drill_id": r.ID, "provider": r.Provider, "kind": r.Kind,
		"failure": r.Failure, "receipt_id": r.ReceiptID, "cost_label": r.CostLabel,
		"requested_at": r.RequestedAt}
	if r.RTOSeconds != nil {
		detail["rto_seconds"] = *r.RTOSeconds
	}
	if r.RPOSeconds != nil {
		detail["rpo_seconds"] = *r.RPOSeconds
	} else if r.RPOReason != "" {
		detail["rpo_unknown"] = r.RPOReason
	}
	return analyzer.Finding{Category: CategoryFailed, Severity: "critical",
		ObjectType: "database", ObjectIdentifier: objectID(r.Database),
		Title:  "Restore drill failed for " + r.Database,
		Detail: detail,
		Recommendation: "A restore of this database's latest backup did not pass its " +
			"checks: " + r.Failure + ". Check the backups and the restore path before " +
			"relying on them; the finding resolves when the next drill passes."}
}

// ReportUnknown opens the info finding recoverability_unknown: no drill
// can run, so pg_sage cannot say whether this database's backups restore.
func ReportUnknown(ctx context.Context, pool *pgxpool.Pool, pager Pager, database,
	reason string) error {
	if pool == nil {
		return fmt.Errorf("report recoverability of %s: %w", database, ErrNoStore)
	}
	f := analyzer.Finding{Category: CategoryUnknown, Severity: "info",
		ObjectType: "database", ObjectIdentifier: objectID(database),
		Title:          "SQL only: recoverability unknown for " + database,
		Detail:         map[string]any{"reason": reason},
		Recommendation: UnknownAdvice}
	return open(ctx, pool, pager, database, f)
}

func open(ctx context.Context, pool *pgxpool.Pool, pager Pager, database string,
	f analyzer.Finding) error {
	res, err := analyzer.UpsertFindingsWithResult(ctx, pool, []analyzer.Finding{f})
	if err != nil {
		return fmt.Errorf("record %s finding for %s: %w", f.Category, database, err)
	}
	if !pages(pager) {
		return nil
	}
	var errs []error
	for _, list := range [][]analyzer.Finding{res.Opened, res.Reopened, res.Escalated} {
		for _, o := range list {
			detail, _ := json.Marshal(o.Detail)
			evt := notify.FindingOpenedEvent(o.Title, string(detail), database, o.Category,
				o.ObjectIdentifier, o.Severity)
			if err := pager.Dispatch(ctx, evt); err != nil {
				errs = append(errs, fmt.Errorf("page %s for %s: %w", o.Category, database, err))
			}
		}
	}
	return errors.Join(errs...)
}

// resolve closes database's open finding of category (only its own
// object, so another database's finding in a shared schema stays open).
func resolve(ctx context.Context, pool *pgxpool.Pool, pager Pager, database,
	category string) error {
	rows, err := pool.Query(ctx, `/* pg_sage restore_drill v1 */
		UPDATE sage.findings SET status = 'resolved', resolved_at = now()
		WHERE category = $1 AND object_identifier = $2 AND status = 'open'
		RETURNING severity, title`, category, objectID(database))
	if err != nil {
		return fmt.Errorf("resolve %s for %s: %w", category, database, err)
	}
	type closed struct{ severity, title string }
	var cleared []closed
	for rows.Next() {
		var c closed
		if err := rows.Scan(&c.severity, &c.title); err != nil {
			rows.Close()
			return fmt.Errorf("resolve %s for %s: %w", category, database, err)
		}
		cleared = append(cleared, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("resolve %s for %s: %w", category, database, err)
	}
	if !pages(pager) {
		return nil
	}
	var errs []error
	for _, c := range cleared {
		evt := notify.FindingResolvedEvent(c.title, database, category, objectID(database),
			c.severity)
		if err := pager.Dispatch(ctx, evt); err != nil {
			errs = append(errs, fmt.Errorf("page resolved %s for %s: %w", category, database,
				err))
		}
	}
	return errors.Join(errs...)
}
