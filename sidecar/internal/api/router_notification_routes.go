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
	d := newDefaultDispatcher(pool, deps)
	ns := store.NewNotificationStore(pool, d).
		WithSecretKey(deps.secretKey).WithTargetPolicy(deps.policy)

	registerNotificationChannelRoutes(mux, ns)
	registerNotificationRuleRoutes(mux, ns)

	logList := http.HandlerFunc(
		listNotificationLogHandler(ns))
	route(mux, "GET /api/v1/notifications/log", gateAdmin, logList)
}

func registerNotificationChannelRoutes(
	mux *http.ServeMux, ns *store.NotificationStore,
) {
	chList := http.HandlerFunc(
		listChannelsHandler(ns))
	route(mux, "GET /api/v1/notifications/channels", gateAdmin, chList)

	chCreate := http.HandlerFunc(
		createChannelHandler(ns))
	route(mux, "POST /api/v1/notifications/channels", gateAdmin, chCreate)

	chUpdate := http.HandlerFunc(
		updateChannelHandler(ns))
	route(mux, "PUT /api/v1/notifications/channels/{id}", gateAdmin, chUpdate)

	chDelete := http.HandlerFunc(
		deleteChannelHandler(ns))
	route(mux, "DELETE /api/v1/notifications/channels/{id}", gateAdmin, chDelete)

	chTest := http.HandlerFunc(
		testChannelHandler(ns))
	route(mux, "POST /api/v1/notifications/channels/{id}/test", gateAdmin, chTest)
}

func registerNotificationRuleRoutes(
	mux *http.ServeMux, ns *store.NotificationStore,
) {
	ruleList := http.HandlerFunc(
		listRulesHandler(ns))
	route(mux, "GET /api/v1/notifications/rules", gateAdmin, ruleList)

	ruleCreate := http.HandlerFunc(
		createRuleHandler(ns))
	route(mux, "POST /api/v1/notifications/rules", gateAdmin, ruleCreate)

	ruleDelete := http.HandlerFunc(
		deleteRuleHandler(ns))
	route(mux, "DELETE /api/v1/notifications/rules/{id}", gateAdmin, ruleDelete)

	ruleUpdate := http.HandlerFunc(
		updateRuleHandler(ns))
	route(mux, "PUT /api/v1/notifications/rules/{id}", gateAdmin, ruleUpdate)
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
