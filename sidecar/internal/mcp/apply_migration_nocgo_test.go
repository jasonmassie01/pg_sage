//go:build !cgo

package mcp

import (
	"context"
	"testing"

	"github.com/pg-sage/sidecar/internal/migration/plan"
	"github.com/pg-sage/sidecar/internal/sqlast"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// migration-02b: a build without the parse tree refuses online-migration
// planning. apply_migration reports that as a refusal naming the build it
// needs (-32602), not as an internal error, and records nothing.
func TestApplyMigrationWithoutTheParseTreeIsARefusal(t *testing.T) {
	store := &recordingIntentStore{}
	response := invoke(t, gatedMigrationServer(t, store), adminContext(),
		toolCall("apply_migration", `{"table":"public.users","cycle":1,"sql":`+
			`"ALTER TABLE public.users ADD CONSTRAINT users_email_key UNIQUE (email)"}`))
	require.Equal(t, codeInvalidParams, response.Error.Code, response.Error.Message)
	require.Contains(t, response.Error.Message,
		"online migration planning needs the parse-tree build")
	require.Equal(t, MigrationRecord{}, store.migration)
	require.Zero(t, store.ddlExecutions)

	executor := NewProductionIntentExecutor(&recordingIntentStore{}, plan.NewPlanner(),
		executingGate())
	_, err := executor.ExecuteConcrete(context.Background(), migrationRequest())
	require.ErrorIs(t, err, sqlast.ErrUnavailable)
	require.ErrorIs(t, err, ErrIntentInvalid)
}
