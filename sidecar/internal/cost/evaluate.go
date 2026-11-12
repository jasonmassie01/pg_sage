package cost

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// RightsizeInterval is how often a database's instance is judged.
const RightsizeInterval = time.Hour

// sharedBuffersSQL reads the running shared_buffers in bytes (read-only).
const sharedBuffersSQL = `/* pg_sage */ SELECT pg_catalog.pg_size_bytes(
	pg_catalog.current_setting('shared_buffers'))::float8`

// EvaluateRightsize judges resource's size from the hourly usage in
// pool's database and records the verdict as a finding.
func EvaluateRightsize(ctx context.Context, pool *pgxpool.Pool, resource, database string,
	rates RateSource, now time.Time) (Verdict, error) {
	if pool == nil || resource == "" {
		return Verdict{}, errors.New("right-sizing needs a database and a provider resource")
	}
	rows, err := LoadHours(ctx, pool, resource, now.Add(-UsageRetention))
	if err != nil {
		return Verdict{}, err
	}
	var sharedBuffers float64
	if err := pool.QueryRow(ctx, sharedBuffersSQL).Scan(&sharedBuffers); err != nil {
		return Verdict{}, fmt.Errorf("read shared_buffers for right-sizing %s: %w",
			resource, err)
	}
	in := RightsizeInput{Resource: resource, Rows: rows, SharedBuffersBytes: sharedBuffers,
		Now: now}
	if rates != nil {
		if r, ok := rates.Rates(resource, database); ok {
			in.Rates = &r
		}
	}
	v := Rightsize(in)
	if err := ApplyRightsize(ctx, pool, resource, v); err != nil {
		return v, err
	}
	return v, nil
}
