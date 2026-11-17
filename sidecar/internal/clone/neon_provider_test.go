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

// lab-05b: clone.provider neon_branch rehearses on a Neon copy-on-write
// branch of production with its own read-write endpoint.

const neonName = "sage-rehearsal-0a1b2c3d"

func neonSpec(name string) clone.CloneSpec { return clone.CloneSpec{ResourceName: name} }

func TestNeonCreate_HappyPathBranchesFromTheMonitoredEndpoint(t *testing.T) {
	f := newNeonFake(t)
	p := newNeonProvider(t, f, nil)
	c, err := p.Create(context.Background(), neonSpec(neonName))
	if err != nil {
		t.Fatal(err)
	}
	b, ok := f.branchNamed(neonName)
	if !ok || c.ID != b.ID || b.ParentID != neonProdBranch {
		t.Fatalf("clone %+v, branch %+v (found %v): want a branch of %s named %s", c, b,
			ok, neonProdBranch, neonName)
	}
	body := f.bodies[0]
	if body.Branch.ExpiresAt != neonNow.Add(time.Hour).Format(time.RFC3339) {
		t.Fatalf("expires_at = %q, want now + ttl", body.Branch.ExpiresAt)
	}
	if len(body.Endpoints) != 1 || body.Endpoints[0].Type != "read_write" ||
		body.Endpoints[0].MaxCU != 1 || body.Endpoints[0].MinCU != 0.25 {
		t.Fatalf("endpoints asked = %+v", body.Endpoints)
	}
	u, err := url.Parse(c.DSN)
	if err != nil {
		t.Fatal("clone DSN does not parse")
	}
	if u.Hostname() == neonProdHost || !strings.HasPrefix(u.Hostname(), "ep-fake-") ||
		u.Query().Get("sslmode") != "require" || u.Path != "/appdb" ||
		u.User.Username() != "app_owner" {
		t.Fatalf("clone DSN host %q sslmode %q path %q user %q", u.Hostname(),
			u.Query().Get("sslmode"), u.Path, u.User.Username())
	}
	if !c.CreatedFrom.Equal(neonNow) {
		t.Fatalf("created from %s, want %s", c.CreatedFrom, neonNow)
	}
	if err := p.Destroy(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	if f.exists(c.ID) {
		t.Fatalf("branch %s still exists after destroy", c.ID)
	}
}

func TestNeonCreate_PoolerHostResolvesTheProductionBranch(t *testing.T) {
	f := newNeonFake(t)
	p := newNeonProvider(t, f, func(c *clone.NeonConfig) {
		c.Production = func(context.Context) (clone.NeonProduction, error) {
			prod := neonProd
			prod.Host = "EP-PROD-100-pooler.us-east-2.aws.neon.tech."
			return prod, nil
		}
	})
	c, err := p.Create(context.Background(), neonSpec(neonName))
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := f.branchNamed(neonName); b.ParentID != neonProdBranch || b.ID != c.ID {
		t.Fatalf("branch %+v: want parent %s", b, neonProdBranch)
	}
}

func TestNeonCreate_ParentBranchByIDOrName(t *testing.T) {
	for _, parent := range []string{neonDevBranch, "dev"} {
		t.Run(parent, func(t *testing.T) {
			f := newNeonFake(t)
			p := newNeonProvider(t, f, func(c *clone.NeonConfig) { c.ParentBranch = parent })
			if _, err := p.Create(context.Background(), neonSpec(neonName)); err != nil {
				t.Fatal(err)
			}
			if b, _ := f.branchNamed(neonName); b.ParentID != neonDevBranch {
				t.Fatalf("parent %q made branch %+v, want parent %s", parent, b,
					neonDevBranch)
			}
		})
	}
}

func TestNeonCreate_ParentBranchOutsideTheProjectIsRefused(t *testing.T) {
	f := newNeonFake(t)
	p := newNeonProvider(t, f, func(c *clone.NeonConfig) { c.ParentBranch = "br-other-999" })
	_, err := p.Create(context.Background(), neonSpec(neonName))
	if !errors.Is(err, clone.ErrInvalidNeonConfig) ||
		!strings.Contains(err.Error(), "br-other-999") ||
		!strings.Contains(err.Error(), neonProject) {
		t.Fatalf("err = %v, want ErrInvalidNeonConfig naming the branch and project", err)
	}
	if f.countRequests("POST ") != 0 {
		t.Fatalf("a branch was asked for: %v", f.requestLog())
	}
}

func TestNeonCreate_MonitoredHostNotInTheProjectIsRefused(t *testing.T) {
	f := newNeonFake(t)
	p := newNeonProvider(t, f, func(c *clone.NeonConfig) {
		c.Production = func(context.Context) (clone.NeonProduction, error) {
			prod := neonProd
			prod.Host = "ep-elsewhere-1.eu-central-1.aws.neon.tech"
			return prod, nil
		}
	})
	_, err := p.Create(context.Background(), neonSpec(neonName))
	if !errors.Is(err, clone.ErrInvalidNeonConfig) ||
		!strings.Contains(err.Error(), "clone.neon.parent_branch") {
		t.Fatalf("err = %v, want ErrInvalidNeonConfig naming clone.neon.parent_branch", err)
	}
	if f.countRequests("POST ") != 0 {
		t.Fatalf("a branch was asked for: %v", f.requestLog())
	}
}

func TestNeonNew_InvalidConfigIsRefused(t *testing.T) {
	f := newNeonFake(t)
	cases := map[string]func(*clone.NeonConfig){
		"uppercase project": func(c *clone.NeonConfig) { c.ProjectID = "Shiny-Wind" },
		"empty project":     func(c *clone.NeonConfig) { c.ProjectID = "" },
		"slash in project":  func(c *clone.NeonConfig) { c.ProjectID = "a/b" },
		"ttl zero":          func(c *clone.NeonConfig) { c.TTL = 0 },
		"ttl above max":     func(c *clone.NeonConfig) { c.TTL = clone.NeonMaxTTL + time.Minute },
		"cu zero":           func(c *clone.NeonConfig) { c.EndpointMaxCU = 0 },
		"cu below min":      func(c *clone.NeonConfig) { c.EndpointMaxCU = 0.1 },
		"cu above max":      func(c *clone.NeonConfig) { c.EndpointMaxCU = 9 },
		"bad parent":        func(c *clone.NeonConfig) { c.ParentBranch = "dev branch" },
		"bad base url":      func(c *clone.NeonConfig) { c.BaseURL = "ftp://x" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			p, err := clone.NewNeonBranchProvider(neonConfig(f, mutate))
			if !errors.Is(err, clone.ErrInvalidNeonConfig) || p != nil {
				t.Fatalf("provider %v, err %v: want ErrInvalidNeonConfig", p, err)
			}
		})
	}
	if n := len(f.requestLog()); n != 0 {
		t.Fatalf("validation contacted Neon %d times", n)
	}
}

