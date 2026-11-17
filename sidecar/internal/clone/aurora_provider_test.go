package clone_test

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/clone"
)

// lab-05c: aurora_clone restores a copy-on-write clone of the monitored
// Aurora PostgreSQL cluster, adds one instance, and hands out the
// monitored DSN on the clone's endpoint.

const auroraName = "sage-rehearsal-0a1b2c3d"

func TestAuroraCreate_HappyPathRestoresThenAddsAnInstance(t *testing.T) {
	f := newAuroraFake(t)
	p := newAuroraProvider(t, f, nil)
	c, err := p.Create(context.Background(), clone.CloneSpec{ResourceName: auroraName})
	if err != nil {
		t.Fatal(err)
	}
	if c.ID != auroraName || !f.exists(auroraName) || !f.instanceExists(auroraName+"-1") {
		t.Fatalf("clone = %+v, cluster %v instance %v", c, f.exists(auroraName),
			f.instanceExists(auroraName+"-1"))
	}
	if err := p.Destroy(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	want := []string{"restore " + auroraName + " from shop",
		"create-instance " + auroraName + "-1 in " + auroraName,
		"delete-instance " + auroraName + "-1", "delete-cluster " + auroraName}
	if got := f.mutating(); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("mutating calls = %q, want %q", got, want)
	}
	if f.exists(auroraName) || f.instanceExists(auroraName+"-1") {
		t.Fatal("the clone outlived its destroy")
	}
}

func TestAuroraCreate_RestoreCopiesTheSourceNetworkAndTagsTheReceipt(t *testing.T) {
	f := newAuroraFake(t)
	p := newAuroraProvider(t, f, nil)
	if _, err := p.Create(context.Background(),
		clone.CloneSpec{ResourceName: auroraName}); err != nil {
		t.Fatal(err)
	}
	r := f.restores[0]
	if r.ID != auroraName || r.Source != "shop" || r.SubnetGroup != "shop-subnets" ||
		r.ParameterGroup != "shop-cluster-params" ||
		strings.Join(r.SecurityGroups, ",") != "sg-0a1,sg-0b2" || r.Serverless != nil {
		t.Fatalf("restore request = %+v", r)
	}
	if r.Tags[clone.AuroraReceiptTag] != auroraName ||
		r.Tags[clone.AuroraExpiresTag] != "2026-11-16T13:00:00Z" {
		t.Fatalf("restore tags = %v", r.Tags)
	}
	in := f.creates[0]
	if in.ID != auroraName+"-1" || in.ClusterID != auroraName || in.Class != "db.r6g.large" ||
		in.ParameterGroup != "shop-params" || in.PubliclyAccessible ||
		in.Tags[clone.AuroraReceiptTag] != auroraName {
		t.Fatalf("instance request = %+v: want the writer's class and parameter group", in)
	}
}

func TestAuroraCreate_DSNIsTheMonitoredDSNOnTheCloneEndpoint(t *testing.T) {
	f := newAuroraFake(t)
	p := newAuroraProvider(t, f, nil)
	c, err := p.Create(context.Background(), clone.CloneSpec{ResourceName: auroraName})
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(c.DSN)
	if err != nil {
		t.Fatal(err)
	}
	pw, _ := u.User.Password()
	q := u.Query()
	if u.Hostname() != auroraName+".cluster-abc123def456.us-east-1.rds.amazonaws.com" ||
		u.Port() != "5432" || u.User.Username() != "app" || pw != auroraPassword ||
		u.Path != "/shop" || q.Get("sslmode") != "verify-full" ||
		q.Get("sslrootcert") != "/etc/rds-ca.pem" {
		t.Fatalf("clone DSN host %s port %s user %s path %s query %v", u.Hostname(),
			u.Port(), u.User.Username(), u.Path, q)
	}
}

