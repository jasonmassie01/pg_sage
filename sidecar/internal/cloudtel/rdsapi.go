package cloudtel

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/rds"

	"github.com/pg-sage/sidecar/internal/managedparam"
)

// The RDS control-plane reads use the official SDK (already a module
// dependency): the instance (class, storage, PI, parameter group,
// replicas), the Aurora cluster's writer and the group's parameters.

type rdsInstance struct {
	ID, Class, Engine, ResourceID, ClusterID string
	AllocatedGiB, MaxAllocatedGiB            int32
	PIEnabled                                bool
	ParameterGroup, ParameterApplyStatus     string
	Replicas                                 []string
	BackupRetention                          *int32
	DeletionProtection                       *bool
}

type rdsAPI struct{ client *rds.Client }

func newRDSAPI(region, endpoint string, creds aws.CredentialsProvider,
	httpClient *http.Client) *rdsAPI {
	opts := rds.Options{Region: region, Credentials: creds, HTTPClient: httpClient,
		RetryMaxAttempts: 1}
	if endpoint != "" {
		opts.BaseEndpoint = aws.String(endpoint)
	}
	return &rdsAPI{client: rds.New(opts)}
}

func (a *rdsAPI) instance(ctx context.Context, id string) (rdsInstance, error) {
	out, err := a.client.DescribeDBInstances(ctx, &rds.DescribeDBInstancesInput{
		DBInstanceIdentifier: aws.String(id)})
	if err != nil {
		return rdsInstance{}, sdkError(ctx, "RDS DescribeDBInstances", err)
	}
	if len(out.DBInstances) != 1 {
		return rdsInstance{}, fmt.Errorf("%w: RDS instance %s not found", ErrIdentity, id)
	}
	d := out.DBInstances[0]
	inst := rdsInstance{ID: aws.ToString(d.DBInstanceIdentifier),
		Class: aws.ToString(d.DBInstanceClass), Engine: aws.ToString(d.Engine),
		ResourceID: aws.ToString(d.DbiResourceId), ClusterID: aws.ToString(d.DBClusterIdentifier),
		AllocatedGiB: aws.ToInt32(d.AllocatedStorage), MaxAllocatedGiB: aws.ToInt32(
			d.MaxAllocatedStorage), PIEnabled: aws.ToBool(d.PerformanceInsightsEnabled),
		Replicas: d.ReadReplicaDBInstanceIdentifiers, BackupRetention: d.BackupRetentionPeriod,
		DeletionProtection: d.DeletionProtection}
	if len(d.DBParameterGroups) > 0 {
		inst.ParameterGroup = aws.ToString(d.DBParameterGroups[0].DBParameterGroupName)
		inst.ParameterApplyStatus = aws.ToString(d.DBParameterGroups[0].ParameterApplyStatus)
	}
	return inst, nil
}

// clusterWriter is the writer instance of an Aurora cluster.
func (a *rdsAPI) clusterWriter(ctx context.Context, cluster string) (string, error) {
	out, err := a.client.DescribeDBClusters(ctx, &rds.DescribeDBClustersInput{
		DBClusterIdentifier: aws.String(cluster)})
	if err != nil {
		return "", sdkError(ctx, "RDS DescribeDBClusters", err)
	}
	for _, c := range out.DBClusters {
		for _, m := range c.DBClusterMembers {
			if aws.ToBool(m.IsClusterWriter) {
				return aws.ToString(m.DBInstanceIdentifier), nil
			}
		}
	}
	return "", fmt.Errorf("%w: Aurora cluster %s has no writer", ErrIdentity, cluster)
}

// parameters pages through the group's parameters (at most 50 pages).
func (a *rdsAPI) parameters(ctx context.Context,
	group string) (map[string]managedparam.GroupParam, error) {
	out := map[string]managedparam.GroupParam{}
	var marker *string
	for page := 0; page < 50; page++ {
		resp, err := a.client.DescribeDBParameters(ctx, &rds.DescribeDBParametersInput{
			DBParameterGroupName: aws.String(group), Marker: marker,
			MaxRecords: aws.Int32(100)})
		if err != nil {
			return nil, sdkError(ctx, "RDS DescribeDBParameters", err)
		}
		for _, p := range resp.Parameters {
			out[aws.ToString(p.ParameterName)] = managedparam.GroupParam{
				Value: aws.ToString(p.ParameterValue), Source: aws.ToString(p.Source),
				ApplyType: aws.ToString(p.ApplyType), Modifiable: aws.ToBool(p.IsModifiable)}
		}
		if aws.ToString(resp.Marker) == "" {
			return out, nil
		}
		marker = resp.Marker
	}
	return nil, fmt.Errorf("%w: parameter group %s has more than 50 pages", ErrProvider, group)
}

type apiError interface {
	ErrorCode() string
	ErrorMessage() string
}

type statusError interface{ HTTPStatusCode() int }

// sdkError maps an SDK error to the package's errors.
func sdkError(ctx context.Context, op string, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if strings.Contains(err.Error(), "failed to retrieve credentials") {
		return fmt.Errorf("%w: %s", ErrNoCredentials, op)
	}
	var api apiError
	if !errors.As(err, &api) {
		return fmt.Errorf("%w: %s request failed; check network access", ErrProvider, op)
	}
	status := 0
	var se statusError
	if errors.As(err, &se) {
		status = se.HTTPStatusCode()
	}
	return awsCodeError(op, status, api.ErrorCode(), api.ErrorMessage())
}
