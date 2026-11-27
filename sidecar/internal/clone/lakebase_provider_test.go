package clone

import (
	"context"
	"errors"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// market-03: a Lakebase branch is a copy of production data, so the
// provider creates it with a server-side expiry, deletes what it made when
// any later step fails, and refuses a branch or endpoint that is
// production's.

const testLakebaseToken = "lakebase-minted-token-never-log"

var testLakebaseID = regexp.MustCompile(`^projects/shop/branches/sage-rh-[0-9a-f]{8}$`)

// fakeBranchAPI is the Lakebase control plane: it records every call in
// order and fails the steps a test names.
type fakeBranchAPI struct {
	mu        sync.Mutex
	calls     []string
	branches  []LakebaseBranchRequest
	endpoints []LakebaseEndpointRequest

	createBranchErr   error
	createEndpointErr error
	credentialErr     error
	deleteEndpointErr error
	deleteBranchErr   error
	blockBranch       bool   // CreateBranch waits until its context ends
	branchUID         string // overrides the created branch's uid
	endpointHost      string // overrides the created endpoint's host
	emptyToken        bool
}

func (f *fakeBranchAPI) record(call string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
}

func (f *fakeBranchAPI) callList() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeBranchAPI) CreateBranch(ctx context.Context,
	req LakebaseBranchRequest) (LakebaseBranch, error) {
	f.mu.Lock()
	f.branches = append(f.branches, req)
	f.mu.Unlock()
	name := req.Parent + "/branches/" + req.BranchID
	f.record("create_branch " + name)
	if f.blockBranch {
		<-ctx.Done()
		return LakebaseBranch{}, ctx.Err()
	}
	if f.createBranchErr != nil {
		return LakebaseBranch{}, f.createBranchErr
	}
	uid := "uid-" + req.BranchID
	if f.branchUID != "" {
		uid = f.branchUID
	}
	return LakebaseBranch{Name: name, UID: uid}, nil
}

func (f *fakeBranchAPI) CreateEndpoint(_ context.Context,
	req LakebaseEndpointRequest) (LakebaseEndpoint, error) {
	f.mu.Lock()
	f.endpoints = append(f.endpoints, req)
	f.mu.Unlock()
	name := req.Parent + "/endpoints/" + req.EndpointID
	f.record("create_endpoint " + name)
	if f.createEndpointErr != nil {
		return LakebaseEndpoint{}, f.createEndpointErr
	}
	host := req.EndpointID + ".lakebase.invalid"
	if f.endpointHost != "" {
		host = f.endpointHost
	}
	return LakebaseEndpoint{Name: name, UID: "ep-" + req.EndpointID, Host: host}, nil
}

func (f *fakeBranchAPI) DatabaseCredential(_ context.Context, endpoint string) (string,
	error) {
	f.record("credential " + endpoint)
	if f.credentialErr != nil {
		return "", f.credentialErr
	}
	if f.emptyToken {
		return "", nil
	}
	return testLakebaseToken, nil
}

func (f *fakeBranchAPI) DeleteEndpoint(_ context.Context, name string) error {
	f.record("delete_endpoint " + name)
	return f.deleteEndpointErr
}

func (f *fakeBranchAPI) DeleteBranch(_ context.Context, name string) error {
	f.record("delete_branch " + name)
	return f.deleteBranchErr
}

func testProduction() LakebaseProduction {
	return LakebaseProduction{BranchName: "projects/shop/branches/production",
		BranchUID: "uid-production", Host: "ep-prod.lakebase.invalid",
		User: "sage@corp.invalid", Database: "databricks_postgres"}
}

func testLakebaseConfig(api LakebaseBranchAPI) LakebaseConfig {
	return LakebaseConfig{Project: "shop", TTL: time.Hour, EndpointMaxCU: 2,
		WaitTimeout: 5 * time.Second,
		API:         func() (LakebaseBranchAPI, error) { return api, nil },
		Production: func(context.Context) (LakebaseProduction, error) {
			return testProduction(), nil
		},
		Now: testNow}
}

