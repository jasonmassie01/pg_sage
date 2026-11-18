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

// lab-05e: cloudsql_clone clones the monitored Cloud SQL for PostgreSQL
// instance (instances.clone, a full copy), waits for the operation, marks
// the clone as pg_sage's, and hands out the monitored DSN on the clone's
// IP of the same kind.

func TestCloudSQL_HappyPathClonesLabelsThenDeletes(t *testing.T) {
	f, base := newCloudSQLFake(t)
	f.doneAfter = 3
	p := newCloudSQLProvider(t, base, nil)
	c, err := p.Create(context.Background(), clone.CloneSpec{ResourceName: sqlName})
	if err != nil {
		t.Fatal(err)
	}
	if c.ID != sqlName || !f.exists(sqlName) || !c.CreatedFrom.Equal(sqlNow) {
		t.Fatalf("clone = %+v, instance exists %v", c, f.exists(sqlName))
	}
	if err := p.Destroy(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	want := []string{"clone shop to " + sqlName, "patch " + sqlName, "delete " + sqlName}
	if got := f.mutating(); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("mutating calls = %q, want %q", got, want)
	}
	if f.exists(sqlName) || !f.exists(sqlSource) || !f.instance(sqlSource).Protected {
		t.Fatal("destroy left the clone or touched the source")
	}
}

func TestCloudSQL_DSNIsTheMonitoredDSNOnTheClonesPrivateIP(t *testing.T) {
	f, base := newCloudSQLFake(t)
	c, err := newCloudSQLProvider(t, base, nil).Create(context.Background(),
		clone.CloneSpec{ResourceName: sqlName})
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(c.DSN)
	if err != nil {
		t.Fatal(err)
	}
	pw, _ := u.User.Password()
	in := f.instance(sqlName)
	if u.Hostname() != in.Private || in.Private == sqlProdIP || u.Port() != "5432" ||
		u.User.Username() != "app" || pw != sqlPassword || u.Path != "/shop" ||
		u.Query().Get("sslmode") != "require" {
		t.Fatalf("clone DSN host %s (clone private %s) port %s user %s db %s sslmode %q",
			u.Hostname(), in.Private, u.Port(), u.User.Username(), u.Path,
			u.Query().Get("sslmode"))
	}
}

func TestCloudSQL_PublicIPSourceGetsTheClonesPublicIP(t *testing.T) {
	f, base := newCloudSQLFake(t)
	c, err := newCloudSQLProvider(t, base, func(c *clone.CloudSQLCloneConfig) {
		c.Production = func(context.Context) (clone.CloudSQLProduction, error) {
			return clone.CloudSQLProduction{DSN: "host=34.1.2.3 user=app dbname=shop " +
				"sslmode=verify-full"}, nil
		}
	}).Create(context.Background(), clone.CloneSpec{ResourceName: sqlName})
	if err != nil {
		t.Fatal(err)
	}
	want := "host=" + f.instance(sqlName).Primary
	if !strings.Contains(c.DSN, want) || !strings.Contains(c.DSN, "sslmode=verify-full") {
		t.Fatalf("clone DSN %q, want %s and the stronger sslmode kept", c.DSN, want)
	}
}

func TestCloudSQL_CloneRequestNamesTheDestination(t *testing.T) {
	f, base := newCloudSQLFake(t)
	if _, err := newCloudSQLProvider(t, base, nil).Create(context.Background(),
		clone.CloneSpec{ResourceName: sqlName}); err != nil {
		t.Fatal(err)
	}
	ctx, _ := f.body("clone " + sqlName)["cloneContext"].(map[string]any)
	if ctx["destinationInstanceName"] != sqlName || ctx["kind"] != "sql#cloneContext" ||
		len(ctx) != 2 {
		t.Fatalf("clone body = %v, want only the destination name", ctx)
	}
}

