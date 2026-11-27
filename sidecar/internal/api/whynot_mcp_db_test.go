package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/mcp"
)

// ux-08b CHECK-02 on a real server: why_not, called through the
// in-process MCP server, returns the same JSON as GET /api/v1/whynot for
// the same question, and records nothing.

// whyNotOnlyBackend is an MCP backend serving only why_not; the legacy
// Backend methods are never called by that tool.
type whyNotOnlyBackend struct {
	mcp.Backend
	mcp.WhyNotBackend
}

type whyNotDirectory []string

func (d whyNotDirectory) Databases() []mcp.DatabaseRef {
	refs := make([]mcp.DatabaseRef, 0, len(d))
	for i, name := range d {
		refs = append(refs, mcp.DatabaseRef{Name: name, ID: int64(i + 1)})
	}
	return refs
}

// pinWhyNotClock fixes the chain's clock so two reads say the same ages.
func pinWhyNotClock(t *testing.T, at time.Time) {
	t.Helper()
	previous := whyNotNow
	whyNotNow = func() time.Time { return at }
	t.Cleanup(func() { whyNotNow = previous })
}

// callWhyNotTool calls why_not as ctx's principal and returns the raw
// structuredContent; failed is the protocol error code, -1 for a tool
// error, 0 for a result.
func callWhyNotTool(t *testing.T, server *mcp.Server, ctx context.Context,
	args string) (json.RawMessage, int) {
	t.Helper()
	request := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"why_not",` +
		`"arguments":` + args + `}}`
	var response struct {
		Result struct {
			IsError           bool            `json:"isError"`
			StructuredContent json.RawMessage `json:"structuredContent"`
		} `json:"result"`
		Error *struct {
			Code int `json:"code"`
		} `json:"error"`
	}
	raw := server.Handle(ctx, json.RawMessage(request))
	if err := json.Unmarshal(raw, &response); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	if response.Error != nil {
		return nil, response.Error.Code
	}
	if response.Result.IsError {
		return response.Result.StructuredContent, -1
	}
	return response.Result.StructuredContent, 0
}

func canonicalJSON(t *testing.T, raw []byte) string {
	t.Helper()
	var value any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	out, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	return string(out)
}

func TestWhyNotMCPMatchesTheRoute(t *testing.T) {
	s := newWhyNotSetup(t, "approval", observedNow(), nil)
	pinWhyNotClock(t, time.Now().UTC().Truncate(time.Second))
	id := strconv.FormatInt(s.seedIndexFinding(t), 10)
	server := mcp.NewServer(whyNotOnlyBackend{WhyNotBackend: WhyNotMCP(s.mgr)}).
		WithDirectory(whyNotDirectory{"prod"})
	operator := mcp.WithPrincipal(context.Background(), mcp.Principal{Actor: "user:1",
		Role: "operator"})
	before := rowCount(t, s.ctx, s.pool, "sage.decision")
	for _, c := range []struct{ query, args string }{
		{"database=prod&finding_id=" + id, `{"database":"prod","finding_id":` + id + `}`},
		{"database=prod&object=public.whynot_idx", `{"object":"public.whynot_idx"}`},
		{"database=prod&category=unused_index", `{"category":"unused_index"}`},
		{"database=prod", `{}`},
	} {
		code, body, raw := getWhyNot(t, s.mux, c.query)
		if code != http.StatusOK || len(body.Links) != 10 {
			t.Fatalf("route %s: %d %s", c.query, code, raw)
		}
		content, failed := callWhyNotTool(t, server, operator, c.args)
		if failed != 0 {
			t.Fatalf("why_not %s failed (%d): %s", c.args, failed, content)
		}
		if got, want := canonicalJSON(t, content), canonicalJSON(t, []byte(raw)); got != want {
			t.Fatalf("why_not %s:\n%s\nroute %s:\n%s", c.args, got, c.query, want)
		}
	}
	if after := rowCount(t, s.ctx, s.pool, "sage.decision"); after != before {
		t.Fatalf("sage.decision %d -> %d: why_not recorded a decision", before, after)
	}
}

// Error propagation and authorization over MCP on a real database: an
// unknown finding is a tool error (not an empty chain), and a token bound
// to another database never reads this one.
func TestWhyNotMCPRefusesUnknownFindingsAndHiddenDatabases(t *testing.T) {
	s := newWhyNotSetup(t, "approval", observedNow(), nil)
	backend := WhyNotMCP(s.mgr)
	if _, err := backend.WhyNot(s.ctx, mcp.WhyNotRequest{Database: "prod",
		FindingID: 99999999}); !errors.Is(err, mcp.ErrWhyNotFindingNotFound) {
		t.Fatalf("unknown finding: %v, want ErrWhyNotFindingNotFound", err)
	}
	server := mcp.NewServer(whyNotOnlyBackend{WhyNotBackend: backend}).
		WithDirectory(whyNotDirectory{"prod", "other"})
	agent := mcp.WithPrincipal(context.Background(), mcp.Principal{Actor: "agent:a",
		Kind: mcp.KindAgent, Scopes: []mcp.Scope{mcp.ScopeRead},
		Databases: []string{"other"}})
	if content, failed := callWhyNotTool(t, server, agent, `{"database":"prod"}`); failed ==
		0 {
		t.Fatalf("an agent bound to other read prod: %s", content)
	}
	if content, failed := callWhyNotTool(t, server, agent,
		`{"database":"prod","finding_id":99999999}`); failed == 0 {
		t.Fatalf("hidden database answered: %s", content)
	}
}
