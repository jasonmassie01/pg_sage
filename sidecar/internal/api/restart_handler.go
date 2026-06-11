package api

import (
	"net/http"
	"time"
)

// restartFunc, when set by main via SetRestartFunc, triggers a graceful
// process restart (the process exits with the restart code so a supervisor
// — the launcher loop or an orchestrator restart policy — relaunches it).
// nil means restart is not supported in this deployment.
var restartFunc func()

// SetRestartFunc wires the process-restart hook. Called once at startup
// before the HTTP server begins serving.
func SetRestartFunc(fn func()) { restartFunc = fn }

// restartHandler restarts the sidecar so that startup-only settings take
// effect. Admin-only (gated by the router).
func restartHandler(w http.ResponseWriter, _ *http.Request) {
	if restartFunc == nil {
		jsonError(w, "restart not supported: no supervisor configured",
			http.StatusNotImplemented)
		return
	}
	jsonResponse(w, map[string]string{
		"status": "restarting",
		"detail": "the sidecar is restarting; reconnect in a few seconds",
	})
	// Respond first, then restart out-of-band so the client sees 200.
	go func() {
		time.Sleep(500 * time.Millisecond)
		restartFunc()
	}()
}
