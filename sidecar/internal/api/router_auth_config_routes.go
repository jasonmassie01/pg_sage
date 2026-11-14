package api

import (
	"log/slog"
	"net/http"
	"time"

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
	route(mux, "POST /api/v1/auth/login", gatePublic, http.HandlerFunc(loginHandler(pool)))
	route(mux, "POST /api/v1/auth/logout", gateViewerWrite, http.HandlerFunc(logoutHandler(pool)))
	route(mux, "GET /api/v1/auth/me", gateViewer, http.HandlerFunc(meHandler()))
	route(mux, "DELETE /api/v1/auth/sessions", gateViewerWrite,
		http.HandlerFunc(signOutOtherSessionsHandler(pool)))
	route(mux, "PUT /api/v1/auth/password", gateViewerWrite,
		http.HandlerFunc(changePasswordHandler(pool)))

	// OAuth routes (always registered; return disabled if not configured).
	route(mux, "GET /api/v1/auth/oauth/config", gatePublic,
		http.HandlerFunc(oauthConfigHandler(oauthProvider, cfg.OAuth.Provider)))
	route(mux, "GET /api/v1/auth/oauth/authorize", gatePublic,
		http.HandlerFunc(oauthAuthorizeRouter(oauthProvider, pool)))
	route(mux, "GET /api/v1/auth/oauth/callback", gatePublic,
		http.HandlerFunc(oauthCallbackHandler(oauthProvider, pool, oauthSnapshot(cfg))))
	registerAccountLinkRoutes(mux, pool, oauthProvider, cfg.OAuth.Provider)
}

// oauthSnapshot copies the OAuth settings at registration: they are
// restart-only, and the handlers must not read the hot-reloaded *cfg.
func oauthSnapshot(cfg *config.Config) *config.OAuthConfig {
	snap := cfg.OAuth
	snap.RoleMapping = append([]config.OAuthRoleMapping(nil), cfg.OAuth.RoleMapping...)
	snap.DatabaseGrantMapping = append([]config.OAuthDatabaseGrant(nil),
		cfg.OAuth.DatabaseGrantMapping...)
	return &snap
}

func registerUserRoutes(
	mux *http.ServeMux, pool *pgxpool.Pool,
) {
	listH := http.HandlerFunc(
		listUsersHandler(pool))
	route(mux, "GET /api/v1/users", gateAdmin, listH)

	createH := http.HandlerFunc(
		createUserHandler(pool))
	route(mux, "POST /api/v1/users", gateAdmin, createH)

	deleteH := http.HandlerFunc(
		deleteUserHandler(pool))
	route(mux, "DELETE /api/v1/users/{id}", gateAdmin, deleteH)

	roleH := http.HandlerFunc(
		updateUserRoleHandler(pool))
	route(mux, "PUT /api/v1/users/{id}/role", gateAdmin, roleH)

	statusH := http.HandlerFunc(
		setUserStatusHandler(pool))
	route(mux, "PUT /api/v1/users/{id}/status", gateAdmin, statusH)

	route(mux, "POST /api/v1/users/{id}/password-reset", gateAdmin,
		http.HandlerFunc(resetPasswordHandler(pool)))
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
	// A window preview reads and writes nothing, so YAML fleet mode serves
	// it too (config-09).
	route(mux, "POST /api/v1/config/validate-window", gateAdmin,
		validateWindowHandler(time.Now))
	if disableWrites {
		registerReadOnlyConfigRoutes(mux, fm, controller)
		return
	}
	cs := store.NewConfigStore(pool)
	baseCfg, err := base()
	if err != nil {
		slog.Error("config base unavailable for read handlers", "error", err)
		baseCfg = config.Clone(cfg)
	}

	globalGet := http.HandlerFunc(configGlobalGetHandler(cs, controller))
	route(mux, "GET /api/v1/config/global", gateAdmin, globalGet)

	globalPut := http.HandlerFunc(configGlobalPutHandler(cs, controller))
	route(mux, "PUT /api/v1/config/global", gateAdmin, globalPut)

	globalDelete := http.HandlerFunc(configGlobalDeleteHandler(cs, base, controller))
	route(mux, "DELETE /api/v1/config/global/{key}", gateAdmin, globalDelete)

	registerDatabaseConfigRoutes(mux, cs, baseCfg, cfg, pool, fm, controller)

	audit := http.HandlerFunc(
		configAuditHandler(cs))
	route(mux, "GET /api/v1/config/audit", gateAdmin, audit)
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
	dbGet := http.HandlerFunc(
		configDBGetHandler(cs, baseCfg, pool))
	route(mux, "GET /api/v1/config/databases/{id}", gateAdmin, dbGet)

	dbPutHandler := configDBPutHandler(cs, cfg, pool, fm)
	dbPut := http.HandlerFunc(dbPutHandler)
	route(mux, "PUT /api/v1/config/databases/{id}", gateAdmin, dbPut)

	dbDeleteHandler := configDBDeleteHandler(cs, cfg, fm, controller)
	dbDelete := http.HandlerFunc(dbDeleteHandler)
	route(mux, "DELETE /api/v1/config/databases/{id}/{key}", gateAdmin, dbDelete)
}

// registerReadOnlyConfigRoutes serves the config API in YAML fleet mode:
// reads come from the running config, every write answers 503.
func registerReadOnlyConfigRoutes(
	mux *http.ServeMux,
	fm *fleet.DatabaseManager,
	controller *config.ConfigController,
) {
	unavailable := http.HandlerFunc(
		configPersistenceUnavailableHandler(),
	)
	globalGet := http.HandlerFunc(configReadOnlyGlobalGetHandler(controller))
	route(mux, "GET /api/v1/config/global", gateAdmin, globalGet)
	route(mux, "PUT /api/v1/config/global", gateAdmin, unavailable)
	route(mux, "DELETE /api/v1/config/global/{key}", gateAdmin, unavailable)
	dbGet := http.HandlerFunc(configReadOnlyDBGetHandler(fm, controller))
	route(mux, "GET /api/v1/config/databases/{id}", gateAdmin, dbGet)
	route(mux, "PUT /api/v1/config/databases/{id}", gateAdmin, unavailable)
	route(mux, "DELETE /api/v1/config/databases/{id}/{key}", gateAdmin, unavailable)
	route(mux, "GET /api/v1/config/audit", gateAdmin, unavailable)
}

func configPersistenceUnavailableHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		jsonError(w,
			"persistent config API is unavailable in YAML fleet mode; edit the YAML file",
			http.StatusServiceUnavailable,
		)
	}
}
