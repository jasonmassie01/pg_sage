package clone_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/rds"
	rdstypes "github.com/aws/aws-sdk-go-v2/service/rds/types"

	"github.com/pg-sage/sidecar/internal/clone"
)

// lab-05c: the RDS SDK adapter sends copy-on-write restores that never
// keep a final snapshot, and maps SDK faults to the clone package's.

type fakeRDS struct {
	restore  *rds.RestoreDBClusterToPointInTimeInput
	create   *rds.CreateDBInstanceInput
	delInst  *rds.DeleteDBInstanceInput
	delClust *rds.DeleteDBClusterInput
	markers  []string
	pages    [][]rdstypes.DBCluster
	err      error
}

func (f *fakeRDS) DescribeDBClusters(_ context.Context, in *rds.DescribeDBClustersInput,
	_ ...func(*rds.Options)) (*rds.DescribeDBClustersOutput, error) {
	if f.err != nil {
		return nil, f.err
	}
	if in.DBClusterIdentifier != nil {
		return &rds.DescribeDBClustersOutput{DBClusters: f.pages[0][:1]}, nil
	}
	f.markers = append(f.markers, aws.ToString(in.Marker))
	page := len(f.markers) - 1
	out := &rds.DescribeDBClustersOutput{DBClusters: f.pages[page]}
	if page+1 < len(f.pages) {
		out.Marker = aws.String(fmt.Sprintf("m%d", page+1))
	}
	return out, nil
}

func (f *fakeRDS) DescribeDBInstances(context.Context, *rds.DescribeDBInstancesInput,
	...func(*rds.Options)) (*rds.DescribeDBInstancesOutput, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &rds.DescribeDBInstancesOutput{DBInstances: []rdstypes.DBInstance{{
		DBInstanceIdentifier: aws.String("shop-1"), DBClusterIdentifier: aws.String("shop"),
		DBInstanceClass: aws.String("db.r6g.large"), DBInstanceStatus: aws.String("available"),
		PubliclyAccessible: aws.Bool(true), DBParameterGroups: []rdstypes.DBParameterGroupStatus{
			{DBParameterGroupName: aws.String("shop-params")}}}}}, nil
}

func (f *fakeRDS) RestoreDBClusterToPointInTime(_ context.Context,
	in *rds.RestoreDBClusterToPointInTimeInput,
	_ ...func(*rds.Options)) (*rds.RestoreDBClusterToPointInTimeOutput, error) {
	f.restore = in
	if f.err != nil {
		return nil, f.err
	}
	return &rds.RestoreDBClusterToPointInTimeOutput{DBCluster: &rdstypes.DBCluster{
		DBClusterIdentifier: in.DBClusterIdentifier, Status: aws.String("creating")}}, nil
}

func (f *fakeRDS) CreateDBInstance(_ context.Context, in *rds.CreateDBInstanceInput,
	_ ...func(*rds.Options)) (*rds.CreateDBInstanceOutput, error) {
	f.create = in
	if f.err != nil {
		return nil, f.err
	}
	return &rds.CreateDBInstanceOutput{DBInstance: &rdstypes.DBInstance{
		DBInstanceIdentifier: in.DBInstanceIdentifier, DBInstanceStatus: aws.String("creating"),
		DBClusterIdentifier: in.DBClusterIdentifier}}, nil
}

func (f *fakeRDS) DeleteDBInstance(_ context.Context, in *rds.DeleteDBInstanceInput,
	_ ...func(*rds.Options)) (*rds.DeleteDBInstanceOutput, error) {
	f.delInst = in
	return &rds.DeleteDBInstanceOutput{}, f.err
}

func (f *fakeRDS) DeleteDBCluster(_ context.Context, in *rds.DeleteDBClusterInput,
	_ ...func(*rds.Options)) (*rds.DeleteDBClusterOutput, error) {
	f.delClust = in
	return &rds.DeleteDBClusterOutput{}, f.err
}