func newLakebaseForTest(t *testing.T, cfg LakebaseConfig) *LakebaseBranchProvider {
	t.Helper()
	p, err := NewLakebaseBranchProvider(cfg)
	if err != nil {
		t.Fatalf("NewLakebaseBranchProvider: %v", err)
	}
	return p
}

func TestLakebase_CreateReadyDestroyInOrder(t *testing.T) {
	api := &fakeBranchAPI{}
	p := newLakebaseForTest(t, testLakebaseConfig(api))
	c, err := p.Create(context.Background(), CloneSpec{IncludeData: true})
	if err != nil {
		t.Fatal(err)
	}
	if !testLakebaseID.MatchString(c.ID) {
		t.Fatalf("clone id %q is not a sage-rh branch of project shop", c.ID)
	}
	id := strings.TrimPrefix(c.ID, "projects/shop/branches/")
	endpoint := c.ID + "/endpoints/" + id
	want := []string{"create_branch " + c.ID, "create_endpoint " + endpoint,
		"credential " + endpoint}
	if got := api.callList(); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("create calls = %q, want %q", got, want)
	}
	b := api.branches[0]
	if b.Parent != "projects/shop" || b.SourceBranch != "projects/shop/branches/production" ||
		!b.ExpireTime.Equal(testNow().Add(time.Hour)) {
		t.Fatalf("branch request = %+v, want the production branch, expiring in 1h", b)
	}
	if e := api.endpoints[0]; e.Parent != c.ID || e.MaxCU != 2 || e.EndpointID != id {
		t.Fatalf("endpoint request = %+v", e)
	}
	assertLakebaseDSN(t, c.DSN, id+".lakebase.invalid")
	if !c.CreatedFrom.Equal(testNow()) {
		t.Fatalf("created from %s, want the branch time %s", c.CreatedFrom, testNow())
	}
	if err := p.Destroy(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	want = append(want, "delete_endpoint "+endpoint, "delete_branch "+c.ID)
	if got := api.callList(); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("calls after destroy = %q, want %q", got, want)
	}
}

// assertLakebaseDSN checks the DSN carries the minted token as password,
// production's role and database, and verified TLS.
func assertLakebaseDSN(t *testing.T, dsn, host string) {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("clone DSN does not parse: %v", err)
	}
	password, _ := u.User.Password()
	if u.Scheme != "postgres" || u.Hostname() != host || u.Port() != "5432" ||
		u.User.Username() != "sage@corp.invalid" || password != testLakebaseToken ||
		u.Path != "/databricks_postgres" || u.Query().Get("sslmode") != "verify-full" {
		t.Fatalf("clone DSN host=%s port=%s user=%s path=%s query=%s (password set: %v)",
			u.Hostname(), u.Port(), u.User.Username(), u.Path, u.RawQuery, password != "")
	}
}

func TestLakebase_ExplicitSourceBranch(t *testing.T) {
	for source, want := range map[string]string{
		"staging":                       "projects/shop/branches/staging",
		"projects/shop/branches/stage2": "projects/shop/branches/stage2",
	} {
		api := &fakeBranchAPI{}
		cfg := testLakebaseConfig(api)
		cfg.SourceBranch = source
		p := newLakebaseForTest(t, cfg)
		if _, err := p.Create(context.Background(), CloneSpec{}); err != nil {
			t.Fatalf("%s: %v", source, err)
		}
		if got := api.branches[0].SourceBranch; got != want {
			t.Errorf("source %q branched from %q, want %q", source, got, want)
		}
	}
}

func TestLakebase_EndpointFailureDeletesBranch(t *testing.T) {
	cause := errors.New("quota exceeded")
	api := &fakeBranchAPI{createEndpointErr: cause}
	p := newLakebaseForTest(t, testLakebaseConfig(api))
	c, err := p.Create(context.Background(), CloneSpec{})
	if !errors.Is(err, cause) || !strings.Contains(err.Error(), "create endpoint") {
		t.Fatalf("endpoint failure = %v, want it named and wrapped", err)
	}
	if c != (Clone{}) {
		t.Fatalf("a failed create returned clone %+v", c)
	}
	calls := api.callList()
	if len(calls) != 4 || !strings.HasPrefix(calls[2], "delete_endpoint ") ||
		!strings.HasPrefix(calls[3], "delete_branch ") ||
		strings.TrimPrefix(calls[3], "delete_branch ") !=
			strings.TrimPrefix(calls[0], "create_branch ") {
		t.Fatalf("calls = %q, want the created branch deleted", calls)
	}
	assertNoToken(t, err)
}

