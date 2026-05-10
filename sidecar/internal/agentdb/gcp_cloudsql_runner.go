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
)

type CloudSQLCreateInput struct {
	Project            string
	Region             string
	Name               string
	DatabaseVersion    string
	Tier               string
	Edition            string
	StorageGB          int64
	IPv4Enabled        bool
	AuthorizedNetworks []string
	RequireSSL         bool
	BackupEnabled      bool
	DeletionProtection bool
	Labels             map[string]string
}

type CloudSQLInstance struct {
	Name             string
	State            string
	ConnectionName   string
	PublicIPAddress  string
	PrivateIPAddress string
}

type CloudSQLClient interface {
	CreateInstance(ctx context.Context, input CloudSQLCreateInput) (CloudSQLInstance, error)
	GetInstance(ctx context.Context, project string, name string) (CloudSQLInstance, error)
	DeleteInstance(ctx context.Context, project string, name string) error
}

type CloudSQLRunner struct {
	client  CloudSQLClient
	project string
	region  string
}

func NewCloudSQLRunner(client CloudSQLClient, project string, region string) CloudSQLRunner {
	return CloudSQLRunner{client: client, project: project, region: region}
}

func (r CloudSQLRunner) Name() string { return "gcp_cloudsql_admin_api" }

func (r CloudSQLRunner) Provider() string { return ProviderGCPCloudSQL }

func (r CloudSQLRunner) Preflight(_ context.Context, req ProvisionRequest) ProvisionResult {
	input, err := r.createInput(req)
	if err != nil {
		return ProvisionResult{Status: "preflight_failed", Error: err}
	}
	return ProvisionResult{
		Status:             "preflight_passed",
		ProviderResourceID: input.Name,
		Detail: map[string]any{
			"project":          input.Project,
			"region":           input.Region,
			"tier":             input.Tier,
			"edition":          input.Edition,
			"ipv4_enabled":     input.IPv4Enabled,
			"backup_enabled":   input.BackupEnabled,
			"authorized_count": len(input.AuthorizedNetworks),
		},
	}
}

func (r CloudSQLRunner) Create(
	ctx context.Context,
	req ProvisionRequest,
) ProvisionResult {
	input, err := r.createInput(req)
	if err != nil {
		return ProvisionResult{Status: "failed", Error: err}
	}
	instance, err := r.client.CreateInstance(ctx, input)
	if err != nil {
		return ProvisionResult{Status: "failed", Error: mapProviderError(r.Provider(), err)}
	}
	return cloudSQLProvisionResult(instance)
}

func (r CloudSQLRunner) Status(
	ctx context.Context,
	req ProvisionRequest,
) ProvisionResult {
	project := cloudSQLProject(r, req.Deployment)
	name := req.Deployment.ProviderResourceID
	if name == "" {
		name, _ = ProviderResourceName(ProviderGCPCloudSQL, req.Deployment.DeploymentID)
	}
	instance, err := r.client.GetInstance(ctx, project, name)
	if err != nil {
		return ProvisionResult{Status: "status_unknown", Error: mapProviderError(r.Provider(), err)}
	}
	return cloudSQLProvisionResult(instance)
}

func (r CloudSQLRunner) Destroy(
	ctx context.Context,
	req ProvisionRequest,
) ProvisionResult {
	project := cloudSQLProject(r, req.Deployment)
	name := req.Deployment.ProviderResourceID
	if name == "" {
		name, _ = ProviderResourceName(ProviderGCPCloudSQL, req.Deployment.DeploymentID)
	}
	if err := r.client.DeleteInstance(ctx, project, name); err != nil {
		mapped := mapProviderError(r.Provider(), err)
		if pe, ok := mapped.(ProviderError); ok && pe.Kind == ProviderErrNotFound {
			return ProvisionResult{Status: "destroyed"}
		}
		return ProvisionResult{Status: "failed", Error: mapped}
	}
	return ProvisionResult{
		Status:             "destroying",
		ProviderResourceID: name,
		Detail:             map[string]any{"project": project},
	}
}

func (r CloudSQLRunner) BackupCheck(
	ctx context.Context,
	req ProvisionRequest,
) ProvisionResult {
	return r.Status(ctx, req)
}