// The clone copies the source's settings: pg_sage labels it with its
// receipt name and expiry, turns off the copied deletion protection and HA
// (a rehearsal never pays for a standby), and keeps the source's labels.
func TestCloudSQL_PatchLabelsTheReceiptAndDropsProtectionAndHA(t *testing.T) {
	f, base := newCloudSQLFake(t)
	if _, err := newCloudSQLProvider(t, base, nil).Create(context.Background(),
		clone.CloneSpec{ResourceName: sqlName}); err != nil {
		t.Fatal(err)
	}
	in := f.instance(sqlName)
	expiry := fmt.Sprint(sqlNow.Add(3*time.Hour + 15*time.Minute).Unix())
	if in.Labels[clone.CloudSQLReceiptLabel] != sqlName ||
		in.Labels[clone.CloudSQLExpiresLabel] != expiry || in.Labels["team"] != "shop" ||
		len(in.Labels) != 3 {
		t.Fatalf("clone labels = %v, want team, receipt %s and expiry %s", in.Labels,
			sqlName, expiry)
	}
	if in.Protected || in.Availability != "ZONAL" {
		t.Fatalf("clone protected %v availability %s, want unprotected ZONAL", in.Protected,
			in.Availability)
	}
	settings, _ := f.body("patch " + sqlName)["settings"].(map[string]any)
	if len(settings) != 3 {
		t.Fatalf("patch settings = %v, want only labels, protection and availability",
			settings)
	}
}

// Invalid input: a destination name Cloud SQL or its labels cannot take is
// refused before anything is asked of Google.
func TestCloudSQL_RefusesInvalidDestinationNames(t *testing.T) {
	long := "sage-" + strings.Repeat("a", 52) + "-0a1b2c3d" // 66 characters
	over98 := "sage-" + strings.Repeat("a", 85) + "-0a1b2c3d"
	for _, name := range []string{"", "sage-Rehearsal-0a1b2c3d", "SAGE-rehearsal-0a1b2c3d",
		"shop", "sage-rehearsal-0A1B2C3D", "sage-rehearsal-0a1b2c3", long, over98,
		"sage-rehearsal-0a1b2c3d/../shop"} {
		f, base := newCloudSQLFake(t)
		_, err := newCloudSQLProvider(t, base, nil).Create(context.Background(),
			clone.CloneSpec{ResourceName: name})
		if !errors.Is(err, clone.ErrInvalidCloudSQLConfig) || len(f.mutating()) != 0 {
			t.Fatalf("name %q: err = %v, mutating calls %q; want refused, nothing asked",
				name, err, f.mutating())
		}
	}
}

// Boundary: the receipt label value is at most 63 characters, so 63 is
// the longest name accepted.
func TestCloudSQL_NameLengthBoundaryIsTheLabelLimit(t *testing.T) {
	at := "sage-" + strings.Repeat("a", 49) + "-0a1b2c3d"
	if len(at) != 63 {
		t.Fatalf("test name is %d characters", len(at))
	}
	f, base := newCloudSQLFake(t)
	c, err := newCloudSQLProvider(t, base, nil).Create(context.Background(),
		clone.CloneSpec{ResourceName: at})
	if err != nil || c.ID != at || !f.exists(at) {
		t.Fatalf("63-character name: clone %+v, %v", c, err)
	}
	over := "sage-" + strings.Repeat("a", 50) + "-0a1b2c3d"
	_, err = newCloudSQLProvider(t, base, nil).Create(context.Background(),
		clone.CloneSpec{ResourceName: over})
	if !errors.Is(err, clone.ErrInvalidCloudSQLConfig) || f.exists(over) {
		t.Fatalf("64-character name: err = %v", err)
	}
}

