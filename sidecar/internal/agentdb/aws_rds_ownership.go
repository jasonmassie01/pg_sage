package agentdb

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
)

func (r AWSRDSRunner) Create(
	ctx context.Context,
	req ProvisionRequest,
) ProvisionResult {
	input, err := r.createInput(req)
	if err != nil {
		return ProvisionResult{Status: "failed", Error: err}
	}
	instance, err := r.client.CreateInstance(ctx, input)
	if err != nil {
		mapped := mapAWSError(err)
		return ProvisionResult{Status: createFailureStatus(mapped), Error: mapped}
	}
	result := rdsProvisionResult(instance)
	result.SecretRefProvider = "aws_secrets_manager"
	result.SecretRef = instance.SecretARN
	result.ConnectionInfo["secret_ref_provider"] = "aws_secrets_manager"
	if instance.SecretARN != "" {
		result.ConnectionInfo["secret_ref"] = instance.SecretARN
	}
	result.Detail["tags"] = input.Tags
	return result
}

// Status reads the recorded resource. Without a recorded id it only adopts a
// resource for an uncertain create, and only when the ownership tags match.
func (r AWSRDSRunner) Status(
	ctx context.Context,
	req ProvisionRequest,
) ProvisionResult {
	identifier, err := statusIdentifier(ProviderAWSRDS, req.Deployment)
	if err != nil {
		return ProvisionResult{Status: "status_unknown", Error: err}
	}
	instance, err := r.client.GetInstance(ctx, identifier)
	if err != nil {
		return ProvisionResult{Status: "status_unknown", Error: mapAWSError(err)}
	}
	if err := verifyOwnershipTags(ProviderAWSRDS, instance.Tags, req.Deployment); err != nil {
		return ProvisionResult{Status: "status_unknown", Error: err}
	}
	return rdsProvisionResult(instance)
}

// Destroy deletes only the recorded resource, and only after its ownership
// tags prove it belongs to this deployment.
func (r AWSRDSRunner) Destroy(
	ctx context.Context,
	req ProvisionRequest,
) ProvisionResult {
	identifier := req.Deployment.ProviderResourceID
	if identifier == "" {
		return ProvisionResult{Status: "failed", Error: errRecordedIDRequired(ProviderAWSRDS)}
	}
	instance, err := r.client.GetInstance(ctx, identifier)
	if err != nil {
		mapped := mapAWSError(err)
		if providerErrorKind(mapped) == ProviderErrNotFound {
			return ProvisionResult{Status: "destroyed", ProviderResourceID: identifier}
		}
		return ProvisionResult{Status: "failed", Error: mapped}
	}
	if err := verifyOwnershipTags(ProviderAWSRDS, instance.Tags, req.Deployment); err != nil {
		return ProvisionResult{Status: "failed", Error: err}
	}
	skipSnapshot := isDisposable(req.Deployment)
	if err := r.client.DeleteInstance(ctx, identifier, skipSnapshot); err != nil {
		mapped := mapAWSError(err)
		if providerErrorKind(mapped) == ProviderErrNotFound {
			return ProvisionResult{Status: "destroyed", ProviderResourceID: identifier}
		}
		return ProvisionResult{Status: "failed", Error: mapped}
	}
	return ProvisionResult{
		Status:             "destroying",
		ProviderResourceID: identifier,
		Detail:             map[string]any{"skip_final_snapshot": skipSnapshot},
	}
}

// approvedRegion binds the create to the region the live policy approved:
// the SDK client is fixed to the runner region, so a different requested
// region is refused instead of silently creating elsewhere (G8-B07).
func (r AWSRDSRunner) approvedRegion(params map[string]any) (string, error) {
	region := stringParam(params, "region")
	if region == "" {
		region = r.region
	}
	if region == "" {
		return "", providerError(ProviderAWSRDS, ProviderErrInvalid,
			"region is required", "set provider_params.region or runner region")
	}
	if r.region != "" && region != r.region {
		return "", providerError(ProviderAWSRDS, ProviderErrInvalid,
			fmt.Sprintf("requested region %s differs from runner region %s", region, r.region),
			"configure a runner for the approved region")
	}
	return region, nil
}

// rejectUnsupportedRDSSettings refuses approved settings the runner cannot
// honour rather than creating something other than what was approved.
func rejectUnsupportedRDSSettings(params map[string]any, public bool) error {
	if boolParamAny(params, "deletion_protection") {
		return providerError(ProviderAWSRDS, ProviderErrInvalid,
			"deletion_protection is not supported for pg_sage-managed RDS",
			"pg_sage owns TTL teardown; remove deletion_protection")
	}
	if boolParamAny(params, "private_network") && public {
		return providerError(ProviderAWSRDS, ProviderErrInvalid,
			"private_network conflicts with publicly_accessible", "")
	}
	return nil
}

func optionalString(value string) *string {
	if value == "" {
		return nil
	}
	return aws.String(value)
}

// rdsFinalSnapshotIdentifier is unique per destroy so a re-created
// deployment never collides with an earlier final snapshot (G8-B22).
func rdsFinalSnapshotIdentifier(identifier string, now time.Time) string {
	suffix := "-final-" + now.UTC().Format("20060102150405")
	if len(identifier)+len(suffix) > 255 {
		identifier = identifier[:255-len(suffix)]
	}
	return identifier + suffix
}

// createFailureStatus distinguishes a definitive provider rejection (safe to
// retry) from an outcome that may have created a billed resource (G8-B06).
func createFailureStatus(err error) string {
	switch providerErrorKind(err) {
	case ProviderErrInvalid, ProviderErrQuota, ProviderErrPermission, ProviderErrThrottle:
		return "failed"
	default:
		return "create_uncertain"
	}
}

func providerErrorKind(err error) ProviderErrorKind {
	var pe ProviderError
	if errors.As(err, &pe) {
		return pe.Kind
	}
	return ""
}

func errRecordedIDRequired(provider string) error {
	return providerError(provider, ProviderErrInvalid,
		"recorded provider resource id is required",
		"pg_sage never targets a name derived from the deployment id")
}

// statusIdentifier returns the recorded id, or the derived name only for an
// uncertain create whose ownership will be verified by tag.
func statusIdentifier(provider string, dep Deployment) (string, error) {
	if dep.ProviderResourceID != "" {
		return dep.ProviderResourceID, nil
	}
	if dep.ProvisioningStatus != "create_uncertain" && dep.CreateOperationID == "" {
		return "", errRecordedIDRequired(provider)
	}
	return ProviderResourceName(provider, dep.DeploymentID)
}

func verifyOwnershipTags(provider string, tags map[string]string, dep Deployment) error {
	want := dep.DeploymentID
	if provider == ProviderGCPCloudSQL {
		want = gcpLabelValue(want)
	}
	if tags["pg_sage_deployment_id"] != want {
		return providerError(provider, ProviderErrPermission,
			"resource ownership tag does not match deployment",
			"refusing to act on a resource pg_sage did not create for this deployment")
	}
	return nil
}