func TestNeonNew_TTLAndCUBoundsAreAccepted(t *testing.T) {
	f := newNeonFake(t)
	for _, c := range []struct {
		ttl time.Duration
		cu  float64
	}{{time.Minute, 0.25}, {clone.NeonMaxTTL, 8}} {
		p, err := clone.NewNeonBranchProvider(neonConfig(f, func(n *clone.NeonConfig) {
			n.TTL, n.EndpointMaxCU = c.ttl, c.cu
		}))
		if err != nil || p == nil {
			t.Fatalf("ttl %s cu %v refused: %v", c.ttl, c.cu, err)
		}
		got, err := p.Describe(context.Background())
		if err != nil || got.MaxTTL != c.ttl {
			t.Fatalf("describe = %+v, %v: want MaxTTL %s", got, err, c.ttl)
		}
	}
}

func TestNeonDescribe_IsABranchReadyUnderAMinute(t *testing.T) {
	p := newNeonProvider(t, newNeonFake(t), nil)
	c, err := clone.CheckOptIn(context.Background(), p, false)
	if err != nil {
		t.Fatalf("a Neon branch needs no full-copy opt-in: %v", err)
	}
	if c.Kind != clone.KindBranch || c.TypicalReady <= 0 || c.TypicalReady >= time.Minute ||
		c.MaxTTL != time.Hour {
		t.Fatalf("capability = %+v", c)
	}
	if p.TargetScope() != "projects/"+neonProject {
		t.Fatalf("scope = %q", p.TargetScope())
	}
	age, err := p.SnapshotAge(context.Background())
	if err != nil || age != 0 {
		t.Fatalf("snapshot age = %s, %v", age, err)
	}
}

