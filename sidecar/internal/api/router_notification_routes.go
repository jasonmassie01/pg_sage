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
	secretKey   []byte
	previousKey []byte // the previous key of a rotation (security-08)
	policy      notify.TargetPolicy
}

func newNotificationRouteDeps(rt RuntimeDeps) notificationRouteDeps {
	return notificationRouteDeps{
		secretKey:   rt.NotificationSecretKey,
		previousKey: rt.NotificationPreviousSecretKey,
		policy:      rt.NotificationTargetPolicy,
	}
}

// registerNotificationRoutes returns the channel store the routes use, so
// the case ticket routes read the Jira integration through it.
func registerNotificationRoutes(
	mux *http.ServeMux, pool *pgxpool.Pool, deps notificationRouteDeps,
) *store.NotificationStore {
	d := newDefaultDispatcher(pool, deps)
	ns := store.NewNotificationStore(pool, d).
		WithSecretKey(deps.secretKey).WithPreviousSecretKey(deps.previousKey).
		WithTargetPolicy(deps.policy)

	registerNotificationChannelRoutes(mux, ns)
	registerNotificationRuleRoutes(mux, ns)

	logList := http.HandlerFunc(
		listNotificationLogHandler(ns))
	route(mux, "GET /api/v1/notifications/log", gateAdmin, logList)
	return ns
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
	return notify.NewDefaultDispatcher(
		notify.NewPoolStore(pool, deps.secretKey).WithPreviousKey(deps.previousKey),
		deps.policy, nil,
	)
}
