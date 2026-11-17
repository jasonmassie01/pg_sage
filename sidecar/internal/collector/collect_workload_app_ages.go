package collector

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// Connection ages by application (review-02e): this database's client
// sessions counted by application and by the whole minutes since they
// connected, floor((now() - backend_start) / 60 s). Sage Review's
// connection generator reads them for recycle herds: many sessions of one
// application that connected in the same minute. The minute is nil when
// the role cannot see backend_start (another role's session without
// pg_read_all_stats), never a guessed age. It is negative for a session
// that connected after this read's transaction began, or under clock
// skew: kept as read, the generator skips it with a count. pg_sage's own
// sessions are left out (notSelf). The biggest buckets are kept first, at
// most WorkloadAppAgeCap rows; one more row marks truncation.

// WorkloadAppAgeCap bounds the application age rows of one snapshot.
const WorkloadAppAgeCap = 500

// WorkloadAppAge is the client sessions of one application that connected
// Minute whole minutes before the read.
type WorkloadAppAge struct {
	Application string `json:"application"`
	Minute      *int64 `json:"minute"`
	Sessions    int    `json:"sessions"`
}

// WorkloadAppAgesSQL reads the application age rows, biggest first.
var WorkloadAppAgesSQL = sageTag + `
SELECT COALESCE(application_name, ''),
       floor(EXTRACT(EPOCH FROM (now() - backend_start)) / 60)::int8,
       count(*)::int
  FROM pg_stat_activity` + workloadSessionFilter + `
 GROUP BY 1, 2
 ORDER BY 3 DESC, 1, 2
 LIMIT ` + fmt.Sprint(WorkloadAppAgeCap+1)

// ScanWorkloadAppAges reads the rows of WorkloadAppAgesSQL and closes
// them. truncated says the cap cut rows. The list is non-nil on success.
func ScanWorkloadAppAges(rows pgx.Rows) (ages []WorkloadAppAge, truncated bool, err error) {
	defer rows.Close()
	ages = []WorkloadAppAge{}
	for rows.Next() {
		var a WorkloadAppAge
		if err := rows.Scan(&a.Application, &a.Minute, &a.Sessions); err != nil {
			return nil, false, err
		}
		ages = append(ages, a)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	if len(ages) > WorkloadAppAgeCap {
		return ages[:WorkloadAppAgeCap], true, nil
	}
	return ages, false, nil
}

func (c *Collector) workloadAppAges(ctx context.Context) ([]WorkloadAppAge, bool, error) {
	rows, err := c.catalogQuery(ctx, WorkloadAppAgesSQL)
	if err != nil {
		return nil, false, err
	}
	return ScanWorkloadAppAges(rows)
}
