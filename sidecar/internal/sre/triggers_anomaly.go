package sre

import (
	"context"
	"encoding/json"

	"github.com/pg-sage/sidecar/internal/cases"
)

// anomalyTriggers are the open critical workload_anomaly findings
// (market-02). Each episode is one investigation: the idempotency key
// names the metric, its key and the episode start, so the finding seen
// again next cycle coalesces into the same investigation.
func (s *PGTriggerSource) anomalyTriggers(ctx context.Context) ([]Trigger, error) {
	rows, err := s.pool.Query(ctx, `SELECT COALESCE(object_type, ''),
		    COALESCE(object_identifier, ''), detail::text
		FROM sage.findings
		WHERE status = 'open' AND category = 'workload_anomaly' AND severity = 'critical'
		ORDER BY last_seen DESC, id LIMIT $1`, triggerBatch)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Trigger
	for rows.Next() {
		var objectType, ident, detail string
		if err := rows.Scan(&objectType, &ident, &detail); err != nil {
			return nil, err
		}
		if tr, ok := anomalyTrigger(objectType, ident, detail, s.database); ok {
			out = append(out, tr)
		}
	}
	return out, rows.Err()
}

// anomalyTrigger is the trigger of one anomaly finding; none when its
// detail lacks the metric, the key or the episode start. The finding id
// is not the key: the episode, not the row, is what one investigation covers.
func anomalyTrigger(objectType, ident, detail, database string) (Trigger, bool) {
	var d struct {
		Metric  string `json:"metric"`
		Key     string `json:"key"`
		Episode string `json:"episode_start"`
	}
	if json.Unmarshal([]byte(detail), &d) != nil || d.Metric == "" || d.Key == "" ||
		d.Episode == "" {
		return Trigger{}, false
	}
	f := cases.SourceFinding{DatabaseName: database, Category: "workload_anomaly",
		ObjectType: objectType, ObjectIdentifier: ident}
	return Trigger{CaseID: clip(cases.IdentityKeyForFinding(f)), Kind: TriggerAnomaly,
		Subject:        clip(d.Metric + " " + d.Key),
		IdempotencyKey: truncateRunes("anomaly:"+d.Metric+":"+d.Key+":"+d.Episode, 160),
	}, true
}
