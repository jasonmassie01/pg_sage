package clone_test

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/clone"
)

// lab-05f: alloydb_restore restores the monitored AlloyDB cluster to a new
// cluster at a point in time of its continuous backup (a full copy), adds
// a primary instance, and hands out the monitored DSN on that instance's
// IP of the same kind.

func createADB(t *testing.T, base string, edit func(*clone.AlloyDBRestoreConfig),
) (clone.Clone, error) {
	t.Helper()
	return newAlloyDBProvider(t, base, edit).Create(context.Background(),
		clone.CloneSpec{ResourceName: adbName})
}

func TestAlloyDB_HappyPathRestoresAddsAPrimaryThenDeletes(t *testing.T) {
	f, base := newAlloyDBFake(t)
	f.doneAfter = 3
	p := newAlloyDBProvider(t, base, nil)
	c, err := p.Create(context.Background(), clone.CloneSpec{ResourceName: adbName})
	if err != nil {
		t.Fatal(err)
	}
	pit := adbNow.Add(-clone.AlloyDBRestoreLag)
	if c.ID != adbName || !f.exists(adbName) || !c.CreatedFrom.Equal(pit) {
		t.Fatalf("clone = %+v (want from %s), cluster exists %v", c, pit, f.exists(adbName))
	}
	if in, ok := f.instance(adbName, adbName); !ok || in.State != "READY" {
		t.Fatalf("primary instance = %+v, %v", in, ok)
	}
	if err := p.Destroy(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	want := []string{"restore " + adbName, "create instance " + adbName + "/" + adbName,
		"delete " + adbName + " force=true"}
	if got := f.mutating(); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("mutating calls = %q, want %q", got, want)
	}
	if f.exists(adbName) || !f.exists(adbSource) {
		t.Fatal("destroy left the clone or touched the source")
	}
	if _, ok := f.instance(adbSource, "shop-primary"); !ok {
		t.Fatal("destroy removed the source's primary instance")
	}
}

// The restore names the source cluster and a point in time AlloyDBRestoreLag
// ago, restores into the source's network with its CMEK key, and labels
// the new cluster with the receipt name and expiry (source labels kept).
func TestAlloyDB_RestoreRequestIsThePointInTimeOfTheSource(t *testing.T) {
	f, base := newAlloyDBFake(t)
	if _, err := createADB(t, base, nil); err != nil {
		t.Fatal(err)
	}
	body := f.body("restore " + adbName)
	src, _ := body["continuousBackupSource"].(map[string]any)
	pit := adbNow.Add(-clone.AlloyDBRestoreLag).Format(time.RFC3339)
	if body["clusterId"] != adbName || src["cluster"] != adbParent+"/clusters/"+adbSource ||
		src["pointInTime"] != pit || len(src) != 2 {
		t.Fatalf("restore body = %v, want cluster %s at %s", body, adbSource, pit)
	}
	cl, _ := body["cluster"].(map[string]any)
	nc, _ := cl["networkConfig"].(map[string]any)
	enc, _ := cl["encryptionConfig"].(map[string]any)
	if nc["network"] != adbVPC || nc["allocatedIpRange"] != "adb-range" ||
		enc["kmsKeyName"] != adbKMS {
		t.Fatalf("restored cluster network %v encryption %v, want the source's", nc, enc)
	}
	labels := f.cluster(adbName).Labels
	expiry := fmt.Sprint(adbNow.Add(3*time.Hour + 15*time.Minute).Unix())
	if labels[clone.AlloyDBReceiptLabel] != adbName ||
		labels[clone.AlloyDBExpiresLabel] != expiry || labels["team"] != "shop" ||
		len(labels) != 3 {
		t.Fatalf("clone labels = %v, want team, receipt %s and expiry %s", labels, adbName,
			expiry)
	}
}

