package agentdb

import (
	"context"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/rds"
	"github.com/aws/aws-sdk-go-v2/service/rds/types"
)

type RDSCreateInput struct {
	Identifier         string
	Class              string
	StorageGB          int32
	BackupRetention    int32
	Region             string
	PubliclyAccessible bool
	StorageEncrypted   bool
	DeletionProtection bool
	Tags               map[string]string
}

type RDSInstance struct {
	Identifier string
	Status     string
	Endpoint   string
	SecretARN  string
}

type AWSRDSClient interface {
	CreateInstance(ctx context.Context, input RDSCreateInput) (RDSInstance, error)
	GetInstance(ctx context.Context, identifier string) (RDSInstance, error)
	DeleteInstance(ctx context.Context, identifier string, skipFinalSnapshot bool) error
}

type AWSRDSRunner struct {
	client AWSRDSClient
	region string
	now    func() time.Time
}

func NewAWSRDSRunner(client AWSRDSClient, region string) AWSRDSRunner {
	return AWSRDSRunner{
		client: client,
		region: region,
		now:    time.Now,
	}
}

func NewAWSRDSRunnerFromDefaultConfig(
	ctx context.Context,
	region string,
) (AWSRDSRunner, error) {
	opts := []func(*awsconfig.LoadOptions) error{}
	if region != "" {
		opts = append(opts, awsconfig.WithRegion(region))
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return AWSRDSRunner{}, err
	}
	return NewAWSRDSRunner(awsRDSSDKClient{client: rds.NewFromConfig(cfg)}, region), nil
}

func (r AWSRDSRunner) Name() string { return "aws_rds_api" }

func (r AWSRDSRunner) Provider() string { return ProviderAWSRDS }

func (r AWSRDSRunner) Preflight(
	_ context.Context,
	req ProvisionRequest,
) ProvisionResult {
	input, err := r.createInput(req)
	if err != nil {
		return ProvisionResult{Status: "preflight_failed", Error: err}
	}
	return ProvisionResult{
		Status:             "preflight_passed",
		ProviderResourceID: input.Identifier,
		Detail: map[string]any{
			"region":              input.Region,
			"db_instance_class":   input.Class,
			"allocated_storage":   input.StorageGB,
			"backup_retention":    input.BackupRetention,
			"publicly_accessible": input.PubliclyAccessible,
		},
	}
}

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
		return ProvisionResult{Status: "failed", Error: mapAWSError(err)}
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

func (r AWSRDSRunner) Status(
	ctx context.Context,
	req ProvisionRequest,
) ProvisionResult {
	identifier := req.Deployment.ProviderResourceID
	if identifier == "" {
		identifier, _ = ProviderResourceName(ProviderAWSRDS, req.Deployment.DeploymentID)
	}
	instance, err := r.client.GetInstance(ctx, identifier)
	if err != nil {
		return ProvisionResult{Status: "status_unknown", Error: mapAWSError(err)}
	}
	return rdsProvisionResult(instance)
}

func (r AWSRDSRunner) Destroy(
	ctx context.Context,
	req ProvisionRequest,
) ProvisionResult {
	identifier := req.Deployment.ProviderResourceID
	if identifier == "" {
		identifier, _ = ProviderResourceName(ProviderAWSRDS, req.Deployment.DeploymentID)
	}
	skipSnapshot := isDisposable(req.Deployment)
	err := r.client.DeleteInstance(ctx, identifier, skipSnapshot)
	if err != nil {
		mapped := mapAWSError(err)
		if pe, ok := mapped.(ProviderError); ok && pe.Kind == ProviderErrNotFound {
			return ProvisionResult{Status: "destroyed"}
		}
		return ProvisionResult{Status: "failed", Error: mapped}
	}
	return ProvisionResult{
		Status:             "destroying",
		ProviderResourceID: identifier,
		Detail:             map[string]any{"skip_final_snapshot": skipSnapshot},
	}
}

func (r AWSRDSRunner) BackupCheck(
	ctx context.Context,
	req ProvisionRequest,
) ProvisionResult {
	return r.Status(ctx, req)
}

