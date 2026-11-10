package auditjob

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/siem"
)

// UndeliverableCategory is the finding a SIEM sink raises when it refused
// an audit event and got a stub in its place: the SIEM's copy of that
// event lacks its content.
const UndeliverableCategory = "siem_event_undeliverable"

// RaiseUndeliverable records u as a warning on the source's database, one
// finding per sink and chain (a later stub refreshes it with its seq). It
// never resolves itself: an operator closes it once the SIEM holds the
// content or the loss is accepted.
func RaiseUndeliverable(ctx context.Context, pool *pgxpool.Pool,
	u siem.Undeliverable) error {
	if pool == nil {
		return fmt.Errorf("auditjob: %s: no database for source %s",
			UndeliverableCategory, u.Source)
	}
	if err := analyzer.UpsertSystemFindings(ctx, pool,
		[]analyzer.Finding{undeliverableFinding(u)}); err != nil {
		return fmt.Errorf("auditjob: raise %s finding on %s: %w", UndeliverableCategory,
			u.Source, err)
	}
	return nil
}

func undeliverableFinding(u siem.Undeliverable) analyzer.Finding {
	return analyzer.Finding{
		Category: UndeliverableCategory, Severity: "warning", ObjectType: "siem_sink",
		ObjectIdentifier: "siem_sink:" + u.Sink + "/" + u.Chain, DatabaseName: u.Source,
		// The title names no sink or source: the findings list hides a title
		// holding "pg_sage", and the identifier and detail name them anyway.
		Title: fmt.Sprintf("A SIEM sink refused audit event %d of %s", u.Seq, u.Chain),
		Detail: map[string]any{"sink": u.Sink, "source": u.Source, "chain": u.Chain,
			"seq": u.Seq, "hash": u.Hash, "reason": u.Reason},
		Recommendation: "The receiver refuses this event however often it is sent (HTTP " +
			"400, 413 or 422, OTLP rejected records, or too large for syslog), so " +
			"pg_sage sent a stub in its place, labelled \"undeliverable\", with the " +
			"link's position, op and hashes but not the row or the query, and export " +
			"went on. The event itself stays in the database, and the chain link proves " +
			"it. Find the stubs in the SIEM by that label, fix the receiver's limit or " +
			"parser, and copy the rows from sage.audit_chain_link and the audit table if " +
			"the SIEM must hold them. Resolve this finding once done.",
	}
}