func TestAuroraCloneDSN_KeywordFormAndTLSFloor(t *testing.T) {
	const host = "c.cluster-abc123def456.us-east-1.rds.amazonaws.com"
	cases := []struct{ name, dsn, want string }{
		{"keyword", "host=" + auroraProdHost + " port=5432 user=app password='a b' " +
			"dbname=shop sslmode=require",
			"host=" + host + " port=5432 user=app password='a b' dbname=shop sslmode=require"},
		{"keyword without sslmode", "host=" + auroraProdHost + " user=app dbname=shop",
			"host=" + host + " user=app dbname=shop sslmode=require"},
		{"url sslmode disable", "postgres://app@" + auroraProdHost + "/shop?sslmode=disable",
			"postgres://app@" + host + ":5432/shop?sslmode=require"},
		{"url no port", "postgresql://app@" + auroraProdHost + "/shop",
			"postgresql://app@" + host + ":5432/shop?sslmode=require"},
	}
	for _, tc := range cases {
		got, err := clone.AuroraCloneDSN(tc.dsn, auroraProdHost, host, 5432)
		if err != nil || got != tc.want {
			t.Errorf("%s: got %q, %v; want %q", tc.name, got, err, tc.want)
		}
	}
}

func TestAuroraCloneDSN_AmbiguousTargetsAreRefused(t *testing.T) {
	const host = "c.cluster-abc123def456.us-east-1.rds.amazonaws.com"
	cases := map[string]string{
		"hostaddr pins production's IP": "host=" + auroraProdHost +
			" hostaddr=10.0.0.5 user=app password=" + auroraPassword,
		"two hosts":          "postgres://app:" + auroraPassword + "@a:5432,b:5432/shop",
		"host query param":   "postgres://app@" + host + "/shop?host=" + auroraProdHost,
		"host not the prod":  "postgres://app:" + auroraPassword + "@other.example.com/shop",
		"empty":              "",
		"unterminated quote": "host=" + auroraProdHost + " password='" + auroraPassword,
	}
	for name, dsn := range cases {
		_, err := clone.AuroraCloneDSN(dsn, auroraProdHost, host, 5432)
		if !errors.Is(err, clone.ErrInvalidAuroraConfig) {
			t.Errorf("%s: err = %v, want ErrInvalidAuroraConfig", name, err)
		}
		if err != nil && strings.Contains(err.Error(), auroraPassword) {
			t.Errorf("%s: the error quotes the password: %v", name, err)
		}
	}
}

func TestAuroraCreate_InstanceClassSettingWins(t *testing.T) {
	f := newAuroraFake(t)
	p := newAuroraProvider(t, f, func(c *clone.AuroraConfig) { c.InstanceClass = "db.t4g.medium" })
	if _, err := p.Create(context.Background(),
		clone.CloneSpec{ResourceName: auroraName}); err != nil {
		t.Fatal(err)
	}
	if f.creates[0].Class != "db.t4g.medium" {
		t.Fatalf("instance class = %q, want the configured one", f.creates[0].Class)
	}
}

func TestAuroraCreate_ServerlessSourceGivesAServerlessClone(t *testing.T) {
	f := newAuroraFake(t)
	f.clusters["shop"].Serverless = &clone.AuroraScaling{Min: 0.5, Max: 16}
	f.instances["shop-1"].Class = clone.AuroraServerlessClass
	p := newAuroraProvider(t, f, nil)
	if _, err := p.Create(context.Background(),
		clone.CloneSpec{ResourceName: auroraName}); err != nil {
		t.Fatal(err)
	}
	if s := f.restores[0].Serverless; s == nil || s.Min != 0.5 || s.Max != 16 ||
		f.creates[0].Class != clone.AuroraServerlessClass {
		t.Fatalf("restore scaling %+v, class %q", s, f.creates[0].Class)
	}
}

func TestAuroraCreate_ServerlessClassNeedsAServerlessSource(t *testing.T) {
	f := newAuroraFake(t)
	p := newAuroraProvider(t, f, func(c *clone.AuroraConfig) {
		c.InstanceClass = clone.AuroraServerlessClass
	})
	_, err := p.Create(context.Background(), clone.CloneSpec{ResourceName: auroraName})
	if !errors.Is(err, clone.ErrInvalidAuroraConfig) || len(f.mutating()) != 0 {
		t.Fatalf("err = %v, calls %q: want refused before any restore", err, f.mutating())
	}
}

