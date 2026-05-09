package agentdb

import (
	"bytes"
	"context"
	"encoding/json"
)

func (s *Store) AuditEvents(ctx context.Context, id string) ([]AuditEvent, error) {
	if err := s.Ensure(ctx); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `
		SELECT audit_id, deployment_id, event, detail, created_at
		FROM sage.agent_db_audit
		WHERE deployment_id=$1
		ORDER BY created_at ASC, audit_id ASC`,
		id,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AuditEvent{}
	for rows.Next() {
		var event AuditEvent
		if err := scanAuditEvent(rows, &event); err != nil {
			return nil, err
		}
		out = append(out, event)
	}
	return out, rows.Err()
}

func (s *Store) AuditJSONL(ctx context.Context, id string) ([]byte, error) {
	events, err := s.AuditEvents(ctx, id)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for _, event := range events {
		if err := enc.Encode(event); err != nil {
			return nil, err
		}
	}
	return buf.Bytes(), nil
}

func scanAuditEvent(row scanner, event *AuditEvent) error {
	return row.Scan(
		&event.AuditID,
		&event.DeploymentID,
		&event.Event,
		&event.Detail,
		&event.CreatedAt,
	)
}
