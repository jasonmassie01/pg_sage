package api

import (
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/store"
)

func registerAuthRoutes(
	mux *http.ServeMux,
	pool *pgxpool.Pool,
	oauthProvider *auth.OAuthProvider,
	cfg *config.Config,
) {
	mux.HandleFunc(
		"POST /api/v1/auth/login", loginHandler(pool))
	mux.HandleFunc(
		"POST /api/v1/auth/logout", logoutHandler(pool))
	mux.HandleFunc(
		"GET /api/v1/auth/me", meHandler())

	// OAuth routes (always registered; return disabled if not configured).
	mux.HandleFunc(
		"GET /api/v1/auth/oauth/config",
		oauthConfigHandler(oauthProvider, cfg.OAuth.Provider))
	mux.HandleFunc(
		"GET /api/v1/auth/oauth/authorize",
		oauthAuthorizeRouter(oauthProvider, pool))
	mux.HandleFunc(
		"GET /api/v1/auth/oauth/callback",
		oauthCallbackHandler(
			oauthProvider, pool,
			cfg.OAuth.DefaultRole, cfg.OAuth.Provider))
	registerAccountLinkRoutes(mux, pool, oauthProvider, cfg.OAuth.Provider)
}

func registerUserRoutes(
	mux *http.ServeMux, pool *pgxpool.Pool,
) {
	adminOnly := RequireRole("admin")

	listH := adminOnly(http.HandlerFunc(
		listUsersHandler(pool)))
	mux.Handle("GET /api/v1/users", listH)

	createH := adminOnly(http.HandlerFunc(
		createUserHandler(pool)))
	mux.Handle("POST /api/v1/users", createH)

	deleteH := adminOnly(http.HandlerFunc(
		deleteUserHandler(pool)))
	mux.Handle("DELETE /api/v1/users/{id}", deleteH)

	roleH := adminOnly(http.HandlerFunc(
		updateUserRoleHandler(pool)))
	mux.Handle("PUT /api/v1/users/{id}/role", roleH)
}

func registerConfigRoutesRuntime(
	mux *http.ServeMux,
	pool *pgxpool.Pool,
	cfg *config.Config,
	fm *fleet.DatabaseManager,
	controller *config.ConfigController,
	base configBaseSource,
	disableWrites bool,
) {
	if disableWrites {
		registerReadOnlyConfigRoutes(mux, cfg, fm, controller)
		return
	}
	adminOnly := RequireRole("admin")
	cs := store.NewConfigStore(pool)
	baseCfg, err := base()
	if err != nil {
		slog.Error("config base unavailable for read handlers", "error", err)
		baseCfg = config.Clone(cfg)
	}

	globalGet := adminOnly(http.HandlerFunc(
		configGlobalGetHandler(cs, baseCfg, controller)))
	mux.Handle("GET /api/v1/config/global", globalGet)

	globalPutHandler := configGlobalPutHandler(cs, cfg, fm, controller)
	globalPut := adminOnly(http.HandlerFunc(globalPutHandler))
	mux.Handle("PUT /api/v1/config/global", globalPut)

	globalDeleteHandler := configGlobalDeleteHandler(
		cs, cfg, base, fm, controller,
	)
	globalDelete := adminOnly(http.HandlerFunc(globalDeleteHandler))
	mux.Handle("DELETE /api/v1/config/global/{key}", globalDelete)

	registerDatabaseConfigRoutes(mux, cs, baseCfg, cfg, pool, fm, controller)

	audit := adminOnly(http.HandlerFunc(
		configAuditHandler(cs)))
	mux.Handle("GET /api/v1/config/audit", audit)
}

// registerDatabaseConfigRoutes covers the persistent per-database config
// override routes.
func registerDatabaseConfigRoutes(
	mux *http.ServeMux,
	cs *store.ConfigStore,
	baseCfg, cfg *config.Config,
	pool *pgxpool.Pool,
	fm *fleet.DatabaseManager,
	controller *config.ConfigController,
) {
	adminOnly := RequireRole("admin")

	dbGet := adminOnly(http.HandlerFunc(
		configDBGetHandler(cs, baseCfg, pool)))
	mux.Handle(
		"GET /api/v1/config/databases/{id}", dbGet)

	dbPutHandler := configDBPutHandler(cs, cfg, pool, fm)
	dbPut := adminOnly(http.HandlerFunc(dbPutHandler))
	mux.Handle(
		"PUT /api/v1/config/databases/{id}", dbPut)

	dbDeleteHandler := configDBDeleteHandler(cs, cfg, fm, controller)
	dbDelete := adminOnly(http.HandlerFunc(dbDeleteHandler))
	mux.Handle(
		"DELETE /api/v1/config/databases/{id}/{key}",
		dbDelete)
}

// registerReadOnlyConfigRoutes serves the config API in YAML fleet mode:
// reads come from the running config, every write answers 503.
func registerReadOnlyConfigRoutes(
	mux *http.ServeMux,
	cfg *config.Config,
	fm *fleet.DatabaseManager,
	controller *config.ConfigController,
) {
	adminOnly := RequireRole("admin")
	unavailable := adminOnly(http.HandlerFunc(
		configPersistenceUnavailableHandler(),
	))
	globalGet := adminOnly(http.HandlerFunc(
		configReadOnlyGlobalGetHandler(cfg, controller),
	))
	mux.Handle("GET /api/v1/config/global", globalGet)
	mux.Handle("PUT /api/v1/config/global", unavailable)
	mux.Handle("DELETE /api/v1/config/global/{key}", unavailable)
	dbGet := adminOnly(http.HandlerFunc(
		configReadOnlyDBGetHandler(cfg, fm, controller),
	))
	mux.Handle("GET /api/v1/config/databases/{id}", dbGet)
	mux.Handle("PUT /api/v1/config/databases/{id}", unavailable)
	mux.Handle("DELETE /api/v1/config/databases/{id}/{key}", unavailable)
	mux.Handle("GET /api/v1/config/audit", unavailable)
}

func configPersistenceUnavailableHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		jsonError(w,
			"persistent config API is unavailable in YAML fleet mode; edit the YAML file",
			http.StatusServiceUnavailable,
		)
	}
}
