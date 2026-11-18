package mcp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// Integration (mcp-01a): a recorded agent session replays against the
// in-process server. Each bad call gets an error whose next the agent
// follows, and the followed call succeeds.

// No concurrent access tests: the session is one agent's ordered calls.

type sessionStep struct {
	Call   *sessionCall   `json:"call"`
	Follow string         `json:"follow"`
	With   map[string]any `json:"with"`
	Expect struct {
		Reason    string `json:"reason"`
		NextTool  string `json:"next_tool"`
		Decision  string `json:"decision"`
		Held      bool   `json:"held"`
		Tool      string `json:"tool"`
		Databases int    `json:"databases"`
	} `json:"expect"`
}

type sessionCall struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
}

type recordedSession struct {
	Databases []string      `json:"databases"`
	Steps     []sessionStep `json:"steps"`
}

// replay is the agent: it remembers the last error's next and the last
// held result's status, and follows them when a step says so.
type replay struct {
	t      *testing.T
	server *Server
	next   map[string]any
	status map[string]any
}

func (r *replay) callFor(step sessionStep) sessionCall {
	switch step.Follow {
	case "":
		return *step.Call
	case "example":
		args := objectMap(r.t, r.next["example"])
		for key, value := range step.With {
			args[key] = value
		}
		return sessionCall{Name: r.next["tool"].(string), Arguments: args}
	case "next":
		return sessionCall{Name: r.next["tool"].(string),
			Arguments: objectMap(r.t, r.next["arguments"])}
	case "status":
		return sessionCall{Name: r.status["tool"].(string),
			Arguments: objectMap(r.t, r.status["arguments"])}
	}
	r.t.Fatalf("unknown follow %q", step.Follow)
	return sessionCall{}
}

func (r *replay) run(i int, step sessionStep) {
	call := r.callFor(step)
	response := invoke(r.t, r.server, adminContext(),
		toolCall(call.Name, mustJSON(r.t, call.Arguments)))
	content := structuredContent(r.t, response)
	if step.Expect.Reason != "" {
		detail := objectMap(r.t, content["error"])
		require.Equal(r.t, step.Expect.Reason, detail["reason"], "step %d", i)
		r.next = objectMap(r.t, detail["next"])
		require.Equal(r.t, step.Expect.NextTool, r.next["tool"], "step %d", i)
		return
	}
	require.Zero(r.t, response.Error.Code, "step %d: %s", i, response.Error.Message)
	if step.Expect.Decision != "" {
		require.Equal(r.t, step.Expect.Decision, content["decision"], "step %d", i)
	}
	if step.Expect.Held {
		require.Contains(r.t, content, "approval_url", "step %d", i)
		r.status = objectMap(r.t, content["status"])
	}
	if step.Expect.Tool != "" {
		require.Equal(r.t, step.Expect.Tool, call.Name, "step %d", i)
	}
	if step.Expect.Databases > 0 {
		require.Len(r.t, content["databases"], step.Expect.Databases, "step %d", i)
	}
}

func TestRecordedAgentSessionFollowsNextToACorrectCall(t *testing.T) {
	raw, err := os.ReadFile(filepath.Clean("testdata/agent_session.json"))
	require.NoError(t, err)
	var session recordedSession
	require.NoError(t, json.Unmarshal(raw, &session))
	require.GreaterOrEqual(t, len(session.Steps), 6)
	backend := &decidingInbox{}
	backend.changeResult = ChangeResult{Decision: "queued", EvidenceID: "ev-77"}
	r := &replay{t: t, server: NewServer(backend).WithDirectory(
		fleetOf(session.Databases...))}
	errors, followed := 0, 0
	for i, step := range session.Steps {
		r.run(i, step)
		if step.Expect.Reason != "" {
			errors++
		}
		if step.Follow != "" {
			followed++
		}
	}
	require.Equal(t, 3, errors, "the session has three bad calls")
	require.Equal(t, 4, followed, "every error was followed, and one status")
	require.Equal(t, 2, backend.changeCalls, "only the corrected calls reached the backend")
}