func (r AWSRDSRunner) createInput(req ProvisionRequest) (RDSCreateInput, error) {
	if r.client == nil {
		return RDSCreateInput{}, providerError(
			ProviderAWSRDS, ProviderErrUnavailable, "aws rds client is not configured",
			"configure AWS SDK credentials and region",
		)
	}
	identifier, err := ProviderResourceName(ProviderAWSRDS, req.Deployment.DeploymentID)
	if err != nil {
		return RDSCreateInput{}, err
	}
	params := providerParams(req.Deployment)
	class := stringParam(params, "db_instance_class")
	if class == "" {
		class = "db.t4g.micro"
	}
	storage := int32(float64Param(params, "allocated_storage"))
	if storage <= 0 {
		storage = 20
	}
	backup := int32(float64Param(params, "backup_retention_days"))
	if backup <= 0 {
		backup = 7
	}
	region := stringParam(params, "region")
	if region == "" {
		region = r.region
	}
	if region == "" {
		return RDSCreateInput{}, providerError(
			ProviderAWSRDS, ProviderErrInvalid, "region is required",
			"set provider_params.region or runner region",
		)
	}
	public := boolParamAny(params, "publicly_accessible")
	if public && !req.Policy.AllowPublicIP {
		return RDSCreateInput{}, providerError(
			ProviderAWSRDS, ProviderErrInvalid, "public rds instances are denied",
			"enable allow_public_ip only for approved test networks",
		)
	}
	return RDSCreateInput{
		Identifier:         identifier,
		Class:              class,
		StorageGB:          storage,
		BackupRetention:    backup,
		Region:             region,
		PubliclyAccessible: public,
		StorageEncrypted:   true,
		DeletionProtection: false,
		Tags: map[string]string{
			"app":                   "pg-sage",
			"pg_sage_deployment_id": req.Deployment.DeploymentID,
			"ttl":                   ttlTag(req.Deployment),
		},
	}, nil
}

func providerParams(dep Deployment) map[string]any {
	if dep.Metadata == nil {
		return map[string]any{}
	}
	if params, ok := dep.Metadata["provider_params"].(map[string]any); ok {
		return params
	}
	return map[string]any{}
}

func isDisposable(dep Deployment) bool {
	if dep.Metadata == nil {
		return false
	}
	value, _ := dep.Metadata["disposable"].(bool)
	return value
}

func ttlTag(dep Deployment) string {
	if dep.LeaseExpiresAt == nil {
		return ""
	}
	return dep.LeaseExpiresAt.UTC().Format(time.RFC3339)
}

func boolParamAny(params map[string]any, key string) bool {
	if params == nil {
		return false
	}
	if value, ok := params[key].(bool); ok {
		return value
	}
	if value, ok := params[key].(string); ok {
		return strings.EqualFold(value, "true")
	}
	return false
}

func mapAWSError(err error) error {
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "dbinstancealreadyexists"):
		return providerError(ProviderAWSRDS, ProviderErrConflict, err.Error(), "")
	case strings.Contains(msg, "throttl"):
		return providerError(ProviderAWSRDS, ProviderErrThrottle, err.Error(), "retry later")
	case strings.Contains(msg, "instancequotaexceeded"):
		return providerError(ProviderAWSRDS, ProviderErrQuota, err.Error(), "request quota")
	case strings.Contains(msg, "notfound"):
		return providerError(ProviderAWSRDS, ProviderErrNotFound, err.Error(), "")
	case strings.Contains(msg, "invalidparametervalue"):
		return providerError(ProviderAWSRDS, ProviderErrInvalid, err.Error(), "")
	default:
		return providerError(ProviderAWSRDS, ProviderErrUnavailable, err.Error(), "")
	}
}

type awsRDSSDKClient struct {
	client *rds.Client
}

