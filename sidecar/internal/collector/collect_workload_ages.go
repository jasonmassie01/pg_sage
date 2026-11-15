package collector

import (
	"context"
	"fmt"
)

// Connection ages (evid-02b): this database's client sessions counted by
// state and by how long ago they connected (now() - backend_start), in
// four buckets. Each bucket starts at its lower bound: a session connected
// exactly 60 minutes ago is in AgeOver1h. A session whose backend_start
// the role cannot see (another role's session without pg_read_all_stats)
// is AgeUnknown, never a guessed age. pg_sage's own sessions are left out
// (notSelf), like the groups. A snapshot written before this read has no
// ages (nil): unknown, never zero.

// The connection-age buckets.
const (
	AgeUnder1m = "lt_1m"
	Age1To10m  = "1m_10m"
	Age10To60m = "10m_1h"
	AgeOver1h  = "ge_1h"
	AgeUnknown = "unknown"
)

// WorkloadAgeBucket is the client sessions of one state and age bucket.
type WorkloadAgeBucket struct {
	State    string `json:"state"`
	Bucket   string `json:"bucket"`
	Sessions int    `json:"sessions"`
}

// workloadAgeCap bounds the rows: a handful of states times four buckets.
const workloadAgeCap = 200

// ageBucketSQL is the bucket of the interval expression age.
func ageBucketSQL(age string) string {
	return `CASE WHEN ` + age + ` IS NULL THEN '` + AgeUnknown + `'
             WHEN ` + age + ` < interval '1 minute' THEN '` + AgeUnder1m + `'
             WHEN ` + age + ` < interval '10 minutes' THEN '` + Age1To10m + `'
             WHEN ` + age + ` < interval '60 minutes' THEN '` + Age10To60m + `'
             ELSE '` + AgeOver1h + `' END`
}

var workloadAgesSQL = sageTag + `
SELECT COALESCE(state, 'unknown'), ` + ageBucketSQL("(now() - backend_start)") + `,
       count(*)::int
  FROM pg_stat_activity` + workloadSessionFilter + `
 GROUP BY 1, 2
 ORDER BY 1, 2
 LIMIT ` + fmt.Sprint(workloadAgeCap)

func (c *Collector) workloadAges(ctx context.Context) ([]WorkloadAgeBucket, error) {
	rows, err := c.catalogQuery(ctx, workloadAgesSQL)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []WorkloadAgeBucket{}
	for rows.Next() {
		var a WorkloadAgeBucket
		if err := rows.Scan(&a.State, &a.Bucket, &a.Sessions); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