func sdkCluster(id, group string, tags map[string]string) rdstypes.DBCluster {
	c := rdstypes.DBCluster{DBClusterIdentifier: aws.String(id),
		Engine: aws.String("aurora-postgresql"), Status: aws.String("available"),
		Endpoint: aws.String(id + ".cluster-abc.us-east-1.rds.amazonaws.com"),
		Port:     aws.Int32(5432), CloneGroupId: aws.String(group),
		DBSubnetGroup: aws.String("subnets"), DBClusterParameterGroup: aws.String("cpg"),
		VpcSecurityGroups: []rdstypes.VpcSecurityGroupMembership{
			{VpcSecurityGroupId: aws.String("sg-1")}},
		DBClusterMembers: []rdstypes.DBClusterMember{{DBInstanceIdentifier: aws.String(
			id + "-1"), IsClusterWriter: aws.Bool(true)}},
		ServerlessV2ScalingConfiguration: &rdstypes.ServerlessV2ScalingConfigurationInfo{
			MinCapacity: aws.Float64(0.5), MaxCapacity: aws.Float64(4)}}
	for k, v := range tags {
		c.TagList = append(c.TagList, rdstypes.Tag{Key: aws.String(k), Value: aws.String(v)})
	}
	return c
}

func TestAuroraSDK_RestoreIsCopyOnWriteFromTheLatestTime(t *testing.T) {
	f := &fakeRDS{}
	api := clone.NewAuroraSDKAPI(f)
	_, err := api.RestoreCluster(context.Background(), clone.AuroraRestoreRequest{
		ID: auroraName, Source: "shop", SubnetGroup: "subnets", ParameterGroup: "cpg",
		SecurityGroups: []string{"sg-1"}, Serverless: &clone.AuroraScaling{Min: 0.5, Max: 4},
		Tags: map[string]string{clone.AuroraReceiptTag: auroraName}})
	if err != nil {
		t.Fatal(err)
	}
	in := f.restore
	if aws.ToString(in.RestoreType) != "copy-on-write" || !aws.ToBool(in.UseLatestRestorableTime) ||
		in.DeletionProtection == nil || aws.ToBool(in.DeletionProtection) ||
		aws.ToString(in.DBClusterIdentifier) != auroraName ||
		aws.ToString(in.SourceDBClusterIdentifier) != "shop" ||
		aws.ToString(in.DBSubnetGroupName) != "subnets" ||
		aws.ToString(in.DBClusterParameterGroupName) != "cpg" ||
		strings.Join(in.VpcSecurityGroupIds, ",") != "sg-1" || in.RestoreToTime != nil {
		t.Fatalf("restore input = %+v", in)
	}
	if s := in.ServerlessV2ScalingConfiguration; s == nil || aws.ToFloat64(s.MinCapacity) != 0.5 ||
		aws.ToFloat64(s.MaxCapacity) != 4 {
		t.Fatalf("serverless = %+v", s)
	}
	if len(in.Tags) != 1 || aws.ToString(in.Tags[0].Key) != clone.AuroraReceiptTag ||
		aws.ToString(in.Tags[0].Value) != auroraName {
		t.Fatalf("tags = %+v", in.Tags)
	}
}

func TestAuroraSDK_InstanceAndDeletes(t *testing.T) {
	f := &fakeRDS{}
	api := clone.NewAuroraSDKAPI(f)
	in, err := api.CreateInstance(context.Background(), clone.AuroraInstanceRequest{
		ID: auroraName + "-1", ClusterID: auroraName, Class: "db.r6g.large",
		ParameterGroup: "pg", PubliclyAccessible: false,
		Tags: map[string]string{clone.AuroraReceiptTag: auroraName}})
	if err != nil || in.ID != auroraName+"-1" || in.Status != "creating" {
		t.Fatalf("instance = %+v, %v", in, err)
	}
	c := f.create
	if aws.ToString(c.Engine) != "aurora-postgresql" || aws.ToString(c.DBInstanceClass) !=
		"db.r6g.large" || aws.ToString(c.DBClusterIdentifier) != auroraName ||
		aws.ToString(c.DBParameterGroupName) != "pg" || c.PubliclyAccessible == nil ||
		aws.ToBool(c.PubliclyAccessible) || len(c.Tags) != 1 {
		t.Fatalf("create input = %+v", c)
	}
	if err := api.DeleteInstance(context.Background(), auroraName+"-1"); err != nil ||
		aws.ToString(f.delInst.DBInstanceIdentifier) != auroraName+"-1" {
		t.Fatalf("delete instance = %+v, %v", f.delInst, err)
	}
	if err := api.DeleteCluster(context.Background(), auroraName); err != nil {
		t.Fatal(err)
	}
	d := f.delClust
	if aws.ToString(d.DBClusterIdentifier) != auroraName || !aws.ToBool(d.SkipFinalSnapshot) ||
		!aws.ToBool(d.DeleteAutomatedBackups) || d.FinalDBSnapshotIdentifier != nil {
		t.Fatalf("delete cluster input = %+v", d)
	}
}

