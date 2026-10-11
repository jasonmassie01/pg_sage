package api

import (
	"fmt"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/decommission"
)

// registerDecommissionRoutes mounts the removed provisioner's inventory and
// its acknowledgement, admin-only, on the control pool (Agent Guard spec §8.3,
// §12). Neither route deletes anything live.
func registerDecommissionRoutes(mux *http.ServeMux, pool *pgxpool.Pool) {
	h := decommission.NewHandlers(pool, authenticatedActor, func(format string, args ...any) {
		slog.Error("api decommission", "err", fmt.Sprintf(format, args...))
	})
	adminOnly := RequireRole("admin")
	mux.Handle("GET "+decommission.InventoryPath, adminOnly(http.HandlerFunc(h.Inventory)))
	mux.Handle("POST "+decommission.AckPath, adminOnly(http.HandlerFunc(h.Ack)))
}
