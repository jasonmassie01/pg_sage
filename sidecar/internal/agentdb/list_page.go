package agentdb

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
)

const maxDeploymentPageSize = 100

type DeploymentListOptions struct {
	TenantID string
	Provider string
	Status   string
	Cursor   string
	Limit    int
}

type DeploymentPage struct {
	Deployments []Deployment `json:"deployments"`
	NextCursor  string       `json:"next_cursor"`
}

func (s *Store) ListPage(
	ctx context.Context,
	options DeploymentListOptions,
) (DeploymentPage, error) {
	if err := s.Ensure(ctx); err != nil {
		return DeploymentPage{}, err
	}
	limit, err := normalizeDeploymentPageLimit(options.Limit)
	if err != nil {
		return DeploymentPage{}, err
	}
	cursor, err := decodeDeploymentCursor(options.Cursor)
	if err != nil {
		return DeploymentPage{}, err
	}
	query, args := deploymentPageQuery(options, cursor, limit+1)
	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return DeploymentPage{}, err
	}
	defer rows.Close()
	deployments := make([]Deployment, 0, limit+1)
	for rows.Next() {
		var deployment Deployment
		if err := scanDeployment(rows, &deployment); err != nil {
			return DeploymentPage{}, err
		}
		deployments = append(deployments, deployment)
	}
	if err := rows.Err(); err != nil {
		return DeploymentPage{}, err
	}
	return deploymentPage(deployments, limit), nil
}

func normalizeDeploymentPageLimit(limit int) (int, error) {
	if limit < 0 {
		return 0, ErrInvalid
	}
	if limit == 0 {
		return maxDeploymentPageSize, nil
	}
	if limit > maxDeploymentPageSize {
		return maxDeploymentPageSize, nil
	}
	return limit, nil
}

func decodeDeploymentCursor(cursor string) (string, error) {
	if cursor == "" {
		return "", nil
	}
	decoded, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil || strings.TrimSpace(string(decoded)) == "" {
		return "", ErrInvalid
	}
	return string(decoded), nil
}

func deploymentPageQuery(
	options DeploymentListOptions,
	cursor string,
	limit int,
) (string, []any) {
	clauses := []string{"status <> 'deleted'"}
	args := make([]any, 0, 5)
	add := func(column, value string) {
		if value == "" {
			return
		}
		args = append(args, value)
		clauses = append(clauses, fmt.Sprintf("%s=$%d", column, len(args)))
	}
	add("tenant_id", options.TenantID)
	add("provider", options.Provider)
	add("status", options.Status)
	if cursor != "" {
		add("deployment_id", cursor)
		clauses[len(clauses)-1] = fmt.Sprintf(
			"deployment_id > $%d", len(args),
		)
	}
	args = append(args, limit)
	query := selectDeploymentsSQL + " WHERE " + strings.Join(clauses, " AND ")
	query += fmt.Sprintf(" ORDER BY deployment_id ASC LIMIT $%d", len(args))
	return query, args
}

func deploymentPage(deployments []Deployment, limit int) DeploymentPage {
	page := DeploymentPage{Deployments: deployments}
	if len(deployments) <= limit {
		return page
	}
	page.Deployments = deployments[:limit]
	lastID := page.Deployments[len(page.Deployments)-1].DeploymentID
	page.NextCursor = base64.RawURLEncoding.EncodeToString([]byte(lastID))
	return page
}
