package clone

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/rds"
	rdstypes "github.com/aws/aws-sdk-go-v2/service/rds/types"
)

// AuroraAPI is the slice of the RDS API the Aurora provider uses;
// NewAuroraSDKAPI implements it over the SDK. A cluster or instance that
// does not exist wraps ErrAuroraNotFound; a throttled or failed reply is a
// ProviderStatusError (lab-05a backoff).
type AuroraAPI interface {
	DescribeCluster(ctx context.Context, id string) (AuroraCluster, error)
	// ListClusters is every cluster of the region (bounded paging).
	ListClusters(ctx context.Context) ([]AuroraCluster, error)
	DescribeInstance(ctx context.Context, id string) (AuroraInstance, error)
	// RestoreCluster restores a copy-on-write clone of req.Source at its
	// latest restorable time, without deletion protection.
	RestoreCluster(ctx context.Context, req AuroraRestoreRequest) (AuroraCluster, error)
	CreateInstance(ctx context.Context, req AuroraInstanceRequest) (AuroraInstance, error)
	DeleteInstance(ctx context.Context, id string) error
	// DeleteCluster deletes a cluster with no final snapshot and no
	// retained automated backups.
	DeleteCluster(ctx context.Context, id string) error
}

// AuroraCluster is what the provider reads of a DB cluster.
type AuroraCluster struct {
	ID, Engine, Status, Endpoint, ReaderEndpoint string
	Port                                         int32
	CloneGroupID, SubnetGroup, ParameterGroup    string
	SecurityGroups                               []string
	Serverless                                   *AuroraScaling // nil: not Serverless v2
	Members                                      []AuroraMember
	Tags                                         map[string]string
	DeletionProtection                           bool
}

// AuroraMember is one instance of a cluster.
type AuroraMember struct {
	ID     string
	Writer bool
}

// AuroraScaling is a Serverless v2 capacity range, in ACUs.
type AuroraScaling struct{ Min, Max float64 }

// AuroraInstance is what the provider reads of a DB instance.
type AuroraInstance struct {
	ID, ClusterID, Class, Status, ParameterGroup string
	PubliclyAccessible                           bool
}

// AuroraRestoreRequest asks for clone cluster ID of Source in the source's
// network, with Tags.
type AuroraRestoreRequest struct {
	ID, Source, SubnetGroup, ParameterGroup string
	SecurityGroups                          []string
	Serverless                              *AuroraScaling
	Tags                                    map[string]string
}

// AuroraInstanceRequest asks for instance ID in cluster ClusterID.
type AuroraInstanceRequest struct {
	ID, ClusterID, Class, ParameterGroup string
	PubliclyAccessible                   bool
	Tags                                 map[string]string
}

// auroraRDS is the slice of *rds.Client the adapter calls.
type auroraRDS interface {
	DescribeDBClusters(context.Context, *rds.DescribeDBClustersInput,
		...func(*rds.Options)) (*rds.DescribeDBClustersOutput, error)
	DescribeDBInstances(context.Context, *rds.DescribeDBInstancesInput,
		...func(*rds.Options)) (*rds.DescribeDBInstancesOutput, error)
	RestoreDBClusterToPointInTime(context.Context, *rds.RestoreDBClusterToPointInTimeInput,
		...func(*rds.Options)) (*rds.RestoreDBClusterToPointInTimeOutput, error)
	CreateDBInstance(context.Context, *rds.CreateDBInstanceInput,
		...func(*rds.Options)) (*rds.CreateDBInstanceOutput, error)
	DeleteDBInstance(context.Context, *rds.DeleteDBInstanceInput,
		...func(*rds.Options)) (*rds.DeleteDBInstanceOutput, error)
	DeleteDBCluster(context.Context, *rds.DeleteDBClusterInput,
		...func(*rds.Options)) (*rds.DeleteDBClusterOutput, error)
}

// NewAuroraSDKAPI adapts an RDS client (rds.NewFromConfig with the AWS
// default credential chain) to AuroraAPI.
func NewAuroraSDKAPI(client auroraRDS) AuroraAPI { return auroraSDK{rds: client} }

type auroraSDK struct{ rds auroraRDS }

func (a auroraSDK) DescribeCluster(ctx context.Context, id string) (AuroraCluster, error) {
	out, err := a.rds.DescribeDBClusters(ctx, &rds.DescribeDBClustersInput{
		DBClusterIdentifier: aws.String(id)})
	if err != nil {
		return AuroraCluster{}, auroraError(ctx, "DescribeDBClusters", err)
	}
	if len(out.DBClusters) != 1 {
		return AuroraCluster{}, fmt.Errorf("%w: cluster %s", ErrAuroraNotFound, id)
	}
	return fromSDKCluster(out.DBClusters[0]), nil
}

