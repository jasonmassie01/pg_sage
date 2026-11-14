package notify

import (
	"context"
	"fmt"
	"time"
)

// RecordDelivery records one delivery attempt with the event's incident
// (ref_id), the provider's message id, and the event's database and
// dedup key (alerting-06c; a fleet-wide event records NULL database). The
// error must already be redacted; it is redacted again here as defense in
// depth.
func (s *PoolStore) RecordDelivery(ctx context.Context, d Delivery) error {
	if s.pool == nil {
		return nil
	}
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, err := s.pool.Exec(qctx,
		`/* pg_sage */ INSERT INTO sage.notification_log
		    (channel_id, event, subject, body, status, error, ref_id, provider_message_id,
		     database, dedup_key)
		 VALUES ($1, $2, $3, $4, $5, $6, NULLIF($7, ''), NULLIF($8, ''),
		         NULLIF($9, ''), NULLIF($10, ''))`,
		d.ChannelID, d.Event.Type, d.Event.Subject, d.Event.Body, d.Status,
		RedactURLs(d.Error), d.Event.RefID, d.ProviderMessageID,
		logDatabase(d.Event), EventDedupKey(d.Event))
	if err != nil {
		return fmt.Errorf("insert notification_log: %w", err)
	}
	return nil
}

// IncidentPages returns, per channel, the first sent page (detected or
// escalated) of the incident, oldest channel first.
func (s *PoolStore) IncidentPages(ctx context.Context, incidentID string) ([]IncidentPage,
	error) {
	if s.pool == nil {
		return nil, fmt.Errorf("incident %s pages: notification pool is nil", incidentID)
	}
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	rows, err := s.pool.Query(qctx,
		`/* pg_sage */ SELECT DISTINCT ON (channel_id) channel_id, subject,
		        COALESCE(provider_message_id, '')
		   FROM sage.notification_log
		  WHERE ref_id = $1 AND status = 'sent' AND channel_id IS NOT NULL
		    AND event IN ('incident_detected', 'incident_escalated')
		  ORDER BY channel_id, sent_at, id`, incidentID)
	if err != nil {
		return nil, fmt.Errorf("incident %s pages: %w", incidentID, err)
	}
	defer rows.Close()
	var out []IncidentPage
	for rows.Next() {
		var p IncidentPage
		if err := rows.Scan(&p.ChannelID, &p.Subject, &p.ProviderMessageID); err != nil {
			return nil, fmt.Errorf("incident %s pages: scan: %w", incidentID, err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("incident %s pages: %w", incidentID, err)
	}
	return out, nil
}

var (
	_ DeliveryRecorder  = (*PoolStore)(nil)
	_ IncidentPageStore = (*PoolStore)(nil)
)