// The primary instance copies the source primary's machine and network
// (public IP and authorized networks), with no standby, and is labelled.
func TestAlloyDB_PrimaryInstanceCopiesTheSourceMachineWithoutHA(t *testing.T) {
	f, base := newAlloyDBFake(t)
	if _, err := createADB(t, base, nil); err != nil {
		t.Fatal(err)
	}
	body := f.body("instance " + adbName + "/" + adbName)
	mc, _ := body["machineConfig"].(map[string]any)
	nc, _ := body["networkConfig"].(map[string]any)
	labels, _ := body["labels"].(map[string]any)
	nets, _ := nc["authorizedExternalNetworks"].([]any)
	if body["instanceType"] != "PRIMARY" || body["availabilityType"] != "ZONAL" ||
		mc["cpuCount"] != float64(4) || nc["enablePublicIp"] != true || len(nets) != 1 ||
		labels[clone.AlloyDBReceiptLabel] != adbName {
		t.Fatalf("instance body = %v", body)
	}
}

func TestAlloyDB_DSNIsTheMonitoredDSNOnTheClonesPrivateIP(t *testing.T) {
	f, base := newAlloyDBFake(t)
	c, err := createADB(t, base, nil)
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(c.DSN)
	if err != nil {
		t.Fatal(err)
	}
	pw, _ := u.User.Password()
	in, _ := f.instance(adbName, adbName)
	if u.Hostname() != in.Private || in.Private == adbProdIP || u.Port() != "5432" ||
		u.User.Username() != "app" || pw != adbPassword || u.Path != "/shop" ||
		u.Query().Get("sslmode") != "require" {
		t.Fatalf("clone DSN host %s (clone private %s) port %s user %s db %s sslmode %q",
			u.Hostname(), in.Private, u.Port(), u.User.Username(), u.Path,
			u.Query().Get("sslmode"))
	}
}

