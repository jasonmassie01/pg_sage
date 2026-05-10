package agentdb

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type LakebaseClient interface {
	CreateBranch(ctx context.Context, req LakebaseBranchRequest) (LakebaseBranch, error)
	GetBranch(ctx context.Context, project string, branch string) (LakebaseBranch, error)
	DeleteBranch(ctx context.Context, project string, branch string) error
}

type LakebaseBranchRequest struct {
	Project      string            `json:"project"`
	SourceBranch string            `json:"source_branch"`
	Branch       string            `json:"branch"`
	TTLSeconds   int               `json:"ttl_seconds"`
	Metadata     map[string]string `json:"metadata"`
}

type LakebaseBranch struct {
	Project       string
	Branch        string
	State         string
	OperationName string
	Endpoint      string
}

type LakebaseRunner struct {
	client            LakebaseClient
	allowInstanceMode bool
}

func NewLakebaseRunner(client LakebaseClient, allowInstanceMode bool) LakebaseRunner {
	return LakebaseRunner{client: client, allowInstanceMode: allowInstanceMode}
}

func (r LakebaseRunner) Name() string { return "databricks_lakebase_api" }

func (r LakebaseRunner) Provider() string { return ProviderDatabricksLakebase }

func (r LakebaseRunner) Preflight(_ context.Context, req ProvisionRequest) ProvisionResult {
	branchReq, err := r.branchRequest(req)
	if err != nil {
		return ProvisionResult{Status: "preflight_failed", Error: err}
	}
	return ProvisionResult{
		Status:             "preflight_passed",
		ProviderResourceID: branchReq.Branch,
		Detail: map[string]any{
			"project":       branchReq.Project,
			"source_branch": branchReq.SourceBranch,
			"branch":        branchReq.Branch,
			"ttl_seconds":   branchReq.TTLSeconds,
		},
	}
}

func (r LakebaseRunner) Create(
	ctx context.Context,
	req ProvisionRequest,
) ProvisionResult {
	branchReq, err := r.branchRequest(req)
	if err != nil {
		return ProvisionResult{Status: "failed", Error: err}
	}
	branch, err := r.client.CreateBranch(ctx, branchReq)
	if err != nil {
		return ProvisionResult{Status: "failed", Error: mapProviderError(r.Provider(), err)}
	}
	return lakebaseResult(branch)
}

func (r LakebaseRunner) Status(
	ctx context.Context,
	req ProvisionRequest,
) ProvisionResult {
	params := providerParams(req.Deployment)
	project := stringParam(params, "project")
	branch := req.Deployment.ProviderResourceID
	if branch == "" {
		branch, _ = ProviderResourceName(ProviderDatabricksLakebase, req.Deployment.DeploymentID)
	}
	got, err := r.client.GetBranch(ctx, project, branch)
	if err != nil {
		return ProvisionResult{Status: "status_unknown", Error: mapProviderError(r.Provider(), err)}
	}
	return lakebaseResult(got)
}

func (r LakebaseRunner) Destroy(
	ctx context.Context,
	req ProvisionRequest,
) ProvisionResult {
	params := providerParams(req.Deployment)
	project := stringParam(params, "project")
	branch := req.Deployment.ProviderResourceID
	if branch == "" {
		branch, _ = ProviderResourceName(ProviderDatabricksLakebase, req.Deployment.DeploymentID)
	}
	if err := r.client.DeleteBranch(ctx, project, branch); err != nil {
		mapped := mapProviderError(r.Provider(), err)
		if pe, ok := mapped.(ProviderError); ok && pe.Kind == ProviderErrNotFound {
			return ProvisionResult{Status: "destroyed"}
		}
		return ProvisionResult{Status: "failed", Error: mapped}
	}
	return ProvisionResult{
		Status:             "destroying",
		ProviderResourceID: branch,
		Detail:             map[string]any{"project": project},
	}
}

func (r LakebaseRunner) BackupCheck(
	ctx context.Context,
	req ProvisionRequest,
) ProvisionResult {
	return r.Status(ctx, req)
}

func (r LakebaseRunner) branchRequest(req ProvisionRequest) (LakebaseBranchRequest, error) {
	if r.client == nil {
		return LakebaseBranchRequest{}, providerError(
			ProviderDatabricksLakebase, ProviderErrUnavailable,
			"lakebase client is not configured",
			"configure Databricks workspace host and auth",
		)
	}
	if lakebasePlanUsesInstances(req.Deployment.ProvisioningPlan) && !r.allowInstanceMode {
		return LakebaseBranchRequest{}, providerError(
			ProviderDatabricksLakebase, ProviderErrInvalid,
			"Lakebase full instance mode is disabled",
			"use branch mode or enable allow_lakebase_instance after validation",
		)
	}
	params := providerParams(req.Deployment)
	project := stringParam(params, "project")
	if project == "" {
		return LakebaseBranchRequest{}, providerError(
			ProviderDatabricksLakebase, ProviderErrInvalid,
			"Lakebase project is required", "set provider_params.project",
		)
	}
	branch, err := ProviderResourceName(ProviderDatabricksLakebase, req.Deployment.DeploymentID)
	if err != nil {
		return LakebaseBranchRequest{}, err
	}
	ttl := 3600
	if req.Deployment.LeaseExpiresAt != nil {
		ttl = int(time.Until(*req.Deployment.LeaseExpiresAt).Seconds())
		if ttl <= 0 {
			ttl = 1
		}
	}
	source := firstNonEmpty(
		stringParam(params, "source_instance"),
		stringParam(params, "source_branch"),
		"main",
	)
	return LakebaseBranchRequest{
		Project:      project,
		SourceBranch: source,
		Branch:       branch,
		TTLSeconds:   ttl,
		Metadata: map[string]string{
			"app":                   "pg-sage",
			"pg_sage_deployment_id": req.Deployment.DeploymentID,
		},
	}, nil
}

