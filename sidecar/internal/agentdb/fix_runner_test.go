package agentdb

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// ownedRDSClient models DescribeDBInstances returning the resource tags.
type ownedRDSClient struct {
	tags     map[string]string
	getErr   error
	createFn func(RDSCreateInput) (RDSInstance, error)
	created  []RDSCreateInput
	deleted  []string
	gets     []string
}

func (c *ownedRDSClient) CreateInstance(
	_ context.Context, input RDSCreateInput,
) (RDSInstance, error) {
	c.created = append(c.created, input)
	if c.createFn != nil {
		return c.createFn(input)
	}
	return RDSInstance{Identifier: input.Identifier, Status: "creating", Tags: input.Tags}, nil
}

func (c *ownedRDSClient) GetInstance(_ context.Context, id string) (RDSInstance, error) {
	c.gets = append(c.gets, id)
	if c.getErr != nil {
		return RDSInstance{}, c.getErr
	}
	return RDSInstance{Identifier: id, Status: "available", Tags: c.tags}, nil
}

func (c *ownedRDSClient) DeleteInstance(_ context.Context, id string, _ bool) error {
	c.deleted = append(c.deleted, id)
	return nil
}

func ownedTags(id string) map[string]string {
	return map[string]string{"app": "pg-sage", "pg_sage_deployment_id": id}
}

// G8-B04: never destroy by a derived name.
func TestAWSDestroyRefusesEmptyRecordedID(t *testing.T) {
	client := &ownedRDSClient{tags: ownedTags("adb_derived")}
	req := awsRunnerRequest("adb_derived")
	req.Deployment.ProviderResourceID = ""
	result := NewAWSRDSRunner(client, "us-east-1").Destroy(context.Background(), req)
	if result.Error == nil || len(client.deleted) != 0 {
		t.Fatalf("destroy without recorded id: result=%#v deleted=%v", result, client.deleted)
	}
}

// G8-B04: a resource whose ownership tag names another deployment is foreign.
func TestAWSDestroyRefusesTagMismatch(t *testing.T) {
	client := &ownedRDSClient{tags: ownedTags("someone_else")}
	req := awsRunnerRequest("adb_mine")
	result := NewAWSRDSRunner(client, "us-east-1").Destroy(context.Background(), req)
	if result.Error == nil || len(client.deleted) != 0 {
		t.Fatalf("tag mismatch destroy: result=%#v deleted=%v", result, client.deleted)
	}
}

func TestAWSDestroyVerifiesTagsThenDeletes(t *testing.T) {
	client := &ownedRDSClient{tags: ownedTags("adb_mine_ok")}
	req := awsRunnerRequest("adb_mine_ok")
	result := NewAWSRDSRunner(client, "us-east-1").Destroy(context.Background(), req)
	if result.Error != nil || len(client.deleted) != 1 ||
		client.deleted[0] != req.Deployment.ProviderResourceID {
		t.Fatalf("owned destroy: result=%#v deleted=%v", result, client.deleted)
	}
}

// G8-B04/B06: status by derived name is only an adopt-by-tag lookup for an
// uncertain create; otherwise a recorded id is required.
func TestAWSStatusWithoutRecordedIDOnlyAdoptsUncertainCreateByTag(t *testing.T) {
	req := awsRunnerRequest("adb_uncertain")
	req.Deployment.ProviderResourceID = ""
	client := &ownedRDSClient{tags: ownedTags("adb_uncertain")}
	runner := NewAWSRDSRunner(client, "us-east-1")
	if got := runner.Status(context.Background(), req); got.Error == nil {
		t.Fatalf("status without recorded id adopted resource: %#v", got)
	}
	req.Deployment.ProvisioningStatus = "create_uncertain"
	adopted := runner.Status(context.Background(), req)
	if adopted.Error != nil || adopted.ProviderResourceID == "" {
		t.Fatalf("uncertain create not adopted by tag: %#v", adopted)
	}
	client.tags = ownedTags("foreign")
	if got := runner.Status(context.Background(), req); got.Error == nil {
		t.Fatalf("foreign resource adopted: %#v", got)
	}
}

// G8-B07: the create must run in the policy-approved region or be refused.
func TestAWSCreateRefusesRegionMismatch(t *testing.T) {
	client := &ownedRDSClient{}
	req := awsRunnerRequest("adb_region")
	req.Deployment.Metadata["provider_params"].(map[string]any)["region"] = "eu-west-1"
	result := NewAWSRDSRunner(client, "us-east-1").Create(context.Background(), req)
	if result.Error == nil || len(client.created) != 0 {
		t.Fatalf("region mismatch create: result=%#v created=%d", result, len(client.created))
	}
}

// G8-B06: an API error after the request may have been accepted is uncertain.
func TestAWSCreateAmbiguousErrorIsUncertain(t *testing.T) {
	cases := map[string]string{
		"read tcp: connection reset by peer":  "create_uncertain",
		"InvalidParameterValue: bad class":    "failed",
		"InstanceQuotaExceeded: too many dbs": "failed",
	}
	for msg, want := range cases {
		client := &ownedRDSClient{createFn: func(RDSCreateInput) (RDSInstance, error) {
			return RDSInstance{}, errors.New(msg)
		}}
		result := NewAWSRDSRunner(client, "us-east-1").
			Create(context.Background(), awsRunnerRequest("adb_ambiguous"))
		if result.Status != want || result.Error == nil {
			t.Fatalf("%q => status %q, want %q", msg, result.Status, want)
		}
	}
}