func (r CloudSQLRunner) createInput(req ProvisionRequest) (CloudSQLCreateInput, error) {
	if r.client == nil {
		return CloudSQLCreateInput{}, providerError(
			ProviderGCPCloudSQL, ProviderErrUnavailable,
			"cloud sql admin client is not configured",
			"configure ADC or service account credentials",
		)
	}
	name, err := ProviderResourceName(ProviderGCPCloudSQL, req.Deployment.DeploymentID)
	if err != nil {
		return CloudSQLCreateInput{}, err
	}
	params := providerParams(req.Deployment)
	project := cloudSQLProject(r, req.Deployment)
	if project == "" {
		return CloudSQLCreateInput{}, providerError(
			ProviderGCPCloudSQL, ProviderErrInvalid,
			"project is required", "set provider_params.project or runner project",
		)
	}
	region := stringParam(params, "region")
	if region == "" {
		region = r.region
	}
	if region == "" {
		return CloudSQLCreateInput{}, providerError(
			ProviderGCPCloudSQL, ProviderErrInvalid,
			"region is required", "set provider_params.region or runner region",
		)
	}
	tier := stringParam(params, "tier")
	if tier == "" {
		tier = "db-custom-1-3840"
	}
	edition := stringParam(params, "edition")
	if edition == "" {
		edition = "ENTERPRISE"
	}
	if tier == "db-f1-micro" && edition != "ENTERPRISE" {
		return CloudSQLCreateInput{}, providerError(
			ProviderGCPCloudSQL, ProviderErrInvalid,
			"db-f1-micro requires enterprise edition in pg_sage policy",
			"use edition=ENTERPRISE",
		)
	}
	ipv4 := boolParamAny(params, "ipv4_enabled")
	if ipv4 && !req.Policy.AllowPublicIP {
		return CloudSQLCreateInput{}, providerError(
			ProviderGCPCloudSQL, ProviderErrInvalid,
			"public ipv4 is denied by policy",
			"use private IP or explicitly allow public IP for test workloads",
		)
	}
	authorized := stringsFromAny(params["authorized_networks"])
	for _, network := range authorized {
		if strings.TrimSpace(network) == "0.0.0.0/0" {
			return CloudSQLCreateInput{}, providerError(
				ProviderGCPCloudSQL, ProviderErrInvalid,
				"0.0.0.0/0 authorized network is denied",
				"use private connectivity or a narrow test CIDR",
			)
		}
	}
	storage := int64(float64Param(params, "storage_size"))
	if storage <= 0 {
		storage = 20
	}
	return CloudSQLCreateInput{
		Project:            project,
		Region:             region,
		Name:               name,
		DatabaseVersion:    firstNonEmpty(stringParam(params, "database_version"), "POSTGRES_16"),
		Tier:               tier,
		Edition:            edition,
		StorageGB:          storage,
		IPv4Enabled:        ipv4,
		AuthorizedNetworks: authorized,
		RequireSSL:         true,
		BackupEnabled:      true,
		DeletionProtection: !isDisposable(req.Deployment),
		Labels: map[string]string{
			"app":                   "pg-sage",
			"pg_sage_deployment_id": req.Deployment.DeploymentID,
		},
	}, nil
}

func cloudSQLProject(r CloudSQLRunner, dep Deployment) string {
	params := providerParams(dep)
	if project := stringParam(params, "project"); project != "" {
		return project
	}
	return r.project
}

func cloudSQLProvisionResult(instance CloudSQLInstance) ProvisionResult {
	status := "status_unknown"
	switch strings.ToUpper(instance.State) {
	case "RUNNABLE":
		status = "available"
	case "PENDING_CREATE", "MAINTENANCE", "PENDING_UPDATE":
		status = "provisioning"
	case "PENDING_DELETE":
		status = "destroying"
	}
	return ProvisionResult{
		Status:             status,
		ProviderResourceID: instance.Name,
		SecretRefProvider:  "gcp_secret_manager",
		ConnectionInfo: map[string]any{
			"instance_connection_name": instance.ConnectionName,
			"private_ip_address":       instance.PrivateIPAddress,
			"secret_ref_provider":      "gcp_secret_manager",
		},
		Detail: map[string]any{"cloudsql_state": instance.State},
	}
}

type CloudSQLHTTPClient struct {
	BaseURL    string
	TokenFunc  func(context.Context) (string, error)
	HTTPClient *http.Client
}

