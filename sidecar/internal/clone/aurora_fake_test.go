package clone_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/clone"
)

// auroraFake is a stateful stand-in for the RDS API (lab-05c): one Aurora
// PostgreSQL source cluster "shop" (writer shop-1) in us-east-1. A
// restored cluster and a new instance are "creating" for readyAfter
// describes, then "available". The knobs inject the failures the provider
// must handle; set them before the provider is used.
type auroraFake struct {
	mu        sync.Mutex
	clusters  map[string]*clone.AuroraCluster
	instances map[string]*clone.AuroraInstance
	polls     map[string]int
	calls     []string
	restores  []clone.AuroraRestoreRequest
	creates   []clone.AuroraInstanceRequest

	readyAfter        int
	neverReady        bool
	instanceEndStatus string // a new instance ends in this status ("": available)
	restoreErr        error
	restoreThenErr    error // the cluster is made, then this is returned
	createInstanceErr error
	deleteClusterErr  error
	deleteInstanceErr error
	listErr           error
	cloneEndpoint     string // every restored cluster answers on this host
}

const (
	auroraProdHost   = "shop.cluster-abc123def456.us-east-1.rds.amazonaws.com"
	auroraReaderHost = "shop.cluster-ro-abc123def456.us-east-1.rds.amazonaws.com"
	auroraPassword   = "s3cret-pw"
	auroraProdDSN    = "postgres://app:" + auroraPassword + "@" + auroraProdHost +
		":5432/shop?sslmode=verify-full&sslrootcert=%2Fetc%2Frds-ca.pem"
	auroraRegion = "us-east-1"
)

func newAuroraFake(t *testing.T) *auroraFake {
	t.Helper()
	f := &auroraFake{clusters: map[string]*clone.AuroraCluster{},
		instances: map[string]*clone.AuroraInstance{}, polls: map[string]int{}, readyAfter: 1}
	f.clusters["shop"] = &clone.AuroraCluster{ID: "shop", Engine: "aurora-postgresql",
		Status: "available", Endpoint: auroraProdHost, ReaderEndpoint: auroraReaderHost,
		Port: 5432, CloneGroupID: "cg-shop", SubnetGroup: "shop-subnets",
		ParameterGroup: "shop-cluster-params", SecurityGroups: []string{"sg-0a1", "sg-0b2"},
		Members: []clone.AuroraMember{{ID: "shop-1", Writer: true}, {ID: "shop-2"}},
		Tags:    map[string]string{"team": "shop"}, DeletionProtection: true}
	f.instances["shop-1"] = &clone.AuroraInstance{ID: "shop-1", ClusterID: "shop",
		Class: "db.r6g.large", Status: "available", ParameterGroup: "shop-params"}
	f.instances["shop-2"] = &clone.AuroraInstance{ID: "shop-2", ClusterID: "shop",
		Class: "db.r6g.xlarge", Status: "available", ParameterGroup: "shop-params"}
	return f
}

func (f *auroraFake) record(call string) {
	f.calls = append(f.calls, call)
}

func notFound(kind, id string) error {
	return fmt.Errorf("%w: %s %s", clone.ErrAuroraNotFound, kind, id)
}

func (f *auroraFake) DescribeCluster(_ context.Context, id string) (clone.AuroraCluster,
	error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("describe-cluster " + id)
	c, ok := f.clusters[id]
	if !ok {
		return clone.AuroraCluster{}, notFound("cluster", id)
	}
	f.advance(id, &c.Status, "available")
	return copyCluster(c), nil
}

func (f *auroraFake) DescribeInstance(_ context.Context, id string) (clone.AuroraInstance,
	error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("describe-instance " + id)
	in, ok := f.instances[id]
	if !ok {
		return clone.AuroraInstance{}, notFound("instance", id)
	}
	end := "available"
	if f.instanceEndStatus != "" {
		end = f.instanceEndStatus
	}
	f.advance(id, &in.Status, end)
	return *in, nil
}

// advance moves a creating resource to end after readyAfter describes.
func (f *auroraFake) advance(id string, status *string, end string) {
	if *status != "creating" || f.neverReady {
		return
	}
	f.polls[id]++
	if f.polls[id] >= f.readyAfter {
		*status = end
	}
}

func (f *auroraFake) ListClusters(context.Context) ([]clone.AuroraCluster, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("list-clusters")
	if f.listErr != nil {
		return nil, f.listErr
	}
	out := make([]clone.AuroraCluster, 0, len(f.clusters))
	for _, c := range f.clusters {
		out = append(out, copyCluster(c))
	}
	return out, nil
}