func TestLakebase_CredentialFailureDeletesEndpointThenBranch(t *testing.T) {
	cause := errors.New("permission denied: CAN_MANAGE required")
	api := &fakeBranchAPI{credentialErr: cause}
	p := newLakebaseForTest(t, testLakebaseConfig(api))
	_, err := p.Create(context.Background(), CloneSpec{})
	if !errors.Is(err, cause) || !strings.Contains(err.Error(), "credential") {
		t.Fatalf("credential failure = %v", err)
	}
	calls := api.callList()
	if len(calls) != 5 || !strings.HasPrefix(calls[3], "delete_endpoint ") ||
		!strings.HasPrefix(calls[4], "delete_branch ") {
		t.Fatalf("calls = %q, want endpoint then branch deleted", calls)
	}
}

func TestLakebase_EmptyTokenIsAFailure(t *testing.T) {
	api := &fakeBranchAPI{emptyToken: true}
	p := newLakebaseForTest(t, testLakebaseConfig(api))
	_, err := p.Create(context.Background(), CloneSpec{})
	if err == nil || !strings.Contains(err.Error(), "empty credential") {
		t.Fatalf("empty token = %v", err)
	}
	if calls := api.callList(); calls[len(calls)-1] != "delete_branch "+
		strings.TrimPrefix(calls[0], "create_branch ") {
		t.Fatalf("calls = %q, want the branch deleted", calls)
	}
}

func TestLakebase_CleanupFailureIsReported(t *testing.T) {
	api := &fakeBranchAPI{createEndpointErr: errors.New("boom"),
		deleteBranchErr: errors.New("control plane down")}
	p := newLakebaseForTest(t, testLakebaseConfig(api))
	_, err := p.Create(context.Background(), CloneSpec{})
	if err == nil || !strings.Contains(err.Error(), "boom") ||
		!strings.Contains(err.Error(), "control plane down") ||
		!strings.Contains(err.Error(), "expires") {
		t.Fatalf("cleanup failure = %v, want both errors and the expiry backstop named", err)
	}
	if !errors.Is(err, ErrCloneLeftBehind) || !uncertainCreate(err) {
		t.Fatalf("a create that left a branch behind is not uncertain: %v", err)
	}
}

// A wait past the deadline is uncertain (the branch may exist): the
// receipt becomes create_uncertain and the server-side expiry, set on the
// request, removes the branch if the cleanup cannot.
func TestLakebase_WaitPastDeadlineIsUncertainWithExpirySet(t *testing.T) {
	api := &fakeBranchAPI{blockBranch: true}
	cfg := testLakebaseConfig(api)
	cfg.WaitTimeout = 30 * time.Millisecond
	p := newLakebaseForTest(t, cfg)
	started := time.Now()
	_, err := p.Create(context.Background(), CloneSpec{})
	if !errors.Is(err, context.DeadlineExceeded) || !uncertainCreate(err) {
		t.Fatalf("timed-out create = %v, want an uncertain deadline error", err)
	}
	if time.Since(started) > 3*time.Second {
		t.Fatalf("create waited %s past a 30ms deadline", time.Since(started))
	}
	if exp := api.branches[0].ExpireTime; !exp.Equal(testNow().Add(time.Hour)) {
		t.Fatalf("server-side expiry = %s, want %s", exp, testNow().Add(time.Hour))
	}
	calls := api.callList()
	if len(calls) != 2 || !strings.HasPrefix(calls[1], "delete_branch ") {
		t.Fatalf("calls = %q, want a best-effort delete of the named branch", calls)
	}
}

func TestLakebase_CallerCancelStopsTheWait(t *testing.T) {
	api := &fakeBranchAPI{blockBranch: true}
	p := newLakebaseForTest(t, testLakebaseConfig(api))
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()
	_, err := p.Create(ctx, CloneSpec{})
	if !errors.Is(err, context.Canceled) || !uncertainCreate(err) {
		t.Fatalf("cancelled create = %v", err)
	}
}

