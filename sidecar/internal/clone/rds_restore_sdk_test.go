package clone_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/rds"
	rdstypes "github.com/aws/aws-sdk-go-v2/service/rds/types"

	"github.com/pg-sage/sidecar/internal/clone"
)

// lab-05d: the RDS SDK adapter sends a point-in-time restore of the latest
// restorable time that is never protected or Multi-AZ, deletes without a
// final snapshot, and maps SDK faults to the clone package's.

type fakeRDSInstances struct {
	restore *rds.RestoreDBInstanceToPointInTimeInput
	del     *rds.DeleteDBInstanceInput
	out     []rdstypes.DBInstance
	err     error
}

func (f *fakeRDSInstances) DescribeDBInstances(_ context.Context,
	_ *rds.DescribeDBInstancesInput, _ ...func(*rds.Options)) (*rds.DescribeDBInstancesOutput,
	error) {
	if f.err != nil {
		return nil, f.err
	}
	return &rds.DescribeDBInstancesOutput{DBInstances: f.out}, nil
}

func (f *fakeRDSInstances) RestoreDBInstanceToPointInTime(_ context.Context,
	in *rds.RestoreDBInstanceToPointInTimeInput,
	_ ...func(*rds.Options)) (*rds.RestoreDBInstanceToPointInTimeOutput, error) {
	f.restore = in
	if f.err != nil {
		return nil, f.err
	}
	return &rds.RestoreDBInstanceToPointInTimeOutput{DBInstance: &rdstypes.DBInstance{
		DBInstanceIdentifier: in.TargetDBInstanceIdentifier,
		DBInstanceStatus:     aws.String("creating")}}, nil
}

func (f *fakeRDSInstances) DeleteDBInstance(_ context.Context, in *rds.DeleteDBInstanceInput,
	_ ...func(*rds.Options)) (*rds.DeleteDBInstanceOutput, error) {
	f.del = in
	if f.err != nil {
		return nil, f.err
	}
	return &rds.DeleteDBInstanceOutput{}, nil
}

func TestRDSRestoreSDK_RestoreIsLatestTimeUnprotectedSingleAZ(t *testing.T) {
	f := &fakeRDSInstances{}
	api := clone.NewRDSRestoreSDKAPI(f)
	got, err := api.RestoreInstance(context.Background(), clone.RDSRestoreRequest{
		ID: rdsName, Source: "shop", Class: "db.m6g.large", SubnetGroup: "shop-subnets",
		ParameterGroup: "shop-params", StorageType: "io1", Iops: 3000,
		SecurityGroups: []string{"sg-0a1"}, PubliclyAccessible: false,
		Tags: map[string]string{clone.AuroraReceiptTag: rdsName, "b": "2"}})
	if err != nil || got.ID != rdsName || got.Status != "creating" {
		t.Fatalf("restore = %+v, %v", got, err)
	}
	in := f.restore
	if aws.ToString(in.SourceDBInstanceIdentifier) != "shop" ||
		aws.ToString(in.TargetDBInstanceIdentifier) != rdsName ||
		!aws.ToBool(in.UseLatestRestorableTime) || in.RestoreTime != nil ||
		aws.ToBool(in.DeletionProtection) || in.DeletionProtection == nil ||
		aws.ToBool(in.MultiAZ) || in.MultiAZ == nil || aws.ToBool(in.PubliclyAccessible) ||
		in.PubliclyAccessible == nil || aws.ToBool(in.CopyTagsToSnapshot) {
		t.Fatalf("restore input is not latest-time, unprotected, single-AZ, private: %+v", in)
	}
	if aws.ToString(in.DBInstanceClass) != "db.m6g.large" ||
		aws.ToString(in.DBSubnetGroupName) != "shop-subnets" ||
		aws.ToString(in.DBParameterGroupName) != "shop-params" ||
		aws.ToString(in.StorageType) != "io1" || aws.ToInt32(in.Iops) != 3000 ||
		strings.Join(in.VpcSecurityGroupIds, ",") != "sg-0a1" {
		t.Fatalf("restore input does not copy the request: %+v", in)
	}
	// sdkTags sends the tags sorted by key: "b" before "pg-sage-receipt".
	if len(in.Tags) != 2 || aws.ToString(in.Tags[1].Key) != clone.AuroraReceiptTag ||
		aws.ToString(in.Tags[1].Value) != rdsName || aws.ToString(in.Tags[0].Key) != "b" {
		t.Fatalf("tags = %+v", in.Tags)
	}
	empty := &fakeRDSInstances{}
	if _, err := clone.NewRDSRestoreSDKAPI(empty).RestoreInstance(context.Background(),
		clone.RDSRestoreRequest{ID: rdsName, Source: "shop"}); err != nil {
		t.Fatal(err)
	}
	if empty.restore.DBInstanceClass != nil || empty.restore.Iops != nil ||
		empty.restore.StorageType != nil || empty.restore.DBSubnetGroupName != nil {
		t.Fatalf("unset fields were sent: %+v", empty.restore)
	}
}

