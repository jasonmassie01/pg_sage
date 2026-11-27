package api

import (
	"log/slog"
	"net/http"

	"github.com/pg-sage/sidecar/internal/config"
)

// newCrossOriginProtection builds the CSRF defence every API route gets
// (apiauth-10). Browsers send Sec-Fetch-Site, and only same-origin (or
// user-initiated "none") writes pass; a browser without it is checked by
// Origin against Host. Requests with neither header (curl, MCP clients,
// SDKs) are not browser requests and pass. GET, HEAD and OPTIONS always
// pass. api.trusted_origins is honoured outside Databricks App mode, where
// the platform ingress is the only trusted origin.
func newCrossOriginProtection(cfg *config.Config, appMode bool) *http.CrossOriginProtection {
	protection := http.NewCrossOriginProtection()
	if appMode {
		return protection
	}
	protection.SetDenyHandler(http.HandlerFunc(refuseCrossOrigin))
	if cfg == nil {
		return protection
	}
	for _, origin := range cfg.API.TrustedOrigins {
		// Config validation already refuses malformed entries; one that
		// slips through is left untrusted, which refuses its writes.
		if err := protection.AddTrustedOrigin(origin); err != nil {
			slog.Error("api.trusted_origins: entry not trusted",
				"origin", origin, "error", err)
		}
	}
	return protection
}

func refuseCrossOrigin(w http.ResponseWriter, _ *http.Request) {
	jsonError(w, "cross-origin request refused: send it from the dashboard's own "+
		"origin, or list that origin in api.trusted_origins", http.StatusForbidden)
}
