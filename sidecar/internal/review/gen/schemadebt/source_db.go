package schemadebt

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/catalogread"
	"github.com/pg-sage/sidecar/internal/migration/plan"
)

// maxSamplesPerSequence bounds the samples read of one sequence (the
// newest ones).
const maxSamplesPerSequence = 5000

// feedingSequenceSQL is the sequence that feeds a column: owned by it
// (serial) or its identity, as the SRE runway probe resolves it, named
// as the runway store names its subject.
const feedingSequenceSQL = `/* pg_sage review:schemadebt */
SELECT pg_catalog.quote_ident(n.nspname) || '.' || pg_catalog.quote_ident(c.relname),
       n.nspname::text, c.relname::text, q.seqincrement
  FROM pg_catalog.pg_attribute a
  JOIN pg_catalog.pg_depend d ON d.refobjid = a.attrelid AND d.refobjsubid = a.attnum
  JOIN pg_catalog.pg_sequence q ON q.seqrelid = d.objid
  JOIN pg_catalog.pg_class c ON c.oid = q.seqrelid
  JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
 WHERE a.attrelid = pg_catalog.to_regclass(pg_catalog.format('%I.%I', $1::text, $2::text))
   AND a.attname = $3
   AND d.classid = 'pg_catalog.pg_class'::pg_catalog.regclass
   AND d.refclassid = 'pg_catalog.pg_class'::pg_catalog.regclass
   AND d.deptype IN ('a', 'i')
 ORDER BY c.oid
 LIMIT 1`

// seriesSQL reads each subject's newest samples since $2 through
// runway_samples_series_idx, at most $3 a subject.
const seriesSQL = `/* pg_sage review:schemadebt */
SELECT u.subject, r.epoch, r.sampled_at, r.value, r.limit_value
  FROM unnest($1::text[]) AS u(subject)
 CROSS JOIN LATERAL (
       SELECT s.epoch, s.sampled_at, s.value, s.limit_value
         FROM sage.runway_samples s
        WHERE s.kind = 'sequence' AND s.subject = u.subject AND s.sampled_at >= $2
        ORDER BY s.sampled_at DESC
        LIMIT $3) r
 ORDER BY u.subject, r.sampled_at DESC`

// DBSource reads the catalog facts of the monitored database through
// catalog (bounded, read-only: a catalogread.Reader) and the runway
// samples from pg_sage's own schema through samples. It writes nothing.
type DBSource struct {
	samples catalogread.Querier
	catalog catalogread.Querier
}

// NewDBSource is a source over the two databases (often one).
func NewDBSource(samples, catalog catalogread.Querier) *DBSource {
	return &DBSource{samples: samples, catalog: catalog}
}

var _ Source = (*DBSource)(nil)

// Read reads at most MaxColumns columns (Truncated says more were asked
// for). A column whose facts cannot be read carries the reason; a
// canceled context and a failed samples read are errors.
func (s *DBSource) Read(ctx context.Context, cols []ColumnRef, since time.Time) (Inputs,
	error) {
	if s.samples == nil || s.catalog == nil {
		return Inputs{}, errors.New("schemadebt source: no database connection")
	}
	in := Inputs{Columns: map[ColumnRef]Column{}, Series: map[string]Series{}}
	if len(cols) > MaxColumns {
		cols, in.Truncated = cols[:MaxColumns], true
	}
	var subjects []string
	for _, c := range cols {
		col, err := s.readColumn(ctx, c)
		if err != nil {
			return Inputs{}, err
		}
		in.Columns[c] = col
		if col.Sequence != "" {
			subjects = append(subjects, col.Sequence)
		}
	}
	if len(subjects) == 0 {
		return in, nil
	}
	series, err := s.readSeries(ctx, subjects, since)
	if err != nil {
		return Inputs{}, fmt.Errorf("read the runway samples of %d sequences: %w",
			len(subjects), err)
	}
	in.Series = series
	return in, nil
}

// readColumn reads c's swap facts and feeding sequence. Only a canceled
// context is an error; any other failure is the column's ReadErr.
func (s *DBSource) readColumn(ctx context.Context, c ColumnRef) (Column, error) {
	facts, err := plan.ReadSwapFacts(ctx, s.catalog, c.Schema, c.Table, c.Column)
	if err == nil {
		var col Column
		col, err = s.readSequence(ctx, c)
		col.Facts = facts
		if err == nil {
			return col, nil
		}
	}
	if cerr := ctx.Err(); cerr != nil {
		return Column{}, fmt.Errorf("read the catalog facts of %s: %w", c, cerr)
	}
	return Column{ReadErr: err.Error()}, nil
}

// readSequence is the sequence feeding c, if any.
func (s *DBSource) readSequence(ctx context.Context, c ColumnRef) (Column, error) {
	var col Column
	err := s.catalog.QueryRow(ctx, feedingSequenceSQL, c.Schema, c.Table, c.Column).Scan(
		&col.Sequence, &col.SequenceSchema, &col.SequenceName, &col.Increment)
	if errors.Is(err, pgx.ErrNoRows) {
		return Column{}, nil
	}
	if err != nil {
		return Column{}, fmt.Errorf("read the sequence feeding %s: %w", c, err)
	}
	return col, nil
}

// readSeries reads each subject's samples and keeps those of its newest
// epoch, oldest first; Limit is the newest sample's.
func (s *DBSource) readSeries(ctx context.Context, subjects []string, since time.Time,
) (map[string]Series, error) {
	rows, err := s.samples.Query(ctx, seriesSQL, subjects, since, maxSamplesPerSequence)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]Series{}
	for rows.Next() {
		var subject, epoch string
		var p Point
		var limit *float64
		if err := rows.Scan(&subject, &epoch, &p.At, &p.Value, &limit); err != nil {
			return nil, err
		}
		sr, seen := out[subject]
		if !seen { // newest first: the first row names the epoch and the limit
			sr = Series{Subject: subject, Epoch: epoch, Limit: limitOf(limit)}
		}
		if epoch == sr.Epoch {
			sr.Points = append(sr.Points, p)
		}
		out[subject] = sr
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for subject, sr := range out { // read newest first; a series is oldest first
		slices.Reverse(sr.Points)
		out[subject] = sr
	}
	return out, nil
}

// limitOf is a stored limit; NULL is no limit of the series' own.
func limitOf(v *float64) float64 {
	if v == nil {
		return typeLimit("bigint", false)
	}
	return *v
}