func TestRDSRestoreSDK_DeleteSkipsTheFinalSnapshot(t *testing.T) {
	f := &fakeRDSInstances{}
	if err := clone.NewRDSRestoreSDKAPI(f).DeleteInstance(context.Background(),
		rdsName); err != nil {
		t.Fatal(err)
	}
	if aws.ToString(f.del.DBInstanceIdentifier) != rdsName ||
		!aws.ToBool(f.del.SkipFinalSnapshot) || !aws.ToBool(f.del.DeleteAutomatedBackups) {
		t.Fatalf("delete input = %+v", f.del)
	}
}

func TestRDSRestoreSDK_DescribeMapsTheInstance(t *testing.T) {
	lrt := time.Date(2026, 11, 16, 11, 55, 0, 0, time.UTC)
	f := &fakeRDSInstances{out: []rdstypes.DBInstance{{
		DBInstanceIdentifier: aws.String("shop"), Engine: aws.String("postgres"),
		DBInstanceStatus: aws.String("available"), DBInstanceClass: aws.String("db.m6g.large"),
		Endpoint:      &rdstypes.Endpoint{Address: aws.String(rdsProdHost), Port: aws.Int32(5433)},
		DBSubnetGroup: &rdstypes.DBSubnetGroup{DBSubnetGroupName: aws.String("shop-subnets")},
		DBParameterGroups: []rdstypes.DBParameterGroupStatus{
			{DBParameterGroupName: aws.String("shop-params")}},
		VpcSecurityGroups: []rdstypes.VpcSecurityGroupMembership{
			{VpcSecurityGroupId: aws.String("sg-0a1")}},
		StorageType: aws.String("gp3"), Iops: aws.Int32(12000), AllocatedStorage: aws.Int32(500),
		MultiAZ: aws.Bool(true), DeletionProtection: aws.Bool(true),
		LatestRestorableTime: aws.Time(lrt), TagList: []rdstypes.Tag{
			{Key: aws.String("team"), Value: aws.String("shop")}}}}}
	in, err := clone.NewRDSRestoreSDKAPI(f).DescribeInstance(context.Background(), "shop")
	if err != nil {
		t.Fatal(err)
	}
	if in.ID != "shop" || in.Engine != "postgres" || in.Endpoint != rdsProdHost ||
		in.Port != 5433 || in.Class != "db.m6g.large" || in.SubnetGroup != "shop-subnets" ||
		in.ParameterGroup != "shop-params" || strings.Join(in.SecurityGroups, ",") != "sg-0a1" ||
		in.StorageType != "gp3" || in.Iops != 12000 || in.AllocatedStorageGB != 500 ||
		!in.MultiAZ || !in.DeletionProtection || !in.LatestRestorableTime.Equal(lrt) ||
		in.Tags["team"] != "shop" || in.ClusterID != "" {
		t.Fatalf("instance = %+v", in)
	}
	f.out = nil
	if _, err := clone.NewRDSRestoreSDKAPI(f).DescribeInstance(context.Background(),
		"gone"); !errors.Is(err, clone.ErrRDSNotFound) {
		t.Fatalf("empty describe = %v, want ErrRDSNotFound", err)
	}
}

func TestRDSRestoreSDK_ErrorsMapToTheCloneErrors(t *testing.T) {
	cases := []struct {
		name            string
		err             error
		notFound, exist bool
		status          int
	}{
		{"not found", &rdstypes.DBInstanceNotFoundFault{Message: aws.String("x")}, true,
			false, 0},
		{"exists", &rdstypes.DBInstanceAlreadyExistsFault{Message: aws.String("x")}, false,
			true, 400},
		{"throttling", fakeAPIError{"Throttling", "Rate exceeded"}, false, false, 429},
		{"access denied", fakeAPIError{"AccessDenied", "not authorized to " +
			"rds:RestoreDBInstanceToPointInTime"}, false, false, 403},
		{"no pitr", fakeAPIError{"PointInTimeRestoreNotEnabled", "backups off"}, false,
			false, 400},
	}
	for _, tc := range cases {
		_, err := clone.NewRDSRestoreSDKAPI(&fakeRDSInstances{err: tc.err}).RestoreInstance(
			context.Background(), clone.RDSRestoreRequest{ID: rdsName, Source: "shop"})
		if errors.Is(err, clone.ErrRDSNotFound) != tc.notFound ||
			errors.Is(err, clone.ErrRDSAlreadyExists) != tc.exist {
			t.Errorf("%s: err = %v", tc.name, err)
		}
		var status *clone.ProviderStatusError
		if tc.status != 0 && (!errors.As(err, &status) || status.StatusCode != tc.status) {
			t.Errorf("%s: err = %v, want status %d", tc.name, err, tc.status)
		}
		if err != nil && strings.Contains(err.Error(), "aurora") {
			t.Errorf("%s: an RDS restore error names Aurora: %v", tc.name, err)
		}
	}
	creds := errors.New("operation error RDS: DescribeDBInstances, failed to retrieve " +
		"credentials: no EC2 IMDS role found")
	_, err := clone.NewRDSRestoreSDKAPI(&fakeRDSInstances{err: creds}).DescribeInstance(
		context.Background(), "shop")
	if !errors.Is(err, clone.ErrCloneUnavailable) {
		t.Fatalf("credentials err = %v", err)
	}
}
