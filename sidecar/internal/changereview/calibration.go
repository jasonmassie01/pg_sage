package changereview

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// CalibrationWindow is how far back the track record printed in every
// review looks.
const CalibrationWindow = 90 * 24 * time.Hour

// VerdictClass is the track record of one verdict class: reviews, those a
// DBA decided and concurred with, and decided receipts (clean, regressed).
type VerdictClass struct {
	Reviews   int `json:"reviews"`
	Decided   int `json:"decided"`
	Concurred int `json:"concurred"`
	Receipts  int `json:"receipts"`
	Clean     int `json:"clean"`
	Regressed int `json:"regressed"`
}

// Calibration is pg_sage's track record on one database since Since: per
// verdict class, and the false-safe count (a safe verdict whose receipt
// regressed), which counts against pg_sage in every review it writes.
type Calibration struct {
	Since     time.Time               `json:"since"`
	Reviews   int                     `json:"reviews"`
	FalseSafe int                     `json:"false_safe"`
	ByVerdict map[string]VerdictClass `json:"by_verdict"`
}

// Calibration reads the track record of complete reviews created since.
func (st *Store) Calibration(ctx context.Context, since time.Time) (Calibration, error) {
	c := Calibration{Since: utc(since), ByVerdict: map[string]VerdictClass{}}
	rows, err := st.pool.Query(ctx, `/* pg_sage change_review_calibration v1 */
		SELECT verdict, count(*), count(*) FILTER (WHERE dba_verdict IS NOT NULL),
		  count(*) FILTER (WHERE dba_verdict = 'concur'),
		  count(*) FILTER (WHERE receipt_verdict IN ('clean', 'clean_low_traffic',
		                                             'regressed', 'unverifiable')),
		  count(*) FILTER (WHERE receipt_verdict IN ('clean', 'clean_low_traffic')),
		  count(*) FILTER (WHERE receipt_verdict = 'regressed')
		FROM sage.change_review WHERE created_at >= $1 AND state = 'complete'
		GROUP BY verdict`, since)
	if err != nil {
		return c, fmt.Errorf("%w: read the calibration: %v", ErrUnavailable, err)
	}
	defer rows.Close()
	for rows.Next() {
		var verdict string
		var v VerdictClass
		if err := rows.Scan(&verdict, &v.Reviews, &v.Decided, &v.Concurred, &v.Receipts,
			&v.Clean, &v.Regressed); err != nil {
			return c, fmt.Errorf("%w: scan the calibration: %v", ErrUnavailable, err)
		}
		c.ByVerdict[verdict] = v
		c.Reviews += v.Reviews
	}
	if err := rows.Err(); err != nil {
		return c, fmt.Errorf("%w: read the calibration: %v", ErrUnavailable, err)
	}
	c.FalseSafe = c.ByVerdict[VerdictSafe].Regressed
	return c, nil
}

// Line is the one-line track record printed in reviews.
func (c Calibration) Line() string {
	var parts []string
	decided := 0
	for _, v := range []string{VerdictSafe, VerdictReview, VerdictRisky} {
		cl, ok := c.ByVerdict[v]
		if !ok || cl.Decided == 0 {
			continue
		}
		decided += cl.Decided
		parts = append(parts, fmt.Sprintf("%s: %d of %d concurred", v, cl.Concurred,
			cl.Decided))
	}
	if decided == 0 {
		return fmt.Sprintf("Track record on this database: no decided reviews yet "+
			"(%d reviewed).", c.Reviews)
	}
	line := "Track record on this database"
	if !c.Since.IsZero() {
		line += " since " + c.Since.Format("2006-01-02")
	}
	line += ": " + strings.Join(parts, "; ")
	return line + fmt.Sprintf("; %d false-safe (safe, then a regressed receipt).",
		c.FalseSafe)
}
