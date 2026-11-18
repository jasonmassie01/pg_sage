package clone

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/rds"
	rdstypes "github.com/aws/aws-sdk-go-v2/service/rds/types"
	"github.com/aws/smithy-go"
)

// drill-02b: the engine upgrade adapter sends the clone's identifier, the
// target version, AllowMajorVersionUpgrade and ApplyImmediately, and maps
// RDS errors as the clone adapters do.

// No concurrent access tests: the adapter is stateless over the client.

type fakeEngineRDS struct {
	clusterIn  *rds.ModifyDBClusterInput
	instanceIn *rds.ModifyDBInstanceInput
	versionsIn *rds.DescribeDBEngineVersionsInput
	err        error
}

func (f *fakeEngineRDS) DescribeDBClusters(_ context.Context, in *rds.DescribeDBClustersInput,
	_ ...func(*rds.Options)) (*rds.DescribeDBClustersOutput, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &rds.DescribeDBClustersOutput{DBClusters: []rdstypes.DBCluster{{
		DBClusterIdentifier: in.DBClusterIdentifier, Engine: aws.String("aurora-postgresql"),
		EngineVersion: aws.String("14.9"), Status: aws.String("upgrading")}}}, nil
}

func (f *fakeEngineRDS) DescribeDBInstances(_ context.Context, in *rds.DescribeDBInstancesInput,
	_ ...func(*rds.Options)) (*rds.DescribeDBInstancesOutput, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &rds.DescribeDBInstancesOutput{}, nil
}

func (f *fakeEngineRDS) DescribeDBEngineVersions(_ context.Context,
	in *rds.DescribeDBEngineVersionsInput, _ ...func(*rds.Options)) (
	*rds.DescribeDBEngineVersionsOutput, error) {
	f.versionsIn = in
	return &rds.DescribeDBEngineVersionsOutput{DBEngineVersions: []rdstypes.DBEngineVersion{{
		ValidUpgradeTarget: []rdstypes.UpgradeTarget{{EngineVersion: aws.String("15.4")},
			{EngineVersion: aws.String("17.4")}}}}}, nil
}

func (f *fakeEngineRDS) ModifyDBCluster(_ context.Context, in *rds.ModifyDBClusterInput,
	_ ...func(*rds.Options)) (*rds.ModifyDBClusterOutput, error) {
	f.clusterIn = in
	return &rds.ModifyDBClusterOutput{}, f.err
}

func (f *fakeEngineRDS) ModifyDBInstance(_ context.Context, in *rds.ModifyDBInstanceInput,
	_ ...func(*rds.Options)) (*rds.ModifyDBInstanceOutput, error) {
	f.instanceIn = in
	return &rds.ModifyDBInstanceOutput{}, f.err
}

func TestEngineSDK_AuroraModifiesTheClusterAllowingAMajorUpgrade(t *testing.T) {
	f := &fakeEngineRDS{}
	api := NewAuroraEngineSDK(f)
	ctx := context.Background()
	state, err := api.Engine(ctx, "sage-rehearsal-0a1b2c3d")
	if err != nil || state != (EngineState{Engine: "aurora-postgresql", Version: "14.9",
		Status: "upgrading"}) {
		t.Fatalf("Engine = %+v %v", state, err)
	}
	targets, err := api.UpgradeTargets(ctx, "aurora-postgresql", "14.9")
	if err != nil || len(targets) != 2 || targets[1] != "17.4" {
		t.Fatalf("UpgradeTargets = %v %v", targets, err)
	}
	if aws.ToString(f.versionsIn.Engine) != "aurora-postgresql" ||
		aws.ToString(f.versionsIn.EngineVersion) != "14.9" {
		t.Fatalf("engine versions asked for %+v", f.versionsIn)
	}
	if err := api.ModifyEngineVersion(ctx, "sage-rehearsal-0a1b2c3d", "17.4"); err != nil {
		t.Fatalf("ModifyEngineVersion: %v", err)
	}
	in := f.clusterIn
	if in == nil || f.instanceIn != nil || aws.ToString(in.DBClusterIdentifier) !=
		"sage-rehearsal-0a1b2c3d" || aws.ToString(in.EngineVersion) != "17.4" ||
		!aws.ToBool(in.AllowMajorVersionUpgrade) || !aws.ToBool(in.ApplyImmediately) {
		t.Fatalf("ModifyDBCluster input = %+v", in)
	}
}

func TestEngineSDK_RDSModifiesTheInstance(t *testing.T) {
	f := &fakeEngineRDS{}
	if err := NewRDSEngineSDK(f).ModifyEngineVersion(context.Background(), "sage-rehearsal-1",
		"17.4"); err != nil {
		t.Fatal(err)
	}
	in := f.instanceIn
	if in == nil || f.clusterIn != nil || aws.ToString(in.DBInstanceIdentifier) !=
		"sage-rehearsal-1" || !aws.ToBool(in.AllowMajorVersionUpgrade) ||
		!aws.ToBool(in.ApplyImmediately) {
		t.Fatalf("ModifyDBInstance input = %+v", in)
	}
	if _, err := NewRDSEngineSDK(f).Engine(context.Background(), "gone"); !errors.Is(err,
		ErrRDSNotFound) {
		t.Fatalf("no instance = %v, want ErrRDSNotFound", err)
	}
}

func TestEngineSDK_MapsRDSErrors(t *testing.T) {
	f := &fakeEngineRDS{err: &smithy.GenericAPIError{Code: "DBClusterNotFoundFault",
		Message: "cluster gone"}}
	if _, err := NewAuroraEngineSDK(f).Engine(context.Background(), "x"); !errors.Is(err,
		ErrAuroraNotFound) {
		t.Fatalf("not found = %v, want ErrAuroraNotFound", err)
	}
	f.err = &smithy.GenericAPIError{Code: "InvalidParameterCombination",
		Message: "cannot upgrade with a custom parameter group"}
	err := NewAuroraEngineSDK(f).ModifyEngineVersion(context.Background(), "x", "17.4")
	var status *ProviderStatusError
	if !errors.As(err, &status) || status.StatusCode != 400 {
		t.Fatalf("modify fault = %v, want a ProviderStatusError 400 with RDS's message", err)
	}
}
