//go:build cloudlive

package cloudtel

import (
	"context"
	"os"
	"testing"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
)

// Owner-run live checks (never in CI): build tag cloudlive plus the
// SAGE_TEST_* variables naming a disposable instance. They only read
// metrics, instance metadata and parameters; nothing is modified. See
// docs/managed-clouds.md for the IAM permissions and the command.

func TestLiveAWSTelemetry(t *testing.T) {
	region, id := os.Getenv("SAGE_TEST_AWS_REGION"), os.Getenv("SAGE_TEST_RDS_INSTANCE")
	if region == "" || id == "" {
		t.Fatal("set SAGE_TEST_AWS_REGION and SAGE_TEST_RDS_INSTANCE (and AWS credentials)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(region))
	if err != nil {
		t.Fatal(err)
	}
	opts := AWSOptions{Region: region, Credentials: cfg.Credentials}
	if os.Getenv("SAGE_TEST_RDS_IS_CLUSTER") == "1" {
		opts.ClusterID = id
	} else {
		opts.InstanceID = id
	}
	src, err := NewAWSSource(opts)
	if err != nil {
		t.Fatal(err)
	}
	s, err := src.Collect(ctx, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if s.CPUPct == nil || s.MemoryTotalBytes == nil {
		t.Fatalf("live sample lacks CPU or memory: %+v", s)
	}
	t.Logf("CHECK-CLOUD-01: PASS %s cpu=%.1f%% mem=%.0f (%s) missing=%v", s.Resource,
		s.CPUPct.Value, s.MemoryTotalBytes.Value, s.MemoryTotalSource, s.Missing)
	target, err := src.Target(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if target.ParameterGroup == "" || len(target.Params) == 0 {
		t.Fatalf("live target lacks the parameter group: %+v", target)
	}
	t.Logf("CHECK-CLOUD-02: PASS parameter group %s (%d parameters, status %s)",
		target.ParameterGroup, len(target.Params), target.ParameterGroupStatus)
}

func TestLiveGCPTelemetry(t *testing.T) {
	project, instance := os.Getenv("SAGE_TEST_GCP_PROJECT"),
		os.Getenv("SAGE_TEST_CLOUDSQL_INSTANCE")
	if project == "" || instance == "" {
		t.Fatal("set SAGE_TEST_GCP_PROJECT and SAGE_TEST_CLOUDSQL_INSTANCE (and ADC)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	token, err := NewGCPTokenSource(ctx, GCPAuthOptions{
		CredentialsFile: os.Getenv("GOOGLE_APPLICATION_CREDENTIALS")})
	if err != nil {
		t.Fatal(err)
	}
	src, err := NewGCPSource(GCPOptions{Project: project, Instance: instance, Token: token})
	if err != nil {
		t.Fatal(err)
	}
	s, err := src.Collect(ctx, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if s.CPUPct == nil || s.MemoryTotalBytes == nil {
		t.Fatalf("live sample lacks CPU or memory: %+v", s)
	}
	t.Logf("CHECK-CLOUD-03: PASS %s cpu=%.1f%% mem=%.0f missing=%v", s.Resource,
		s.CPUPct.Value, s.MemoryTotalBytes.Value, s.Missing)
	target, err := src.Target(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("CHECK-CLOUD-04: PASS %d database flags read (known=%t)", len(target.Flags),
		target.FlagsKnown)
}