func TestLakebase_IdentityGuardRefusesProductionEndpoint(t *testing.T) {
	for name, api := range map[string]*fakeBranchAPI{
		"endpoint host":           {endpointHost: "ep-prod.lakebase.invalid"},
		"endpoint host, any case": {endpointHost: "EP-PROD.lakebase.invalid."},
		"branch uid":              {branchUID: "uid-production"},
	} {
		p := newLakebaseForTest(t, testLakebaseConfig(api))
		c, err := p.Create(context.Background(), CloneSpec{})
		if !errors.Is(err, ErrCloneIsProduction) || c != (Clone{}) {
			t.Errorf("%s: create = %+v, %v; want ErrCloneIsProduction", name, c, err)
			continue
		}
		calls := api.callList()
		if last := calls[len(calls)-1]; !strings.HasPrefix(last, "delete_branch ") {
			t.Errorf("%s: calls = %q, want what was created deleted", name, calls)
		}
		for _, call := range calls {
			if strings.HasPrefix(call, "credential ") {
				t.Errorf("%s: a credential was minted for a refused target", name)
			}
		}
	}
}

func TestLakebase_BlankCloneIdentityIsRefused(t *testing.T) {
	for name, api := range map[string]*fakeBranchAPI{
		"blank endpoint host": {endpointHost: " "},
		"blank branch uid":    {branchUID: " "},
	} {
		p := newLakebaseForTest(t, testLakebaseConfig(api))
		_, err := p.Create(context.Background(), CloneSpec{})
		if err == nil || !strings.Contains(err.Error(), "cannot be shown") {
			t.Errorf("%s: %v, want an unverified clone refused", name, err)
			continue
		}
		calls := api.callList()
		if last := calls[len(calls)-1]; !strings.HasPrefix(last, "delete_branch ") {
			t.Errorf("%s: calls = %q, want the branch deleted", name, calls)
		}
	}
}

func TestLakebase_IncompleteProductionIdentityFailsClosed(t *testing.T) {
	for name, mutate := range map[string]func(*LakebaseProduction){
		"no branch uid": func(p *LakebaseProduction) { p.BranchUID = "" },
		"no host":       func(p *LakebaseProduction) { p.Host = "" },
		"no user":       func(p *LakebaseProduction) { p.User = "" },
		"bad branch":    func(p *LakebaseProduction) { p.BranchName = "main" },
	} {
		api := &fakeBranchAPI{}
		cfg := testLakebaseConfig(api)
		cfg.Production = func(context.Context) (LakebaseProduction, error) {
			prod := testProduction()
			mutate(&prod)
			return prod, nil
		}
		p := newLakebaseForTest(t, cfg)
		if _, err := p.Create(context.Background(), CloneSpec{}); !errors.Is(err,
			ErrCloneUnavailable) {
			t.Errorf("%s: %v, want ErrCloneUnavailable", name, err)
		}
		if calls := api.callList(); len(calls) != 0 {
			t.Errorf("%s: control plane called: %q", name, calls)
		}
	}
}

func TestLakebase_DefaultSourceOutsideProjectIsRefused(t *testing.T) {
	api := &fakeBranchAPI{}
	cfg := testLakebaseConfig(api)
	cfg.Production = func(context.Context) (LakebaseProduction, error) {
		prod := testProduction()
		prod.BranchName = "projects/other/branches/main"
		return prod, nil
	}
	p := newLakebaseForTest(t, cfg)
	_, err := p.Create(context.Background(), CloneSpec{})
	if !errors.Is(err, ErrInvalidLakebaseConfig) || !strings.Contains(err.Error(), "other") {
		t.Fatalf("monitored branch outside the project = %v", err)
	}
	if calls := api.callList(); len(calls) != 0 {
		t.Fatalf("control plane called: %q", calls)
	}
}

