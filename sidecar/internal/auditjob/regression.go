package auditjob

import (
	"context"
	"fmt"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/auditchain"
	"github.com/pg-sage/sidecar/internal/siem"
)

// RegressedCategory is the finding a chain raises when it went back behind
// what a SIEM sink already received (a restore, a PITR, deleted links),
// whether or not new links have reused the positions since.
const RegressedCategory = "audit_chain_regressed"

// CursorReader reads the SIEM export cursors of one chain of one source
// (*siem.Exporter is one).
type CursorReader interface {
	Cursors(ctx context.Context, source, chain string) ([]siem.Cursor, error)
}

// regressedSink is one sink whose cursor no longer finds its link.
type regressedSink struct {
	Sink   string `json:"sink"`
	Seq    int64  `json:"seq"`
	Reason string `json:"reason"`
}

// checkRegressions raises a finding per chain whose link at some sink's
// cursor is missing or is another link, and resolves the rest. Without a
// running exporter (no cursors) it checks nothing and resolves nothing: a
// regression raised earlier stays open, since export being off or failing
// to start proves no chain anchored again. A failed check resolves
// nothing either: an open regression stays until a check proves it gone.
func checkRegressions(ctx context.Context, src Source, chains []string) error {
	if src.Cursors == nil {
		return nil
	}
	var findings []analyzer.Finding
	for _, chain := range chains {
		spec, ok := auditchain.SpecFor(chain)
		if !ok {
			continue
		}
		sinks, err := regressedSinks(ctx, src, chain)
		if err != nil {
			return fmt.Errorf("auditjob: %s: SIEM cursors of %s: %w", src.Name, chain, err)
		}
		if len(sinks) > 0 {
			head, _, err := auditchain.Head(ctx, src.Pool, chain)
			if err != nil {
				return fmt.Errorf("auditjob: %s: %w", src.Name, err)
			}
			findings = append(findings, regressedFinding(src.Name, spec, head, sinks))
		}
	}
	return record(ctx, src.Pool, findings, RegressedCategory)
}

func regressedSinks(ctx context.Context, src Source, chain string) ([]regressedSink,
	error) {
	cursors, err := src.Cursors.Cursors(ctx, src.Name, chain)
	if err != nil {
		return nil, err
	}
	var out []regressedSink
	for _, c := range cursors {
		a, err := siem.CheckAnchor(ctx, src.Pool, c)
		if err != nil {
			return nil, err
		}
		if !a.OK {
			out = append(out, regressedSink{Sink: c.Sink, Seq: c.Seq, Reason: a.Reason})
		}
	}
	return out, nil
}

func regressedFinding(db string, spec auditchain.Spec, head int64,
	sinks []regressedSink) analyzer.Finding {
	return analyzer.Finding{
		Category: RegressedCategory, Severity: "critical", ObjectType: "audit_trail",
		ObjectIdentifier: ObjectIdentifier(spec), DatabaseName: db,
		Title: fmt.Sprintf("The audit trail %s went back behind its SIEM export",
			spec.Chain),
		Detail: map[string]any{"chain": spec.Chain, "head_seq": head, "sinks": sinks},
		Recommendation: "The chain no longer holds links a SIEM sink already received: " +
			"the database was restored (backup or PITR) or links were deleted, and new " +
			"links may reuse those positions. Export of this chain to those sinks is " +
			"stopped, so nothing is skipped silently. Compare the SIEM's copy with the " +
			"database, then re-anchor each sink with POST /api/v1/audit/siem/{sink}/" +
			"reanchor (source, chain, from_seq): the links re-exported over positions " +
			"the SIEM already holds are labelled \"fork\". Links before from_seq are " +
			"not re-sent: from_seq 1 is always safe, a higher one must be the first " +
			"position whose hash differs from the SIEM's copy.",
	}
}
