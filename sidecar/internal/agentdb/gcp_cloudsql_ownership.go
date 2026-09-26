package agentdb

import (
	"context"
	"fmt"
)

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
		mapped := mapProviderError(r.Provider(), err)
		return ProvisionResult{Status: createFailureStatus(mapped), Error: mapped}
	}
	return cloudSQLProvisionResult(instance)
}

// Status reads the recorded instance; without one it only adopts an
// uncertain create whose userLabels prove ownership.
func (r CloudSQLRunner) Status(
	ctx context.Context,
	req ProvisionRequest,
) ProvisionResult {
	name, err := statusIdentifier(ProviderGCPCloudSQL, req.Deployment)
	if err != nil {
		return ProvisionResult{Status: "status_unknown", Error: err}
	}
	instance, err := r.client.GetInstance(ctx, cloudSQLProject(r, req.Deployment), name)
	if err != nil {
		return ProvisionResult{Status: "status_unknown", Error: mapProviderError(r.Provider(), err)}
	}
	if err := verifyOwnershipTags(ProviderGCPCloudSQL, instance.Labels, req.Deployment); err != nil {
		return ProvisionResult{Status: "status_unknown", Error: err}
	}
	return cloudSQLProvisionResult(instance)
}

// Destroy deletes only the recorded, label-verified instance. Deletion
// protection is lifted inside the authorized destroy; a failure to do so is
// returned as an error instead of leaving the teardown silently stuck.
func (r CloudSQLRunner) Destroy(
	ctx context.Context,
	req ProvisionRequest,
) ProvisionResult {
	name := req.Deployment.ProviderResourceID
	if name == "" {
		return ProvisionResult{Status: "failed", Error: errRecordedIDRequired(ProviderGCPCloudSQL)}
	}
	project := cloudSQLProject(r, req.Deployment)
	instance, err := r.client.GetInstance(ctx, project, name)
	if err != nil {
		return cloudSQLDestroyError(r.Provider(), name, err)
	}
	if err := verifyOwnershipTags(ProviderGCPCloudSQL, instance.Labels, req.Deployment); err != nil {
		return ProvisionResult{Status: "failed", Error: err}
	}
	if instance.DeletionProtection {
		if err := r.client.SetDeletionProtection(ctx, project, name, false); err != nil {
			mapped := mapProviderError(r.Provider(), err)
			return ProvisionResult{Status: "failed",
				Error: fmt.Errorf("disable deletion protection: %w", mapped)}
		}
	}
	if err := r.client.DeleteInstance(ctx, project, name); err != nil {
		return cloudSQLDestroyError(r.Provider(), name, err)
	}
	return ProvisionResult{
		Status:             "destroying",
		ProviderResourceID: name,
		Detail: map[string]any{"project": project,
			"deletion_protection_lifted": instance.DeletionProtection},
	}
}

func cloudSQLDestroyError(provider, name string, err error) ProvisionResult {
	mapped := mapProviderError(provider, err)
	if providerErrorKind(mapped) == ProviderErrNotFound {
		return ProvisionResult{Status: "destroyed", ProviderResourceID: name}
	}
	return ProvisionResult{Status: "failed", Error: mapped}
}

func stringMapFromAny(value any) map[string]string {
	raw, _ := value.(map[string]any)
	out := make(map[string]string, len(raw))
	for key, item := range raw {
		if text, ok := item.(string); ok {
			out[key] = text
		}
	}
	return out
}
