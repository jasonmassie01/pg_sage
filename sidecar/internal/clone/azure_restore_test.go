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

// lab-05g: azure_restore restores the monitored Azure Database for
// PostgreSQL flexible server to a new server at a point in time (a full
// copy, same region and major version), and hands out the monitored DSN
// on the new server's FQDN.

func TestAzureRestore_HappyPathRestoresThenDeletes(t *testing.T) {
	f, base := newAzureFake(t)
	f.doneAfter = 3
	p := newAzureProvider(t, base, nil)
	c, err := p.Create(context.Background(), clone.CloneSpec{ResourceName: azName})
	if err != nil {
		t.Fatal(err)
	}
	pit := azNow.Add(-clone.AzureRestoreLag)
	if c.ID != azName || !f.exists(azName) || !c.CreatedFrom.Equal(pit) ||
		f.server(azName).State != "Ready" {
		t.Fatalf("clone = %+v (want from %s), server exists %v", c, pit, f.exists(azName))
	}
	if err := p.Destroy(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	want := []string{"put " + azName, "delete " + azName}
	if got := f.mutating(); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("mutating calls = %q, want %q", got, want)
	}
	if f.exists(azName) || !f.exists(azServer) || !f.exists("shop-replica") {
		t.Fatal("destroy left the clone or touched the source")
	}
}

// The PUT is a PointInTimeRestore of the source's resource ID at
// AzureRestoreLag ago, in the source's region, into the source's subnet
// under its customer key, without a standby, with the shortest backup
// retention, tagged with the receipt name and expiry (source tags kept),
// and only if no server of that name exists.
func TestAzureRestore_RequestIsAPointInTimeRestoreOfTheSource(t *testing.T) {
	f, base := newAzureFake(t)
	if _, err := createAZ(t, base, nil); err != nil {
		t.Fatal(err)
	}
	body := f.body("put " + azName)
	props, _ := body["properties"].(map[string]any)
	pit := azNow.Add(-clone.AzureRestoreLag).Format(time.RFC3339)
	if body["location"] != azLocation || props["createMode"] != "PointInTimeRestore" ||
		props["sourceServerResourceId"] != azSourceID || props["pointInTimeUTC"] != pit {
		t.Fatalf("restore body = %v, want a restore of %s at %s in %s", body, azSourceID,
			pit, azLocation)
	}
	net, _ := props["network"].(map[string]any)
	ha, _ := props["highAvailability"].(map[string]any)
	backup, _ := props["backup"].(map[string]any)
	if net["delegatedSubnetResourceId"] != azSubnet ||
		net["privateDnsZoneArmResourceId"] != azDNSZone || len(ha) != 1 ||
		ha["mode"] != "Disabled" || backup["backupRetentionDays"] != float64(7) {
		t.Fatalf("network %v, HA %v, backup %v; want the source's subnet, no standby, 7 days",
			net, ha, backup)
	}
	enc, _ := props["dataEncryption"].(map[string]any)
	ident, _ := body["identity"].(map[string]any)
	ids, _ := ident["userAssignedIdentities"].(map[string]any)
	if enc["type"] != "AzureKeyVault" || enc["primaryKeyURI"] != azKey ||
		enc["primaryUserAssignedIdentityId"] != azIdentity || ident["type"] != "UserAssigned" ||
		len(ids) != 1 || ids[azIdentity] == nil {
		t.Fatalf("encryption %v identity %v; want the source's customer key", enc, ident)
	}
	if _, ok := props["administratorLoginPassword"]; ok {
		t.Fatal("the restore sent an administrator password")
	}
	tags := f.server(azName).Tags
	expiry := fmt.Sprint(azNow.Add(3*time.Hour + 15*time.Minute).Unix())
	if tags[clone.AzureReceiptTag] != azName || tags[clone.AzureExpiresTag] != expiry ||
		tags["team"] != "shop" || len(tags) != 3 {
		t.Fatalf("clone tags = %v, want team, receipt %s and expiry %s", tags, azName, expiry)
	}
	if got := f.header("put " + azName).Get("If-None-Match"); got != "*" {
		t.Fatalf("PUT If-None-Match = %q, want * (never update an existing server)", got)
	}
}