func (c CloudSQLHTTPClient) CreateInstance(
	ctx context.Context,
	input CloudSQLCreateInput,
) (CloudSQLInstance, error) {
	body := map[string]any{
		"name":            input.Name,
		"region":          input.Region,
		"databaseVersion": input.DatabaseVersion,
		"settings": map[string]any{
			"tier":                      input.Tier,
			"edition":                   input.Edition,
			"dataDiskSizeGb":            input.StorageGB,
			"deletionProtectionEnabled": input.DeletionProtection,
			"userLabels":                input.Labels,
			"backupConfiguration": map[string]any{
				"enabled":                    input.BackupEnabled,
				"pointInTimeRecoveryEnabled": input.BackupEnabled,
			},
			"ipConfiguration": map[string]any{
				"ipv4Enabled": input.IPv4Enabled,
				"requireSsl":  input.RequireSSL,
				"authorizedNetworks": cloudSQLAuthorizedNetworks(
					input.AuthorizedNetworks,
				),
			},
		},
	}
	if _, err := c.do(ctx, http.MethodPost,
		"/sql/v1beta4/projects/"+url.PathEscape(input.Project)+"/instances",
		body, nil); err != nil {
		return CloudSQLInstance{}, err
	}
	return CloudSQLInstance{Name: input.Name, State: "PENDING_CREATE"}, nil
}

func (c CloudSQLHTTPClient) GetInstance(
	ctx context.Context,
	project string,
	name string,
) (CloudSQLInstance, error) {
	var raw map[string]any
	if _, err := c.do(ctx, http.MethodGet,
		"/sql/v1beta4/projects/"+url.PathEscape(project)+
			"/instances/"+url.PathEscape(name), nil, &raw); err != nil {
		return CloudSQLInstance{}, err
	}
	return cloudSQLInstanceFromAPI(raw), nil
}

func (c CloudSQLHTTPClient) DeleteInstance(
	ctx context.Context,
	project string,
	name string,
) error {
	_, err := c.do(ctx, http.MethodDelete,
		"/sql/v1beta4/projects/"+url.PathEscape(project)+
			"/instances/"+url.PathEscape(name), nil, nil)
	return err
}

func (c CloudSQLHTTPClient) do(
	ctx context.Context,
	method string,
	path string,
	body any,
	out any,
) (map[string]any, error) {
	client := c.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	var reader io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL()+path, reader)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.TokenFunc != nil {
		token, err := c.TokenFunc(ctx)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, fmt.Errorf("cloud sql api status %d: %s",
			resp.StatusCode, redactString(string(body)))
	}
	var raw map[string]any
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return nil, err
		}
		return raw, nil
	}
	_ = json.NewDecoder(resp.Body).Decode(&raw)
	return raw, nil
}

func (c CloudSQLHTTPClient) baseURL() string {
	if c.BaseURL != "" {
		return strings.TrimRight(c.BaseURL, "/")
	}
	return "https://sqladmin.googleapis.com"
}

func cloudSQLAuthorizedNetworks(networks []string) []map[string]string {
	entries := make([]map[string]string, 0, len(networks))
	for _, network := range networks {
		entries = append(entries, map[string]string{"value": network})
	}
	return entries
}

func cloudSQLInstanceFromAPI(raw map[string]any) CloudSQLInstance {
	ipAddresses := mapSliceValue(raw, "ipAddresses")
	return CloudSQLInstance{
		Name:             stringMapValue(raw, "name"),
		State:            stringMapValue(raw, "state"),
		ConnectionName:   stringMapValue(raw, "connectionName"),
		PublicIPAddress:  cloudSQLIPAddress(ipAddresses, "PRIMARY"),
		PrivateIPAddress: cloudSQLIPAddress(ipAddresses, "PRIVATE"),
	}
}

func mapSliceValue(raw map[string]any, key string) []any {
	if raw == nil {
		return nil
	}
	values, _ := raw[key].([]any)
	return values
}

func cloudSQLIPAddress(values []any, kind string) string {
	for _, value := range values {
		item, ok := value.(map[string]any)
		if !ok || stringMapValue(item, "type") != kind {
			continue
		}
		return stringMapValue(item, "ipAddress")
	}
	return ""
}
