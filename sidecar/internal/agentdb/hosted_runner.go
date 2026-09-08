package agentdb

import (
	"context"
	"errors"
	"fmt"
)

type HostedCreateInput struct {
	Mode, Scope, Name, Source, Region, Database string
}

type HostedResource struct {
	ID, Name, Scope, State, Endpoint, ChildProject string
	Default, Protected                             bool
}

type HostedBackupEvidence struct {
	Available        bool
	Kind             string
	Count            int
	RetentionSeconds int
}

type HostedResourceClient interface {
	CreateResource(context.Context, HostedCreateInput) (HostedResource, error)
	GetResource(context.Context, string, string, string) (HostedResource, error)
	DeleteResource(context.Context, string, string, string) error
	BackupEvidence(context.Context, string, string, string) (HostedBackupEvidence, error)
}

type HostedRunner struct {
	provider string
	client   HostedResourceClient
}

func NewHostedRunner(provider string, client HostedResourceClient) HostedRunner {
	return HostedRunner{provider: normalizeProvider(provider), client: client}
}

func (r HostedRunner) Name() string     { return r.provider + "_management_api" }
func (r HostedRunner) Provider() string { return r.provider }

func (r HostedRunner) input(req ProvisionRequest) (HostedCreateInput, error) {
	if r.client == nil || req.Deployment.DeploymentID == "" ||
		req.Deployment.Provider != r.provider || req.Deployment.ProvisioningLevel != LevelInstance {
		return HostedCreateInput{}, ErrInvalid
	}
	params := providerParams(req.Deployment)
	mode := firstNonEmpty(stringParam(params, "mode"), "branch")
	if mode != "branch" && mode != "project" {
		return HostedCreateInput{}, ErrInvalid
	}
	scope := stringParam(params, "project")
	if mode == "project" {
		scope = stringParam(params, "organization")
	}
	if scope == "" {
		return HostedCreateInput{}, fmt.Errorf("hosted %s scope is required", mode)
	}
	name, err := ProviderResourceName(r.provider, req.Deployment.DeploymentID)
	if err != nil {
		return HostedCreateInput{}, err
	}
	return HostedCreateInput{Mode: mode, Scope: scope, Name: name,
		Source: stringParam(params, "source_branch"), Region: stringParam(params, "region"),
		Database: req.Deployment.DatabaseName}, nil
}

func (r HostedRunner) Preflight(ctx context.Context, req ProvisionRequest) ProvisionResult {
	in, err := r.input(req)
	if err != nil {
		return ProvisionResult{Status: "preflight_failed", Error: err}
	}
	if err := r.validateScope(ctx, in); err != nil {
		return ProvisionResult{Status: "preflight_failed", Error: err}
	}
	return ProvisionResult{Status: "preflight_passed", Detail: map[string]any{
		"mode": in.Mode, "scope": in.Scope, "resource_name": in.Name,
		"credential_validation": "configured provider scope validated",
	}}
}

func (r HostedRunner) Create(ctx context.Context, req ProvisionRequest) ProvisionResult {
	in, err := r.input(req)
	if err != nil {
		return ProvisionResult{Status: "failed", Error: err}
	}
	if err := r.validateScope(ctx, in); err != nil {
		return ProvisionResult{Status: "failed", Error: err}
	}
	resource, err := r.client.CreateResource(ctx, in)
	if err != nil {
		return ProvisionResult{Status: "status_unknown",
			Error: fmt.Errorf("create hosted resource: %w", err)}
	}
	if resource.ID == "" || resource.Name != in.Name || resource.Scope != in.Scope {
		return ProvisionResult{Status: "status_unknown", Error: ErrInvalid}
	}
	return hostedProvisionResult(resource, req.Deployment.SecretRef)
}

func (r HostedRunner) validateScope(ctx context.Context, in HostedCreateInput) error {
	if client, ok := r.client.(interface {
		ValidateScope(context.Context, HostedCreateInput) error
	}); ok {
		return client.ValidateScope(ctx, in)
	}
	return nil
}

func (r HostedRunner) ownedResource(
	ctx context.Context, req ProvisionRequest,
) (HostedCreateInput, HostedResource, error) {
	in, err := r.input(req)
	if err != nil {
		return in, HostedResource{}, err
	}
	id := req.Deployment.ProviderResourceID
	if id == "" {
		return in, HostedResource{}, fmt.Errorf("recorded provider resource ID is required")
	}
	resource, err := r.client.GetResource(ctx, in.Mode, in.Scope, id)
	if err != nil {
		return in, resource, err
	}
	if resource.ID != id || resource.Name != in.Name || resource.Scope != in.Scope {
		return in, resource, fmt.Errorf("hosted resource ownership does not match deployment")
	}
	return in, resource, nil
}

func (r HostedRunner) Status(ctx context.Context, req ProvisionRequest) ProvisionResult {
	_, resource, err := r.ownedResource(ctx, req)
	if hostedResourceAbsent(err) && req.Deployment.ProvisioningStatus == "destroying" {
		return ProvisionResult{Status: "destroyed", ProviderResourceID: req.Deployment.ProviderResourceID}
	}
	if err != nil {
		return ProvisionResult{Status: "status_unknown", Error: err}
	}
	return hostedProvisionResult(resource, req.Deployment.SecretRef)
}

func (r HostedRunner) Destroy(ctx context.Context, req ProvisionRequest) ProvisionResult {
	in, resource, err := r.ownedResource(ctx, req)
	if hostedResourceAbsent(err) {
		return ProvisionResult{Status: "destroyed", ProviderResourceID: req.Deployment.ProviderResourceID}
	}
	if err != nil {
		return ProvisionResult{Status: "failed", Error: err}
	}
	if resource.Default || resource.Protected {
		return ProvisionResult{Status: "failed",
			Error: fmt.Errorf("default or protected resource cannot be destroyed")}
	}
	if err := r.client.DeleteResource(ctx, in.Mode, in.Scope, resource.ID); err != nil {
		return ProvisionResult{Status: "status_unknown", Error: err}
	}
	return ProvisionResult{Status: "destroying", ProviderResourceID: resource.ID}
}

func hostedResourceAbsent(err error) bool {
	var providerErr ProviderError
	return errors.As(err, &providerErr) && providerErr.Kind == ProviderErrNotFound
}

func (r HostedRunner) BackupCheck(ctx context.Context, req ProvisionRequest) ProvisionResult {
	in, resource, err := r.ownedResource(ctx, req)
	if err != nil {
		return ProvisionResult{Status: "failed", Error: err}
	}
	evidence, err := r.client.BackupEvidence(ctx, in.Mode, in.Scope, resource.ID)
	if err != nil {
		return ProvisionResult{Status: "failed", Error: err}
	}
	status := "unverified"
	if evidence.Available {
		status = "verified"
	}
	return ProvisionResult{Status: status, ProviderResourceID: resource.ID, Detail: map[string]any{
		"backup_kind": evidence.Kind, "backup_count": evidence.Count,
		"retention_seconds": evidence.RetentionSeconds, "restore_verified": false,
	}}
}

func hostedProvisionResult(resource HostedResource, secretRef string) ProvisionResult {
	status := resource.State
	switch status {
	case "available", "provisioning", "destroying", "destroyed", "failed":
	default:
		status = "status_unknown"
	}
	return ProvisionResult{Status: status, ProviderResourceID: resource.ID, SecretRef: secretRef,
		ConnectionInfo: map[string]any{"endpoint": resource.Endpoint,
			"project": resource.ChildProject}, Detail: map[string]any{"provider_state": resource.State}}
}
