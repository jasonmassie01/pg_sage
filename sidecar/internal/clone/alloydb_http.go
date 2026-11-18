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
	"regexp"
	"strings"
	"time"
)

// alloyDBCluster is what the provider reads of an AlloyDB cluster. The
// network and encryption settings are kept verbatim, so a restore lands in
// the source's VPC under its key.
type alloyDBCluster struct {
	Name                   string            `json:"name"`
	State                  string            `json:"state"`
	ClusterType            string            `json:"clusterType"`
	DatabaseVersion        string            `json:"databaseVersion"`
	Labels                 map[string]string `json:"labels"`
	Network                string            `json:"network"`
	NetworkConfig          json.RawMessage   `json:"networkConfig"`
	EncryptionConfig       json.RawMessage   `json:"encryptionConfig"`
	ContinuousBackupConfig struct {
		Enabled bool `json:"enabled"`
	} `json:"continuousBackupConfig"`
	ContinuousBackupInfo struct {
		EarliestRestorableTime time.Time `json:"earliestRestorableTime"`
	} `json:"continuousBackupInfo"`
}

// alloyDBInstance is what the provider reads of an AlloyDB instance.
type alloyDBInstance struct {
	Name            string          `json:"name"`
	State           string          `json:"state"`
	InstanceType    string          `json:"instanceType"`
	IPAddress       string          `json:"ipAddress"`
	PublicIPAddress string          `json:"publicIpAddress"`
	MachineConfig   json.RawMessage `json:"machineConfig"`
	NetworkConfig   json.RawMessage `json:"networkConfig"`
}

// ipKind is "private" or "public" when host is one of the instance's
// addresses, else "".
func (in alloyDBInstance) ipKind(host string) string {
	switch {
	case sameHost(in.IPAddress, host):
		return "private"
	case sameHost(in.PublicIPAddress, host):
		return "public"
	}
	return ""
}

// ipOf is the instance's address of kind.
func (in alloyDBInstance) ipOf(kind string) string {
	if kind == "public" {
		return in.PublicIPAddress
	}
	return in.IPAddress
}

