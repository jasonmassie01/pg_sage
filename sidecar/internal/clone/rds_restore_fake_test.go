package clone_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/clone"
)

// rdsFake is a stateful stand-in for the RDS instance API (lab-05d): one
// RDS for PostgreSQL source instance "shop" in us-east-1 whose latest
// restorable time is five minutes before the fake clock. A restored
// instance is "creating" for readyAfter describes, then endStatus. The
// knobs inject the failures the provider must handle; set them before the
// provider is used.
type rdsFake struct {
	mu        sync.Mutex
	instances map[string]*clone.RDSInstance
	polls     map[string]int
	calls     []string
	restores  []clone.RDSRestoreRequest

	readyAfter     int
	neverReady     bool
	endStatus      string // a restored instance ends in this status ("": available)
	restoreErr     error  // nothing is made, this is returned
	restoreThenErr error  // the instance is made, then this is returned
	deleteErr      error
	describeErr    error // every describe fails with this
	cloneEndpoint  string
}

const (
	rdsProdHost = "shop.abc123def456.us-east-1.rds.amazonaws.com"
	rdsPassword = "rds-s3cret"
	rdsProdDSN  = "postgres://app:" + rdsPassword + "@" + rdsProdHost +
		":5432/shop?sslmode=prefer"
	rdsRegion = "us-east-1"
	rdsName   = "sage-rehearsal-0a1b2c3d"
)

var rdsNow = time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)

func newRDSFake(t *testing.T) *rdsFake {
	t.Helper()
	f := &rdsFake{instances: map[string]*clone.RDSInstance{}, polls: map[string]int{},
		readyAfter: 1}
	f.instances["shop"] = &clone.RDSInstance{ID: "shop", Engine: "postgres",
		Status: "available", Endpoint: rdsProdHost, Port: 5432, Class: "db.m6g.large",
		SubnetGroup: "shop-subnets", ParameterGroup: "shop-params", StorageType: "gp3",
		AllocatedStorageGB: 500, SecurityGroups: []string{"sg-0a1", "sg-0b2"},
		MultiAZ: true, DeletionProtection: true,
		LatestRestorableTime: rdsNow.Add(-5 * time.Minute),
		Tags:                 map[string]string{"team": "shop"}}
	return f
}

func (f *rdsFake) record(call string) { f.calls = append(f.calls, call) }

func rdsNotFound(id string) error {
	return fmt.Errorf("%w: instance %s", clone.ErrRDSNotFound, id)
}

func (f *rdsFake) DescribeInstance(_ context.Context, id string) (clone.RDSInstance,
	error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("describe " + id)
	if f.describeErr != nil {
		return clone.RDSInstance{}, f.describeErr
	}
	in, ok := f.instances[id]
	if !ok {
		return clone.RDSInstance{}, rdsNotFound(id)
	}
	if in.Status == "creating" && !f.neverReady {
		f.polls[id]++
		if f.polls[id] >= f.readyAfter {
			in.Status = "available"
			if f.endStatus != "" {
				in.Status = f.endStatus
			}
		}
	}
	return copyRDSInstance(in), nil
}

func (f *rdsFake) RestoreInstance(_ context.Context,
	req clone.RDSRestoreRequest) (clone.RDSInstance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("restore " + req.ID + " from " + req.Source)
	f.restores = append(f.restores, req)
	if f.restoreErr != nil {
		return clone.RDSInstance{}, f.restoreErr
	}
	if _, ok := f.instances[req.ID]; ok {
		return clone.RDSInstance{}, fmt.Errorf("%w: %s", clone.ErrRDSAlreadyExists, req.ID)
	}
	endpoint := req.ID + ".abc123def456.us-east-1.rds.amazonaws.com"
	if f.cloneEndpoint != "" {
		endpoint = f.cloneEndpoint
	}
	src := f.instances[req.Source]
	in := &clone.RDSInstance{ID: req.ID, Engine: src.Engine, Status: "creating",
		Endpoint: endpoint, Port: src.Port, Class: req.Class, SubnetGroup: req.SubnetGroup,
		ParameterGroup: req.ParameterGroup, StorageType: req.StorageType,
		AllocatedStorageGB: src.AllocatedStorageGB, SecurityGroups: req.SecurityGroups,
		PubliclyAccessible: req.PubliclyAccessible, Tags: req.Tags}
	f.instances[req.ID] = in
	if f.restoreThenErr != nil {
		return clone.RDSInstance{}, f.restoreThenErr
	}
	return copyRDSInstance(in), nil
}

func (f *rdsFake) DeleteInstance(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("delete " + id)
	if f.deleteErr != nil {
		return f.deleteErr
	}
	if _, ok := f.instances[id]; !ok {
		return rdsNotFound(id)
	}
	delete(f.instances, id)
	return nil
}

func copyRDSInstance(in *clone.RDSInstance) clone.RDSInstance {
	out := *in
	out.SecurityGroups = append([]string(nil), in.SecurityGroups...)
	out.Tags = map[string]string{}
	for k, v := range in.Tags {
		out.Tags[k] = v
	}
	return out
}

// put adds or replaces an instance.
func (f *rdsFake) put(in clone.RDSInstance) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.instances[in.ID] = &in
}

func (f *rdsFake) exists(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.instances[id]
	return ok
}

func (f *rdsFake) setSource(mutate func(*clone.RDSInstance)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	mutate(f.instances["shop"])
}

// mutating are the restore and delete calls, in order.
func (f *rdsFake) mutating() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		if !strings.HasPrefix(c, "describe ") {
			out = append(out, c)
		}
	}
	return out
}

func (f *rdsFake) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func rdsConfig(f *rdsFake, mutate func(*clone.RDSRestoreConfig)) clone.RDSRestoreConfig {
	cfg := clone.RDSRestoreConfig{Region: rdsRegion, Deadline: 2 * time.Hour,
		TTL: 3*time.Hour + 15*time.Minute,
		API: func(context.Context) (clone.RDSRestoreClient, error) { return f, nil },
		Production: func(context.Context) (clone.RDSProduction, error) {
			return clone.RDSProduction{DSN: rdsProdDSN}, nil
		},
		PollEvery: time.Millisecond, Now: func() time.Time { return rdsNow }}
	if mutate != nil {
		mutate(&cfg)
	}
	return cfg
}

func newRDSAPI(t *testing.T, f *rdsFake,
	mutate func(*clone.RDSRestoreConfig)) *clone.RDSRestoreAPI {
	t.Helper()
	a, err := clone.NewRDSRestoreAPI(rdsConfig(f, mutate))
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func newRDSProvider(t *testing.T, f *rdsFake,
	mutate func(*clone.RDSRestoreConfig)) *clone.SnapshotProvider {
	t.Helper()
	p, err := clone.NewRDSRestoreProvider(rdsConfig(f, mutate))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

var _ clone.RDSRestoreClient = (*rdsFake)(nil)
