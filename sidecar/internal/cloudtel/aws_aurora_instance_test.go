package cloudtel

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/managedparam"
)

// auroraInstanceFixture is an Aurora PostgreSQL cluster "shop" whose
// writer writer-1 and reader reader-2 are reached by their own instance
// endpoints (writer-1.abc.us-east-1.rds.amazonaws.com), not the cluster
// endpoint.
func auroraInstanceFixture(t *testing.T) *fakeAWS {
	f := newFakeAWS(t, awsNow)
	f.clusters["shop"] = clusterXML("shop", "writer-1", "reader-2")
	f.instances["writer-1"] = instanceXML("writer-1", "db.r7g.xlarge", "aurora-postgresql",
		"shop-writer-pg16", "in-sync", 1, 0, false, "shop")
	f.instances["reader-2"] = instanceXML("reader-2", "db.r7g.xlarge", "aurora-postgresql",
		"shop-reader-pg16", "in-sync", 1, 0, false, "shop")
	for _, id := range []string{"writer-1", "reader-2"} {
		f.metric(id, "CPUUtilization", time.Minute, 12)
		f.metric(id, "FreeableMemory", time.Minute, 20*gib)
		f.metric(id, "AuroraReplicaLagMaximum", time.Minute, 1500)
	}
	f.params["shop-writer-pg16"] = []fakeParam{
		{"shared_buffers", "{DBInstanceClassMemory/12038}", "engine-default", "static", true}}
	f.params["shop-reader-pg16"] = []fakeParam{
		{"shared_buffers", "{DBInstanceClassMemory/12038}", "engine-default", "static", true}}
	return f
}

func auroraIntent(t *testing.T) managedparam.Intent {
	t.Helper()
	in, err := managedparam.NewIntent("aurora", "shared_buffers", "8GB", "requires a restart")
	if err != nil {
		t.Fatalf("NewIntent: %v", err)
	}
	return in
}

func pollRuntime(t *testing.T, src Source) *Runtime {
	t.Helper()
	rt, err := NewRuntime(src, RuntimeOptions{Database: "shop",
		Now: func() time.Time { return awsNow }})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	if err := rt.Poll(context.Background()); err != nil {
		t.Fatalf("Poll: %v", err)
	}
	return rt
}

// Happy path: the writer's instance endpoint is Aurora, not RDS, so the
// status, the sample and the managed-change target all say aurora and an
// Aurora intent builds a proposal that names the cluster.
func TestAuroraInstanceEndpointReportsAurora(t *testing.T) {
	f := auroraInstanceFixture(t)
	src := f.source(t, AWSOptions{InstanceID: "writer-1", DetectedProvider: "aurora"})
	if src.Provider() != "aurora" {
		t.Fatalf("Provider before any call = %q, want aurora", src.Provider())
	}
	st := pollRuntime(t, src).Status()
	if st.Provider != "aurora" || st.Resource != "aurora:us-east-1/writer-1" ||
		st.Cluster != "shop" || st.InstanceRole != managedparam.InstanceRoleWriter {
		t.Fatalf("status = provider %q resource %q cluster %q role %q", st.Provider,
			st.Resource, st.Cluster, st.InstanceRole)
	}
	target, err := src.Target(context.Background())
	if err != nil {
		t.Fatalf("Target: %v", err)
	}
	if target.Provider != "aurora" || target.InstanceID != "writer-1" ||
		target.ParameterGroup != "shop-writer-pg16" || target.Cluster != "shop" ||
		target.InstanceRole != managedparam.InstanceRoleWriter || !target.Known {
		t.Fatalf("target = %+v", target)
	}
	p, err := managedparam.Build(auroraIntent(t), target, nil)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	notes := strings.Join(p.Notes, " ")
	if p.Provider != "aurora" || p.Target != "shop-writer-pg16" ||
		!strings.Contains(notes, "cluster shop") || strings.Contains(notes, "may differ") {
		t.Fatalf("proposal target %q notes %q", p.Target, notes)
	}
}

