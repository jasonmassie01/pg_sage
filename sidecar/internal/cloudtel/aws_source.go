package cloudtel

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"

	"github.com/pg-sage/sidecar/internal/egress"
	"github.com/pg-sage/sidecar/internal/managedparam"
)

// AWSOptions identify one RDS instance or Aurora cluster (writer) and how
// to reach AWS. Credentials come from the AWS default chain (environment,
// shared config/SSO, web identity, ECS task or EC2 instance role).
type AWSOptions struct {
	Region         string
	InstanceID     string
	ClusterID      string
	ReaderEndpoint bool // the endpoint was a cluster reader endpoint
	// DetectedProvider is the platform PostgreSQL itself reports ("rds"
	// or "aurora"; "" unknown): an instance endpoint of an Aurora writer
	// is then Aurora before AWS is asked, and a contradicting engine is
	// an identity error instead of a mislabelled target.
	DetectedProvider string
	Credentials      aws.CredentialsProvider
	HTTPClient       *http.Client
	// Endpoint overrides (tests, VPC endpoints); default: AWS public ones.
	CloudWatchEndpoint, PIEndpoint, RDSEndpoint string
	Now                                         func() time.Time
	// IsolationMaxAge is how recent a manual snapshot must be to count as a
	// backup that survives a delete (backup.max_age_hours); 0 is 26 h.
	IsolationMaxAge time.Duration
}

// AWSSource collects CloudWatch and Performance Insights telemetry.
type AWSSource struct {
	opts     AWSOptions
	detected string // normalized DetectedProvider
	client   *awsClient
	rds      *rdsAPI

	mu       sync.Mutex
	resolved awsIdentity // as of the last resolved instance
	records  *rdsRecordsRead
}

// awsIdentity is what DescribeDBInstances (and, for an Aurora instance
// endpoint, DescribeDBClusters) said about the instance.
type awsIdentity struct {
	engine, cluster, role string
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
	case opts.DetectedProvider != "" &&
		!slices.Contains([]string{"rds", "aurora"},
			managedparam.NormalizeProvider(opts.DetectedProvider)):
		return nil, fmt.Errorf("%w: detected provider %q is not RDS or Aurora",
			ErrIdentity, opts.DetectedProvider)
	case opts.Credentials == nil:
		return nil, fmt.Errorf("%w: no AWS credential provider", ErrNoCredentials)
	}
	if opts.HTTPClient == nil {
		opts.HTTPClient = egress.HTTPClient(20 * time.Second)
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
		detected: managedparam.NormalizeProvider(opts.DetectedProvider),
		client: &awsClient{creds: creds, http: opts.HTTPClient, signer: v4.NewSigner(),
			region: opts.Region, now: opts.Now},
		rds: newRDSAPI(opts.Region, opts.RDSEndpoint, creds, opts.HTTPClient)}, nil
}

// Provider is "aurora" for a cluster endpoint, an instance PostgreSQL
// reports as Aurora, or one whose engine AWS reported as aurora*; else
// "rds".
func (s *AWSSource) Provider() string { return s.providerFor(s.identity().engine) }

// providerFor is Provider as of a resolve that found engine.
func (s *AWSSource) providerFor(engine string) string {
	if s.opts.ClusterID != "" || s.detected == "aurora" ||
		strings.HasPrefix(engine, "aurora") {
		return "aurora"
	}
	return "rds"
}

// ClusterMember is the Aurora cluster and the instance's role in it, as
// last resolved ("" before then and outside any cluster).
func (s *AWSSource) ClusterMember() (cluster, role string) {
	ident := s.identity()
	return ident.cluster, ident.role
}

func (s *AWSSource) identity() awsIdentity {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.resolved
}

