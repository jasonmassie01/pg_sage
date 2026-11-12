package collector

import "context"

// WaitEventLoad is how many of this database's active client sessions
// were in one wait class and event when the cycle sampled them; the class
// is CPU for a session without a wait event (market-01).
type WaitEventLoad struct {
	WaitEventType string `json:"wait_event_type"`
	WaitEvent     string `json:"wait_event"`
	Sessions      int    `json:"sessions"`
}

// collectWaitEvents samples the active sessions by wait class
// (activeByWaitSQL). An idle database is an empty list, which differs
// from the missing row a failed read leaves.
func (c *Collector) collectWaitEvents(ctx context.Context) ([]WaitEventLoad, error) {
	rows, err := c.catalogQuery(ctx, activeByWaitSQL)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []WaitEventLoad{}
	for rows.Next() {
		var w WaitEventLoad
		if err := rows.Scan(&w.WaitEventType, &w.WaitEvent, &w.Sessions); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
