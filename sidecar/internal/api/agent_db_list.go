package api

import (
	"net/http"
	"strconv"

	"github.com/pg-sage/sidecar/internal/agentdb"
)

func agentDBListOptions(r *http.Request) (agentdb.DeploymentListOptions, error) {
	query := r.URL.Query()
	options := agentdb.DeploymentListOptions{
		TenantID: query.Get("tenant_id"),
		Provider: query.Get("provider"),
		Status:   query.Get("status"),
		Cursor:   query.Get("cursor"),
	}
	if query.Get("limit") == "" {
		return options, nil
	}
	limit, err := strconv.Atoi(query.Get("limit"))
	if err != nil {
		return agentdb.DeploymentListOptions{}, agentdb.ErrInvalid
	}
	options.Limit = limit
	return options, nil
}
