package mcp

import (
	"context"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// drill-02a: upgrade_readiness with brief: true asks for the upgrade
// brief; target_major is then optional (the default target).

func TestUpgradeReadinessToolBriefPassesThrough(t *testing.T) {
	backend := &upgradeRecordingBackend{}
	server := NewServer(backend).WithDirectory(fleetOf("orders"))
	response := invoke(t, server, context.Background(), toolCall("upgrade_readiness",
		`{"brief":true,"method":"blue_green"}`))
	require.Equal(t, 0, response.Error.Code, response.Error.Message)
	require.Equal(t, 1, backend.calls)
	require.True(t, backend.last.Brief, "brief requested")
	require.Equal(t, 0, backend.last.TargetMajor)
	require.Equal(t, "blue_green", backend.last.Method)
	require.Equal(t, "orders", backend.database)
	response = invoke(t, server, context.Background(), toolCall("upgrade_readiness",
		`{"brief":true,"target_major":17}`))
	require.Equal(t, 0, response.Error.Code, response.Error.Message)
	require.Equal(t, 17, backend.last.TargetMajor)
}

func TestUpgradeReadinessToolBriefRejectsBadArguments(t *testing.T) {
	backend := &upgradeRecordingBackend{}
	for _, args := range []string{`{"brief":"yes"}`, `{"brief":true,"target_major":19}`,
		`{"brief":true,"method":"dump"}`, `{"brief":true,"target_major":-1}`,
		`{"brief":false}`} {
		response := invoke(t, NewServer(backend), context.Background(),
			toolCall("upgrade_readiness", args))
		require.Equal(t, codeInvalidParams, response.Error.Code, args)
	}
	require.Equal(t, 0, backend.calls)
}

func TestUpgradeReadinessToolSchemaOffersBrief(t *testing.T) {
	for _, tool := range NewServer(&upgradeRecordingBackend{}).Tools() {
		if tool.Name == "upgrade_readiness" {
			require.True(t, strings.Contains(string(tool.InputSchema), `"brief"`),
				"schema has brief")
			require.True(t, strings.Contains(tool.Description, "brief"), "described")
			return
		}
	}
	t.Fatal("upgrade_readiness is not listed")
}