func TestNeonCreate_NoAPIKeyIsUnavailableAndCallsNothing(t *testing.T) {
	f := newNeonFake(t)
	p := newNeonProvider(t, f, func(c *clone.NeonConfig) { c.APIKey = "  " })
	_, err := p.Create(context.Background(), neonSpec(neonName))
	if !errors.Is(err, clone.ErrCloneUnavailable) ||
		!strings.Contains(err.Error(), "clone.neon.api_key") {
		t.Fatalf("err = %v, want ErrCloneUnavailable naming clone.neon.api_key", err)
	}
	if _, _, err := p.FindByName(context.Background(), neonName); !errors.Is(err,
		clone.ErrCloneUnavailable) {
		t.Fatalf("find without a key: %v", err)
	}
	if err := p.Destroy(context.Background(), clone.Clone{ID: "br-fake-1"}); !errors.Is(err,
		clone.ErrCloneUnavailable) {
		t.Fatalf("destroy without a key: %v", err)
	}
	if n := len(f.requestLog()); n != 0 {
		t.Fatalf("Neon was called %d times without a key", n)
	}
}

func TestNeonCreate_NoProductionIdentityIsUnavailable(t *testing.T) {
	cases := map[string]func(context.Context) (clone.NeonProduction, error){
		"nil source": nil,
		"source error": func(context.Context) (clone.NeonProduction, error) {
			return clone.NeonProduction{}, errors.New("pool closed")
		},
		"no host": func(context.Context) (clone.NeonProduction, error) {
			return clone.NeonProduction{User: "u", Database: "d"}, nil
		},
		"no role": func(context.Context) (clone.NeonProduction, error) {
			return clone.NeonProduction{Host: neonProdHost, Database: "d"}, nil
		},
		"no database": func(context.Context) (clone.NeonProduction, error) {
			return clone.NeonProduction{Host: neonProdHost, User: "u"}, nil
		},
	}
	for name, source := range cases {
		t.Run(name, func(t *testing.T) {
			f := newNeonFake(t)
			p := newNeonProvider(t, f, func(c *clone.NeonConfig) { c.Production = source })
			_, err := p.Create(context.Background(), neonSpec(neonName))
			if !errors.Is(err, clone.ErrCloneUnavailable) {
				t.Fatalf("err = %v, want ErrCloneUnavailable", err)
			}
			if f.countRequests("POST ") != 0 {
				t.Fatal("a branch was asked for without a production identity")
			}
		})
	}
}

func TestNeonCreate_ResourceNameOutsideTheOwnedShapeIsRefused(t *testing.T) {
	f := newNeonFake(t)
	p := newNeonProvider(t, f, nil)
	for _, name := range []string{"", "main", "sage-rehearsal-XYZ", "sage-rehearsal-0a1b2c3",
		"sage--0a1b2c3d", "prod-rehearsal-0a1b2c3d"} {
		_, err := p.Create(context.Background(), neonSpec(name))
		if !errors.Is(err, clone.ErrInvalidNeonConfig) {
			t.Fatalf("name %q: err = %v, want ErrInvalidNeonConfig", name, err)
		}
	}
	if n := len(f.requestLog()); n != 0 {
		t.Fatalf("Neon was called %d times for refused names", n)
	}
}

// Concurrent access: two creates at once give two branches, two names
// and two DSNs (run with -race where cgo is available).
func TestNeonCreate_ConcurrentCreatesGiveTwoBranches(t *testing.T) {
	f := newNeonFake(t)
	p := newNeonProvider(t, f, nil)
	names := []string{"sage-rehearsal-00000001", "sage-rehearsal-00000002"}
	got := make([]clone.Clone, len(names))
	errs := make([]error, len(names))
	var wg sync.WaitGroup
	for i, name := range names {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got[i], errs[i] = p.Create(context.Background(), neonSpec(name))
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
	}
	if got[0].ID == got[1].ID || got[0].DSN == got[1].DSN {
		t.Fatalf("two creates shared a branch: %+v", got)
	}
	for i, name := range names {
		if b, ok := f.branchNamed(name); !ok || b.ID != got[i].ID {
			t.Fatalf("branch %s = %+v (found %v), want id %s", name, b, ok, got[i].ID)
		}
	}
}