func (c awsRDSSDKClient) CreateInstance(
	ctx context.Context,
	input RDSCreateInput,
) (RDSInstance, error) {
	out, err := c.client.CreateDBInstance(ctx, &rds.CreateDBInstanceInput{
		AllocatedStorage:         aws.Int32(input.StorageGB),
		AutoMinorVersionUpgrade:  aws.Bool(true),
		BackupRetentionPeriod:    aws.Int32(input.BackupRetention),
		DBInstanceClass:          aws.String(input.Class),
		DBInstanceIdentifier:     aws.String(input.Identifier),
		DeletionProtection:       aws.Bool(input.DeletionProtection),
		Engine:                   aws.String("postgres"),
		ManageMasterUserPassword: aws.Bool(true),
		MasterUsername:           aws.String("postgres"),
		PubliclyAccessible:       aws.Bool(input.PubliclyAccessible),
		StorageEncrypted:         aws.Bool(input.StorageEncrypted),
		Tags:                     awsRDSTags(input.Tags),
	})
	if err != nil {
		return RDSInstance{}, err
	}
	return sdkRDSInstance(out.DBInstance), nil
}

func (c awsRDSSDKClient) GetInstance(
	ctx context.Context,
	identifier string,
) (RDSInstance, error) {
	out, err := c.client.DescribeDBInstances(ctx, &rds.DescribeDBInstancesInput{
		DBInstanceIdentifier: aws.String(identifier),
	})
	if err != nil {
		return RDSInstance{}, err
	}
	if len(out.DBInstances) == 0 {
		return RDSInstance{}, providerError(
			ProviderAWSRDS, ProviderErrNotFound, "db instance not found", "",
		)
	}
	return sdkRDSInstance(&out.DBInstances[0]), nil
}

func (c awsRDSSDKClient) DeleteInstance(
	ctx context.Context,
	identifier string,
	skipFinalSnapshot bool,
) error {
	input := &rds.DeleteDBInstanceInput{
		DBInstanceIdentifier: aws.String(identifier),
		SkipFinalSnapshot:    aws.Bool(skipFinalSnapshot),
	}
	if !skipFinalSnapshot {
		input.FinalDBSnapshotIdentifier = aws.String(identifier + "-final")
	}
	_, err := c.client.DeleteDBInstance(ctx, input)
	return err
}

func awsRDSTags(tags map[string]string) []types.Tag {
	out := make([]types.Tag, 0, len(tags))
	for key, value := range tags {
		out = append(out, types.Tag{Key: aws.String(key), Value: aws.String(value)})
	}
	return out
}

func sdkRDSInstance(instance *types.DBInstance) RDSInstance {
	if instance == nil {
		return RDSInstance{}
	}
	out := RDSInstance{
		Identifier: aws.ToString(instance.DBInstanceIdentifier),
		Status:     aws.ToString(instance.DBInstanceStatus),
	}
	if instance.Endpoint != nil {
		out.Endpoint = aws.ToString(instance.Endpoint.Address)
	}
	if instance.MasterUserSecret != nil {
		out.SecretARN = aws.ToString(instance.MasterUserSecret.SecretArn)
	}
	return out
}

func rdsProvisionResult(instance RDSInstance) ProvisionResult {
	status := "status_unknown"
	switch instance.Status {
	case "available":
		status = "available"
	case "creating", "modifying", "backing-up":
		status = "provisioning"
	case "deleting":
		status = "destroying"
	}
	return ProvisionResult{
		Status:             status,
		ProviderResourceID: instance.Identifier,
		SecretRef:          instance.SecretARN,
		SecretRefProvider:  secretProviderIfPresent(instance.SecretARN, "aws_secrets_manager"),
		ConnectionInfo: map[string]any{
			"endpoint":             instance.Endpoint,
			"provider_resource_id": instance.Identifier,
			"secret_ref":           instance.SecretARN,
			"secret_ref_provider":  secretProviderIfPresent(instance.SecretARN, "aws_secrets_manager"),
		},
		Detail: map[string]any{"aws_status": instance.Status},
	}
}

func secretProviderIfPresent(secretRef string, provider string) string {
	if secretRef == "" {
		return ""
	}
	return provider
}
