package api

import (
	"fmt"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/decommission"
)

// registerDecommissionRoutes mounts the removed provisioner's inventory, its
// acknowledgement and the legacy table drop, admin-only, on the control pool
// (agent governance spec §8.3, §12). No route deletes anything live; the drop
// removes only pg_sage's own legacy tables, once everything is acknowledged.
func registerDecommissionRoutes(mux *http.ServeMux, pool *pgxpool.Pool) {
	h := decommission.NewHandlers(pool, authenticatedActor, func(format string, args ...any) {
		slog.Error("api decommission", "err", fmt.Sprintf(format, args...))
	})
	route(mux, "GET "+decommission.InventoryPath, gateAdmin, http.HandlerFunc(h.Inventory))
	route(mux, "POST "+decommission.AckPath, gateAdmin, http.HandlerFunc(h.Ack))
	route(mux, "POST "+decommission.DropPath, gateAdmin, http.HandlerFunc(h.Drop))
}
