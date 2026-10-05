package cloudtel

import (
	"context"
	"errors"
	"testing"
)

// AWSSource.Target resolves the parameter group behind the instance and
// its parameters (paged), so a managed proposal names the exact group and
// knows whether a parameter is static (reboot) or dynamic.
func TestAWSTargetReadsParameterGroup(t *testing.T) {
	f := rdsFixture(t, true)
	f.instances["orders"] = instanceXML("orders", "db.r6g.large", "postgres",
		"orders-pg16", "pending-reboot", 100, 0, true, "")
	f.params["orders-pg16"] = []fakeParam{
		{"shared_buffers", "{DBInstanceClassMemory/32768}", "engine-default", "static", true},
		{"work_mem", "65536", "user", "dynamic", true},
		{"max_connections", "400", "user", "static", true},
		{"rds.force_ssl", "1", "system", "dynamic", false},
		{"log_min_duration_statement", "", "engine-default", "dynamic", true},
	}
	target, err := f.source(t, AWSOptions{InstanceID: "orders"}).Target(context.Background())
	if err != nil {
		t.Fatalf("Target: %v", err)
	}
	if target.Provider != "rds" || target.Region != "us-east-1" || target.InstanceID != "orders" ||
		target.ParameterGroup != "orders-pg16" || target.ParameterGroupIsDefault ||
		target.ParameterGroupStatus != "pending-reboot" {
		t.Fatalf("target = %+v", target)
	}
	if len(target.Params) != 5 {
		t.Fatalf("params = %d, want all 5 across 3 pages", len(target.Params))
	}
	wm := target.Params["work_mem"]
	if wm.Value != "65536" || wm.Source != "user" || wm.ApplyType != "dynamic" || !wm.Modifiable {
		t.Fatalf("work_mem = %+v", wm)
	}
	if sb := target.Params["shared_buffers"]; sb.ApplyType != "static" ||
		sb.Value != "{DBInstanceClassMemory/32768}" {
		t.Fatalf("shared_buffers = %+v", sb)
	}
	if f.called("rds:DescribeDBParameters") != 3 {
		t.Fatalf("DescribeDBParameters calls = %d, want 3 pages",
			f.called("rds:DescribeDBParameters"))
	}
}

func TestAWSTargetDefaultGroupIsFlagged(t *testing.T) {
	f := rdsFixture(t, true)
	f.instances["orders"] = instanceXML("orders", "db.r6g.large", "postgres",
		"default.postgres16", "in-sync", 100, 0, true, "")
	target, err := f.source(t, AWSOptions{InstanceID: "orders"}).Target(context.Background())
	if err != nil {
		t.Fatalf("Target: %v", err)
	}
	if !target.ParameterGroupIsDefault {
		t.Fatalf("default.postgres16 not flagged as a default group: %+v", target)
	}
}

func TestAWSTargetErrors(t *testing.T) {
	f := rdsFixture(t, true)
	f.fail["rds:DescribeDBParameters"] = fakeFailure{status: 403, code: "AccessDenied",
		message: "rds:DescribeDBParameters denied"}
	_, err := f.source(t, AWSOptions{InstanceID: "orders"}).Target(context.Background())
	if !errors.Is(err, ErrAuth) {
		t.Fatalf("err = %v, want ErrAuth", err)
	}
	g := rdsFixture(t, true)
	_, err = g.source(t, AWSOptions{InstanceID: "orders", Credentials: failingCreds()}).
		Target(context.Background())
	if !errors.Is(err, ErrNoCredentials) {
		t.Fatalf("err = %v, want ErrNoCredentials", err)
	}
}