// A public-access source with service-managed keys sends no network, no
// key and no identity: the restore keeps the source's own settings.
func TestAzureRestore_PublicSourceSendsNoNetworkOrKey(t *testing.T) {
	f, base := newAzureFake(t)
	f.edit(azServer, func(s *fakeAZServer) { s.Public, s.SystemKey = true, true })
	if _, err := createAZ(t, base, nil); err != nil {
		t.Fatal(err)
	}
	body := f.body("put " + azName)
	props, _ := body["properties"].(map[string]any)
	for _, key := range []string{"network", "dataEncryption"} {
		if _, ok := props[key]; ok {
			t.Errorf("properties.%s sent for a public, service-keyed source: %v", key,
				props[key])
		}
	}
	if _, ok := body["identity"]; ok {
		t.Errorf("identity sent for a service-keyed source: %v", body["identity"])
	}
}

func TestAzureRestore_DSNIsTheMonitoredDSNOnTheClonesFQDN(t *testing.T) {
	_, base := newAzureFake(t)
	c, err := createAZ(t, base, nil)
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(c.DSN)
	if err != nil {
		t.Fatal(err)
	}
	pw, _ := u.User.Password()
	if u.Hostname() != azName+azSuffix || u.Port() != "5432" || u.User.Username() != "app" ||
		pw != azPassword || u.Path != "/shop" || u.Query().Get("sslmode") != "require" {
		t.Fatalf("clone DSN host %s port %s user %s db %s sslmode %q", u.Hostname(),
			u.Port(), u.User.Username(), u.Path, u.Query().Get("sslmode"))
	}
}

