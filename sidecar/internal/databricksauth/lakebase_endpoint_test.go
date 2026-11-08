package databricksauth

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/databricks/databricks-sdk-go/apierr"
	"github.com/databricks/databricks-sdk-go/service/postgres"
	"github.com/jackc/pgx/v5/pgxpool"
)

// fakeEndpointAPI stands in for the workspace Postgres service. getErrs is
// consumed one error per GetEndpoint call before endpoints answer.
type fakeEndpointAPI struct {
	mu        sync.Mutex
	endpoints map[string]*postgres.Endpoint
	getErrs   []error
	gets      int
	mints     []string
}

func (f *fakeEndpointAPI) GetEndpoint(
	_ context.Context, req postgres.GetEndpointRequest,
) (*postgres.Endpoint, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gets++
	if len(f.getErrs) > 0 {
		err := f.getErrs[0]
		f.getErrs = f.getErrs[1:]
		return nil, err
	}
	ep, ok := f.endpoints[req.Name]
	if !ok {
		return nil, &apierr.APIError{StatusCode: 404, Message: "endpoint not found"}
	}
	return ep, nil
}

func (f *fakeEndpointAPI) GenerateDatabaseCredential(
	_ context.Context, req postgres.GenerateDatabaseCredentialRequest,
) (*postgres.DatabaseCredential, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mints = append(f.mints, req.Endpoint)
	return credentialFor("minted-for-"+req.Endpoint, time.Hour), nil
}

func (f *fakeEndpointAPI) counts() (int, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.gets, append([]string(nil), f.mints...)
}

func directEndpoint(name, host string) *postgres.Endpoint {
	return &postgres.Endpoint{Name: name, Status: &postgres.EndpointStatus{
		Hosts: &postgres.EndpointHosts{Host: host}}}
}

// installFakeAPI routes ConfigurePool to fake and counts constructor calls.
func installFakeAPI(t *testing.T, fake *fakeEndpointAPI) *int {
	t.Helper()
	isolateDatabricksSDK(t)
	var built int
	old := newEndpointAPI
	newEndpointAPI = func() (endpointAPI, error) {
		built++
		return fake, nil
	}
	t.Cleanup(func() { newEndpointAPI = old })
	return &built
}

func poolFor(t *testing.T, host string) *pgxpool.Config {
	t.Helper()
	cfg, err := pgxpool.ParseConfig("postgres://test@" + host + "/test?sslmode=verify-full")
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.Password = "bootstrap"
	return cfg
}

const (
	endpointA = "projects/p/branches/b/endpoints/a"
	endpointB = "projects/p/branches/b/endpoints/b"
)

func TestFleetPoolsMintOnlyFromTheirOwnEndpoint(t *testing.T) {
	fake := &fakeEndpointAPI{endpoints: map[string]*postgres.Endpoint{
		endpointA: directEndpoint(endpointA, "a.example"),
		endpointB: directEndpoint(endpointB, "b.example"),
	}}
	installFakeAPI(t, fake)
	t.Setenv("SAGE_LAKEBASE_ENDPOINT", "")
	t.Setenv("SAGE_LAKEBASE_ENDPOINTS",
		`{"a.example":"`+endpointA+`","b.example":"`+endpointB+`"}`)
	for _, tc := range []struct{ host, endpoint string }{
		{"a.example", endpointA}, {"b.example", endpointB},
	} {
		cfg := poolFor(t, tc.host)
		if err := ConfigurePool(t.Context(), cfg); err != nil {
			t.Fatalf("%s: %v", tc.host, err)
		}
		got, err := connectPassword(cfg)
		if err != nil || got != "minted-for-"+tc.endpoint {
			t.Fatalf("%s connected with %q, %v; want its own endpoint's token", tc.host, got, err)
		}
		if state, ok := CredentialStatus(tc.endpoint); !ok || state.ExpiresAt.IsZero() {
			t.Fatalf("%s status = %+v, %v; want a recorded mint", tc.endpoint, state, ok)
		}
	}
	gets, mints := fake.counts()
	if gets != 2 || len(mints) != 2 || mints[0] != endpointA || mints[1] != endpointB {
		t.Fatalf("GetEndpoint calls = %d, mints = %v; want one each, per endpoint", gets, mints)
	}
}

