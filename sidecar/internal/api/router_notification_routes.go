package api

import (
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/notify"
	"github.com/pg-sage/sidecar/internal/store"
)

// notificationRouteDeps carries the channel secret key and target policy
// the runtime dispatchers use, so API writes and runtime reads agree.
type notificationRouteDeps struct {
	secretKey []byte
	policy    notify.TargetPolicy
}

func registerNotificationRoutes(
	mux *http.ServeMux, pool *pgxpool.Pool, deps notificationRouteDeps,
) {
	adminOnly := RequireRole("admin")
	d := newDefaultDispatcher(pool, deps)
	ns := store.NewNotificationStore(pool, d).
		WithSecretKey(deps.secretKey).WithTargetPolicy(deps.policy)

	registerNotificationChannelRoutes(mux, ns, adminOnly)
	registerNotificationRuleRoutes(mux, ns, adminOnly)

	logList := adminOnly(http.HandlerFunc(
		listNotificationLogHandler(ns)))
	mux.Handle(
		"GET /api/v1/notifications/log", logList)
}

func registerNotificationChannelRoutes(
	mux *http.ServeMux, ns *store.NotificationStore,
	adminOnly func(http.Handler) http.Handler,
) {
	chList := adminOnly(http.HandlerFunc(
		listChannelsHandler(ns)))
	mux.Handle(
		"GET /api/v1/notifications/channels", chList)

	chCreate := adminOnly(http.HandlerFunc(
		createChannelHandler(ns)))
	mux.Handle(
		"POST /api/v1/notifications/channels", chCreate)

	chUpdate := adminOnly(http.HandlerFunc(
		updateChannelHandler(ns)))
	mux.Handle(
		"PUT /api/v1/notifications/channels/{id}",
		chUpdate)

	chDelete := adminOnly(http.HandlerFunc(
		deleteChannelHandler(ns)))
	mux.Handle(
		"DELETE /api/v1/notifications/channels/{id}",
		chDelete)

	chTest := adminOnly(http.HandlerFunc(
		testChannelHandler(ns)))
	mux.Handle(
		"POST /api/v1/notifications/channels/{id}/test",
		chTest)
}

func registerNotificationRuleRoutes(
	mux *http.ServeMux, ns *store.NotificationStore,
	adminOnly func(http.Handler) http.Handler,
) {
	ruleList := adminOnly(http.HandlerFunc(
		listRulesHandler(ns)))
	mux.Handle(
		"GET /api/v1/notifications/rules", ruleList)

	ruleCreate := adminOnly(http.HandlerFunc(
		createRuleHandler(ns)))
	mux.Handle(
		"POST /api/v1/notifications/rules", ruleCreate)

	ruleDelete := adminOnly(http.HandlerFunc(
		deleteRuleHandler(ns)))
	mux.Handle(
		"DELETE /api/v1/notifications/rules/{id}",
		ruleDelete)

	ruleUpdate := adminOnly(http.HandlerFunc(
		updateRuleHandler(ns)))
	mux.Handle(
		"PUT /api/v1/notifications/rules/{id}",
		ruleUpdate)
}

func newDefaultDispatcher(
	pool *pgxpool.Pool, deps notificationRouteDeps,
) *notify.Dispatcher {
	logFn := func(_, _ string, _ ...any) {}
	d := notify.NewDispatcherWithStore(
		notify.NewPoolStore(pool, deps.secretKey), logFn,
	)
	d.RegisterSender(notify.NewSlackSenderWithPolicy(deps.policy))
	d.RegisterSender(notify.NewEmailSenderWithPolicy(deps.policy))
	d.RegisterSender(notify.NewPagerDutySender())
	d.RegisterSender(notify.NewTelegramSenderWithPolicy(deps.policy))
	return d
}