func (f *auroraFake) RestoreCluster(_ context.Context,
	req clone.AuroraRestoreRequest) (clone.AuroraCluster, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("restore " + req.ID + " from " + req.Source)
	f.restores = append(f.restores, req)
	if f.restoreErr != nil {
		return clone.AuroraCluster{}, f.restoreErr
	}
	if _, ok := f.clusters[req.ID]; ok {
		return clone.AuroraCluster{}, &clone.ProviderStatusError{Op: "fake restore",
			StatusCode: 400}
	}
	src := f.clusters[req.Source]
	endpoint := req.ID + ".cluster-abc123def456.us-east-1.rds.amazonaws.com"
	if f.cloneEndpoint != "" {
		endpoint = f.cloneEndpoint
	}
	c := &clone.AuroraCluster{ID: req.ID, Engine: src.Engine, Status: "creating",
		Endpoint: endpoint, ReaderEndpoint: strings.Replace(endpoint, ".cluster-",
			".cluster-ro-", 1), Port: src.Port, CloneGroupID: src.CloneGroupID,
		SubnetGroup: req.SubnetGroup, ParameterGroup: req.ParameterGroup,
		SecurityGroups: req.SecurityGroups, Serverless: req.Serverless, Tags: req.Tags}
	f.clusters[req.ID] = c
	if f.restoreThenErr != nil {
		return clone.AuroraCluster{}, f.restoreThenErr
	}
	return copyCluster(c), nil
}

func (f *auroraFake) CreateInstance(_ context.Context,
	req clone.AuroraInstanceRequest) (clone.AuroraInstance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("create-instance " + req.ID + " in " + req.ClusterID)
	f.creates = append(f.creates, req)
	if f.createInstanceErr != nil {
		return clone.AuroraInstance{}, f.createInstanceErr
	}
	c, ok := f.clusters[req.ClusterID]
	if !ok {
		return clone.AuroraInstance{}, notFound("cluster", req.ClusterID)
	}
	in := &clone.AuroraInstance{ID: req.ID, ClusterID: req.ClusterID, Class: req.Class,
		Status: "creating", ParameterGroup: req.ParameterGroup,
		PubliclyAccessible: req.PubliclyAccessible}
	f.instances[req.ID] = in
	c.Members = append(c.Members, clone.AuroraMember{ID: req.ID, Writer: len(c.Members) == 0})
	return *in, nil
}

func (f *auroraFake) DeleteInstance(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("delete-instance " + id)
	if f.deleteInstanceErr != nil {
		return f.deleteInstanceErr
	}
	in, ok := f.instances[id]
	if !ok {
		return notFound("instance", id)
	}
	delete(f.instances, id)
	if c, ok := f.clusters[in.ClusterID]; ok {
		kept := c.Members[:0]
		for _, m := range c.Members {
			if m.ID != id {
				kept = append(kept, m)
			}
		}
		c.Members = kept
	}
	return nil
}

func (f *auroraFake) DeleteCluster(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("delete-cluster " + id)
	if f.deleteClusterErr != nil {
		return f.deleteClusterErr
	}
	c, ok := f.clusters[id]
	if !ok {
		return notFound("cluster", id)
	}
	if len(c.Members) > 0 {
		return errors.New("InvalidDBClusterStateFault: the cluster still has instances")
	}
	delete(f.clusters, id)
	return nil
}

func copyCluster(c *clone.AuroraCluster) clone.AuroraCluster {
	out := *c
	out.Members = append([]clone.AuroraMember(nil), c.Members...)
	out.SecurityGroups = append([]string(nil), c.SecurityGroups...)
	out.Tags = map[string]string{}
	for k, v := range c.Tags {
		out.Tags[k] = v
	}
	return out
}

// addClones puts n copy-on-write clones of shop in its clone group.
func (f *auroraFake) addClones(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range n {
		id := fmt.Sprintf("shop-copy-%d", i)
		f.clusters[id] = &clone.AuroraCluster{ID: id, Engine: "aurora-postgresql",
			Status: "available", CloneGroupID: "cg-shop"}
	}
}

func (f *auroraFake) exists(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.clusters[id]
	return ok
}

func (f *auroraFake) instanceExists(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.instances[id]
	return ok
}

// callsWith are the calls that start with prefix, in order.
func (f *auroraFake) callsWith(prefix string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			out = append(out, c)
		}
	}
	return out
}

// mutating are the calls that change something, in order.
func (f *auroraFake) mutating() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		if !strings.HasPrefix(c, "describe-") && c != "list-clusters" {
			out = append(out, c)
		}
	}
	return out
}

func auroraConfig(f *auroraFake, mutate func(*clone.AuroraConfig)) clone.AuroraConfig {
	cfg := clone.AuroraConfig{Region: auroraRegion, TTL: time.Hour,
		API: func(context.Context) (clone.AuroraAPI, error) { return f, nil },
		Production: func(context.Context) (clone.AuroraProduction, error) {
			return clone.AuroraProduction{DSN: auroraProdDSN}, nil
		},
		WaitTimeout: 5 * time.Second, PollEvery: time.Millisecond,
		Now: func() time.Time { return time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC) }}
	if mutate != nil {
		mutate(&cfg)
	}
	return cfg
}

func newAuroraProvider(t *testing.T, f *auroraFake,
	mutate func(*clone.AuroraConfig)) *clone.AuroraCloneProvider {
	t.Helper()
	p, err := clone.NewAuroraCloneProvider(auroraConfig(f, mutate))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

var _ clone.AuroraAPI = (*auroraFake)(nil)
