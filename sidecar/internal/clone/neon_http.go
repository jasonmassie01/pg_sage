package clone

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// neonAPI is the slice of the Neon API v2 the neon_branch provider uses,
// all within one project.
type neonAPI interface {
	ListEndpoints(ctx context.Context) ([]neonEndpoint, error)
	ListBranches(ctx context.Context, search string) ([]neonBranch, error)
	GetBranch(ctx context.Context, id string) (neonBranch, error)
	CreateBranch(ctx context.Context, req neonCreateRequest) (neonCreated, error)
	Operation(ctx context.Context, id string) (neonOperation, error)
	ConnectionURI(ctx context.Context, q neonURIQuery) (string, error)
	DeleteBranch(ctx context.Context, id string) error
}

type neonBranch struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	ParentID  string `json:"parent_id"`
	Default   bool   `json:"default"`
	Protected bool   `json:"protected"`
	ExpiresAt string `json:"expires_at"`
}

type neonEndpoint struct {
	ID       string `json:"id"`
	Host     string `json:"host"`
	BranchID string `json:"branch_id"`
	Type     string `json:"type"`
}

type neonOperation struct {
	ID     string `json:"id"`
	Action string `json:"action"`
	Status string `json:"status"`
	Error  string `json:"error"`
}

type neonCreated struct {
	Branch     neonBranch      `json:"branch"`
	Endpoints  []neonEndpoint  `json:"endpoints"`
	Operations []neonOperation `json:"operations"`
}

type neonCreateRequest struct {
	Branch struct {
		Name      string `json:"name"`
		ParentID  string `json:"parent_id"`
		ExpiresAt string `json:"expires_at"`
	} `json:"branch"`
	Endpoints []neonEndpointRequest `json:"endpoints"`
}

type neonEndpointRequest struct {
	Type  string  `json:"type"`
	MinCU float64 `json:"autoscaling_limit_min_cu"`
	MaxCU float64 `json:"autoscaling_limit_max_cu"`
}

type neonURIQuery struct{ BranchID, EndpointID, Database, Role string }

// neonHTTP is neonAPI over plain net/http (the egress floor's client).
// The key is sent only in the Authorization header and never appears in
// an error.
type neonHTTP struct {
	base, key, project string
	client             *http.Client
}

// neonMaxBody bounds one response read.
const neonMaxBody = 1 << 20

func (a *neonHTTP) ListEndpoints(ctx context.Context) ([]neonEndpoint, error) {
	var out struct {
		Endpoints []neonEndpoint `json:"endpoints"`
	}
	err := a.do(ctx, "list endpoints", http.MethodGet, "endpoints", nil, nil, &out)
	return out.Endpoints, err
}

func (a *neonHTTP) ListBranches(ctx context.Context, search string) ([]neonBranch, error) {
	var q url.Values
	if search != "" {
		q = url.Values{"search": {search}}
	}
	var out struct {
		Branches []neonBranch `json:"branches"`
	}
	err := a.do(ctx, "list branches", http.MethodGet, "branches", q, nil, &out)
	return out.Branches, err
}

func (a *neonHTTP) GetBranch(ctx context.Context, id string) (neonBranch, error) {
	var out struct {
		Branch neonBranch `json:"branch"`
	}
	err := a.do(ctx, "get branch", http.MethodGet, "branches/"+url.PathEscape(id), nil, nil,
		&out)
	return out.Branch, err
}

func (a *neonHTTP) CreateBranch(ctx context.Context, req neonCreateRequest) (neonCreated,
	error) {
	var out neonCreated
	err := a.do(ctx, "create branch", http.MethodPost, "branches", nil, req, &out)
	return out, err
}

func (a *neonHTTP) Operation(ctx context.Context, id string) (neonOperation, error) {
	var out struct {
		Operation neonOperation `json:"operation"`
	}
	err := a.do(ctx, "get operation", http.MethodGet, "operations/"+url.PathEscape(id), nil,
		nil, &out)
	return out.Operation, err
}

func (a *neonHTTP) ConnectionURI(ctx context.Context, q neonURIQuery) (string, error) {
	v := url.Values{"branch_id": {q.BranchID}, "database_name": {q.Database},
		"role_name": {q.Role}}
	if q.EndpointID != "" {
		v.Set("endpoint_id", q.EndpointID)
	}
	var out struct {
		URI string `json:"uri"`
	}
	err := a.do(ctx, "get connection URI", http.MethodGet, "connection_uri", v, nil, &out)
	return out.URI, err
}

func (a *neonHTTP) DeleteBranch(ctx context.Context, id string) error {
	return a.do(ctx, "delete branch", http.MethodDelete, "branches/"+url.PathEscape(id), nil,
		nil, nil)
}

// do sends one request to /projects/<project>/<path> and decodes a 2xx
// reply into out.
func (a *neonHTTP) do(ctx context.Context, op, method, path string, q url.Values, body,
	out any) error {
	req, err := a.request(ctx, method, path, q, body)
	if err != nil {
		return fmt.Errorf("neon %s: %w", op, err)
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("neon %s: %w", op, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, neonMaxBody))
	if err != nil {
		return fmt.Errorf("neon %s: read the reply: %w", op, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return a.statusError(op, resp.StatusCode, data)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("neon %s: decode the reply: %w", op, err)
	}
	return nil
}

func (a *neonHTTP) request(ctx context.Context, method, path string, q url.Values,
	body any) (*http.Request, error) {
	target := a.base + "/projects/" + url.PathEscape(a.project) + "/" + path
	if len(q) > 0 {
		target += "?" + q.Encode()
	}
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("encode the request: %w", err)
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return nil, errors.New("build the request: invalid API base URL or path")
	}
	req.Header.Set("Authorization", "Bearer "+a.key)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

// statusError is a non-2xx reply: ErrNeonNotFound for 404, else a
// ProviderStatusError (429 and 5xx are throttled, lab-05a) with Neon's
// message; 401 and 403 name the key setting.
func (a *neonHTTP) statusError(op string, code int, data []byte) error {
	var reply struct {
		Message string `json:"message"`
	}
	msg := strings.TrimSpace(string(data))
	if json.Unmarshal(data, &reply) == nil && reply.Message != "" {
		msg = reply.Message
	}
	if len(msg) > 200 {
		msg = msg[:200] + "..."
	}
	if a.key != "" {
		msg = strings.ReplaceAll(msg, a.key, "[REDACTED]")
	}
	status := &ProviderStatusError{Op: "neon " + op, StatusCode: code}
	switch code {
	case http.StatusNotFound:
		return fmt.Errorf("%w: %w: %s", ErrNeonNotFound, status, msg)
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("%w: %s: check that clone.neon.api_key (SAGE_CLONE_NEON_API_KEY) "+
			"is a Neon API key with access to project %s", status, msg, a.project)
	}
	return fmt.Errorf("%w: %s", status, msg)
}