// Invalid input: only an Aurora PostgreSQL cluster endpoint names a source.
func TestAuroraCreate_NotAnAuroraClusterIsRefused(t *testing.T) {
	cases := map[string]func(*auroraFake) string{
		"instance endpoint": func(*auroraFake) string {
			return "postgres://app@shop-1.abc123def456.us-east-1.rds.amazonaws.com/shop"
		},
		"custom dns": func(*auroraFake) string { return "postgres://app@db.example.com/shop" },
		"custom endpoint": func(*auroraFake) string {
			return "postgres://app@etl.cluster-custom-abc123def456.us-east-1." +
				"rds.amazonaws.com/shop"
		},
		"aurora mysql": func(f *auroraFake) string {
			f.clusters["shop"].Engine = "aurora-mysql"
			return auroraProdDSN
		},
		"endpoint is not the cluster's": func(f *auroraFake) string {
			f.clusters["shop"].Endpoint = "other.cluster-abc123def456.us-east-1.rds.amazonaws.com"
			f.clusters["shop"].ReaderEndpoint = ""
			return auroraProdDSN
		},
		"source cluster gone": func(f *auroraFake) string {
			delete(f.clusters, "shop")
			return auroraProdDSN
		},
	}
	for name, setup := range cases {
		f := newAuroraFake(t)
		dsn := setup(f)
		p := newAuroraProvider(t, f, func(c *clone.AuroraConfig) {
			c.Production = func(context.Context) (clone.AuroraProduction, error) {
				return clone.AuroraProduction{DSN: dsn}, nil
			}
		})
		_, err := p.Create(context.Background(), clone.CloneSpec{ResourceName: auroraName})
		if !errors.Is(err, clone.ErrNotAuroraCluster) ||
			!strings.Contains(err.Error(), "not an Aurora cluster") {
			t.Errorf("%s: err = %v, want ErrNotAuroraCluster", name, err)
		}
		if len(f.mutating()) != 0 {
			t.Errorf("%s: calls %q, want nothing created", name, f.mutating())
		}
	}
}

func TestAuroraCreate_ReaderEndpointNamesTheCluster(t *testing.T) {
	f := newAuroraFake(t)
	p := newAuroraProvider(t, f, func(c *clone.AuroraConfig) {
		c.Production = func(context.Context) (clone.AuroraProduction, error) {
			return clone.AuroraProduction{DSN: "postgres://app@" + auroraReaderHost + "/shop"},
				nil
		}
	})
	if _, err := p.Create(context.Background(),
		clone.CloneSpec{ResourceName: auroraName}); err != nil {
		t.Fatal(err)
	}
	if f.restores[0].Source != "shop" {
		t.Fatalf("source = %q", f.restores[0].Source)
	}
}

func TestAuroraCreate_RegionOtherThanTheConfiguredIsRefused(t *testing.T) {
	f := newAuroraFake(t)
	p := newAuroraProvider(t, f, func(c *clone.AuroraConfig) { c.Region = "eu-west-1" })
	_, err := p.Create(context.Background(), clone.CloneSpec{ResourceName: auroraName})
	if !errors.Is(err, clone.ErrInvalidAuroraConfig) || !strings.Contains(err.Error(),
		"eu-west-1") || len(f.mutating()) != 0 {
		t.Fatalf("err = %v, calls %q", err, f.mutating())
	}
}

// Boundary: the 16th copy-on-write clone of a source is a full copy at
// full price, so 15 existing clones refuse the create and 14 allow it.
func TestAuroraCreate_FifteenCopyOnWriteClonesIsTheLimit(t *testing.T) {
	for _, tc := range []struct {
		existing int
		refused  bool
	}{{0, false}, {14, false}, {15, true}, {20, true}} {
		f := newAuroraFake(t)
		f.addClones(tc.existing)
		p := newAuroraProvider(t, f, nil)
		_, err := p.Create(context.Background(), clone.CloneSpec{ResourceName: auroraName})
		if !tc.refused {
			if err != nil {
				t.Errorf("%d existing clones: %v", tc.existing, err)
			}
			continue
		}
		if !errors.Is(err, clone.ErrAuroraCloneLimit) || !strings.Contains(err.Error(), "15") {
			t.Errorf("%d existing clones: err = %v, want the 15-clone limit", tc.existing, err)
		}
		if len(f.callsWith("restore")) != 0 {
			t.Errorf("%d existing clones: restored anyway", tc.existing)
		}
	}
}