func TestCloudSQL_InvalidConfigurationIsRefused(t *testing.T) {
	cases := map[string]func(*clone.CloudSQLCloneConfig){
		"no project":     func(c *clone.CloudSQLCloneConfig) { c.Project = "" },
		"bad project":    func(c *clone.CloudSQLCloneConfig) { c.Project = "Acme_Prod" },
		"no instance":    func(c *clone.CloudSQLCloneConfig) { c.Instance = "" },
		"bad instance":   func(c *clone.CloudSQLCloneConfig) { c.Instance = "Shop/../x" },
		"pg_sage clone":  func(c *clone.CloudSQLCloneConfig) { c.Instance = sqlName },
		"short deadline": func(c *clone.CloudSQLCloneConfig) { c.Deadline = 29 * time.Minute },
		"long deadline":  func(c *clone.CloudSQLCloneConfig) { c.Deadline = 13 * time.Hour },
		"ttl not past deadline": func(c *clone.CloudSQLCloneConfig) {
			c.TTL = c.Deadline
		},
		"ttl over a day": func(c *clone.CloudSQLCloneConfig) { c.TTL = 25 * time.Hour },
		"bad base URL":   func(c *clone.CloudSQLCloneConfig) { c.BaseURL = "ftp://x" },
	}
	for name, edit := range cases {
		cfg := sqlConfig("http://127.0.0.1:1")
		edit(&cfg)
		if api, err := clone.NewCloudSQLCloneAPI(cfg); !errors.Is(err,
			clone.ErrInvalidCloudSQLConfig) || api != nil {
			t.Errorf("%s: api %v, err = %v; want ErrInvalidCloudSQLConfig", name, api, err)
		}
	}
	cfg := sqlConfig("")
	cfg.Deadline, cfg.TTL = 30*time.Minute, 24*time.Hour
	if _, err := clone.NewCloudSQLCloneAPI(cfg); err != nil {
		t.Fatalf("bounds 30m deadline / 24h TTL refused: %v", err)
	}
}

func TestCloudSQL_DescribeIsAFullCopyBoundByTheTTL(t *testing.T) {
	_, base := newCloudSQLFake(t)
	api := newCloudSQLAPI(t, base, nil)
	got, err := newCloudSQLProvider(t, base, nil).Describe(context.Background())
	if err != nil || got.Kind != clone.KindFullCopy || got.MaxTTL != 3*time.Hour+15*time.Minute ||
		got.TypicalReady != clone.SnapshotTypicalReady {
		t.Fatalf("describe = %+v, %v", got, err)
	}
	if api.TargetScope() != "gcp/"+sqlProject {
		t.Fatalf("scope = %q", api.TargetScope())
	}
}

// Nil/empty: no token source, a failing one, or an empty token makes the
// provider unavailable before any Cloud SQL call.
func TestCloudSQL_NoCredentialsIsUnavailableAndCallsNothing(t *testing.T) {
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
			return staticToken{tok: "  "}, nil
		},
	}
	for name, tok := range cases {
		f, base := newCloudSQLFake(t)
		p := newCloudSQLProvider(t, base, func(c *clone.CloudSQLCloneConfig) { c.Token = tok })
		_, err := p.Create(context.Background(), clone.CloneSpec{ResourceName: sqlName})
		derr := p.Destroy(context.Background(), clone.Clone{ID: sqlName})
		if !errors.Is(err, clone.ErrCloneUnavailable) ||
			!errors.Is(derr, clone.ErrCloneUnavailable) || f.callCount() != 0 {
			t.Errorf("%s: create %v, destroy %v, calls %d; want unavailable, none", name,
				err, derr, f.callCount())
		}
	}
}

func TestCloudSQL_NoProductionNeverCreates(t *testing.T) {
	f, base := newCloudSQLFake(t)
	p := newCloudSQLProvider(t, base, func(c *clone.CloudSQLCloneConfig) {
		c.Production = nil
	})
	_, err := p.Create(context.Background(), clone.CloneSpec{ResourceName: sqlName})
	if !errors.Is(err, clone.ErrCloneUnavailable) || f.callCount() != 0 {
		t.Fatalf("err = %v, calls %d; want unavailable before any call", err, f.callCount())
	}
}

// Two clones in parallel get distinct instances and DSNs. -race is not
// available on this machine (no cgo); the parallel run still checks state.
func TestCloudSQL_TwoClonesInParallel(t *testing.T) {
	f, base := newCloudSQLFake(t)
	f.doneAfter = 2
	p := newCloudSQLProvider(t, base, nil)
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