func (a auroraSDK) ListClusters(ctx context.Context) ([]AuroraCluster, error) {
	var out []AuroraCluster
	var marker *string
	for range auroraListPages {
		resp, err := a.rds.DescribeDBClusters(ctx, &rds.DescribeDBClustersInput{
			Marker: marker, MaxRecords: aws.Int32(100)})
		if err != nil {
			return nil, auroraError(ctx, "DescribeDBClusters", err)
		}
		for _, c := range resp.DBClusters {
			out = append(out, fromSDKCluster(c))
		}
		if aws.ToString(resp.Marker) == "" {
			return out, nil
		}
		marker = resp.Marker
	}
	return nil, fmt.Errorf("aurora_clone: the region has more than %d pages of clusters",
		auroraListPages)
}

func (a auroraSDK) DescribeInstance(ctx context.Context, id string) (AuroraInstance,
	error) {
	out, err := a.rds.DescribeDBInstances(ctx, &rds.DescribeDBInstancesInput{
		DBInstanceIdentifier: aws.String(id)})
	if err != nil {
		return AuroraInstance{}, auroraError(ctx, "DescribeDBInstances", err)
	}
	if len(out.DBInstances) != 1 {
		return AuroraInstance{}, fmt.Errorf("%w: instance %s", ErrAuroraNotFound, id)
	}
	return fromSDKInstance(out.DBInstances[0]), nil
}

func (a auroraSDK) RestoreCluster(ctx context.Context,
	req AuroraRestoreRequest) (AuroraCluster, error) {
	in := &rds.RestoreDBClusterToPointInTimeInput{
		DBClusterIdentifier: aws.String(req.ID), SourceDBClusterIdentifier: aws.String(
			req.Source), RestoreType: aws.String("copy-on-write"),
		UseLatestRestorableTime: aws.Bool(true), DeletionProtection: aws.Bool(false),
		DBSubnetGroupName: optional(req.SubnetGroup), VpcSecurityGroupIds: req.SecurityGroups,
		DBClusterParameterGroupName: optional(req.ParameterGroup), Tags: sdkTags(req.Tags)}
	if s := req.Serverless; s != nil {
		in.ServerlessV2ScalingConfiguration = &rdstypes.ServerlessV2ScalingConfiguration{
			MinCapacity: aws.Float64(s.Min), MaxCapacity: aws.Float64(s.Max)}
	}
	out, err := a.rds.RestoreDBClusterToPointInTime(ctx, in)
	if err != nil {
		return AuroraCluster{}, auroraError(ctx, "RestoreDBClusterToPointInTime", err)
	}
	if out.DBCluster == nil {
		return AuroraCluster{}, errors.New("aurora_clone: the restore reply has no DBCluster")
	}
	return fromSDKCluster(*out.DBCluster), nil
}

func (a auroraSDK) CreateInstance(ctx context.Context,
	req AuroraInstanceRequest) (AuroraInstance, error) {
	out, err := a.rds.CreateDBInstance(ctx, &rds.CreateDBInstanceInput{
		DBInstanceIdentifier: aws.String(req.ID), DBClusterIdentifier: aws.String(req.ClusterID),
		DBInstanceClass: aws.String(req.Class), Engine: aws.String(auroraEngine),
		DBParameterGroupName: optional(req.ParameterGroup),
		PubliclyAccessible:   aws.Bool(req.PubliclyAccessible), Tags: sdkTags(req.Tags)})
	if err != nil {
		return AuroraInstance{}, auroraError(ctx, "CreateDBInstance", err)
	}
	if out.DBInstance == nil {
		return AuroraInstance{}, errors.New("aurora_clone: the create reply has no DBInstance")
	}
	return fromSDKInstance(*out.DBInstance), nil
}

func (a auroraSDK) DeleteInstance(ctx context.Context, id string) error {
	_, err := a.rds.DeleteDBInstance(ctx, &rds.DeleteDBInstanceInput{
		DBInstanceIdentifier: aws.String(id)})
	if err != nil {
		return auroraError(ctx, "DeleteDBInstance", err)
	}
	return nil
}

func (a auroraSDK) DeleteCluster(ctx context.Context, id string) error {
	_, err := a.rds.DeleteDBCluster(ctx, &rds.DeleteDBClusterInput{
		DBClusterIdentifier: aws.String(id), SkipFinalSnapshot: aws.Bool(true),
		DeleteAutomatedBackups: aws.Bool(true)})
	if err != nil {
		return auroraError(ctx, "DeleteDBCluster", err)
	}
	return nil
}