func TestAuroraCreate_ClonesOfOtherGroupsDoNotCount(t *testing.T) {
	f := newAuroraFake(t)
	f.addClones(15)
	for id, c := range f.clusters {
		if id != "shop" {
			c.CloneGroupID = "cg-other"
		}
	}
	p := newAuroraProvider(t, f, nil)
	if _, err := p.Create(context.Background(),
		clone.CloneSpec{ResourceName: auroraName}); err != nil {
		t.Fatalf("clones of another group counted: %v", err)
	}
}

func TestAuroraCreate_ListFailureRefusesTheCreate(t *testing.T) {
	f := newAuroraFake(t)
	f.listErr = errors.New("list exploded")
	p := newAuroraProvider(t, f, nil)
	_, err := p.Create(context.Background(), clone.CloneSpec{ResourceName: auroraName})
	if err == nil || !strings.Contains(err.Error(), "list exploded") ||
		len(f.callsWith("restore")) != 0 {
		t.Fatalf("err = %v, restores %q: want refused when the clones cannot be counted",
			err, f.callsWith("restore"))
	}
}

// Nil/empty: without AWS credentials nothing can be made or deleted.
func TestAuroraCreate_NoCredentialsIsUnavailable(t *testing.T) {
	f := newAuroraFake(t)
	cases := map[string]func(*clone.AuroraConfig){
		"no API": func(c *clone.AuroraConfig) { c.API = nil },
		"no credentials": func(c *clone.AuroraConfig) {
			c.API = func(context.Context) (clone.AuroraAPI, error) {
				return nil, errors.New("no EC2 IMDS role found")
			}
		},
	}
	for name, mutate := range cases {
		p := newAuroraProvider(t, f, mutate)
		_, err := p.Create(context.Background(), clone.CloneSpec{ResourceName: auroraName})
		if !errors.Is(err, clone.ErrCloneUnavailable) ||
			!strings.Contains(err.Error(), "AWS credentials") {
			t.Errorf("%s: create err = %v, want unavailable naming AWS credentials", name, err)
		}
		if err := p.Destroy(context.Background(), clone.Clone{ID: auroraName}); !errors.Is(err,
			clone.ErrCloneUnavailable) {
			t.Errorf("%s: destroy err = %v", name, err)
		}
		if _, _, err := p.FindByName(context.Background(), auroraName); !errors.Is(err,
			clone.ErrCloneUnavailable) {
			t.Errorf("%s: find err = %v", name, err)
		}
	}
	if len(f.calls) != 0 {
		t.Fatalf("calls without credentials: %q", f.calls)
	}
}

func TestAuroraCreate_NoProductionIdentityIsUnavailable(t *testing.T) {
	f := newAuroraFake(t)
	cases := map[string]func(context.Context) (clone.AuroraProduction, error){
		"nil": nil,
		"error": func(context.Context) (clone.AuroraProduction, error) {
			return clone.AuroraProduction{}, errors.New("pool closed")
		},
		"empty": func(context.Context) (clone.AuroraProduction, error) {
			return clone.AuroraProduction{}, nil
		},
	}
	for name, prod := range cases {
		p := newAuroraProvider(t, f, func(c *clone.AuroraConfig) { c.Production = prod })
		_, err := p.Create(context.Background(), clone.CloneSpec{ResourceName: auroraName})
		if !errors.Is(err, clone.ErrCloneUnavailable) {
			t.Errorf("%s: err = %v, want unavailable", name, err)
		}
	}
	if len(f.mutating()) != 0 {
		t.Fatalf("calls = %q", f.mutating())
	}
}

func TestAuroraCreate_ResourceNameOutsideTheOwnedShapeIsRefused(t *testing.T) {
	f := newAuroraFake(t)
	p := newAuroraProvider(t, f, nil)
	for _, name := range []string{"", "shop", "sage-rehearsal-XYZ", "sage--0a1b2c3d",
		"sage-rehearsal-0a1b2c3d-extra"} {
		_, err := p.Create(context.Background(), clone.CloneSpec{ResourceName: name})
		if !errors.Is(err, clone.ErrInvalidAuroraConfig) {
			t.Errorf("name %q: err = %v", name, err)
		}
	}
	if len(f.calls) != 0 {
		t.Fatalf("calls = %q", f.calls)
	}
}

