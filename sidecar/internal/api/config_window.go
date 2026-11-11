package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/policy"
)

// maxWindowExpressionLen bounds a previewed trust.maintenance_window value;
// real windows are a few dozen characters.
const maxWindowExpressionLen = 256

// maxWindowRequestBytes bounds the request body of a window preview.
const maxWindowRequestBytes = 4 << 10

type windowPreviewRequest struct {
	Expression string `json:"expression"`
}

// validateWindowHandler serves POST /api/v1/config/validate-window
// (config-09): it checks a trust.maintenance_window value exactly as
// config validation does and previews when the window next opens. It reads
// and writes nothing; now is the clock, injectable for tests.
func validateWindowHandler(now func() time.Time) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		expression, err := decodeWindowRequest(w, r)
		if err != nil {
			jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
		if strings.TrimSpace(expression) == "" || config.MaintenanceWindowDisabled(expression) {
			jsonResponse(w, map[string]any{
				"valid": true, "disabled": true, "open_now": false,
				"detail": "the maintenance window is closed: MODERATE actions never " +
					"run unattended",
			})
			return
		}
		if err := config.ValidateMaintenanceWindow(expression); err != nil {
			jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
		window, err := policy.ParseWindow(expression)
		if err != nil {
			jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
		jsonResponse(w, windowPreviewResponse(window.Preview(now(), policy.PreviewHorizon)))
	}
}

func decodeWindowRequest(w http.ResponseWriter, r *http.Request) (string, error) {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxWindowRequestBytes))
	var request windowPreviewRequest
	if err := decoder.Decode(&request); err != nil {
		return "", fmt.Errorf(
			"request body must be JSON {\"expression\": \"<window>\"}: %w", err)
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return "", errors.New("request body must hold one JSON object")
	}
	if len(request.Expression) > maxWindowExpressionLen {
		return "", fmt.Errorf("expression is %d characters; a window is at most %d",
			len(request.Expression), maxWindowExpressionLen)
	}
	return request.Expression, nil
}

func windowPreviewResponse(preview policy.WindowPreview) map[string]any {
	return map[string]any{
		"valid":         true,
		"disabled":      false,
		"open_now":      preview.OpenNow,
		"closes_at":     instantOrNil(preview.ClosesAt),
		"next_open":     instantOrNil(preview.NextOpen),
		"next_close":    instantOrNil(preview.NextClose),
		"horizon_hours": int(policy.PreviewHorizon / time.Hour),
	}
}

func instantOrNil(at time.Time) any {
	if at.IsZero() {
		return nil
	}
	return at.Format(time.RFC3339)
}
