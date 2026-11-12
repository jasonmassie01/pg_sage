package cases

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
)

// JiraConfig is a Jira Cloud ticket integration (market-14): the site
// URL, the account email and API token (basic auth), the project key and
// the issue type (default Task).
type JiraConfig struct {
	BaseURL, Email, APIToken, ProjectKey, IssueType string
}

// JiraClient talks to the Jira Cloud REST API v3. The caller supplies the
// http.Client, which is the notification target policy's client.
type JiraClient struct {
	cfg  JiraConfig
	http *http.Client
}

var jiraIssueKey = regexp.MustCompile(`^[A-Z][A-Z0-9_]*-[0-9]+$`)

// NewJiraClient checks cfg and returns a client over client.
func NewJiraClient(cfg JiraConfig, client *http.Client) (*JiraClient, error) {
	cfg.BaseURL = strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if cfg.BaseURL == "" || cfg.Email == "" || cfg.APIToken == "" || cfg.ProjectKey == "" ||
		client == nil {
		return nil, fmt.Errorf("%w: jira needs base_url, email, api_token and project_key",
			ErrTicketInvalid)
	}
	if cfg.IssueType == "" {
		cfg.IssueType = "Task"
	}
	return &JiraClient{cfg: cfg, http: client}, nil
}

// adfDoc is an Atlassian Document Format document of paragraphs.
type adfDoc struct {
	Type    string    `json:"type"`
	Version int       `json:"version"`
	Content []adfNode `json:"content"`
}

type adfNode struct {
	Type    string    `json:"type"`
	Text    string    `json:"text,omitempty"`
	Content []adfNode `json:"content,omitempty"`
	Marks   []adfMark `json:"marks,omitempty"`
}

type adfMark struct {
	Type  string            `json:"type"`
	Attrs map[string]string `json:"attrs"`
}

func adfText(text string) adfNode {
	return adfNode{Type: "paragraph", Content: []adfNode{{Type: "text", Text: text}}}
}

// CreateIssue creates the issue and returns its key and browse URL.
func (c *JiraClient) CreateIssue(ctx context.Context, req IssueRequest) (IssueRef, error) {
	doc := adfDoc{Type: "doc", Version: 1, Content: []adfNode{adfText(req.Description)}}
	if req.CaseURL != "" {
		doc.Content = append(doc.Content, adfNode{Type: "paragraph", Content: []adfNode{{
			Type: "text", Text: "Open the case in pg_sage",
			Marks: []adfMark{{Type: "link", Attrs: map[string]string{"href": req.CaseURL}}},
		}}})
	}
	body := map[string]any{"fields": map[string]any{
		"project":     map[string]string{"key": c.cfg.ProjectKey},
		"issuetype":   map[string]string{"name": c.cfg.IssueType},
		"summary":     req.Summary,
		"labels":      []string{"pg_sage"},
		"description": doc,
	}}
	var out struct {
		Key string `json:"key"`
	}
	if err := c.do(ctx, http.MethodPost, "/rest/api/3/issue", body, &out); err != nil {
		return IssueRef{}, err
	}
	if !jiraIssueKey.MatchString(out.Key) {
		return IssueRef{}, fmt.Errorf("jira answered with issue key %q", out.Key)
	}
	return IssueRef{Key: out.Key, URL: c.cfg.BaseURL + "/browse/" + out.Key}, nil
}

// IssueStatus reads the issue's status and status category.
func (c *JiraClient) IssueStatus(ctx context.Context, key string) (IssueStatus, error) {
	if !jiraIssueKey.MatchString(key) {
		return IssueStatus{}, fmt.Errorf("%w: issue key %q", ErrTicketInvalid, key)
	}
	var out struct {
		Fields struct {
			Status struct {
				Name     string `json:"name"`
				Category struct {
					Key string `json:"key"`
				} `json:"statusCategory"`
			} `json:"status"`
		} `json:"fields"`
	}
	path := "/rest/api/3/issue/" + key + "?fields=status"
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return IssueStatus{}, err
	}
	return IssueStatus{Name: out.Fields.Status.Name, Category: out.Fields.Status.Category.Key}, nil
}

// AddComment adds a plain-text comment to the issue.
func (c *JiraClient) AddComment(ctx context.Context, key, text string) error {
	if !jiraIssueKey.MatchString(key) {
		return fmt.Errorf("%w: issue key %q", ErrTicketInvalid, key)
	}
	body := map[string]any{"body": adfDoc{Type: "doc", Version: 1,
		Content: []adfNode{adfText(text)}}}
	return c.do(ctx, http.MethodPost, "/rest/api/3/issue/"+key+"/comment", body, nil)
}

// do sends one request. 401 and 403 are ErrTicketUnauthorized, 404 is
// ErrTicketNotFound; the answer body is never echoed, so no token or
// issue text reaches a log.
func (c *JiraClient) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("jira %s: marshal: %w", path, err)
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.cfg.BaseURL+path, body)
	if err != nil {
		return fmt.Errorf("jira request: %w", err)
	}
	req.SetBasicAuth(c.cfg.Email, c.cfg.APIToken)
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("jira %s %s: %w", method, req.URL.Path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	return jiraAnswer(resp, out)
}

func jiraAnswer(resp *http.Response, out any) error {
	limited := io.LimitReader(resp.Body, 1<<20)
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return fmt.Errorf("jira returned %d: %w", resp.StatusCode, ErrTicketUnauthorized)
	case resp.StatusCode == http.StatusNotFound:
		return fmt.Errorf("jira returned 404: %w", ErrTicketNotFound)
	case resp.StatusCode < 200 || resp.StatusCode >= 300:
		return fmt.Errorf("jira returned status %d", resp.StatusCode)
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, limited)
		return nil
	}
	if err := json.NewDecoder(limited).Decode(out); err != nil {
		return fmt.Errorf("jira answer is not JSON: %w", err)
	}
	return nil
}
