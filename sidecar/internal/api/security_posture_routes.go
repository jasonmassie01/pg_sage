package api

import (
	"context"
	"log/slog"
	"net/http"
)

// SecretsPosture is how stored secrets are protected (security-08):
// secrets_at_rest is sealed, plaintext or key_missing; key_source is
// configured, generated or none; plaintext_rows counts unsealed secrets.
type SecretsPosture struct {
	SecretsAtRest string `json:"secrets_at_rest"`
	KeySource     string `json:"key_source"`
	PlaintextRows int    `json:"plaintext_rows"`
}

// registerSecurityPostureRoutes serves GET /api/v1/security/posture to
// admins. A nil posture source registers nothing.
func registerSecurityPostureRoutes(
	mux *http.ServeMux, posture func(context.Context) (SecretsPosture, error),
) {
	if posture == nil {
		return
	}
	route(mux, "GET /api/v1/security/posture", gateAdmin, http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			got, err := posture(r.Context())
			if err != nil {
				slog.Error("security posture unavailable", "error", err)
				jsonError(w, "security posture is unavailable; see the sidecar log",
					http.StatusInternalServerError)
				return
			}
			jsonResponse(w, got)
		}))
}