// A keyword DSN keeps its form and a stronger sslmode.
func TestAzureRestore_KeywordDSNKeepsVerifyFull(t *testing.T) {
	_, base := newAzureFake(t)
	c, err := createAZ(t, base, func(c *clone.AzureRestoreConfig) {
		c.Production = func(context.Context) (clone.AzureProduction, error) {
			return clone.AzureProduction{DSN: "host=SHOP.postgres.database.azure.com " +
				"user=app dbname=shop sslmode=verify-full"}, nil
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(c.DSN, "host="+azName+azSuffix) ||
		!strings.Contains(c.DSN, "sslmode=verify-full") {
		t.Fatalf("clone DSN %q, want the clone's FQDN and verify-full", c.DSN)
	}
}

// SnapshotAge is the restore lag: the point in time is always recent.
func TestAzureRestore_SnapshotIsThePointInTimeOfTheSource(t *testing.T) {
	f, base := newAzureFake(t)
	age, err := newAzureProvider(t, base, nil).SnapshotAge(context.Background())
	if err != nil || age != clone.AzureRestoreLag || len(f.mutating()) != 0 {
		t.Fatalf("snapshot age = %s, %v, mutating %q", age, err, f.mutating())
	}
}

// Boundary: a point in time before the earliest restore date (a server
// created recently) is refused; one exactly at it is accepted.
func TestAzureRestore_PointInTimeMustBeAfterTheEarliestRestoreDate(t *testing.T) {
	f, base := newAzureFake(t)
	f.edit(azServer, func(s *fakeAZServer) {
		s.Earliest = azNow.Add(-clone.AzureRestoreLag + time.Second)
	})
	_, err := createAZ(t, base, nil)
	if !errors.Is(err, clone.ErrCloneUnavailable) ||
		!strings.Contains(err.Error(), "earliest restore") || len(f.mutating()) != 0 {
		t.Fatalf("err = %v, mutating %q; want unavailable, nothing asked", err, f.mutating())
	}
	f, base = newAzureFake(t)
	f.edit(azServer, func(s *fakeAZServer) { s.Earliest = azNow.Add(-clone.AzureRestoreLag) })
	if c, err := createAZ(t, base, nil); err != nil || c.ID != azName {
		t.Fatalf("point in time at the earliest restore date: %+v, %v", c, err)
	}
	f, base = newAzureFake(t)
	f.edit(azServer, func(s *fakeAZServer) { s.Earliest = time.Time{} })
	if _, err := createAZ(t, base, nil); !errors.Is(err, clone.ErrCloneUnavailable) ||
		len(f.mutating()) != 0 {
		t.Fatalf("no earliest restore date: err = %v, mutating %q", err, f.mutating())
	}
}

// Invalid input: a server name Azure cannot take (over 63 characters,
// uppercase, a path) is refused before anything is asked of Azure.
func TestAzureRestore_RefusesInvalidServerNames(t *testing.T) {
	long := "sage-" + strings.Repeat("a", 50) + "-0a1b2c3d" // 64 characters
	for _, name := range []string{"", "sage-Rehearsal-0a1b2c3d", "shop",
		"sage-rehearsal-0A1B2C3D", "sage-rehearsal-0a1b2c3", long,
		"sage-rehearsal-0a1b2c3d/../shop", "sage-rehearsal-0a1b2c3d?api-version=1"} {
		f, base := newAzureFake(t)
		_, err := newAzureProvider(t, base, nil).Create(context.Background(),
			clone.CloneSpec{ResourceName: name})
		if !errors.Is(err, clone.ErrInvalidAzureRestoreConfig) || len(f.mutating()) != 0 {
			t.Fatalf("name %q: err = %v, mutating %q; want refused, nothing asked", name,
				err, f.mutating())
		}
		if name == long && !strings.Contains(err.Error(), "63") {
			t.Fatalf("64-character name: err %v does not name the 63-character limit", err)
		}
	}
	at := "sage-" + strings.Repeat("a", 49) + "-0a1b2c3d"
	f, base := newAzureFake(t)
	if c, err := newAzureProvider(t, base, nil).Create(context.Background(),
		clone.CloneSpec{ResourceName: at}); err != nil || c.ID != at || !f.exists(at) {
		t.Fatalf("63-character name: %+v, %v", c, err)
	}
}

func TestAzureRestore_InvalidConfigurationIsRefused(t *testing.T) {
	cases := map[string]func(*clone.AzureRestoreConfig){
		"no subscription":  func(c *clone.AzureRestoreConfig) { c.SubscriptionID = "" },
		"bad subscription": func(c *clone.AzureRestoreConfig) { c.SubscriptionID = "acme" },
		"no group":         func(c *clone.AzureRestoreConfig) { c.ResourceGroup = "" },
		"bad group":        func(c *clone.AzureRestoreConfig) { c.ResourceGroup = "rg/../x" },
		"dotted group":     func(c *clone.AzureRestoreConfig) { c.ResourceGroup = "rg." },
		"no server":        func(c *clone.AzureRestoreConfig) { c.Server = "" },
		"bad server":       func(c *clone.AzureRestoreConfig) { c.Server = "Shop" },
		"short server":     func(c *clone.AzureRestoreConfig) { c.Server = "ab" },
		"pg_sage clone":    func(c *clone.AzureRestoreConfig) { c.Server = azName },
		"short deadline": func(c *clone.AzureRestoreConfig) {
			c.Deadline = 29 * time.Minute
		},
		"long deadline": func(c *clone.AzureRestoreConfig) { c.Deadline = 13 * time.Hour },
		"ttl not past deadline": func(c *clone.AzureRestoreConfig) {
			c.TTL = c.Deadline
		},
		"ttl over a day": func(c *clone.AzureRestoreConfig) { c.TTL = 25 * time.Hour },
		"bad base URL":   func(c *clone.AzureRestoreConfig) { c.BaseURL = "ftp://x" },
	}
	for name, edit := range cases {
		cfg := azConfig("http://127.0.0.1:1")
		edit(&cfg)
		if api, err := clone.NewAzureRestoreAPI(cfg); !errors.Is(err,
			clone.ErrInvalidAzureRestoreConfig) || api != nil {
			t.Errorf("%s: api %v, err = %v; want ErrInvalidAzureRestoreConfig", name, api, err)
		}
	}
	cfg := azConfig("")
	cfg.Deadline, cfg.TTL = 30*time.Minute, 24*time.Hour
	cfg.SubscriptionID = strings.ToUpper(azSub)
	cfg.ResourceGroup = "RG_Shop-(prod).1"
	if _, err := clone.NewAzureRestoreAPI(cfg); err != nil {
		t.Fatalf("bounds 30m deadline / 24h TTL, uppercase subscription refused: %v", err)
	}
}

func TestAzureRestore_DescribeIsAFullCopyBoundByTheTTL(t *testing.T) {
	_, base := newAzureFake(t)
	got, err := newAzureProvider(t, base, nil).Describe(context.Background())
	if err != nil || got.Kind != clone.KindFullCopy ||
		got.MaxTTL != 3*time.Hour+15*time.Minute || got.TypicalReady != clone.SnapshotTypicalReady {
		t.Fatalf("describe = %+v, %v", got, err)
	}
	if scope := newAzureAPI(t, base, nil).TargetScope(); scope != "azure/"+azSub+"/"+azGroup {
		t.Fatalf("scope = %q", scope)
	}
}

// Nil/empty: no token source, a failing one, or an empty token makes the
// provider unavailable before any ARM call.
func TestAzureRestore_NoCredentialsIsUnavailableAndCallsNothing(t *testing.T) {
	cases := map[string]func(context.Context) (string, error){
		"nil chain": nil,
		"chain error": func(context.Context) (string, error) {
			return "", errors.New("DefaultAzureCredential: no credential")
		},
		"empty token": func(context.Context) (string, error) { return " ", nil },
	}
	for name, tok := range cases {
		f, base := newAzureFake(t)
		p := newAzureProvider(t, base, func(c *clone.AzureRestoreConfig) { c.Token = tok })
		_, err := p.Create(context.Background(), clone.CloneSpec{ResourceName: azName})
		derr := p.Destroy(context.Background(), clone.Clone{ID: azName})
		if !errors.Is(err, clone.ErrCloneUnavailable) ||
			!errors.Is(derr, clone.ErrCloneUnavailable) || f.callCount() != 0 ||
			!strings.Contains(err.Error(), "azure_restore") ||
			!strings.Contains(err.Error(), "AZURE_") {
			t.Errorf("%s: create %v, destroy %v, calls %d; want unavailable, none", name,
				err, derr, f.callCount())
		}
	}
}

func TestAzureRestore_NoProductionNeverCreates(t *testing.T) {
	f, base := newAzureFake(t)
	_, err := createAZ(t, base, func(c *clone.AzureRestoreConfig) { c.Production = nil })
	if !errors.Is(err, clone.ErrCloneUnavailable) || f.callCount() != 0 {
		t.Fatalf("err = %v, calls %d; want unavailable before any call", err, f.callCount())
	}
	f, base = newAzureFake(t)
	_, err = createAZ(t, base, func(c *clone.AzureRestoreConfig) {
		c.Production = func(context.Context) (clone.AzureProduction, error) {
			return clone.AzureProduction{DSN: "  "}, nil
		}
	})
	if !errors.Is(err, clone.ErrCloneUnavailable) || len(f.mutating()) != 0 {
		t.Fatalf("empty DSN: err = %v, mutating %q", err, f.mutating())
	}
}

// Invalid input: pg_sage restores only the server it monitors, reached on
// that server's FQDN, and only a ready primary.
func TestAzureRestore_SourceMustBeTheMonitoredPrimaryServer(t *testing.T) {
	notAZ, stopped := clone.ErrNotAzureFlexibleServer, func(s *fakeAZServer) {
		s.State = "Stopped"
	}
	cases := map[string]struct {
		dsn    string
		server string
		edit   func(*fakeAZServer)
		want   error
	}{
		"an IP host":      {dsn: "postgres://app:x@10.0.0.5/shop", want: notAZ},
		"another server":  {dsn: "postgres://app:x@other" + azSuffix + "/shop", want: notAZ},
		"a replica":       {server: "shop-replica", want: notAZ},
		"no such server":  {server: "gone-server", want: notAZ},
		"source stopped":  {edit: stopped, want: clone.ErrCloneUnavailable},
		"a pgbouncer DSN": {dsn: "postgres://app:x@" + azProdFQDN + ":6432/shop"},
	}
	for name, tc := range cases {
		f, base := newAzureFake(t)
		if tc.edit != nil {
			f.edit(azServer, tc.edit)
		}
		_, err := createAZ(t, base, func(c *clone.AzureRestoreConfig) {
			if tc.server != "" {
				c.Server = tc.server
				if tc.dsn == "" {
					tc.dsn = "postgres://app:x@" + tc.server + azSuffix + "/shop"
				}
			}
			if tc.dsn != "" {
				c.Production = func(context.Context) (clone.AzureProduction, error) {
					return clone.AzureProduction{DSN: tc.dsn}, nil
				}
			}
		})
		if tc.want == nil {
			if err != nil {
				t.Errorf("%s: %v", name, err)
			}
			continue
		}
		if !errors.Is(err, tc.want) || len(f.mutating()) != 0 {
			t.Errorf("%s: err = %v, mutating %q; want %v, nothing made", name, err,
				f.mutating(), tc.want)
		}
	}
}

// ARM may answer the PUT with a Location header only: that is polled
// until it stops answering 202.
func TestAzureRestore_LocationHeaderIsPolledWhenNoAsyncOperation(t *testing.T) {
	f, base := newAzureFake(t)
	f.locationOnly, f.doneAfter = true, 3
	c, err := createAZ(t, base, nil)
	if err != nil || c.ID != azName || f.server(azName).State != "Ready" {
		t.Fatalf("clone %+v, %v", c, err)
	}
	if polls := len(f.pollAuth()); polls != 3 {
		t.Fatalf("Location polled %d times, want 3", polls)
	}
}

// Two restores in parallel get distinct servers and DSNs. -race is not
// available on this machine (no cgo); the parallel run still checks state.
func TestAzureRestore_TwoRestoresInParallel(t *testing.T) {
	f, base := newAzureFake(t)
	f.doneAfter = 2
	p := newAzureProvider(t, base, nil)
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