func TestLakebase_NoDatabricksAuthIsUnavailable(t *testing.T) {
	cfg := testLakebaseConfig(nil)
	cfg.API = func() (LakebaseBranchAPI, error) {
		return nil, errors.New("default auth: cannot configure default credentials")
	}
	p := newLakebaseForTest(t, cfg)
	_, err := p.Create(context.Background(), CloneSpec{})
	if !errors.Is(err, ErrCloneUnavailable) || !strings.Contains(err.Error(), "Databricks") ||
		!strings.Contains(err.Error(), "default credentials") {
		t.Fatalf("no auth = %v, want unavailable with the reason", err)
	}
	if err := p.Destroy(context.Background(),
		Clone{ID: "projects/shop/branches/sage-rh-0123abcd"}); !errors.Is(err,
		ErrCloneUnavailable) {
		t.Fatalf("destroy without auth = %v", err)
	}
}

func TestLakebase_NoProductionSourceIsUnavailable(t *testing.T) {
	api := &fakeBranchAPI{}
	cfg := testLakebaseConfig(api)
	cfg.Production = nil
	p := newLakebaseForTest(t, cfg)
	_, err := p.Create(context.Background(), CloneSpec{})
	if !errors.Is(err, ErrCloneUnavailable) || !strings.Contains(err.Error(), "production") {
		t.Fatalf("no production identity = %v", err)
	}
	cfg.Production = func(context.Context) (LakebaseProduction, error) {
		return LakebaseProduction{}, errors.New("endpoint not found")
	}
	p = newLakebaseForTest(t, cfg)
	if _, err = p.Create(context.Background(), CloneSpec{}); !errors.Is(err,
		ErrCloneUnavailable) || !strings.Contains(err.Error(), "endpoint not found") {
		t.Fatalf("production lookup error = %v", err)
	}
	if calls := api.callList(); len(calls) != 0 {
		t.Fatalf("control plane called without a production identity: %q", calls)
	}
}

func TestNewLakebaseBranchProvider_Invalid(t *testing.T) {
	cases := map[string]func(*LakebaseConfig){
		"no project":        func(c *LakebaseConfig) { c.Project = "" },
		"uppercase project": func(c *LakebaseConfig) { c.Project = "Shop" },
		"ttl zero":          func(c *LakebaseConfig) { c.TTL = 0 },
		"ttl over a day":    func(c *LakebaseConfig) { c.TTL = 24*time.Hour + time.Minute },
		"ttl negative":      func(c *LakebaseConfig) { c.TTL = -time.Minute },
		"max cu zero":       func(c *LakebaseConfig) { c.EndpointMaxCU = 0 },
		"max cu negative":   func(c *LakebaseConfig) { c.EndpointMaxCU = -1 },
		"no api":            func(c *LakebaseConfig) { c.API = nil },
		"source in other project": func(c *LakebaseConfig) {
			c.SourceBranch = "projects/x/branches/main"
		},
		"uppercase source": func(c *LakebaseConfig) { c.SourceBranch = "Main" },
		"source with endpoint": func(c *LakebaseConfig) {
			c.SourceBranch = "projects/shop/branches/main/endpoints/e"
		},
		"source leading hyphen": func(c *LakebaseConfig) { c.SourceBranch = "-main" },
	}
	for name, mutate := range cases {
		cfg := testLakebaseConfig(&fakeBranchAPI{})
		mutate(&cfg)
		p, err := NewLakebaseBranchProvider(cfg)
		if !errors.Is(err, ErrInvalidLakebaseConfig) || p != nil {
			t.Errorf("%s: %v, %v; want ErrInvalidLakebaseConfig", name, p, err)
		}
	}
}

func TestNewLakebaseBranchProvider_Boundaries(t *testing.T) {
	for _, ttl := range []time.Duration{time.Minute, 24 * time.Hour} {
		cfg := testLakebaseConfig(&fakeBranchAPI{})
		cfg.TTL = ttl
		if _, err := NewLakebaseBranchProvider(cfg); err != nil {
			t.Errorf("ttl %s refused: %v", ttl, err)
		}
	}
	cfg := testLakebaseConfig(&fakeBranchAPI{})
	cfg.WaitTimeout, cfg.Now = 0, nil
	p := newLakebaseForTest(t, cfg)
	if p.cfg.WaitTimeout != defaultLakebaseWait || p.cfg.Now == nil {
		t.Fatalf("zero wait/clock not defaulted: %s", p.cfg.WaitTimeout)
	}
}
