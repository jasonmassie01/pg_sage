package discovery

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/rds"
	rdstypes "github.com/aws/aws-sdk-go-v2/service/rds/types"
	"github.com/databricks/databricks-sdk-go/listing"
	"github.com/databricks/databricks-sdk-go/service/postgres"
	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/lakebaseid"
	"github.com/pg-sage/sidecar/internal/store"
)

// Beginner opens a transaction (a member's *pgxpool.Pool).
type Beginner interface {
	BeginTx(ctx context.Context, opts pgx.TxOptions) (pgx.Tx, error)
}

// ClusterSource lists the other databases on an existing member's server,
// read-only and under statement and lock timeouts.
type ClusterSource struct {
	name, host string
	port, cap  int
	pool       func() (Beginner, error)
}

// NewClusterSource lists through pool, resolved each cycle so a replaced
// member pool is followed.
func NewClusterSource(name, host string, port int, pool func() (Beginner, error),
	cap int) *ClusterSource {
	return &ClusterSource{name: name, host: host, port: port, cap: cap, pool: pool}
}

// Name is the source name.
func (c *ClusterSource) Name() string { return c.name }

// List returns every connectable non-template database except the
// member's own.
func (c *ClusterSource) List(ctx context.Context) ([]Candidate, error) {
	pool, err := c.pool()
	if err != nil {
		return nil, fmt.Errorf("cluster source %s: %w", c.name, err)
	}
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, fmt.Errorf("cluster source %s: begin read-only listing: %w", c.name, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SET LOCAL statement_timeout = '5s';
		SET LOCAL lock_timeout = '1s'`); err != nil {
		return nil, fmt.Errorf("cluster source %s: set timeouts: %w", c.name, err)
	}
	rows, err := tx.Query(ctx, `/* pg_sage */ SELECT datname FROM pg_catalog.pg_database
		 WHERE NOT datistemplate AND datallowconn AND datname <> current_database()
		 ORDER BY datname LIMIT $1`, c.cap)
	if err != nil {
		return nil, fmt.Errorf("cluster source %s: list pg_database: %w", c.name, err)
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("cluster source %s: read pg_database: %w", c.name, err)
	}
	out := make([]Candidate, 0, len(names))
	for _, n := range names {
		out = append(out, Candidate{ExternalID: c.host + ":" + strconv.Itoa(c.port) + "/" + n,
			Host: c.host, Port: c.port, DBName: n})
	}
	return out, nil
}

// LakebaseAPI is the part of the Databricks Postgres API discovery uses.
type LakebaseAPI interface {
	ListBranches(ctx context.Context,
		r postgres.ListBranchesRequest) listing.Iterator[postgres.Branch]
	ListEndpoints(ctx context.Context,
		r postgres.ListEndpointsRequest) listing.Iterator[postgres.Endpoint]
}

// lakebaseDatabase is the database every Lakebase endpoint serves.
const lakebaseDatabase = "databricks_postgres"

// LakebaseSource lists the endpoints of every branch of one project.
type LakebaseSource struct {
	name, project string
	api           LakebaseAPI
	cap           int
}

// NewLakebaseSource lists project's branch endpoints, at most cap.
func NewLakebaseSource(name, project string, api LakebaseAPI, cap int) *LakebaseSource {
	return &LakebaseSource{name: name, project: project, api: api, cap: cap}
}

// Name is the source name.
func (l *LakebaseSource) Name() string { return l.name }

// List walks branches and their endpoints; both walks stop at the cap, so
// a page-token loop cannot spin. pg_sage's own rehearsal branches are
// skipped but still count toward the branch walk.
func (l *LakebaseSource) List(ctx context.Context) ([]Candidate, error) {
	parent := "projects/" + l.project
	branches := l.api.ListBranches(ctx, postgres.ListBranchesRequest{Parent: parent})
	var out []Candidate
	for walked := 0; walked < l.cap && len(out) < l.cap && branches.HasNext(ctx); walked++ {
		b, err := branches.Next(ctx)
		if err != nil {
			return nil, fmt.Errorf("lakebase source %s: list branches of %s: %w",
				l.name, parent, err)
		}
		if isRehearsalBranch(b.Name) {
			continue
		}
		eps := l.api.ListEndpoints(ctx, postgres.ListEndpointsRequest{Parent: b.Name})
		// Endpoints without a host add no candidate, so the walk counts too.
		for seen := 0; seen < l.cap && len(out) < l.cap && eps.HasNext(ctx); seen++ {
			ep, err := eps.Next(ctx)
			if err != nil {
				return nil, fmt.Errorf("lakebase source %s: list endpoints of %s: %w",
					l.name, b.Name, err)
			}
			if c, ok := lakebaseCandidate(ep); ok {
				out = append(out, c)
			}
		}
	}
	return out, nil
}

// isRehearsalBranch reports whether name (projects/<p>/branches/<id>, or a
// bare id) is a branch pg_sage created for a rehearsal: its id, the last
// path segment, starts with the rehearsal prefix.
func isRehearsalBranch(name string) bool {
	id := name[strings.LastIndex(name, "/")+1:]
	return strings.HasPrefix(id, lakebaseid.RehearsalBranchPrefix)
}

func lakebaseCandidate(ep postgres.Endpoint) (Candidate, bool) {
	if ep.Status == nil || ep.Status.Hosts == nil || ep.Status.Hosts.Host == "" {
		return Candidate{}, false
	}
	id := ep.Uid
	if id == "" {
		id = ep.Name
	}
	return Candidate{ExternalID: id, Host: ep.Status.Hosts.Host, Port: 5432,
		DBName: lakebaseDatabase}, true
}

// RDSAPI is the part of the RDS API discovery uses.
type RDSAPI interface {
	DescribeDBInstances(ctx context.Context, in *rds.DescribeDBInstancesInput,
		opts ...func(*rds.Options)) (*rds.DescribeDBInstancesOutput, error)
}

// RDSSource lists PostgreSQL instances carrying a tag.
type RDSSource struct {
	name, tagKey, tagValue string
	api                    RDSAPI
	cap                    int
}

// NewRDSSource lists instances tagged tagKey (=tagValue when set).
func NewRDSSource(name, tagKey, tagValue string, api RDSAPI, cap int) *RDSSource {
	return &RDSSource{name: name, tagKey: tagKey, tagValue: tagValue, api: api, cap: cap}
}

// Name is the source name.
func (s *RDSSource) Name() string { return s.name }

// rdsMaxPages bounds the marker walk whatever the cap.
const rdsMaxPages = 20

// List pages DescribeDBInstances (engine-filtered; RDS has no tag filter
// there) and keeps tagged instances with an endpoint.
func (s *RDSSource) List(ctx context.Context) ([]Candidate, error) {
	in := &rds.DescribeDBInstancesInput{MaxRecords: aws.Int32(100),
		Filters: []rdstypes.Filter{{Name: aws.String("engine"),
			Values: []string{"postgres", "aurora-postgresql"}}}}
	seen := map[string]bool{}
	var out []Candidate
	for page := 0; page < rdsMaxPages && len(out) < s.cap; page++ {
		res, err := s.api.DescribeDBInstances(ctx, in)
		if err != nil {
			return nil, fmt.Errorf("rds source %s: RDS DescribeDBInstances: %w", s.name, err)
		}
		for _, d := range res.DBInstances {
			c, ok := s.candidate(d)
			if ok && !seen[c.ExternalID] && len(out) < s.cap {
				seen[c.ExternalID] = true
				out = append(out, c)
			}
		}
		next := aws.ToString(res.Marker)
		if next == "" || next == aws.ToString(in.Marker) {
			break
		}
		in.Marker = aws.String(next)
	}
	return out, nil
}

func (s *RDSSource) candidate(d rdstypes.DBInstance) (Candidate, bool) {
	if d.Endpoint == nil || aws.ToString(d.Endpoint.Address) == "" || !s.tagged(d.TagList) {
		return Candidate{}, false
	}
	db := aws.ToString(d.DBName)
	if db == "" {
		db = "postgres"
	}
	return Candidate{ExternalID: aws.ToString(d.DbiResourceId),
		Host: aws.ToString(d.Endpoint.Address), Port: int(aws.ToInt32(d.Endpoint.Port)),
		DBName: db}, aws.ToString(d.DbiResourceId) != ""
}

func (s *RDSSource) tagged(tags []rdstypes.Tag) bool {
	for _, t := range tags {
		if aws.ToString(t.Key) == s.tagKey &&
			(s.tagValue == "" || aws.ToString(t.Value) == s.tagValue) {
			return true
		}
	}
	return false
}

// SourceDeps builds the sources' clients. Cloud clients are made only for
// configured sources.
type SourceDeps struct {
	ClusterPool func(member string) (Beginner, error)
	Member      func(member string) (host string, port int, err error)
	Lakebase    func() (LakebaseAPI, error)
	RDS         func(region string) (RDSAPI, error)
}

// Bindings builds a binding per configured source. Only cluster sources
// with reuse_credentials enrol; Lakebase and RDS candidates stay
// candidates (no credential path for them yet).
func Bindings(cfg config.FleetDiscoveryConfig, deps SourceDeps) ([]Binding, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	out := make([]Binding, 0, len(cfg.Sources))
	for _, sc := range cfg.Sources {
		src, err := buildSource(sc, deps)
		if err != nil {
			return nil, fmt.Errorf("fleet.discovery source %s: %w", sc.Name, err)
		}
		b := Binding{Source: src, Owner: sc.Owner}
		if sc.Type == "cluster" && sc.ReuseCredentials {
			b.EnrolFrom = sc.Member
		}
		out = append(out, b)
	}
	return out, nil
}

func buildSource(sc config.DiscoverySourceConfig, deps SourceDeps) (Source, error) {
	switch sc.Type {
	case "cluster":
		host, port, err := deps.Member(sc.Member)
		if err != nil {
			return nil, err
		}
		member := sc.Member
		return NewClusterSource(sc.Name, host, port,
			func() (Beginner, error) { return deps.ClusterPool(member) }, CycleCap), nil
	case "lakebase":
		api, err := deps.Lakebase()
		if err != nil {
			return nil, err
		}
		return NewLakebaseSource(sc.Name, sc.Project, api, CycleCap), nil
	default: // rds; Validate admits nothing else
		api, err := deps.RDS(sc.Region)
		if err != nil {
			return nil, err
		}
		return NewRDSSource(sc.Name, sc.TagKey, sc.TagValue, api, CycleCap), nil
	}
}

var memberNameUnsafe = regexp.MustCompile(`[^a-zA-Z0-9_-]+`)

// MemberName is the fleet name of an enrolled row: source, database and
// row id, at most 63 characters, unique by the row id.
func MemberName(row store.DiscoveredRow) string {
	suffix := "-" + strconv.FormatInt(row.ID, 10)
	base := memberNameUnsafe.ReplaceAllString(row.Source+"-"+row.DBName, "_")
	if room := 63 - len(suffix); len(base) > room {
		base = base[:room]
	}
	return base + suffix
}
