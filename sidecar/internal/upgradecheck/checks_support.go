package upgradecheck

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/pgversion"
)

// shortSupport is how soon a target's support may end before U7 warns
// that the upgrade buys little time.
const shortSupport = 365 * 24 * time.Hour

// U7: the provider's dated support windows of the source and target
// majors. A provider without a dated table, or a major its table does not
// list, is unknown: never pass.
func checkSupport(_ context.Context, _ pgx.Tx, in input) (Check, error) {
	provider := in.target.Provider
	out := Check{Rows: []Row{}}
	if w, ok := pgversion.Support(provider, in.src.major); ok {
		out.Rows = append(out.Rows, windowRow("source", w))
	}
	tw, ok := pgversion.Support(provider, in.target.Major)
	if !ok {
		out.Status = Unknown
		out.Reason = fmt.Sprintf("pg_sage has no dated support table for provider %q and "+
			"PostgreSQL %d; check the provider's release calendar", provider, in.target.Major)
		out.Total = len(out.Rows)
		return out, nil
	}
	out.Rows = append(out.Rows, windowRow("target", tw))
	out.Total = len(out.Rows)
	end := tw.EndTime()
	target := "PostgreSQL " + strconv.Itoa(in.target.Major)
	switch {
	case end.IsZero():
		out.Status = Unknown
		out.Reason = "the dated table's end date for " + target + " does not parse"
	case !in.now.Before(end.Add(24 * time.Hour)):
		out.Status = Block
		out.Reason = fmt.Sprintf("%s left %s standard support on %s: upgrade to a "+
			"supported major instead", target, provider, tw.End)
	case end.Sub(in.now) <= shortSupport:
		out.Status = Warn
		out.Reason = fmt.Sprintf("%s leaves %s standard support on %s, within a year: "+
			"consider a newer target", target, provider, tw.End)
	default:
		out.Status = Pass
		out.Reason = fmt.Sprintf("%s is supported by %s until %s (read %s from %s)",
			target, provider, tw.End, tw.Read, tw.Source)
	}
	return out, nil
}

func windowRow(side string, w pgversion.Window) Row {
	return Row{"side": side, "major": strconv.Itoa(w.Major), "provider": w.Provider,
		"support_ends": w.End, "source": w.Source, "read": w.Read}
}