func TestLakebaseMappingMatchesTrailingDotAndCase(t *testing.T) {
	fake := &fakeEndpointAPI{endpoints: map[string]*postgres.Endpoint{
		endpointA: directEndpoint(endpointA, "a.example"),
	}}
	installFakeAPI(t, fake)
	t.Setenv("SAGE_LAKEBASE_ENDPOINT", "")
	t.Setenv("SAGE_LAKEBASE_ENDPOINTS", `{"A.Example.":"`+endpointA+`"}`)
	cfg := poolFor(t, "a.example")
	if err := ConfigurePool(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.BeforeConnect == nil || cfg.ConnConfig.Password != "" {
		t.Fatal("mixed-case trailing-dot mapping did not opt the pool into OAuth")
	}
}

func TestUnmappedHostKeepsPasswordAndMakesNoCalls(t *testing.T) {
	fake := &fakeEndpointAPI{}
	built := installFakeAPI(t, fake)
	t.Setenv("SAGE_LAKEBASE_ENDPOINT", "")
	t.Setenv("SAGE_LAKEBASE_ENDPOINTS", `{"a.example":"`+endpointA+`"}`)
	cfg := poolFor(t, "other.example")
	if err := ConfigurePool(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.BeforeConnect != nil || cfg.ConnConfig.Password != "bootstrap" {
		t.Fatal("unmapped host lost ordinary authentication")
	}
	if gets, _ := fake.counts(); gets != 0 || *built != 0 {
		t.Fatalf("unmapped host made %d endpoint calls (%d clients)", gets, *built)
	}
}

func TestBothEndpointVariablesMakeNoCalls(t *testing.T) {
	fake := &fakeEndpointAPI{}
	built := installFakeAPI(t, fake)
	t.Setenv("SAGE_LAKEBASE_ENDPOINT", endpointA)
	t.Setenv("SAGE_LAKEBASE_ENDPOINTS", `{"a.example":"`+endpointA+`"}`)
	err := ConfigurePool(t.Context(), poolFor(t, "a.example"))
	if err == nil || !strings.Contains(err.Error(), "set only one of") {
		t.Fatalf("both variables = %v; want the conflict refused", err)
	}
	if !errors.Is(err, ErrEndpointPermanent) {
		t.Fatalf("configuration conflict %v is retryable; want permanent", err)
	}
	if gets, _ := fake.counts(); gets != 0 || *built != 0 {
		t.Fatalf("conflicting variables made %d endpoint calls (%d clients)", gets, *built)
	}
}

func TestEndpointWithoutHostsIsRefused(t *testing.T) {
	cases := map[string]*postgres.Endpoint{
		"no status": {Name: endpointA},
		"no hosts":  {Name: endpointA, Status: &postgres.EndpointStatus{}},
	}
	for name, ep := range cases {
		t.Run(name, func(t *testing.T) {
			fake := &fakeEndpointAPI{endpoints: map[string]*postgres.Endpoint{endpointA: ep}}
			installFakeAPI(t, fake)
			t.Setenv("SAGE_LAKEBASE_ENDPOINTS", "")
			t.Setenv("SAGE_LAKEBASE_ENDPOINT", endpointA)
			cfg := poolFor(t, "a.example")
			err := ConfigurePool(t.Context(), cfg)
			if err == nil || !strings.Contains(err.Error(), "no connection hosts") {
				t.Fatalf("endpoint without hosts = %v", err)
			}
			if cfg.BeforeConnect != nil || cfg.ConnConfig.Password != "bootstrap" {
				t.Fatal("refused endpoint changed the pool's authentication")
			}
		})
	}
}

func TestGetEndpointErrorClassification(t *testing.T) {
	cases := []struct {
		name      string
		err       error
		permanent bool
	}{
		{"403", &apierr.APIError{StatusCode: 403, Message: "no access"}, true},
		{"404", &apierr.APIError{StatusCode: 404, Message: "missing"}, true},
		{"503", &apierr.APIError{StatusCode: 503, Message: "unavailable"}, false},
		{"429", &apierr.APIError{StatusCode: 429, Message: "slow down"}, false},
		{"network", errors.New("dial tcp: connection refused"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeEndpointAPI{getErrs: []error{tc.err}}
			installFakeAPI(t, fake)
			t.Setenv("SAGE_LAKEBASE_ENDPOINTS", "")
			t.Setenv("SAGE_LAKEBASE_ENDPOINT", endpointA)
			err := ConfigurePool(t.Context(), poolFor(t, "a.example"))
			if !errors.Is(err, tc.err) {
				t.Fatalf("error = %v; want the SDK error wrapped", err)
			}
			if errors.Is(err, ErrEndpointPermanent) != tc.permanent {
				t.Fatalf("permanent = %v; want %v (%v)", !tc.permanent, tc.permanent, err)
			}
			if !strings.Contains(err.Error(), "resolve Lakebase endpoint "+endpointA) {
				t.Fatalf("error %q does not name the endpoint resource", err)
			}
			if gets, mints := fake.counts(); gets != 1 || len(mints) != 0 {
				t.Fatalf("GetEndpoint calls = %d, mints = %d; want exactly one call",
					gets, len(mints))
			}
		})
	}
}

func TestTransientResolveFailureSucceedsOnLaterAttempt(t *testing.T) {
	unavailable := &apierr.APIError{StatusCode: 503, Message: "unavailable"}
	fake := &fakeEndpointAPI{getErrs: []error{unavailable, unavailable},
		endpoints: map[string]*postgres.Endpoint{endpointA: directEndpoint(endpointA, "a.example")}}
	installFakeAPI(t, fake)
	t.Setenv("SAGE_LAKEBASE_ENDPOINTS", "")
	t.Setenv("SAGE_LAKEBASE_ENDPOINT", endpointA)
	for attempt := 1; attempt <= 2; attempt++ {
		cfg := poolFor(t, "a.example")
		if err := ConfigurePool(t.Context(), cfg); err == nil || cfg.BeforeConnect != nil {
			t.Fatalf("attempt %d: want the 503 returned without installing a hook", attempt)
		}
	}
	cfg := poolFor(t, "a.example")
	if err := ConfigurePool(t.Context(), cfg); err != nil {
		t.Fatalf("third attempt: %v", err)
	}
	if got, err := connectPassword(cfg); err != nil || got != "minted-for-"+endpointA {
		t.Fatalf("connect after recovery = %q, %v", got, err)
	}
}

func TestConfigurationErrorsArePermanent(t *testing.T) {
	fake := &fakeEndpointAPI{}
	installFakeAPI(t, fake)
	t.Setenv("SAGE_LAKEBASE_ENDPOINTS", "")
	t.Setenv("SAGE_LAKEBASE_ENDPOINT", endpointA)
	cfg, err := pgxpool.ParseConfig("postgres://test@a.example/test?sslmode=require")
	if err != nil {
		t.Fatal(err)
	}
	err = ConfigurePool(t.Context(), cfg)
	if !errors.Is(err, ErrUnverifiedTLS) || !errors.Is(err, ErrEndpointPermanent) {
		t.Fatalf("unverified TLS = %v; want ErrUnverifiedTLS marked permanent", err)
	}
	if err.Error() != ErrUnverifiedTLS.Error() {
		t.Fatalf("message = %q; want the actionable TLS refusal unchanged", err)
	}
	t.Setenv("SAGE_LAKEBASE_ENDPOINT", "projects/bad")
	err = ConfigurePool(t.Context(), poolFor(t, "a.example"))
	if !errors.Is(err, ErrEndpointPermanent) {
		t.Fatalf("malformed endpoint = %v; want permanent", err)
	}
	if gets, _ := fake.counts(); gets != 0 {
		t.Fatalf("configuration errors made %d endpoint calls", gets)
	}
}

func TestWorkspaceAuthenticationFailureIsPermanent(t *testing.T) {
	isolateDatabricksSDK(t)
	authErr := errors.New("cannot configure default credentials")
	old := newEndpointAPI
	newEndpointAPI = func() (endpointAPI, error) { return nil, authErr }
	t.Cleanup(func() { newEndpointAPI = old })
	t.Setenv("SAGE_LAKEBASE_ENDPOINTS", "")
	t.Setenv("SAGE_LAKEBASE_ENDPOINT", endpointA)
	err := ConfigurePool(t.Context(), poolFor(t, "a.example"))
	if !errors.Is(err, authErr) || !errors.Is(err, ErrEndpointPermanent) {
		t.Fatalf("workspace auth failure = %v; want it wrapped and permanent", err)
	}
	if !strings.Contains(err.Error(), "configure Lakebase unified authentication") {
		t.Fatalf("error %q lacks context", err)
	}
}

func TestPooledHostRefusalIsPermanent(t *testing.T) {
	ep := &postgres.Endpoint{Name: endpointA, Status: &postgres.EndpointStatus{
		Hosts: &postgres.EndpointHosts{Host: "a.example", ReadWritePooledHost: "pooled.example"}}}
	fake := &fakeEndpointAPI{endpoints: map[string]*postgres.Endpoint{endpointA: ep}}
	installFakeAPI(t, fake)
	t.Setenv("SAGE_LAKEBASE_ENDPOINTS", "")
	t.Setenv("SAGE_LAKEBASE_ENDPOINT", endpointA)
	err := ConfigurePool(t.Context(), poolFor(t, "pooled.example"))
	if !errors.Is(err, ErrEndpointPermanent) || !strings.Contains(err.Error(), "pooled") {
		t.Fatalf("pooled host = %v; want a permanent pooled-endpoint refusal", err)
	}
	if _, mints := fake.counts(); len(mints) != 0 {
		t.Fatal("pooled host received a minted credential")
	}
}

// No concurrent access test here: ConfigurePool keeps no shared state of its
// own; the credential cache's concurrency is tested in lakebase_credential_test.
