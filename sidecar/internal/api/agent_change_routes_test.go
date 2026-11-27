package api

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/pg-sage/sidecar/internal/agentguard"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// No concurrent access tests: changeError is a stateless error mapping.

func TestChangeError_AgentChangedIs409(t *testing.T) {
	rec := httptest.NewRecorder()
	err := fmt.Errorf("%w: principal agp_x", agentguard.ErrChangeWidens)
	require.True(t, changeError(rec, err, "the agent changed"))
	require.Equal(t, http.StatusConflict, rec.Code)
	require.Contains(t, rec.Body.String(), `"code":"agent_changed"`)
	require.Contains(t, rec.Body.String(), "the agent changed")
}

func TestChangeError_OtherErrorsAreNotChangeErrors(t *testing.T) {
	for _, err := range []error{errors.New("boom"), agentguard.ErrNotFound,
		agentguard.ErrInvalid} {
		rec := httptest.NewRecorder()
		require.False(t, changeError(rec, err, "x"), "%v", err)
		require.Equal(t, 0, rec.Body.Len(), "nothing written for %v", err)
	}
}