// alloyDBOperation is a long-running Admin API operation.
type alloyDBOperation struct {
	Name  string `json:"name"`
	Done  bool   `json:"done"`
	Error *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// alloyDBHTTP is the Admin API v1 of one project and region over plain
// net/http. The token is sent only in the Authorization header and never
// appears in an error.
type alloyDBHTTP struct {
	base, project, parent string
	token                 CloudSQLTokenSource
	client                *http.Client
	poll                  time.Duration
}

// The IAM permission each call needs, named when Google refuses it.
var alloyDBPermissions = map[string]string{
	"clusters.get": "alloydb.clusters.get", "clusters.restore": "alloydb.clusters.restore",
	"clusters.delete": "alloydb.clusters.delete", "instances.list": "alloydb.instances.list",
	"instances.get": "alloydb.instances.get", "instances.create": "alloydb.instances.create",
	"operations.get": "alloydb.operations.get",
}

var alloyDBOpID = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

func (a *alloyDBHTTP) clusterName(id string) string { return a.parent + "/clusters/" + id }

func (a *alloyDBHTTP) GetCluster(ctx context.Context, id string) (alloyDBCluster, error) {
	var out alloyDBCluster
	err := a.do(ctx, "clusters.get", http.MethodGet, "clusters/"+url.PathEscape(id), nil,
		nil, &out)
	if err == nil && out.Name != a.clusterName(id) {
		err = fmt.Errorf("alloydb clusters.get %s: the reply names cluster %q", id, out.Name)
	}
	return out, err
}

func (a *alloyDBHTTP) ListInstances(ctx context.Context, cluster string) ([]alloyDBInstance,
	error) {
	var out struct {
		Instances []alloyDBInstance `json:"instances"`
	}
	err := a.do(ctx, "instances.list", http.MethodGet,
		"clusters/"+url.PathEscape(cluster)+"/instances", url.Values{"pageSize": {"100"}},
		nil, &out)
	return out.Instances, err
}

func (a *alloyDBHTTP) GetInstance(ctx context.Context, cluster, id string) (alloyDBInstance,
	error) {
	var out alloyDBInstance
	err := a.do(ctx, "instances.get", http.MethodGet, "clusters/"+url.PathEscape(cluster)+
		"/instances/"+url.PathEscape(id), nil, nil, &out)
	if want := a.clusterName(cluster) + "/instances/" + id; err == nil && out.Name != want {
		err = fmt.Errorf("alloydb instances.get %s: the reply names instance %q", want,
			out.Name)
	}
	return out, err
}

func (a *alloyDBHTTP) Restore(ctx context.Context, body any) (alloyDBOperation, error) {
	var out alloyDBOperation
	err := a.do(ctx, "clusters.restore", http.MethodPost, "clusters:restore", nil, body, &out)
	return out, err
}

func (a *alloyDBHTTP) CreateInstance(ctx context.Context, cluster, id string,
	body any) (alloyDBOperation, error) {
	var out alloyDBOperation
	err := a.do(ctx, "instances.create", http.MethodPost,
		"clusters/"+url.PathEscape(cluster)+"/instances", url.Values{"instanceId": {id}},
		body, &out)
	return out, err
}

// DeleteCluster deletes cluster id with force, so its instances go too.
func (a *alloyDBHTTP) DeleteCluster(ctx context.Context, id string) error {
	return a.do(ctx, "clusters.delete", http.MethodDelete, "clusters/"+url.PathEscape(id),
		url.Values{"force": {"true"}}, nil, nil)
}

// Operation reads operation name, which must be one of this project and
// region's operations.
func (a *alloyDBHTTP) Operation(ctx context.Context, name string) (alloyDBOperation, error) {
	id, ok := strings.CutPrefix(name, a.parent+"/operations/")
	if !ok || !alloyDBOpID.MatchString(id) {
		return alloyDBOperation{}, fmt.Errorf("alloydb operations.get: operation %q is not "+
			"one of %s", name, a.parent)
	}
	var out alloyDBOperation
	err := a.do(ctx, "operations.get", http.MethodGet, "operations/"+id, nil, nil, &out)
	return out, err
}

// do sends one request to /v1/<parent>/<path> and decodes a 2xx reply
// into out.
func (a *alloyDBHTTP) do(ctx context.Context, op, method, path string, query url.Values,
	body, out any) error {
	req, err := a.request(ctx, method, path, query, body)
	if err != nil {
		return fmt.Errorf("alloydb %s: %w", op, err)
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("alloydb %s: %w", op, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, cloudSQLMaxBody))
	if err != nil {
		return fmt.Errorf("alloydb %s: read the reply: %w", op, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return a.statusError(op, resp.StatusCode, data)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("alloydb %s: decode the reply: %w", op, err)
	}
	return nil
}

func (a *alloyDBHTTP) request(ctx context.Context, method, path string, query url.Values,
	body any) (*http.Request, error) {
	target := a.base + "/v1/" + a.parent + "/" + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("encode the request: %w", err)
		}
		reader = bytes.NewReader(data)
	}
	tok, err := a.token.Token(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w: refresh the Google access token: %w",
			ErrCloneUnavailable, err)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return nil, errors.New("build the request: invalid API base URL or path")
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

// statusError is a non-2xx reply: ErrAlloyDBNotFound for 404,
// ErrAlloyDBAlreadyExists for a create's 409 (another call's 409 is an
// operation in progress), ErrAlloyDBPermission naming the IAM permission
// for 401 and 403, each also a ProviderStatusError (429 and 5xx are
// throttled, lab-05a) with Google's message.
func (a *alloyDBHTTP) statusError(op string, code int, data []byte) error {
	var reply struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	msg := strings.TrimSpace(string(data))
	if json.Unmarshal(data, &reply) == nil && reply.Error.Message != "" {
		msg = reply.Error.Message
	}
	if len(msg) > 200 {
		msg = msg[:200] + "..."
	}
	status := &ProviderStatusError{Op: "alloydb " + op, StatusCode: code}
	switch code {
	case http.StatusNotFound:
		return fmt.Errorf("%w: %w: %s", ErrAlloyDBNotFound, status, msg)
	case http.StatusConflict:
		if op != "clusters.restore" && op != "instances.create" {
			return fmt.Errorf("%w: %s (another operation on the cluster is running)",
				status, msg)
		}
		return fmt.Errorf("%w: %w: %s", ErrAlloyDBAlreadyExists, status, msg)
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("%w: %w: %s: grant the pg_sage identity %s on project %s",
			ErrAlloyDBPermission, status, msg, alloyDBPermissions[op], a.project)
	}
	return fmt.Errorf("%w: %s", status, msg)
}
