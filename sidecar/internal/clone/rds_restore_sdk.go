package clone

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/rds"
	rdstypes "github.com/aws/aws-sdk-go-v2/service/rds/types"
)

// RDSRestoreClient is the slice of the RDS API the rds_restore provider
// uses; NewRDSRestoreSDKAPI implements it over the SDK. An instance that
// does not exist wraps ErrRDSNotFound; a throttled or failed reply is a
// ProviderStatusError (lab-05a backoff).
type RDSRestoreClient interface {
	DescribeInstance(ctx context.Context, id string) (RDSInstance, error)
	// RestoreInstance restores req.Source to its latest restorable time as
	// instance req.ID, single-AZ and without deletion protection.
	RestoreInstance(ctx context.Context, req RDSRestoreRequest) (RDSInstance, error)
	// DeleteInstance deletes an instance with no final snapshot and no
	// retained automated backups.
	DeleteInstance(ctx context.Context, id string) error
}

// RDSInstance is what the provider reads of a DB instance.
type RDSInstance struct {
	ID, Engine, Status, Endpoint, Class string
	Port                                int32
	SubnetGroup, ParameterGroup         string
	StorageType                         string
	Iops, AllocatedStorageGB            int32
	SecurityGroups                      []string
	PubliclyAccessible, MultiAZ         bool
	DeletionProtection                  bool
	// ClusterID is the Aurora or Multi-AZ DB cluster the instance belongs
	// to; "" for a single instance.
	ClusterID            string
	LatestRestorableTime time.Time
	Tags                 map[string]string
}

// RDSRestoreRequest asks for instance ID restored from Source, in the
// source's network, with Tags. Iops is sent only when non-zero.
type RDSRestoreRequest struct {
	ID, Source, Class, SubnetGroup, ParameterGroup, StorageType string
	Iops                                                        int32
	SecurityGroups                                              []string
	PubliclyAccessible                                          bool
	Tags                                                        map[string]string
}

// rdsRestoreRDS is the slice of *rds.Client the adapter calls.
type rdsRestoreRDS interface {
	DescribeDBInstances(context.Context, *rds.DescribeDBInstancesInput,
		...func(*rds.Options)) (*rds.DescribeDBInstancesOutput, error)
	RestoreDBInstanceToPointInTime(context.Context, *rds.RestoreDBInstanceToPointInTimeInput,
		...func(*rds.Options)) (*rds.RestoreDBInstanceToPointInTimeOutput, error)
	DeleteDBInstance(context.Context, *rds.DeleteDBInstanceInput,
		...func(*rds.Options)) (*rds.DeleteDBInstanceOutput, error)
}

// NewRDSRestoreSDKAPI adapts an RDS client (rds.NewFromConfig with the AWS
// default credential chain) to RDSRestoreClient.
func NewRDSRestoreSDKAPI(client rdsRestoreRDS) RDSRestoreClient {
	return rdsRestoreSDK{rds: client}
}

type rdsRestoreSDK struct{ rds rdsRestoreRDS }

func rdsRestoreError(ctx context.Context, op string, err error) error {
	return rdsSDKError(ctx, "rds_restore", op, err, ErrRDSNotFound, ErrRDSAlreadyExists)
}

func (a rdsRestoreSDK) DescribeInstance(ctx context.Context, id string) (RDSInstance,
	error) {
	out, err := a.rds.DescribeDBInstances(ctx, &rds.DescribeDBInstancesInput{
		DBInstanceIdentifier: aws.String(id)})
	if err != nil {
		return RDSInstance{}, rdsRestoreError(ctx, "DescribeDBInstances", err)
	}
	if len(out.DBInstances) != 1 {
		return RDSInstance{}, fmt.Errorf("%w: instance %s", ErrRDSNotFound, id)
	}
	return fromSDKRDSInstance(out.DBInstances[0]), nil
}

func (a rdsRestoreSDK) RestoreInstance(ctx context.Context,
	req RDSRestoreRequest) (RDSInstance, error) {
	in := &rds.RestoreDBInstanceToPointInTimeInput{
		SourceDBInstanceIdentifier: aws.String(req.Source),
		TargetDBInstanceIdentifier: aws.String(req.ID), UseLatestRestorableTime: aws.Bool(true),
		DBInstanceClass: optional(req.Class), DBSubnetGroupName: optional(req.SubnetGroup),
		DBParameterGroupName: optional(req.ParameterGroup),
		StorageType:          optional(req.StorageType), VpcSecurityGroupIds: req.SecurityGroups,
		PubliclyAccessible: aws.Bool(req.PubliclyAccessible), MultiAZ: aws.Bool(false),
		DeletionProtection: aws.Bool(false), CopyTagsToSnapshot: aws.Bool(false),
		Tags: sdkTags(req.Tags)}
	if req.Iops > 0 {
		in.Iops = aws.Int32(req.Iops)
	}
	out, err := a.rds.RestoreDBInstanceToPointInTime(ctx, in)
	if err != nil {
		return RDSInstance{}, rdsRestoreError(ctx, "RestoreDBInstanceToPointInTime", err)
	}
	if out.DBInstance == nil {
		return RDSInstance{}, errors.New("rds_restore: the restore reply has no DBInstance")
	}
	return fromSDKRDSInstance(*out.DBInstance), nil
}

func (a rdsRestoreSDK) DeleteInstance(ctx context.Context, id string) error {
	_, err := a.rds.DeleteDBInstance(ctx, &rds.DeleteDBInstanceInput{
		DBInstanceIdentifier: aws.String(id), SkipFinalSnapshot: aws.Bool(true),
		DeleteAutomatedBackups: aws.Bool(true)})
	if err != nil {
		return rdsRestoreError(ctx, "DeleteDBInstance", err)
	}
	return nil
}

func fromSDKRDSInstance(d rdstypes.DBInstance) RDSInstance {
	out := RDSInstance{ID: aws.ToString(d.DBInstanceIdentifier),
		Engine: aws.ToString(d.Engine), Status: aws.ToString(d.DBInstanceStatus),
		Class: aws.ToString(d.DBInstanceClass), StorageType: aws.ToString(d.StorageType),
		Iops: aws.ToInt32(d.Iops), AllocatedStorageGB: aws.ToInt32(d.AllocatedStorage),
		PubliclyAccessible: aws.ToBool(d.PubliclyAccessible), MultiAZ: aws.ToBool(d.MultiAZ),
		DeletionProtection:   aws.ToBool(d.DeletionProtection),
		ClusterID:            aws.ToString(d.DBClusterIdentifier),
		LatestRestorableTime: aws.ToTime(d.LatestRestorableTime),
		Tags:                 map[string]string{}}
	if e := d.Endpoint; e != nil {
		out.Endpoint, out.Port = aws.ToString(e.Address), aws.ToInt32(e.Port)
	}
	if d.DBSubnetGroup != nil {
		out.SubnetGroup = aws.ToString(d.DBSubnetGroup.DBSubnetGroupName)
	}
	if len(d.DBParameterGroups) > 0 {
		out.ParameterGroup = aws.ToString(d.DBParameterGroups[0].DBParameterGroupName)
	}
	for _, g := range d.VpcSecurityGroups {
		out.SecurityGroups = append(out.SecurityGroups, aws.ToString(g.VpcSecurityGroupId))
	}
	for _, t := range d.TagList {
		out.Tags[aws.ToString(t.Key)] = aws.ToString(t.Value)
	}
	return out
}
