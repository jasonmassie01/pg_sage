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

// azureServer is what the provider reads of a flexible server. The
// network, encryption and identity are kept verbatim, so a restore lands
// in the source's subnet under its key.
type azureServer struct {
	ID       string            `json:"id"`
	Name     string            `json:"name"`
	Location string            `json:"location"`
	Tags     map[string]string `json:"tags"`
	Identity json.RawMessage   `json:"identity"`
	Props    struct {
		State           string          `json:"state"`
		FQDN            string          `json:"fullyQualifiedDomainName"`
		ReplicationRole string          `json:"replicationRole"`
		Network         json.RawMessage `json:"network"`
		DataEncryption  json.RawMessage `json:"dataEncryption"`
		Backup          struct {
			EarliestRestoreDate time.Time `json:"earliestRestoreDate"`
		} `json:"backup"`
	} `json:"properties"`
}

// azureOperation is an Azure-AsyncOperation status.
type azureOperation struct {
	Status string `json:"status"`
	Error  *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// azureHTTP is ARM's flexibleServers of one resource group over plain
// net/http. The token is sent only in the Authorization header, only to
// the ARM endpoint, and never appears in an error.
type azureHTTP struct {
	base, sub, group string
	token            func(context.Context) (string, error)
	client           *http.Client
	poll             time.Duration
}

// AzureFlexibleServerAPIVersion is the Microsoft.DBforPostgreSQL
// api-version of every call (the one cloud telemetry reads).
const AzureFlexibleServerAPIVersion = "2022-12-01"

const azureProviderPath = "/providers/Microsoft.DBforPostgreSQL/"

// The RBAC action each call needs, named when ARM refuses it.
var azurePermissions = map[string]string{
	"servers.get":    "Microsoft.DBforPostgreSQL/flexibleServers/read",
	"servers.put":    "Microsoft.DBforPostgreSQL/flexibleServers/write",
	"servers.delete": "Microsoft.DBforPostgreSQL/flexibleServers/delete",
	"operations.get": "Microsoft.DBforPostgreSQL/locations/azureAsyncOperation/read " +
		"(and .../locations/operationResults/read)",
}

var azureOpPath = regexp.MustCompile(`^/subscriptions/([0-9a-fA-F-]{36})` +
	`/providers/Microsoft\.DBforPostgreSQL/locations/[A-Za-z0-9]+` +
	`/(azureAsyncOperation|operationResults)/[A-Za-z0-9._-]+$`)

func (a *azureHTTP) serverPath(name string) string {
	return "/subscriptions/" + a.sub + "/resourceGroups/" + url.PathEscape(a.group) +
		azureProviderPath + "flexibleServers/" + url.PathEscape(name)
}

// serverID is the resource ID a server of this group is named by.
func (a *azureHTTP) serverID(name string) string {
	return "/subscriptions/" + a.sub + "/resourceGroups/" + a.group + azureProviderPath +
		"flexibleServers/" + name
}

func (a *azureHTTP) GetServer(ctx context.Context, name string) (azureServer, error) {
	var out azureServer
	_, err := a.do(ctx, "servers.get", http.MethodGet, a.serverURL(name), nil, nil, &out)
	if err == nil && (!strings.EqualFold(out.ID, a.serverID(name)) || out.Name != name) {
		err = fmt.Errorf("azure servers.get %s: the reply names server %q", name, out.ID)
	}
	return out, err
}

// PutServer creates server name from body, only if none exists
// (If-None-Match: *; ARM's PUT would otherwise update one), and returns
// the URL that reports its progress ("" when ARM finished synchronously).
func (a *azureHTTP) PutServer(ctx context.Context, name string, body any) (string, error) {
	reply, err := a.do(ctx, "servers.put", http.MethodPut, a.serverURL(name),
		map[string]string{"If-None-Match": "*"}, body, nil)
	if err != nil {
		return "", err
	}
	return a.progressURL(reply.header)
}

// DeleteServer asks ARM to delete server name; a 404 is ErrAzureNotFound.
func (a *azureHTTP) DeleteServer(ctx context.Context, name string) error {
	_, err := a.do(ctx, "servers.delete", http.MethodDelete, a.serverURL(name), nil, nil, nil)
	return err
}

func (a *azureHTTP) serverURL(name string) string {
	return a.base + a.serverPath(name) + "?api-version=" + AzureFlexibleServerAPIVersion
}

// progressURL is the Azure-AsyncOperation (or Location) URL of a reply,
// refused unless it is an operation of this subscription on the ARM
// endpoint: the poll carries the bearer token.
func (a *azureHTTP) progressURL(h http.Header) (string, error) {
	raw := h.Get("Azure-AsyncOperation")
	if raw == "" {
		raw = h.Get("Location")
	}
	if raw == "" {
		return "", nil
	}
	u, err := url.Parse(raw)
	base, berr := url.Parse(a.base)
	if err != nil || berr != nil || u.Scheme != base.Scheme || u.Host != base.Host ||
		u.User != nil || strings.Contains(u.Path, "..") {
		return "", fmt.Errorf("azure servers.put: the operation URL is not on the ARM "+
			"endpoint %s; not polled", a.base)
	}
	m := azureOpPath.FindStringSubmatch(u.Path)
	if m == nil || !strings.EqualFold(m[1], a.sub) {
		return "", fmt.Errorf("azure servers.put: the operation URL path %q is not an "+
			"operation of subscription %s; not polled", u.Path, a.sub)
	}
	return u.String(), nil
}

// Operation reads one progress URL: done reports a finished operation,
// and err its failure.
func (a *azureHTTP) Operation(ctx context.Context, rawURL string) (bool, error) {
	var op azureOperation
	reply, err := a.do(ctx, "operations.get", http.MethodGet, rawURL, nil, nil, &op)
	switch {
	case err != nil:
		return false, err
	case reply.status == http.StatusAccepted:
		return false, nil // a Location URL still running
	case op.Status == "" || strings.EqualFold(op.Status, "Succeeded"):
		return true, nil // a Location URL answers 200 or 204 when done
	case strings.EqualFold(op.Status, "Failed") || strings.EqualFold(op.Status, "Canceled"):
		msg := "no error detail"
		if op.Error != nil {
			msg = sanitizeARM(op.Error.Code) + ": " + sanitizeARM(op.Error.Message)
		}
		return true, fmt.Errorf("%w: %s: %s", errAzureOpFailed, op.Status, msg)
	}
	return false, nil
}

// azureReply is a 2xx reply's status and headers.
type azureReply struct {
	status int
	header http.Header
}

// do sends one request and decodes a 2xx reply with a body into out.
func (a *azureHTTP) do(ctx context.Context, op, method, target string,
	headers map[string]string, body, out any) (azureReply, error) {
	req, err := a.request(ctx, method, target, headers, body)
	if err != nil {
		return azureReply{}, fmt.Errorf("azure %s: %w", op, err)
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return azureReply{}, fmt.Errorf("azure %s: %w", op, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, cloudSQLMaxBody))
	if err != nil {
		return azureReply{}, fmt.Errorf("azure %s: read the reply: %w", op, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return azureReply{}, a.statusError(op, resp.StatusCode, data)
	}
	reply := azureReply{status: resp.StatusCode, header: resp.Header}
	if out == nil || len(bytes.TrimSpace(data)) == 0 {
		return reply, nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return reply, fmt.Errorf("azure %s: decode the reply: %w", op, err)
	}
	return reply, nil
}

func (a *azureHTTP) request(ctx context.Context, method, target string,
	headers map[string]string, body any) (*http.Request, error) {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("encode the request: %w", err)
		}
		reader = bytes.NewReader(data)
	}
	tok, err := a.token(ctx)
	if err == nil && blank(tok) {
		err = errors.New("the credential chain returned an empty token")
	}
	if err != nil {
		return nil, fmt.Errorf("%w: get the Azure Resource Manager token: %s",
			ErrCloneUnavailable, sanitizeARM(err.Error()))
	}
	req, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return nil, errors.New("build the request: invalid ARM endpoint or path")
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return req, nil
}

