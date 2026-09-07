package agentdb

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestAWSRDSRunnerCreateStatusDestroy(t *testing.T) {
	client := &fakeRDSClient{}
	runner := NewAWSRDSRunner(client, "us-east-1")
	req := awsRunnerRequest("12345678-abcd")

	preflight := runner.Preflight(context.Background(), req)
	if preflight.Status != "preflight_passed" ||
		preflight.ProviderResourceID != "pgsage-12345678-abcd" {
		t.Fatalf("preflight = %#v", preflight)
	}
	created := runner.Create(context.Background(), req)
	if created.Status != "available" ||
		created.ProviderResourceID != "pgsage-12345678-abcd" {
		t.Fatalf("created = %#v", created)
	}
	if !client.created.StorageEncrypted || client.created.BackupRetention != 7 {
		t.Fatalf("create input missing safety settings: %#v", client.created)
	}
	if client.created.DeletionProtection {
		t.Fatalf("pg_sage-created disposable RDS should remain cleanup-safe: %#v",
			client.created)
	}
	if created.SecretRef != "arn:aws:secretsmanager:us-east-1:123:secret:rds" ||
		created.SecretRefProvider != "aws_secrets_manager" {
		t.Fatalf("created secret ref = %#v", created)
	}
	status := runner.Status(context.Background(), req)
	if status.Status != "available" {
		t.Fatalf("status = %#v", status)
	}
	destroy := runner.Destroy(context.Background(), req)
	if destroy.Status != "destroying" || !client.skipFinalSnapshot {
		t.Fatalf("destroy = %#v skip=%v", destroy, client.skipFinalSnapshot)
	}
}

func TestAWSRDSRunnerMapsProviderErrors(t *testing.T) {
	runner := NewAWSRDSRunner(
		&fakeRDSClient{err: errors.New("Throttling: slow down")},
		"us-east-1",
	)
	result := runner.Create(context.Background(), awsRunnerRequest("adb_err"))
	if result.Error == nil {
		t.Fatal("expected provider error")
	}
	if mapped := publicProviderError(result.Error); !errors.Is(mapped, ErrRateLimited) {
		t.Fatalf("mapped error = %v, want ErrRateLimited", mapped)
	}
	runner = NewAWSRDSRunner(
		&fakeRDSClient{err: errors.New("DBInstanceNotFound")},
		"us-east-1",
	)
	result = runner.Destroy(context.Background(), awsRunnerRequest("adb_missing"))
	if result.Status != "destroyed" {
		t.Fatalf("not found destroy result = %#v", result)
	}
}

func TestAWSRDSRunnerRejectsPublicIPByDefault(t *testing.T) {
	req := awsRunnerRequest("adb_public")
	req.Deployment.Metadata["provider_params"].(map[string]any)["publicly_accessible"] = true
	result := NewAWSRDSRunner(&fakeRDSClient{}, "us-east-1").
		Preflight(context.Background(), req)
	if result.Error == nil {
		t.Fatal("expected public ip rejection")
	}
}

func TestAWSRDSRunnerCreatePreservesProvisioningStatus(t *testing.T) {
	runner := NewAWSRDSRunner(
		&fakeRDSClient{status: "creating"},
		"us-east-1",
	)
	result := runner.Create(context.Background(), awsRunnerRequest("adb_creating"))
	if result.Status != "provisioning" {
		t.Fatalf("create status = %q, want provisioning", result.Status)
	}
}

func awsRunnerRequest(id string) ProvisionRequest {
	expires := time.Now().UTC().Add(time.Hour)
	return ProvisionRequest{
		Operation: ProvisionOpCreate,
		Policy:    LiveProvisionPolicy{AllowedRegions: []string{"us-east-1"}},
		Deployment: Deployment{
			DeploymentID:       id,
			Provider:           ProviderAWSRDS,
			ProviderResourceID: "pgsage-" + resourceName(id),
			LeaseExpiresAt:     &expires,
			Metadata: map[string]any{
				"disposable": true,
				"provider_params": map[string]any{
					"db_instance_class":     "db.t4g.micro",
					"allocated_storage":     20,
					"backup_retention_days": 7,
					"region":                "us-east-1",
				},
			},
		},
	}
}

type fakeRDSClient struct {
	created           RDSCreateInput
	skipFinalSnapshot bool
	status            string
	err               error
}

func (f *fakeRDSClient) CreateInstance(
	_ context.Context,
	input RDSCreateInput,
) (RDSInstance, error) {
	if f.err != nil {
		return RDSInstance{}, f.err
	}
	f.created = input
	status := f.status
	if status == "" {
		status = "available"
	}
	return RDSInstance{
		Identifier: input.Identifier,
		Status:     status,
		Endpoint:   input.Identifier + ".example",
		SecretARN:  "arn:aws:secretsmanager:us-east-1:123:secret:rds",
	}, nil
}

func (f *fakeRDSClient) GetInstance(
	_ context.Context,
	identifier string,
) (RDSInstance, error) {
	if f.err != nil {
		return RDSInstance{}, f.err
	}
	return RDSInstance{
		Identifier: identifier,
		Status:     "available",
		Endpoint:   identifier + ".example",
	}, nil
}

func (f *fakeRDSClient) DeleteInstance(
	_ context.Context,
	_ string,
	skipFinalSnapshot bool,
) error {
	if f.err != nil {
		return f.err
	}
	f.skipFinalSnapshot = skipFinalSnapshot
	return nil
}