func lakebaseResult(branch LakebaseBranch) ProvisionResult {
	status := "status_unknown"
	switch strings.ToUpper(branch.State) {
	case "READY", "RUNNING", "AVAILABLE":
		status = "available"
	case "CREATING", "UPDATING":
		status = "provisioning"
	case "DELETING":
		status = "destroying"
	}
	return ProvisionResult{
		Status:             status,
		ProviderResourceID: branch.Branch,
		ConnectionInfo: map[string]any{
			"endpoint": branch.Endpoint,
			"project":  branch.Project,
		},
		Detail: map[string]any{
			"lakebase_state": branch.State,
			"operation_name": branch.OperationName,
		},
	}
}

type LakebaseHTTPClient struct {
	BaseURL    string
	TokenFunc  func(context.Context) (string, error)
	HTTPClient *http.Client
}

func (c LakebaseHTTPClient) CreateBranch(
	ctx context.Context,
	req LakebaseBranchRequest,
) (LakebaseBranch, error) {
	body := map[string]any{
		"spec": map[string]any{
			"source_branch": req.SourceBranch,
			"ttl":           fmt.Sprintf("%ds", req.TTLSeconds),
		},
	}
	return c.doBranch(ctx, http.MethodPost,
		"/api/2.0/postgres/projects/"+url.PathEscape(req.Project)+
			"/branches?branch_id="+url.QueryEscape(req.Branch), body)
}

func (c LakebaseHTTPClient) GetBranch(
	ctx context.Context,
	project string,
	branch string,
) (LakebaseBranch, error) {
	return c.doBranch(ctx, http.MethodGet,
		"/api/2.0/postgres/projects/"+url.PathEscape(project)+
			"/branches/"+url.PathEscape(branch), nil)
}

func (c LakebaseHTTPClient) DeleteBranch(
	ctx context.Context,
	project string,
	branch string,
) error {
	_, err := c.doBranch(ctx, http.MethodDelete,
		"/api/2.0/postgres/projects/"+url.PathEscape(project)+
			"/branches/"+url.PathEscape(branch), nil)
	return err
}

func (c LakebaseHTTPClient) doBranch(
	ctx context.Context,
	method string,
	path string,
	body any,
) (LakebaseBranch, error) {
	client := c.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	var reader io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.BaseURL, "/")+path, reader)
	if err != nil {
		return LakebaseBranch{}, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.TokenFunc != nil {
		token, err := c.TokenFunc(ctx)
		if err != nil {
			return LakebaseBranch{}, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return LakebaseBranch{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return LakebaseBranch{}, fmt.Errorf("lakebase api status %d: %s",
			resp.StatusCode, redactString(string(body)))
	}
	var raw map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&raw)
	return lakebaseBranchFromAPI(raw), nil
}

func lakebaseBranchFromAPI(raw map[string]any) LakebaseBranch {
	name := stringMapValue(raw, "name")
	status := mapMapValue(raw, "status")
	return LakebaseBranch{
		Project:       lakebaseProjectFromName(name),
		Branch:        lakebaseBranchFromName(name),
		State:         firstNonEmpty(stringMapValue(status, "current_state"), stringMapValue(raw, "state")),
		OperationName: stringMapValue(raw, "name"),
		Endpoint:      stringMapValue(status, "endpoint"),
	}
}

func lakebaseProjectFromName(name string) string {
	parts := strings.Split(name, "/")
	if len(parts) >= 2 && parts[0] == "projects" {
		return parts[1]
	}
	return ""
}

func lakebaseBranchFromName(name string) string {
	parts := strings.Split(name, "/")
	for i := 0; i+1 < len(parts); i++ {
		if parts[i] == "branches" {
			return parts[i+1]
		}
	}
	return ""
}

func mapMapValue(raw map[string]any, key string) map[string]any {
	if raw == nil {
		return nil
	}
	if value, ok := raw[key].(map[string]any); ok {
		return value
	}
	return nil
}

func stringMapValue(raw map[string]any, key string) string {
	if raw == nil {
		return ""
	}
	if value, ok := raw[key].(string); ok {
		return value
	}
	return ""
}
