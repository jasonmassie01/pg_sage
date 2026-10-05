package cloudtel

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
)

// AWSOptions identify one RDS instance or Aurora cluster (writer) and how
// to reach AWS. Credentials come from the AWS default chain (environment,
// shared config/SSO, web identity, ECS task or EC2 instance role).
type AWSOptions struct {
	Region         string
	InstanceID     string
	ClusterID      string
	ReaderEndpoint bool // the endpoint was a cluster reader endpoint
	Credentials    aws.CredentialsProvider
	HTTPClient     *http.Client
	// Endpoint overrides (tests, VPC endpoints); default: AWS public ones.
	CloudWatchEndpoint, PIEndpoint, RDSEndpoint string
	Now                                         func() time.Time
}

// AWSSource collects CloudWatch and Performance Insights telemetry.
type AWSSource struct {
	opts   AWSOptions
	client *awsClient
	rds    *rdsAPI
}

// NewAWSSource validates the identity; it makes no network call.
func NewAWSSource(opts AWSOptions) (*AWSSource, error) {
	switch {
	case !ValidAWSRegion(opts.Region):
		return nil, fmt.Errorf("%w: invalid AWS region %q", ErrIdentity, opts.Region)
	case opts.ReaderEndpoint:
		return nil, fmt.Errorf("%w: an Aurora reader endpoint does not name one instance; "+
			"connect pg_sage to the cluster (writer) endpoint", ErrIdentity)
	case (opts.InstanceID == "") == (opts.ClusterID == ""):
		return nil, fmt.Errorf("%w: name exactly one RDS instance or Aurora cluster",
			ErrIdentity)
	case opts.InstanceID != "" && !ValidRDSIdentifier(opts.InstanceID),
		opts.ClusterID != "" && !ValidRDSIdentifier(opts.ClusterID):
		return nil, fmt.Errorf("%w: invalid RDS identifier", ErrIdentity)
	case opts.Credentials == nil:
		return nil, fmt.Errorf("%w: no AWS credential provider", ErrNoCredentials)
	}
	if opts.HTTPClient == nil {
		opts.HTTPClient = &http.Client{Timeout: 20 * time.Second}
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	suffix := "amazonaws.com"
	if strings.HasPrefix(opts.Region, "cn-") {
		suffix = "amazonaws.com.cn"
	}
	if opts.CloudWatchEndpoint == "" {
		opts.CloudWatchEndpoint = "https://monitoring." + opts.Region + "." + suffix + "/"
	}
	if opts.PIEndpoint == "" {
		opts.PIEndpoint = "https://pi." + opts.Region + "." + suffix + "/"
	}
	creds := opts.Credentials
	if _, cached := creds.(*aws.CredentialsCache); !cached {
		creds = aws.NewCredentialsCache(creds)
	}
	return &AWSSource{opts: opts,
		client: &awsClient{creds: creds, http: opts.HTTPClient, signer: v4.NewSigner(),
			region: opts.Region, now: opts.Now},
		rds: newRDSAPI(opts.Region, opts.RDSEndpoint, creds, opts.HTTPClient)}, nil
}

// Provider is "aurora" for a cluster, else "rds".
func (s *AWSSource) Provider() string {
	if s.opts.ClusterID != "" {
		return "aurora"
	}
	return "rds"
}

// resolve finds the instance (the writer for a cluster) after checking
// that credentials exist, so nothing is sent without them.
func (s *AWSSource) resolve(ctx context.Context) (rdsInstance, error) {
	if err := ctx.Err(); err != nil {
		return rdsInstance{}, err
	}
	if _, err := s.client.credentials(ctx); err != nil {
		return rdsInstance{}, err
	}
	id := s.opts.InstanceID
	if s.opts.ClusterID != "" {
		writer, err := s.rds.clusterWriter(ctx, s.opts.ClusterID)
		if err != nil {
			return rdsInstance{}, err
		}
		id = writer
	}
	return s.rds.instance(ctx, id)
}

// Collect gathers one sample: CloudWatch is required, Performance
// Insights optional (its failure is partial data).
func (s *AWSSource) Collect(ctx context.Context, now time.Time) (Sample, error) {
	inst, err := s.resolve(ctx)
	if err != nil {
		return Sample{}, err
	}
	aurora := s.opts.ClusterID != "" || strings.HasPrefix(inst.Engine, "aurora")
	sample := Sample{Provider: s.Provider(), CollectedAt: now,
		Resource: s.Provider() + ":" + s.opts.Region + "/" + inst.ID}
	queries, fields := cloudWatchQueries(inst, aurora)
	points, err := s.client.getMetricData(ctx, s.opts.CloudWatchEndpoint, queries, now)
	if err != nil {
		return Sample{}, err
	}
	if err := applyCloudWatch(&sample, points, fields, now); err != nil {
		return Sample{}, err
	}
	s.applyStorage(&sample, inst, aurora, now)
	if err := s.applyPI(ctx, &sample, inst, now); err != nil {
		return Sample{}, err
	}
	if sample.MemoryTotalBytes == nil {
		if mem := InstanceClassMemoryBytes(inst.Class); mem > 0 {
			sample.MemoryTotalBytes = &Point{Value: mem, At: now}
			sample.MemoryTotalSource = MemorySourceInstanceClass
		} else {
			sample.Missing = append(sample.Missing, "memory_total: unknown instance class "+
				inst.Class+" and no Performance Insights OS metrics")
		}
	}
	return sample, nil
}

func (s *AWSSource) applyStorage(sample *Sample, inst rdsInstance, aurora bool,
	now time.Time) {
	if aurora {
		sample.StorageAutoGrows = true // the cluster volume grows to 128 TiB
		return
	}
	if inst.AllocatedGiB <= 0 {
		return
	}
	allocated := float64(inst.AllocatedGiB) * gibF
	sample.AllocatedStorageBytes = &Point{Value: allocated, At: now}
	capacity := allocated
	if inst.MaxAllocatedGiB > inst.AllocatedGiB {
		capacity = float64(inst.MaxAllocatedGiB) * gibF
	}
	sample.StorageCapacityBytes = &Point{Value: capacity, At: now}
}

// applyPI adds DB load and the OS memory total; any PI failure other than
// a clock problem or cancellation is recorded as missing data.
func (s *AWSSource) applyPI(ctx context.Context, sample *Sample, inst rdsInstance,
	now time.Time) error {
	if !inst.PIEnabled || inst.ResourceID == "" {
		sample.Missing = append(sample.Missing, "db_load: Performance Insights is not enabled")
		return nil
	}
	res, err := s.client.resourceMetrics(ctx, s.opts.PIEndpoint, inst.ResourceID, now)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		sample.Missing = append(sample.Missing, "db_load: "+sanitize(err.Error()))
		return nil
	}
	if res.load == nil {
		sample.Missing = append(sample.Missing, "db_load: no Performance Insights datapoint")
	}
	sample.DBLoad = res.load
	if len(res.byWait) > 0 {
		sample.DBLoadByWait = res.byWait
	}
	if res.memTotal != nil {
		sample.MemoryTotalBytes, sample.MemoryTotalSource = res.memTotal,
			MemorySourcePerformanceInsights
	}
	return nil
}