// G8-B20: approved settings reach the provider call or the create is refused.
func TestAWSCreateHonorsApprovedSettings(t *testing.T) {
	client := &ownedRDSClient{}
	req := awsRunnerRequest("adb_settings")
	params := req.Deployment.Metadata["provider_params"].(map[string]any)
	params["multi_az"] = true
	params["engine_version"] = "16.4"
	result := NewAWSRDSRunner(client, "us-east-1").Create(context.Background(), req)
	if result.Error != nil || len(client.created) != 1 {
		t.Fatalf("create: %#v", result)
	}
	if !client.created[0].MultiAZ || client.created[0].EngineVersion != "16.4" {
		t.Fatalf("approved settings dropped: %#v", client.created[0])
	}
	params["deletion_protection"] = true
	client2 := &ownedRDSClient{}
	denied := NewAWSRDSRunner(client2, "us-east-1").Create(context.Background(), req)
	if denied.Error == nil || len(client2.created) != 0 {
		t.Fatalf("unsupported deletion_protection silently ignored: %#v", denied)
	}
}

// G8-B04/B09: Cloud SQL destroy verifies labels and lifts deletion
// protection inside the authorized destroy instead of failing forever.
func TestCloudSQLDestroyVerifiesLabelsAndUnprotects(t *testing.T) {
	req := cloudSQLRequest("adb_gcp_owned")
	client := &ownedCloudSQLClient{labels: map[string]string{
		"pg_sage_deployment_id": gcpLabelValue("adb_gcp_owned")}, protected: true}
	result := NewCloudSQLRunner(client, "proj", "us-central1").Destroy(context.Background(), req)
	if result.Error != nil {
		t.Fatalf("destroy: %v", result.Error)
	}
	if strings.Join(client.calls, ",") != "get,unprotect,delete" {
		t.Fatalf("call order = %v", client.calls)
	}
	foreign := &ownedCloudSQLClient{labels: map[string]string{
		"pg_sage_deployment_id": "someone_else"}}
	denied := NewCloudSQLRunner(foreign, "proj", "us-central1").Destroy(context.Background(), req)
	if denied.Error == nil || strings.Contains(strings.Join(foreign.calls, ","), "delete") {
		t.Fatalf("foreign instance destroyed: %#v calls=%v", denied, foreign.calls)
	}
	req.Deployment.ProviderResourceID = ""
	empty := &ownedCloudSQLClient{}
	if got := NewCloudSQLRunner(empty, "proj", "us-central1").
		Destroy(context.Background(), req); got.Error == nil || len(empty.calls) != 0 {
		t.Fatalf("derived-name destroy: %#v calls=%v", got, empty.calls)
	}
}

// G8-B04: Lakebase has no ownership tag surface, so a recorded id is required.
func TestLakebaseDestroyRefusesEmptyRecordedID(t *testing.T) {
	req := lakebaseRequest("adb_lb_derived")
	req.Deployment.ProviderResourceID = ""
	result := NewLakebaseRunner(&fakeLakebaseClient{}, false).
		Destroy(context.Background(), req)
	if result.Error == nil {
		t.Fatalf("lakebase derived-name destroy: %#v", result)
	}
}

// G8-B22: substring "rate" must not classify ordinary failures as throttling.
func TestMapProviderErrorDoesNotTreatRateSubstringAsThrottle(t *testing.T) {
	for _, msg := range []string{"operation failed", "generate plan failed", "migrate"} {
		err := mapProviderError(ProviderGCPCloudSQL, errors.New(msg))
		var pe ProviderError
		if !errors.As(err, &pe) || pe.Kind == ProviderErrThrottle {
			t.Fatalf("%q classified as %v", msg, err)
		}
	}
	err := mapProviderError(ProviderGCPCloudSQL, errors.New("429 rate limit exceeded"))
	var pe ProviderError
	if !errors.As(err, &pe) || pe.Kind != ProviderErrThrottle {
		t.Fatalf("real rate limit classified as %v", err)
	}
}

type ownedCloudSQLClient struct {
	labels    map[string]string
	protected bool
	calls     []string
}

func (c *ownedCloudSQLClient) CreateInstance(
	_ context.Context, in CloudSQLCreateInput,
) (CloudSQLInstance, error) {
	c.calls = append(c.calls, "create")
	return CloudSQLInstance{Name: in.Name, State: "PENDING_CREATE", Labels: in.Labels}, nil
}

func (c *ownedCloudSQLClient) GetInstance(
	_ context.Context, _ string, name string,
) (CloudSQLInstance, error) {
	c.calls = append(c.calls, "get")
	return CloudSQLInstance{Name: name, State: "RUNNABLE", Labels: c.labels,
		DeletionProtection: c.protected}, nil
}

func (c *ownedCloudSQLClient) DeleteInstance(context.Context, string, string) error {
	c.calls = append(c.calls, "delete")
	return nil
}

func (c *ownedCloudSQLClient) SetDeletionProtection(
	_ context.Context, _ string, _ string, enabled bool,
) error {
	if !enabled {
		c.calls = append(c.calls, "unprotect")
	}
	return nil
}