// Nil/empty: without a detected provider the instance endpoint is RDS
// until the engine says otherwise, then Aurora from then on (state
// transition), and the runtime reads the provider on each Status.
func TestAuroraInstanceEndpointLearnsEngineWithoutDetection(t *testing.T) {
	f := auroraInstanceFixture(t)
	src := f.source(t, AWSOptions{InstanceID: "writer-1"})
	rt, err := NewRuntime(src, RuntimeOptions{Database: "shop",
		Now: func() time.Time { return awsNow }})
	if err != nil {
		t.Fatal(err)
	}
	if got := rt.Status().Provider; got != "rds" {
		t.Fatalf("provider before the first poll = %q, want rds", got)
	}
	if err := rt.Poll(context.Background()); err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if st := rt.Status(); st.Provider != "aurora" || st.Resource != "aurora:us-east-1/writer-1" {
		t.Fatalf("after the engine is known: provider %q resource %q", st.Provider,
			st.Resource)
	}
	f.fail["rds:DescribeDBInstances"] = fakeFailure{status: 400, code: "Throttling",
		message: "slow down"}
	_ = rt.Poll(context.Background())
	if got := src.Provider(); got != "aurora" {
		t.Fatalf("a failed poll must not forget the engine: provider = %q", got)
	}
}

// Boundary: a plain RDS instance endpoint with a postgres engine stays
// rds, names no cluster and never asks for one.
func TestAuroraBoundaryRDSInstanceStaysRDS(t *testing.T) {
	for _, detected := range []string{"", "rds", "aws"} {
		t.Run("detected="+detected, func(t *testing.T) {
			f := rdsFixture(t, true)
			src := f.source(t, AWSOptions{InstanceID: "orders", DetectedProvider: detected})
			st := pollRuntime(t, src).Status()
			if st.Provider != "rds" || st.Cluster != "" || st.InstanceRole != "" {
				t.Fatalf("status = %q cluster %q role %q", st.Provider, st.Cluster,
					st.InstanceRole)
			}
			target, err := src.Target(context.Background())
			if err != nil || target.Provider != "rds" || target.Cluster != "" ||
				target.InstanceRole != "" {
				t.Fatalf("target = %+v, %v", target, err)
			}
			if n := f.called("rds:DescribeDBClusters"); n != 0 {
				t.Fatalf("DescribeDBClusters calls = %d, want 0 for plain RDS", n)
			}
		})
	}
}

// Error propagation: a detected platform the engine contradicts is one
// identity error naming the instance and both providers.
func TestAuroraTargetRefusesProviderMismatch(t *testing.T) {
	cases := []struct {
		name, id, detected, wantEngine string
	}{
		{"aurora detected, postgres engine", "orders", "aurora", "postgres"},
		{"rds detected, aurora engine", "writer-1", "rds", "aurora-postgresql"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := auroraInstanceFixture(t)
			f.instances["orders"] = instanceXML("orders", "db.r6g.large", "postgres",
				"orders-pg16", "in-sync", 100, 0, false, "")
			src := f.source(t, AWSOptions{InstanceID: tc.id, DetectedProvider: tc.detected})
			_, err := src.Target(context.Background())
			if !errors.Is(err, ErrIdentity) {
				t.Fatalf("err = %v, want ErrIdentity", err)
			}
			for _, want := range []string{tc.id, tc.detected, tc.wantEngine} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("err %q does not name %q", err, want)
				}
			}
			if n := f.called("rds:DescribeDBParameters"); n != 0 {
				t.Fatalf("parameters read for a mismatched target: %d calls", n)
			}
		})
	}
}

// Error propagation: a failed DescribeDBInstances names the instance and
// keeps its class, so the worker reports one target error.
func TestAuroraTargetDescribeFailureNamesInstance(t *testing.T) {
	f := auroraInstanceFixture(t)
	f.fail["rds:DescribeDBInstances"] = fakeFailure{status: 403, code: "AccessDenied",
		message: "rds:DescribeDBInstances denied"}
	src := f.source(t, AWSOptions{InstanceID: "writer-1", DetectedProvider: "aurora"})
	_, err := src.Target(context.Background())
	if !errors.Is(err, ErrAuth) || !strings.Contains(err.Error(), "writer-1") {
		t.Fatalf("err = %v, want ErrAuth naming writer-1", err)
	}
	assertNoSecrets(t, err)
}

// Nil/empty: an empty detected provider keeps today's engine-free
// behaviour for a cluster endpoint, whatever the writer's engine says.
func TestAuroraClusterEndpointWithoutDetectionIsEngineFree(t *testing.T) {
	f := auroraInstanceFixture(t)
	f.clusters["odd"] = clusterXML("odd", "odd-1")
	f.instances["odd-1"] = instanceXML("odd-1", "db.r7g.xlarge", "postgres",
		"shop-writer-pg16", "in-sync", 1, 0, false, "odd")
	src := f.source(t, AWSOptions{ClusterID: "odd"})
	target, err := src.Target(context.Background())
	if err != nil || target.Provider != "aurora" || target.Cluster != "odd" ||
		target.InstanceRole != managedparam.InstanceRoleWriter {
		t.Fatalf("target = %+v, %v", target, err)
	}
}