func TestAuroraNew_InvalidConfigIsRefused(t *testing.T) {
	f := newAuroraFake(t)
	cases := map[string]func(*clone.AuroraConfig){
		"no region":       func(c *clone.AuroraConfig) { c.Region = "" },
		"bad region":      func(c *clone.AuroraConfig) { c.Region = "Mars-1" },
		"ttl too short":   func(c *clone.AuroraConfig) { c.TTL = 29 * time.Minute },
		"ttl too long":    func(c *clone.AuroraConfig) { c.TTL = 25 * time.Hour },
		"ttl zero":        func(c *clone.AuroraConfig) { c.TTL = 0 },
		"bad class":       func(c *clone.AuroraConfig) { c.InstanceClass = "r6g.large" },
		"uppercase class": func(c *clone.AuroraConfig) { c.InstanceClass = "db.R6G.large" },
	}
	for name, mutate := range cases {
		if _, err := clone.NewAuroraCloneProvider(auroraConfig(f, mutate)); !errors.Is(err,
			clone.ErrInvalidAuroraConfig) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	for _, ttl := range []time.Duration{30 * time.Minute, 24 * time.Hour} {
		if _, err := clone.NewAuroraCloneProvider(auroraConfig(f, func(c *clone.AuroraConfig) {
			c.TTL = ttl
		})); err != nil {
			t.Errorf("ttl %s refused: %v", ttl, err)
		}
	}
}

func TestAuroraAdapter_IsRegistered(t *testing.T) {
	if clone.AuroraAdapter != "aurora_clone" {
		t.Fatalf("adapter name = %q", clone.AuroraAdapter)
	}
	for _, name := range clone.AdapterNames() {
		if name == clone.AuroraAdapter {
			return
		}
	}
	t.Fatalf("aurora_clone is not in %v", clone.AdapterNames())
}

func TestAuroraDescribe_IsCopyOnWriteWithTheVendorReadyTime(t *testing.T) {
	p := newAuroraProvider(t, newAuroraFake(t), func(c *clone.AuroraConfig) {
		c.TTL = 90 * time.Minute
	})
	c, err := p.Describe(context.Background())
	if err != nil || c.Kind != clone.KindCopyOnWrite || c.TypicalReady != 15*time.Minute ||
		c.MaxTTL != 90*time.Minute {
		t.Fatalf("describe = %+v, %v", c, err)
	}
	est := c.Estimate(2*time.Hour, &clone.Rate{PerHour: 0.5, Currency: "USD",
		AsOf: "2026-11-01", Source: "aurora:us-east-1/shop"})
	if est.InstanceHours != 2 || !est.Priced || est.Amount != 1 ||
		est.Kind != clone.KindCopyOnWrite {
		t.Fatalf("estimate = %+v", est)
	}
	if age, err := p.SnapshotAge(context.Background()); err != nil || age != 0 {
		t.Fatalf("snapshot age = %s, %v", age, err)
	}
	if p.TargetScope() != "aws/us-east-1" {
		t.Fatalf("scope = %q", p.TargetScope())
	}
}

// Concurrent access: two creates at once give two tagged clusters (run
// with -race where cgo is available).
func TestAuroraCreate_ConcurrentCreatesGiveTwoTaggedClusters(t *testing.T) {
	f := newAuroraFake(t)
	p := newAuroraProvider(t, f, nil)
	names := []string{"sage-rehearsal-00000001", "sage-rehearsal-00000002"}
	clones := make([]clone.Clone, 2)
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			clones[i], errs[i] = p.Create(context.Background(),
				clone.CloneSpec{ResourceName: names[i]})
		}()
	}
	wg.Wait()
	for i := range 2 {
		if errs[i] != nil || clones[i].ID != names[i] || !f.exists(names[i]) {
			t.Fatalf("create %d = %+v, %v", i, clones[i], errs[i])
		}
		if f.clusters[names[i]].Tags[clone.AuroraReceiptTag] != names[i] {
			t.Fatalf("cluster %s tags = %v", names[i], f.clusters[names[i]].Tags)
		}
	}
	if clones[0].DSN == clones[1].DSN {
		t.Fatal("two clones share a DSN")
	}
}
