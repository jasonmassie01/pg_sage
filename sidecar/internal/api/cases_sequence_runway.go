package api

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/cases"
)

// sequenceRunwayReadSQL reads the open runway forecasts of the named
// sequences in one bounded statement (playbooks-03b). The analyzer names a
// sequence schema.name as the catalog spells it; the runway probe quotes
// each part with quote_ident. Both spellings are matched, quote_ident
// running on the server so its keyword list is the probe's; the newest
// forecast per name wins. A var so a test can point it at a broken view.
var sequenceRunwayReadSQL = `/* pg_sage */ SELECT DISTINCT ON (w.ident) w.ident,
       f.detail->'rate_per_s',
       f.detail->'seconds_to_limit', f.last_seen
  FROM pg_catalog.unnest($1::text[]) AS w(ident)
  JOIN sage.findings f
    ON f.object_identifier = w.ident
    OR f.object_identifier = CASE WHEN pg_catalog.strpos(w.ident, '.') > 0 THEN
         pg_catalog.quote_ident(pg_catalog.split_part(w.ident, '.', 1)) || '.' ||
         pg_catalog.quote_ident(pg_catalog.substr(w.ident, pg_catalog.strpos(w.ident, '.') + 1))
       END
 WHERE f.status = 'open' AND f.category = 'forecast_sequence_runway'
 ORDER BY w.ident, f.last_seen DESC, f.id DESC
 LIMIT $2`

// sequenceExhaustionCategory is the finding category that gets the rate.
const sequenceExhaustionCategory = "sequence_exhaustion"

// sequenceRunway is one open runway forecast as read.
type sequenceRunway struct {
	rate, seconds any
	readAt        time.Time
}

// attachSequenceRunway copies the open runway forecast of each
// sequence_exhaustion row's sequence into the row's detail, so its
// forward-fix script states the measured rate. It runs before
// projectFindingRows, which stays pure. A failed read logs one WARN and
// the rows project without a rate: it never fails the cases response.
func attachSequenceRunway(
	ctx context.Context, pool *pgxpool.Pool, database string, rows []map[string]any,
) {
	names := sequenceRunwayNames(rows)
	if pool == nil || len(names) == 0 {
		return
	}
	found, err := readSequenceRunways(ctx, pool, names)
	if err != nil {
		slog.Warn("sequence runway read failed; sequence scripts show no rate",
			"database", database, "sequences", len(names), "error", err)
		return
	}
	for _, row := range rows {
		r, ok := found[sequenceRunwayName(row)]
		if !ok {
			continue
		}
		detail, _ := row["detail"].(map[string]any)
		if detail == nil {
			detail = map[string]any{}
			row["detail"] = detail
		}
		detail[cases.DetailRunwayRatePerS] = r.rate
		detail[cases.DetailRunwaySecondsToLimit] = r.seconds
		detail[cases.DetailRunwayReadAt] = r.readAt.UTC()
	}
}

// sequenceRunwayName is the sequence a row names, or "" when the row is
// not a sequence_exhaustion finding with a detail object (or none).
func sequenceRunwayName(row map[string]any) string {
	if stringValue(row["category"]) != sequenceExhaustionCategory {
		return ""
	}
	if d := row["detail"]; d != nil {
		if _, ok := d.(map[string]any); !ok {
			return ""
		}
	}
	return strings.TrimSpace(stringValue(row["object_identifier"]))
}

// sequenceRunwayNames lists each named sequence once, in row order.
func sequenceRunwayNames(rows []map[string]any) []string {
	seen := map[string]bool{}
	var out []string
	for _, row := range rows {
		name := sequenceRunwayName(row)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	return out
}

// readSequenceRunways reads the newest open forecast of each name: one
// row per name at most.
func readSequenceRunways(
	ctx context.Context, pool *pgxpool.Pool, names []string,
) (map[string]sequenceRunway, error) {
	rows, err := pool.Query(ctx, sequenceRunwayReadSQL, names, len(names))
	if err != nil {
		return nil, fmt.Errorf("read runway forecasts of %d sequences: %w", len(names), err)
	}
	defer rows.Close()
	out := make(map[string]sequenceRunway, len(names))
	for rows.Next() {
		var name string
		var r sequenceRunway
		if err := rows.Scan(&name, &r.rate, &r.seconds, &r.readAt); err != nil {
			return nil, fmt.Errorf("scan a runway forecast: %w", err)
		}
		out[name] = r
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read runway forecasts of %d sequences: %w", len(names), err)
	}
	return out, nil
}
