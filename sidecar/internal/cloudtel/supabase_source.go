package cloudtel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/egress"
)

// Supabase Data API exposure (fleetpol-02c). Whether a table that anon can
// read is reachable from the internet depends on which schemas the
// project's Data API (PostgREST) serves, which SQL cannot see. One
// Management API read answers it: GET /v1/projects/{ref}/postgrest, whose
// db_schema lists the served schemas. Only db_schema is decoded: the same
// response carries the project's JWT secret, which is never kept, logged
// or echoed.

// SupabaseAPI is the one Management API read the Data API exposure needs.
type SupabaseAPI interface {
	DataAPISettings(ctx context.Context) (SupabaseDataAPISettings, error)
}

// SupabaseDataAPISettings is the project's Data API configuration.
type SupabaseDataAPISettings struct {
	// Schemas are the schemas the Data API serves, in the project's order.
	Schemas []string
}

// Errors of the Management API read, each distinguishable with errors.Is.
var (
	ErrSupabaseUnauthorized = errors.New("supabase management API refused the token")
	ErrSupabaseRateLimited  = errors.New("supabase management API rate limit reached")
	ErrSupabaseMalformed    = errors.New("supabase management API response unusable")
	ErrSupabaseAPI          = errors.New("supabase management API call failed")
)

// supabaseMaxResponse bounds one Management API response.
const supabaseMaxResponse = 1 << 20

var supabaseProjectRef = regexp.MustCompile(`^[a-z0-9]{20}$`)

// SupabaseHTTPOptions name the project and the Management API token.
type SupabaseHTTPOptions struct {
	Project, Token string
	Endpoint       string // "" is https://api.supabase.com; tests use a loopback URL
	HTTPClient     *http.Client
}

// SupabaseHTTPAPI reads one project's Data API settings over HTTPS.
type SupabaseHTTPAPI struct {
	project, token, endpoint string
	client                   *http.Client
}

// NewSupabaseHTTPAPI validates the options; it makes no network call.
func NewSupabaseHTTPAPI(opts SupabaseHTTPOptions) (*SupabaseHTTPAPI, error) {
	if !supabaseProjectRef.MatchString(opts.Project) {
		return nil, errors.New("supabase data API: the project ref must be 20 lower-case " +
			"letters or digits")
	}
	if strings.TrimSpace(opts.Token) == "" || strings.ContainsAny(opts.Token, "\r\n") {
		return nil, errors.New("supabase data API: a Management API token is required " +
			"(one line)")
	}
	endpoint := opts.Endpoint
	if endpoint == "" {
		endpoint = "https://api.supabase.com"
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" || u.User != nil ||
		(u.Scheme != "https" && !(u.Scheme == "http" && isLoopbackHost(u.Hostname()))) {
		return nil, fmt.Errorf("supabase data API: endpoint %q must be an https URL",
			endpoint)
	}
	client := opts.HTTPClient
	if client == nil {
		client = egress.HTTPClient(20 * time.Second)
	}
	c := *client
	// A redirect is refused: the bearer token is never replayed elsewhere.
	c.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &SupabaseHTTPAPI{project: opts.Project, token: opts.Token,
		endpoint: strings.TrimRight(endpoint, "/"), client: &c}, nil
}

func isLoopbackHost(host string) bool {
	return host == "127.0.0.1" || host == "localhost" || host == "::1"
}

// DataAPISettings reads the project's served schemas. A response without
// them is ErrSupabaseMalformed: unknown, never "no schema served".
func (a *SupabaseHTTPAPI) DataAPISettings(ctx context.Context) (SupabaseDataAPISettings,
	error) {
	raw, err := a.get(ctx, "/v1/projects/"+a.project+"/postgrest")
	if err != nil {
		return SupabaseDataAPISettings{}, err
	}
	return parseDataAPISettings(raw)
}

// get sends the authorized GET and returns a bounded 200 body. Errors name
// the call and the status, never the token or the body.
func (a *SupabaseHTTPAPI) get(ctx context.Context, path string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.endpoint+path, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: build the data API settings request", ErrSupabaseAPI)
	}
	req.Header.Set("Authorization", "Bearer "+a.token)
	req.Header.Set("Accept", "application/json")
	resp, err := a.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("%w: data API settings: api.supabase.com unreachable; "+
			"check network access", ErrSupabaseAPI)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, supabaseMaxResponse+1))
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return nil, fmt.Errorf("%w: data API settings HTTP %d; check the token and its "+
			"access to the project", ErrSupabaseUnauthorized, resp.StatusCode)
	case resp.StatusCode == http.StatusTooManyRequests:
		return nil, fmt.Errorf("%w: data API settings HTTP 429; retried in an hour",
			ErrSupabaseRateLimited)
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("%w: data API settings HTTP %d", ErrSupabaseAPI,
			resp.StatusCode)
	case err != nil || len(raw) > supabaseMaxResponse:
		return nil, fmt.Errorf("%w: data API settings response unreadable or larger "+
			"than %d bytes", ErrSupabaseMalformed, supabaseMaxResponse)
	}
	return raw, nil
}

// parseDataAPISettings decodes db_schema, a comma-separated list. Missing,
// null, non-string or empty is malformed.
func parseDataAPISettings(raw []byte) (SupabaseDataAPISettings, error) {
	var body struct {
		DBSchema *string `json:"db_schema"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return SupabaseDataAPISettings{}, fmt.Errorf("%w: data API settings are not "+
			"the expected JSON object", ErrSupabaseMalformed)
	}
	if body.DBSchema == nil {
		return SupabaseDataAPISettings{}, fmt.Errorf("%w: data API settings name no "+
			"db_schema", ErrSupabaseMalformed)
	}
	var schemas []string
	for _, s := range strings.Split(*body.DBSchema, ",") {
		s = strings.TrimSpace(s)
		if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
			s = s[1 : len(s)-1]
		}
		if s != "" {
			schemas = append(schemas, s)
		}
	}
	if len(schemas) == 0 {
		return SupabaseDataAPISettings{}, fmt.Errorf("%w: data API settings list no "+
			"schema in db_schema", ErrSupabaseMalformed)
	}
	return SupabaseDataAPISettings{Schemas: schemas}, nil
}
