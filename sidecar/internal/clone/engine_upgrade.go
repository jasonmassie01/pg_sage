package clone

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/rds"
)

// The engine upgrade of a rehearsal clone (drill-02b): the slice of the
// RDS API that moves an Aurora clone cluster or an RDS clone instance to
// another engine version. The upgrade rehearsal calls it only on a
// resource a live rehearsal receipt names, never on the source.

// EngineState is a cluster's or instance's engine, engine version and
// status.
type EngineState struct {
	Engine, Version, Status string
}

// EngineUpgradeAPI upgrades one kind of resource (an Aurora cluster or an
// RDS instance).
type EngineUpgradeAPI interface {
	// Engine reads the resource's engine state.
	Engine(ctx context.Context, id string) (EngineState, error)
	// UpgradeTargets lists the engine versions engine at version may be
	// upgraded to.
	UpgradeTargets(ctx context.Context, engine, version string) ([]string, error)
	// ModifyEngineVersion moves the resource to version now, allowing a
	// major version upgrade.
	ModifyEngineVersion(ctx context.Context, id, version string) error
}

// engineUpgradeRDS is the slice of *rds.Client the engine upgrade calls.
type engineUpgradeRDS interface {
	DescribeDBClusters(context.Context, *rds.DescribeDBClustersInput,
		...func(*rds.Options)) (*rds.DescribeDBClustersOutput, error)
	DescribeDBInstances(context.Context, *rds.DescribeDBInstancesInput,
		...func(*rds.Options)) (*rds.DescribeDBInstancesOutput, error)
	DescribeDBEngineVersions(context.Context, *rds.DescribeDBEngineVersionsInput,
		...func(*rds.Options)) (*rds.DescribeDBEngineVersionsOutput, error)
	ModifyDBCluster(context.Context, *rds.ModifyDBClusterInput,
		...func(*rds.Options)) (*rds.ModifyDBClusterOutput, error)
	ModifyDBInstance(context.Context, *rds.ModifyDBInstanceInput,
		...func(*rds.Options)) (*rds.ModifyDBInstanceOutput, error)
}

// NewAuroraEngineSDK upgrades Aurora clusters (the aurora_clone adapter's
// clones) through client.
func NewAuroraEngineSDK(client engineUpgradeRDS) EngineUpgradeAPI {
	return engineSDK{rds: client, cluster: true}
}

// NewRDSEngineSDK upgrades RDS instances (the rds_restore adapter's
// clones) through client.
func NewRDSEngineSDK(client engineUpgradeRDS) EngineUpgradeAPI {
	return engineSDK{rds: client}
}

type engineSDK struct {
	rds     engineUpgradeRDS
	cluster bool
}

func (e engineSDK) Engine(ctx context.Context, id string) (EngineState, error) {
	if e.cluster {
		out, err := e.rds.DescribeDBClusters(ctx, &rds.DescribeDBClustersInput{
			DBClusterIdentifier: aws.String(id)})
		if err != nil {
			return EngineState{}, auroraError(ctx, "DescribeDBClusters", err)
		}
		if len(out.DBClusters) != 1 {
			return EngineState{}, fmt.Errorf("%w: cluster %s", ErrAuroraNotFound, id)
		}
		c := out.DBClusters[0]
		return EngineState{Engine: aws.ToString(c.Engine),
			Version: aws.ToString(c.EngineVersion), Status: aws.ToString(c.Status)}, nil
	}
	out, err := e.rds.DescribeDBInstances(ctx, &rds.DescribeDBInstancesInput{
		DBInstanceIdentifier: aws.String(id)})
	if err != nil {
		return EngineState{}, rdsRestoreError(ctx, "DescribeDBInstances", err)
	}
	if len(out.DBInstances) != 1 {
		return EngineState{}, fmt.Errorf("%w: instance %s", ErrRDSNotFound, id)
	}
	d := out.DBInstances[0]
	return EngineState{Engine: aws.ToString(d.Engine), Version: aws.ToString(d.EngineVersion),
		Status: aws.ToString(d.DBInstanceStatus)}, nil
}

func (e engineSDK) UpgradeTargets(ctx context.Context, engine, version string) ([]string,
	error) {
	out, err := e.rds.DescribeDBEngineVersions(ctx, &rds.DescribeDBEngineVersionsInput{
		Engine: aws.String(engine), EngineVersion: aws.String(version)})
	if err != nil {
		return nil, e.mapErr(ctx, "DescribeDBEngineVersions", err)
	}
	var targets []string
	for _, v := range out.DBEngineVersions {
		for _, t := range v.ValidUpgradeTarget {
			targets = append(targets, aws.ToString(t.EngineVersion))
		}
	}
	return targets, nil
}

func (e engineSDK) ModifyEngineVersion(ctx context.Context, id, version string) error {
	if e.cluster {
		_, err := e.rds.ModifyDBCluster(ctx, &rds.ModifyDBClusterInput{
			DBClusterIdentifier: aws.String(id), EngineVersion: aws.String(version),
			AllowMajorVersionUpgrade: aws.Bool(true), ApplyImmediately: aws.Bool(true)})
		if err != nil {
			return auroraError(ctx, "ModifyDBCluster", err)
		}
		return nil
	}
	_, err := e.rds.ModifyDBInstance(ctx, &rds.ModifyDBInstanceInput{
		DBInstanceIdentifier: aws.String(id), EngineVersion: aws.String(version),
		AllowMajorVersionUpgrade: aws.Bool(true), ApplyImmediately: aws.Bool(true)})
	if err != nil {
		return rdsRestoreError(ctx, "ModifyDBInstance", err)
	}
	return nil
}

func (e engineSDK) mapErr(ctx context.Context, op string, err error) error {
	if e.cluster {
		return auroraError(ctx, op, err)
	}
	return rdsRestoreError(ctx, op, err)
}