func TestAlloyDB_PublicIPSourceGetsTheClonesPublicIP(t *testing.T) {
	f, base := newAlloyDBFake(t)
	c, err := createADB(t, base, func(c *clone.AlloyDBRestoreConfig) {
		c.Production = func(context.Context) (clone.AlloyDBProduction, error) {
			return clone.AlloyDBProduction{DSN: "host=" + adbPublicIP + " user=app " +
				"dbname=shop sslmode=verify-full"}, nil
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	in, _ := f.instance(adbName, adbName)
	want := "host=" + in.Public
	if in.Public == adbPublicIP || !strings.Contains(c.DSN, want) ||
		!strings.Contains(c.DSN, "sslmode=verify-full") {
		t.Fatalf("clone DSN %q, want %s and the stronger sslmode kept", c.DSN, want)
	}
}

// SnapshotAge is the restore lag: the point in time is always recent.
func TestAlloyDB_SnapshotIsThePointInTimeOfTheSource(t *testing.T) {
	f, base := newAlloyDBFake(t)
	age, err := newAlloyDBProvider(t, base, nil).SnapshotAge(context.Background())
	if err != nil || age != clone.AlloyDBRestoreLag || len(f.mutating()) != 0 {
		t.Fatalf("snapshot age = %s, %v, mutating %q", age, err, f.mutating())
	}
}

// Invalid input: continuous backup off on the source cannot restore to a
// point in time; refused with the setting named, nothing asked for.
func TestAlloyDB_ContinuousBackupDisabledIsRefusedNamingTheSetting(t *testing.T) {
	f, base := newAlloyDBFake(t)
	f.clusters[adbSource].Backup = false
	_, err := createADB(t, base, nil)
	if !errors.Is(err, clone.ErrAlloyDBContinuousBackupOff) ||
		!strings.Contains(err.Error(), "continuousBackupConfig.enabled") ||
		!strings.Contains(err.Error(), adbSource) || len(f.mutating()) != 0 {
		t.Fatalf("err = %v, mutating %q; want refused naming the setting", err, f.mutating())
	}
}

// Boundary: a point in time before the earliest restorable time (backup
// just turned on) is refused; one exactly at it is accepted.
func TestAlloyDB_PointInTimeMustBeInTheRecoveryWindow(t *testing.T) {
	f, base := newAlloyDBFake(t)
	f.clusters[adbSource].Earliest = adbNow.Add(-clone.AlloyDBRestoreLag + time.Second)
	_, err := createADB(t, base, nil)
	if !errors.Is(err, clone.ErrCloneUnavailable) ||
		!strings.Contains(err.Error(), "earliest restorable") || len(f.mutating()) != 0 {
		t.Fatalf("err = %v, mutating %q; want unavailable, nothing asked", err, f.mutating())
	}
	f, base = newAlloyDBFake(t)
	f.clusters[adbSource].Earliest = adbNow.Add(-clone.AlloyDBRestoreLag)
	if c, err := createADB(t, base, nil); err != nil || c.ID != adbName {
		t.Fatalf("point in time at the earliest restorable time: %+v, %v", c, err)
	}
}

// Invalid input: a cluster id AlloyDB or its labels cannot take is refused
// before anything is asked of Google.
func TestAlloyDB_RefusesInvalidClusterNames(t *testing.T) {
	long := "sage-" + strings.Repeat("a", 50) + "-0a1b2c3d" // 64 characters
	for _, name := range []string{"", "sage-Rehearsal-0a1b2c3d", "shop",
		"sage-rehearsal-0A1B2C3D", "sage-rehearsal-0a1b2c3", long,
		"sage-rehearsal-0a1b2c3d/../shop", "sage-rehearsal-0a1b2c3d?force=true"} {
		f, base := newAlloyDBFake(t)
		_, err := newAlloyDBProvider(t, base, nil).Create(context.Background(),
			clone.CloneSpec{ResourceName: name})
		if !errors.Is(err, clone.ErrInvalidAlloyDBConfig) || len(f.mutating()) != 0 {
			t.Fatalf("name %q: err = %v, mutating %q; want refused, nothing asked", name,
				err, f.mutating())
		}
	}
	at := "sage-" + strings.Repeat("a", 49) + "-0a1b2c3d"
	f, base := newAlloyDBFake(t)
	if c, err := newAlloyDBProvider(t, base, nil).Create(context.Background(),
		clone.CloneSpec{ResourceName: at}); err != nil || c.ID != at || !f.exists(at) {
		t.Fatalf("63-character name: %+v, %v", c, err)
	}
}

func TestAlloyDB_InvalidConfigurationIsRefused(t *testing.T) {
	cases := map[string]func(*clone.AlloyDBRestoreConfig){
		"no project":    func(c *clone.AlloyDBRestoreConfig) { c.Project = "" },
		"bad project":   func(c *clone.AlloyDBRestoreConfig) { c.Project = "Acme_Prod" },
		"no location":   func(c *clone.AlloyDBRestoreConfig) { c.Location = "" },
		"bad location":  func(c *clone.AlloyDBRestoreConfig) { c.Location = "us-central1/x" },
		"zone":          func(c *clone.AlloyDBRestoreConfig) { c.Location = "us-central1-a" },
		"no cluster":    func(c *clone.AlloyDBRestoreConfig) { c.Cluster = "" },
		"bad cluster":   func(c *clone.AlloyDBRestoreConfig) { c.Cluster = "Shop" },
		"pg_sage clone": func(c *clone.AlloyDBRestoreConfig) { c.Cluster = adbName },
		"short deadline": func(c *clone.AlloyDBRestoreConfig) {
			c.Deadline = 29 * time.Minute
		},
		"long deadline": func(c *clone.AlloyDBRestoreConfig) { c.Deadline = 13 * time.Hour },
		"ttl not past deadline": func(c *clone.AlloyDBRestoreConfig) {
			c.TTL = c.Deadline
		},
		"ttl over a day": func(c *clone.AlloyDBRestoreConfig) { c.TTL = 25 * time.Hour },
		"bad base URL":   func(c *clone.AlloyDBRestoreConfig) { c.BaseURL = "ftp://x" },
	}
	for name, edit := range cases {
		cfg := adbConfig("http://127.0.0.1:1")
		edit(&cfg)
		if api, err := clone.NewAlloyDBRestoreAPI(cfg); !errors.Is(err,
			clone.ErrInvalidAlloyDBConfig) || api != nil {
			t.Errorf("%s: api %v, err = %v; want ErrInvalidAlloyDBConfig", name, api, err)
		}
	}
	cfg := adbConfig("")
	cfg.Deadline, cfg.TTL = 30*time.Minute, 24*time.Hour
	cfg.Location = "northamerica-northeast1"
	if _, err := clone.NewAlloyDBRestoreAPI(cfg); err != nil {
		t.Fatalf("bounds 30m deadline / 24h TTL refused: %v", err)
	}
}

func TestAlloyDB_DescribeIsAFullCopyBoundByTheTTL(t *testing.T) {
	_, base := newAlloyDBFake(t)
	got, err := newAlloyDBProvider(t, base, nil).Describe(context.Background())
	if err != nil || got.Kind != clone.KindFullCopy ||
		got.MaxTTL != 3*time.Hour+15*time.Minute || got.TypicalReady != clone.SnapshotTypicalReady {
		t.Fatalf("describe = %+v, %v", got, err)
	}
	scope := newAlloyDBAPI(t, base, nil).TargetScope()
	if scope != "gcp/"+adbProject+"/"+adbLocation {
		t.Fatalf("scope = %q", scope)
	}
}

// Nil/empty: no token source, a failing one, or an empty token makes the
// provider unavailable before any AlloyDB call.
func TestAlloyDB_NoCredentialsIsUnavailableAndCallsNothing(t *testing.T) {
	cases := map[string]func(context.Context) (clone.CloudSQLTokenSource, error){
		"nil chain": nil,
		"chain error": func(context.Context) (clone.CloudSQLTokenSource, error) {
			return nil, errors.New("no Google credentials")
		},
		"nil source": func(context.Context) (clone.CloudSQLTokenSource, error) {
			return nil, nil
		},
		"token error": func(context.Context) (clone.CloudSQLTokenSource, error) {
			return staticToken{err: errors.New("refresh failed")}, nil
		},
		"empty token": func(context.Context) (clone.CloudSQLTokenSource, error) {
			return staticToken{tok: " "}, nil
		},
	}
	for name, tok := range cases {
		f, base := newAlloyDBFake(t)
		p := newAlloyDBProvider(t, base, func(c *clone.AlloyDBRestoreConfig) { c.Token = tok })
		_, err := p.Create(context.Background(), clone.CloneSpec{ResourceName: adbName})
		derr := p.Destroy(context.Background(), clone.Clone{ID: adbName})
		if !errors.Is(err, clone.ErrCloneUnavailable) ||
			!errors.Is(derr, clone.ErrCloneUnavailable) || f.callCount() != 0 ||
			!strings.Contains(err.Error(), "alloydb_restore") {
			t.Errorf("%s: create %v, destroy %v, calls %d; want unavailable, none", name,
				err, derr, f.callCount())
		}
	}
}

func TestAlloyDB_NoProductionNeverCreates(t *testing.T) {
	f, base := newAlloyDBFake(t)
	_, err := createADB(t, base, func(c *clone.AlloyDBRestoreConfig) { c.Production = nil })
	if !errors.Is(err, clone.ErrCloneUnavailable) || f.callCount() != 0 {
		t.Fatalf("err = %v, calls %d; want unavailable before any call", err, f.callCount())
	}
	f, base = newAlloyDBFake(t)
	_, err = createADB(t, base, func(c *clone.AlloyDBRestoreConfig) {
		c.Production = func(context.Context) (clone.AlloyDBProduction, error) {
			return clone.AlloyDBProduction{DSN: "  "}, nil
		}
	})
	if !errors.Is(err, clone.ErrCloneUnavailable) || len(f.mutating()) != 0 {
		t.Fatalf("empty DSN: err = %v, mutating %q", err, f.mutating())
	}
}

// Two restores in parallel get distinct clusters and DSNs. -race is not
// available on this machine (no cgo); the parallel run still checks state.
func TestAlloyDB_TwoRestoresInParallel(t *testing.T) {
	f, base := newAlloyDBFake(t)
	f.doneAfter = 2
	p := newAlloyDBProvider(t, base, nil)
	names := []string{"sage-rehearsal-0000aaaa", "sage-rehearsal-0000bbbb"}
	got := make([]clone.Clone, 2)
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i := range names {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			got[i], errs[i] = p.Create(context.Background(),
				clone.CloneSpec{ResourceName: names[i]})
		}(i)
	}
	wg.Wait()
	for i := range names {
		if errs[i] != nil || got[i].ID != names[i] || !f.exists(names[i]) {
			t.Fatalf("clone %d = %+v, %v", i, got[i], errs[i])
		}
	}
	if got[0].DSN == got[1].DSN {
		t.Fatalf("both clones have DSN %q", got[0].DSN)
	}
}