// resolve finds the instance (the writer for a cluster) after checking
// that credentials exist, so nothing is sent without them. It returns the
// identity it recorded, so a caller never pairs this instance with the
// identity a concurrent resolve recorded.
func (s *AWSSource) resolve(ctx context.Context) (rdsInstance, awsIdentity, error) {
	if err := ctx.Err(); err != nil {
		return rdsInstance{}, awsIdentity{}, err
	}
	if _, err := s.client.credentials(ctx); err != nil {
		return rdsInstance{}, awsIdentity{}, err
	}
	id := s.opts.InstanceID
	var cluster rdsCluster
	if s.opts.ClusterID != "" {
		var err error
		if cluster, err = s.rds.clusterWriter(ctx, s.opts.ClusterID); err != nil {
			return rdsInstance{}, awsIdentity{}, err
		}
		id = cluster.Writer
	}
	inst, err := s.rds.instance(ctx, id)
	if err != nil {
		return rdsInstance{}, awsIdentity{}, fmt.Errorf("RDS instance %s: %w", id, err)
	}
	ident := awsIdentity{engine: inst.Engine, cluster: s.opts.ClusterID,
		role: managedparam.InstanceRoleWriter}
	if s.opts.ClusterID == "" {
		ident.role, cluster = s.memberRole(ctx, inst)
		ident.cluster = inst.ClusterID
		if err := ctx.Err(); err != nil {
			return rdsInstance{}, awsIdentity{}, err
		}
	}
	if cluster.LatestRestorable != nil {
		inst.LatestRestorable = cluster.LatestRestorable
	}
	if cluster.Writer != "" {
		inst.EarliestRestorable = cluster.EarliestRestorable
		inst.AWSBackupARN = cluster.AWSBackupARN
		inst.ClusterDeletionProtection = cluster.DeletionProtection
	}
	s.mu.Lock()
	s.resolved = ident
	s.mu.Unlock()
	return inst, ident, nil
}

// memberRole is an instance endpoint's role in its Aurora cluster ("" for
// an instance outside any cluster). An unreadable cluster leaves the role
// unknown rather than failing collection, which never needed it. The
// cluster is returned as read (zero when unread).
func (s *AWSSource) memberRole(ctx context.Context, inst rdsInstance) (string, rdsCluster) {
	if inst.ClusterID == "" {
		return "", rdsCluster{}
	}
	cluster, err := s.rds.clusterWriter(ctx, inst.ClusterID)
	switch {
	case err != nil:
		return managedparam.InstanceRoleUnknown, rdsCluster{}
	case cluster.Writer == inst.ID:
		return managedparam.InstanceRoleWriter, cluster
	}
	return managedparam.InstanceRoleReader, cluster
}

// checkDetected refuses an instance whose engine contradicts the platform
// PostgreSQL reports: proposals built for one would not match the
// advisor's intents (or would name the wrong kind of resource).
func (s *AWSSource) checkDetected(inst rdsInstance) error {
	if s.detected == "" {
		return nil
	}
	engine := "rds"
	if strings.HasPrefix(inst.Engine, "aurora") {
		engine = "aurora"
	}
	if engine == s.detected {
		return nil
	}
	return fmt.Errorf("%w: PostgreSQL reports %s but RDS instance %s runs engine %q "+
		"(%s); check that the connection host is this database's own endpoint",
		ErrIdentity, s.detected, inst.ID, inst.Engine, engine)
}

// Collect gathers one sample: CloudWatch is required, Performance
// Insights optional (its failure is partial data).
func (s *AWSSource) Collect(ctx context.Context, now time.Time) (Sample, error) {
	inst, ident, err := s.resolve(ctx)
	if err != nil {
		return Sample{}, err
	}
	aurora := s.opts.ClusterID != "" || strings.HasPrefix(inst.Engine, "aurora")
	provider := s.providerFor(ident.engine)
	sample := Sample{Provider: provider, CollectedAt: now,
		Resource:      provider + ":" + s.opts.Region + "/" + inst.ID,
		InstanceClass: inst.Class, VCPUs: InstanceClassVCPUs(inst.Class),
		Serverless: strings.EqualFold(inst.Class, "db.serverless")}
	queries, fields := cloudWatchQueries(inst, aurora)
	points, err := s.client.getMetricData(ctx, s.opts.CloudWatchEndpoint, queries, now)
	if err != nil {
		return Sample{}, err
	}
	if err := applyCloudWatch(&sample, points, fields, now); err != nil {
		return Sample{}, err
	}
	s.applyStorage(&sample, inst, aurora, now)
	deletionProtection := inst.DeletionProtection
	if aurora {
		deletionProtection = inst.ClusterDeletionProtection
	}
	sample.Backup = rdsBackup(inst.BackupRetention, deletionProtection, inst.LatestRestorable)
	s.applyIsolation(ctx, sample.Backup, inst, ident.cluster, now)
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