// statusError is a non-2xx reply: ErrAzureNotFound for 404,
// ErrAzureAlreadyExists for a PUT's 409 (a name in use; another call's
// 409 is an operation in progress) or 412 (If-None-Match: * met a server
// of the name), ErrAzurePermission naming the RBAC
// action for 401 and 403, each also a ProviderStatusError (429 and 5xx
// are throttled, lab-05a) with ARM's code and message.
func (a *azureHTTP) statusError(op string, code int, data []byte) error {
	var reply struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	msg := strings.TrimSpace(string(data))
	if json.Unmarshal(data, &reply) == nil && reply.Error.Message != "" {
		msg = reply.Error.Code + ": " + reply.Error.Message
	}
	msg = sanitizeARM(msg)
	status := &ProviderStatusError{Op: "azure " + op, StatusCode: code}
	if code == http.StatusPreconditionFailed && op == "servers.put" {
		return fmt.Errorf("%w: %w: %s (a server of the name exists)", ErrAzureAlreadyExists,
			status, msg)
	}
	switch code {
	case http.StatusNotFound:
		return fmt.Errorf("%w: %w: %s", ErrAzureNotFound, status, msg)
	case http.StatusConflict:
		if op != "servers.put" {
			return fmt.Errorf("%w: %s (another operation on the server is running)",
				status, msg)
		}
		return fmt.Errorf("%w: %w: %s", ErrAzureAlreadyExists, status, msg)
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("%w: %w: %s: grant the pg_sage identity %s on resource group %s "+
			"of subscription %s", ErrAzurePermission, status, msg, azurePermissions[op],
			a.group, a.sub)
	}
	return fmt.Errorf("%w: %s", status, msg)
}

// sanitizeARM bounds a message from ARM or the credential chain and keeps
// it on one line.
func sanitizeARM(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 300 {
		s = s[:300] + "..."
	}
	return s
}