func fromSDKCluster(c rdstypes.DBCluster) AuroraCluster {
	out := AuroraCluster{ID: aws.ToString(c.DBClusterIdentifier),
		Engine: aws.ToString(c.Engine), Status: aws.ToString(c.Status),
		Endpoint: aws.ToString(c.Endpoint), ReaderEndpoint: aws.ToString(c.ReaderEndpoint),
		Port: aws.ToInt32(c.Port), CloneGroupID: aws.ToString(c.CloneGroupId),
		SubnetGroup: aws.ToString(c.DBSubnetGroup), ParameterGroup: aws.ToString(
			c.DBClusterParameterGroup), DeletionProtection: aws.ToBool(c.DeletionProtection),
		Tags: map[string]string{}}
	for _, g := range c.VpcSecurityGroups {
		out.SecurityGroups = append(out.SecurityGroups, aws.ToString(g.VpcSecurityGroupId))
	}
	for _, m := range c.DBClusterMembers {
		out.Members = append(out.Members, AuroraMember{ID: aws.ToString(m.DBInstanceIdentifier),
			Writer: aws.ToBool(m.IsClusterWriter)})
	}
	for _, t := range c.TagList {
		out.Tags[aws.ToString(t.Key)] = aws.ToString(t.Value)
	}
	if s := c.ServerlessV2ScalingConfiguration; s != nil {
		out.Serverless = &AuroraScaling{Min: aws.ToFloat64(s.MinCapacity),
			Max: aws.ToFloat64(s.MaxCapacity)}
	}
	return out
}

func fromSDKInstance(d rdstypes.DBInstance) AuroraInstance {
	out := AuroraInstance{ID: aws.ToString(d.DBInstanceIdentifier),
		ClusterID: aws.ToString(d.DBClusterIdentifier), Class: aws.ToString(d.DBInstanceClass),
		Status: aws.ToString(d.DBInstanceStatus), PubliclyAccessible: aws.ToBool(
			d.PubliclyAccessible)}
	if len(d.DBParameterGroups) > 0 {
		out.ParameterGroup = aws.ToString(d.DBParameterGroups[0].DBParameterGroupName)
	}
	return out
}

func sdkTags(tags map[string]string) []rdstypes.Tag {
	keys := make([]string, 0, len(tags))
	for k := range tags {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]rdstypes.Tag, 0, len(keys))
	for _, k := range keys {
		out = append(out, rdstypes.Tag{Key: aws.String(k), Value: aws.String(tags[k])})
	}
	return out
}

func optional(s string) *string {
	if s == "" {
		return nil
	}
	return aws.String(s)
}

type rdsAPIError interface {
	ErrorCode() string
	ErrorMessage() string
}

type rdsStatusError interface{ HTTPStatusCode() int }

// auroraError maps an SDK error of the Aurora adapter (rdsSDKError).
func auroraError(ctx context.Context, op string, err error) error {
	return rdsSDKError(ctx, "aurora", op, err, ErrAuroraNotFound, ErrAuroraAlreadyExists)
}

// rdsSDKError maps an RDS SDK error of adapter svc: a missing cluster or
// instance to notFound, an existing one to exists (also a
// ProviderStatusError), missing credentials to ErrCloneUnavailable, any
// other fault to a ProviderStatusError (throttling as 429, a denied
// permission as 403) with RDS's code and message. A transport error keeps
// its cause, so a timeout reads as an uncertain create.
func rdsSDKError(ctx context.Context, svc, op string, err, notFound, exists error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return fmt.Errorf("%s %s: %w", svc, op, ctxErr)
	}
	if strings.Contains(err.Error(), "failed to retrieve credentials") {
		return fmt.Errorf("%w: %s %s: no AWS credentials in the default chain",
			ErrCloneUnavailable, svc, op)
	}
	var api rdsAPIError
	if !errors.As(err, &api) {
		return fmt.Errorf("%s %s request failed; check network access to RDS: %w", svc, op,
			err)
	}
	code := api.ErrorCode()
	status := &ProviderStatusError{Op: svc + " " + op, StatusCode: rdsStatus(err, code)}
	switch code {
	case "DBClusterNotFoundFault", "DBInstanceNotFound", "DBInstanceNotFoundFault":
		return fmt.Errorf("%w: %s %s: %s", notFound, svc, op, api.ErrorMessage())
	case "DBClusterAlreadyExistsFault", "DBInstanceAlreadyExists",
		"DBInstanceAlreadyExistsFault":
		return fmt.Errorf("%w: %s %s: %s: %s: %w", exists, svc, op, code,
			api.ErrorMessage(), status)
	}
	return fmt.Errorf("%s %s: %s: %s: %w", svc, op, code, api.ErrorMessage(), status)
}

func rdsStatus(err error, code string) int {
	switch code {
	case "Throttling", "ThrottlingException", "RequestLimitExceeded",
		"TooManyRequestsException":
		return http.StatusTooManyRequests
	case "AccessDenied", "AccessDeniedException", "UnauthorizedOperation",
		"NotAuthorized":
		return http.StatusForbidden
	}
	var se rdsStatusError
	if errors.As(err, &se) && se.HTTPStatusCode() > 0 {
		return se.HTTPStatusCode()
	}
	return http.StatusBadRequest
}

// auroraPollPause waits d or until ctx ends.
func auroraPollPause(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}
