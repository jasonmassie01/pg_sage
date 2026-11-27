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
	"time"
)

// cloudSQLInstance is what the provider reads of a Cloud SQL instance.
type cloudSQLInstance struct {
	Name            string `json:"name"`
	State           string `json:"state"`
	DatabaseVersion string `json:"databaseVersion"`
	InstanceType    string `json:"instanceType"`
	IPAddresses     []struct {
		Type      string `json:"type"`
		IPAddress string `json:"ipAddress"`
	} `json:"ipAddresses"`
	Settings struct {
		Tier                      string            `json:"tier"`
		UserLabels                map[string]string `json:"userLabels"`
		DeletionProtectionEnabled bool              `json:"deletionProtectionEnabled"`
	} `json:"settings"`
}

// ipOfType is the instance's address of type (PRIMARY, PRIVATE, ...).
func (in cloudSQLInstance) ipOfType(kind string) string {
	for _, ip := range in.IPAddresses {
		if ip.Type == kind {
			return ip.IPAddress
		}
	}
	return ""
}

// typeOfIP is the type of the instance's address host ("": not its).
func (in cloudSQLInstance) typeOfIP(host string) string {
	for _, ip := range in.IPAddresses {
		if sameHost(ip.IPAddress, host) {
			return ip.Type
		}
	}
	return ""
}

// cloudSQLOperation is a long-running Admin API operation.
type cloudSQLOperation struct {
	Name     string `json:"name"`
	Status   string `json:"status"`
	TargetID string `json:"targetId"`
	Error    *struct {
		Errors []struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"errors"`
	} `json:"error"`
}

// failure is the operation's error ("" when it has none).
func (op cloudSQLOperation) failure() string {
	if op.Error == nil {
		return ""
	}
	var parts []string
	for _, e := range op.Error.Errors {
		parts = append(parts, strings.TrimSpace(e.Code+": "+e.Message))
	}
	if len(parts) == 0 {
		return "the operation failed without a reason"
	}
	return strings.Join(parts, "; ")
}

// cloudSQLPatch is a partial settings update.
type cloudSQLPatch struct {
	Settings cloudSQLPatchSettings `json:"settings"`
}

type cloudSQLPatchSettings struct {
	UserLabels         map[string]string `json:"userLabels,omitempty"`
	DeletionProtection *bool             `json:"deletionProtectionEnabled,omitempty"`
	AvailabilityType   string            `json:"availabilityType,omitempty"`
}

// cloudSQLHTTP is the Admin API v1 of one project over plain net/http.
// The token is sent only in the Authorization header and never appears in
// an error.
type cloudSQLHTTP struct {
	base, project string
	token         CloudSQLTokenSource
	client        *http.Client
	poll          time.Duration
}

// cloudSQLMaxBody bounds one response read.
const cloudSQLMaxBody = 1 << 20

// The IAM permission each call needs, named when Google refuses it.
var cloudSQLPermissions = map[string]string{
	"instances.get": "cloudsql.instances.get", "instances.clone": "cloudsql.instances.clone",
	"instances.patch":  "cloudsql.instances.update",
	"instances.delete": "cloudsql.instances.delete",
	"operations.get":   "cloudsql.instances.get",
}

func (a *cloudSQLHTTP) GetInstance(ctx context.Context, name string) (cloudSQLInstance,
	error) {
	var out cloudSQLInstance
	err := a.do(ctx, "instances.get", http.MethodGet, "instances/"+url.PathEscape(name), nil,
		&out)
	if err == nil && out.Name != name {
		err = fmt.Errorf("cloudsql instances.get %s: the reply names instance %q", name,
			out.Name)
	}
	return out, err
}

func (a *cloudSQLHTTP) Clone(ctx context.Context, source, dest string) (cloudSQLOperation,
	error) {
	body := map[string]any{"cloneContext": map[string]string{"kind": "sql#cloneContext",
		"destinationInstanceName": dest}}
	var out cloudSQLOperation
	err := a.do(ctx, "instances.clone", http.MethodPost,
		"instances/"+url.PathEscape(source)+"/clone", body, &out)
	return out, err
}

func (a *cloudSQLHTTP) Patch(ctx context.Context, name string,
	p cloudSQLPatch) (cloudSQLOperation, error) {
	var out cloudSQLOperation
	err := a.do(ctx, "instances.patch", http.MethodPatch, "instances/"+url.PathEscape(name),
		p, &out)
	return out, err
}

func (a *cloudSQLHTTP) Delete(ctx context.Context, name string) error {
	return a.do(ctx, "instances.delete", http.MethodDelete,
		"instances/"+url.PathEscape(name), nil, nil)
}

func (a *cloudSQLHTTP) Operation(ctx context.Context, name string) (cloudSQLOperation,
	error) {
	var out cloudSQLOperation
	err := a.do(ctx, "operations.get", http.MethodGet, "operations/"+url.PathEscape(name),
		nil, &out)
	return out, err
}

// do sends one request to /v1/projects/<project>/<path> and decodes a 2xx
// reply into out.
func (a *cloudSQLHTTP) do(ctx context.Context, op, method, path string, body,
	out any) error {
	req, err := a.request(ctx, method, path, body)
	if err != nil {
		return fmt.Errorf("cloudsql %s: %w", op, err)
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("cloudsql %s: %w", op, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, cloudSQLMaxBody))
	if err != nil {
		return fmt.Errorf("cloudsql %s: read the reply: %w", op, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return a.statusError(op, resp.StatusCode, data)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("cloudsql %s: decode the reply: %w", op, err)
	}
	return nil
}

func (a *cloudSQLHTTP) request(ctx context.Context, method, path string,
	body any) (*http.Request, error) {
	target := a.base + "/v1/projects/" + url.PathEscape(a.project) + "/" + path
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

// statusError is a non-2xx reply: ErrCloudSQLNotFound for 404,
// ErrCloudSQLAlreadyExists for a clone's 409 (another op's 409 is an
// operation in progress), ErrCloudSQLPermission naming the IAM
// permission for 401 and 403, each also a ProviderStatusError (429 and 5xx
// are throttled, lab-05a) with Google's message.
func (a *cloudSQLHTTP) statusError(op string, code int, data []byte) error {
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
	status := &ProviderStatusError{Op: "cloudsql " + op, StatusCode: code}
	switch code {
	case http.StatusNotFound:
		return fmt.Errorf("%w: %w: %s", ErrCloudSQLNotFound, status, msg)
	case http.StatusConflict:
		if op != "instances.clone" {
			return fmt.Errorf("%w: %s (another operation on the instance is running)",
				status, msg)
		}
		return fmt.Errorf("%w: %w: %s", ErrCloudSQLAlreadyExists, status, msg)
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("%w: %w: %s: grant the pg_sage identity %s on project %s",
			ErrCloudSQLPermission, status, msg, cloudSQLPermissions[op], a.project)
	}
	return fmt.Errorf("%w: %s", status, msg)
}