func TestAuroraSDK_DescribeAndListMapTheCluster(t *testing.T) {
	f := &fakeRDS{pages: [][]rdstypes.DBCluster{
		{sdkCluster("shop", "cg", nil), sdkCluster("a", "cg", nil)},
		{sdkCluster(auroraName, "cg", map[string]string{clone.AuroraReceiptTag: auroraName})}}}
	api := clone.NewAuroraSDKAPI(f)
	c, err := api.DescribeCluster(context.Background(), "shop")
	if err != nil || c.ID != "shop" || c.Engine != "aurora-postgresql" || c.Port != 5432 ||
		c.CloneGroupID != "cg" || c.SubnetGroup != "subnets" || c.ParameterGroup != "cpg" ||
		len(c.SecurityGroups) != 1 || len(c.Members) != 1 || !c.Members[0].Writer ||
		c.Serverless == nil || c.Serverless.Max != 4 {
		t.Fatalf("cluster = %+v, %v", c, err)
	}
	all, err := api.ListClusters(context.Background())
	if err != nil || len(all) != 3 || all[2].Tags[clone.AuroraReceiptTag] != auroraName {
		t.Fatalf("list = %+v, %v", all, err)
	}
	if strings.Join(f.markers, ",") != ",m1" {
		t.Fatalf("markers = %q", f.markers)
	}
	inst, err := api.DescribeInstance(context.Background(), "shop-1")
	if err != nil || inst.Class != "db.r6g.large" || inst.ParameterGroup != "shop-params" ||
		!inst.PubliclyAccessible || inst.ClusterID != "shop" {
		t.Fatalf("instance = %+v, %v", inst, err)
	}
}

type fakeAPIError struct{ code, msg string }

func (e fakeAPIError) Error() string        { return e.code + ": " + e.msg }
func (e fakeAPIError) ErrorCode() string    { return e.code }
func (e fakeAPIError) ErrorMessage() string { return e.msg }

func TestAuroraSDK_ErrorsMapToTheCloneErrors(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		notFound bool
		status   int
	}{
		{"cluster not found", &rdstypes.DBClusterNotFoundFault{Message: aws.String("x")},
			true, 0},
		{"instance not found", &rdstypes.DBInstanceNotFoundFault{Message: aws.String("x")},
			true, 0},
		{"throttling", fakeAPIError{"Throttling", "Rate exceeded"}, false, 429},
		{"access denied", fakeAPIError{"AccessDenied", "not authorized to " +
			"rds:RestoreDBClusterToPointInTime"}, false, 403},
		{"quota", fakeAPIError{"InstanceQuotaExceeded", "quota"}, false, 400},
	}
	for _, tc := range cases {
		api := clone.NewAuroraSDKAPI(&fakeRDS{err: tc.err})
		_, err := api.DescribeCluster(context.Background(), "shop")
		if errors.Is(err, clone.ErrAuroraNotFound) != tc.notFound {
			t.Errorf("%s: err = %v, not found want %v", tc.name, err, tc.notFound)
		}
		var status *clone.ProviderStatusError
		if tc.status != 0 && (!errors.As(err, &status) || status.StatusCode != tc.status) {
			t.Errorf("%s: err = %v, want status %d", tc.name, err, tc.status)
		}
		if tc.name == "access denied" && !strings.Contains(err.Error(),
			"rds:RestoreDBClusterToPointInTime") {
			t.Errorf("access denied does not name the permission: %v", err)
		}
	}
	creds := errors.New("operation error RDS: DescribeDBClusters, failed to retrieve " +
		"credentials: no EC2 IMDS role found")
	_, err := clone.NewAuroraSDKAPI(&fakeRDS{err: creds}).ListClusters(context.Background())
	if !errors.Is(err, clone.ErrCloneUnavailable) || !strings.Contains(err.Error(),
		"AWS credentials") {
		t.Fatalf("credentials err = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = clone.NewAuroraSDKAPI(&fakeRDS{err: context.Canceled}).DescribeCluster(ctx, "x")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel err = %v", err)
	}
}