// Invalid input: a detected provider that is not AWS is an identity error.
func TestAuroraRejectsNonAWSDetectedProvider(t *testing.T) {
	for _, detected := range []string{"cloud-sql", "azure", "neon"} {
		_, err := NewAWSSource(AWSOptions{Region: "us-east-1", InstanceID: "orders",
			DetectedProvider: detected, Credentials: staticCreds()})
		if !errors.Is(err, ErrIdentity) || !strings.Contains(err.Error(), detected) {
			t.Fatalf("detected %q: err = %v, want ErrIdentity naming it", detected, err)
		}
	}
	src, err := NewAWSSource(AWSOptions{Region: "us-east-1", InstanceID: "orders",
		DetectedProvider: " AURORA ", Credentials: staticCreds()})
	if err != nil || src.Provider() != "aurora" {
		t.Fatalf("normalized detection: %v, provider %v", err, src)
	}
}

// State transitions: a reader instance reports reader, its proposal warns
// that its parameter group may differ, and a failover that makes it the
// writer is seen on the next poll.
func TestAuroraReaderInstanceAndFailover(t *testing.T) {
	f := auroraInstanceFixture(t)
	src := f.source(t, AWSOptions{InstanceID: "reader-2", DetectedProvider: "aurora"})
	rt := pollRuntime(t, src)
	if st := rt.Status(); st.InstanceRole != managedparam.InstanceRoleReader ||
		st.Cluster != "shop" {
		t.Fatalf("reader status: cluster %q role %q", st.Cluster, st.InstanceRole)
	}
	target, err := src.Target(context.Background())
	if err != nil || target.InstanceRole != managedparam.InstanceRoleReader {
		t.Fatalf("reader target = %+v, %v", target, err)
	}
	p, err := managedparam.Build(auroraIntent(t), target, nil)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	notes := strings.Join(p.Notes, " ")
	if !strings.Contains(notes, "reader-2 is a reader of Aurora cluster shop") ||
		!strings.Contains(notes, "may differ") {
		t.Fatalf("reader notes = %q", notes)
	}
	f.mu.Lock()
	f.clusters["shop"] = clusterXML("shop", "reader-2", "writer-1")
	f.mu.Unlock()
	if err := rt.Poll(context.Background()); err != nil {
		t.Fatalf("Poll after failover: %v", err)
	}
	if st := rt.Status(); st.InstanceRole != managedparam.InstanceRoleWriter {
		t.Fatalf("after failover role = %q, want writer", st.InstanceRole)
	}
}

// Error propagation, partial: without rds:DescribeDBClusters an instance
// endpoint still collects metrics (as before) and reports the role as
// unknown; the proposal says it could not tell.
func TestAuroraRoleUnknownWhenClusterUnreadable(t *testing.T) {
	f := auroraInstanceFixture(t)
	f.fail["rds:DescribeDBClusters"] = fakeFailure{status: 403, code: "AccessDenied",
		message: "rds:DescribeDBClusters denied"}
	src := f.source(t, AWSOptions{InstanceID: "writer-1", DetectedProvider: "aurora"})
	st := pollRuntime(t, src).Status()
	if !st.Available || st.InstanceRole != managedparam.InstanceRoleUnknown ||
		st.Cluster != "shop" {
		t.Fatalf("status available %v cluster %q role %q", st.Available, st.Cluster,
			st.InstanceRole)
	}
	target, err := src.Target(context.Background())
	if err != nil || target.InstanceRole != managedparam.InstanceRoleUnknown {
		t.Fatalf("target = %+v, %v", target, err)
	}
	p, err := managedparam.Build(auroraIntent(t), target, nil)
	if err != nil || !strings.Contains(strings.Join(p.Notes, " "), "could not tell") {
		t.Fatalf("notes = %v, %v", p.Notes, err)
	}
}

// Concurrent access: Provider and Status are read while polls record the
// resolved engine and role.
func TestAuroraIdentityConcurrentReads(t *testing.T) {
	f := auroraInstanceFixture(t)
	src := f.source(t, AWSOptions{InstanceID: "writer-1"})
	rt, err := NewRuntime(src, RuntimeOptions{Database: "shop",
		Now: func() time.Time { return awsNow }})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			_ = rt.Poll(context.Background())
		}()
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				if p := rt.Status().Provider; p != "rds" && p != "aurora" {
					t.Errorf("provider = %q", p)
				}
			}
		}()
	}
	wg.Wait()
	if st := rt.Status(); st.Provider != "aurora" || st.InstanceRole != "writer" {
		t.Fatalf("after concurrent polls: provider %q role %q", st.Provider, st.InstanceRole)
	}
}
