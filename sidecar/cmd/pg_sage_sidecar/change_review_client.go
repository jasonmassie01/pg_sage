package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// The review CLI's MCP client: Streamable HTTP, one JSON-RPC message per
// POST to <url>/api/v1/mcp with the MCP token as a bearer token. It
// initializes once, then calls tools. The token is sent only in the
// Authorization header and is scrubbed from every message it prints.

const (
	reviewMCPPath     = "/api/v1/mcp"
	reviewMCPProtocol = "2025-06-18"
	maxMCPResponse    = 16 << 20
)

type mcpClient struct {
	endpoint string
	token    string
	http     *http.Client
	nextID   int
	ready    bool
}

// newMCPClient checks the base URL: https, or http to this machine only
// (a bearer token never crosses a network in the clear), and no
// credentials in the URL.
func newMCPClient(raw, token string, hc *http.Client) (*mcpClient, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return nil, usagef("--url must be an http(s) URL of pg_sage, like " +
			"https://pg-sage.example.com:8080")
	}
	if u.User != nil {
		return nil, usagef("--url must not carry credentials; the token comes from " +
			"--token-file or PG_SAGE_TOKEN_FILE")
	}
	if u.Scheme == "http" && !loopbackHost(u.Hostname()) {
		return nil, usagef("--url must be https (plain http is accepted only for " +
			"localhost)")
	}
	u.RawQuery, u.Fragment = "", ""
	u.Path = strings.TrimRight(u.Path, "/")
	if !strings.HasSuffix(u.Path, reviewMCPPath) {
		u.Path += reviewMCPPath
	}
	return &mcpClient{endpoint: u.String(), token: token, http: hc}, nil
}

func loopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// scrub removes the token from text shown to the user.
func (c *mcpClient) scrub(s string) string {
	if c.token == "" {
		return s
	}
	return strings.ReplaceAll(s, c.token, "[redacted]")
}

func (c *mcpClient) initialize(ctx context.Context) error {
	params := map[string]any{"protocolVersion": reviewMCPProtocol,
		"capabilities": map[string]any{},
		"clientInfo":   map[string]any{"name": "pg_sage-review", "version": version}}
	if _, err := c.rpc(ctx, "initialize", params, false); err != nil {
		return err
	}
	c.ready = true
	_, err := c.rpc(ctx, "notifications/initialized", map[string]any{}, true)
	return err
}

// call runs one tool and returns its structuredContent.
func (c *mcpClient) call(ctx context.Context, tool string, args any) (map[string]any,
	error) {
	raw, err := c.rpc(ctx, "tools/call", map[string]any{"name": tool, "arguments": args},
		false)
	if err != nil {
		return nil, err
	}
	var result struct {
		IsError    bool           `json:"isError"`
		Structured map[string]any `json:"structuredContent"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, transportError{msg: "pg_sage answered " + tool + " with an unreadable " +
			"result"}
	}
	if result.IsError {
		failed, _ := result.Structured["error"].(map[string]any)
		code, _ := failed["code"].(float64)
		msg, _ := failed["message"].(string)
		return nil, reviewToolError{code: int(code), msg: c.scrub(msg)}
	}
	if result.Structured == nil {
		return nil, transportError{msg: "pg_sage answered " + tool + " without a result"}
	}
	return result.Structured, nil
}

// rpc posts one JSON-RPC message; a notification expects no result.
func (c *mcpClient) rpc(ctx context.Context, method string, params any, notification bool) (
	json.RawMessage, error) {
	msg := map[string]any{"jsonrpc": "2.0", "method": method, "params": params}
	if !notification {
		c.nextID++
		msg["id"] = c.nextID
	}
	body, err := json.Marshal(msg)
	if err != nil {
		return nil, usagef("cannot encode the %s request: %v", method, err)
	}
	resp, err := c.post(ctx, body)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxMCPResponse))
	if notification && resp.StatusCode == http.StatusAccepted {
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, transportError{msg: fmt.Sprintf("pg_sage answered %s with HTTP %d%s",
			method, resp.StatusCode, httpHint(resp.StatusCode))}
	}
	var out struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, transportError{msg: "pg_sage answered " + method + " with invalid JSON"}
	}
	if out.Error != nil {
		return nil, reviewToolError{code: out.Error.Code, msg: c.scrub(out.Error.Message)}
	}
	return out.Result, nil
}

func (c *mcpClient) post(ctx context.Context, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint,
		bytes.NewReader(body))
	if err != nil {
		return nil, usagef("bad --url: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("User-Agent", "pg_sage-review/"+version)
	if c.ready {
		req.Header.Set("MCP-Protocol-Version", reviewMCPProtocol)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return nil, transportError{msg: c.scrub(fmt.Sprintf("cannot reach pg_sage at %s: %v",
			c.endpoint, err))}
	}
	return resp, nil
}

func httpHint(status int) string {
	switch status {
	case http.StatusUnauthorized:
		return " (the MCP token is missing, wrong, expired or revoked)"
	case http.StatusForbidden:
		return " (origin or permission refused)"
	case http.StatusRequestEntityTooLarge:
		return " (the request is too large)"
	case http.StatusNotFound:
		return " (is MCP enabled with mcp.transport http?)"
	}
	return ""
}
