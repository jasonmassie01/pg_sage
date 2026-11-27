package doctor

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	titleMeta   = "Meta database"
	titleNotify = "Test notification"
)

// metaLedgerSQL reads, catalog first, whether the migration ledger exists,
// then the recorded steps and the version that recorded the latest one.
const metaLedgerSQL = `/* pg_sage doctor */ SELECT
  COALESCE((SELECT array_agg(id ORDER BY id) FROM sage.schema_migrations
             WHERE id <> 'catalog fingerprint'), '{}'),
  COALESCE((SELECT pg_sage_version FROM sage.schema_migrations
             ORDER BY applied_at DESC LIMIT 1), '')`

// metaCheck pings the meta database and compares its migration ledger
// with the steps this build applies. Nothing is created or written.
func metaCheck(ctx context.Context, meta *pgxpool.Pool, openErr error,
	expected []string) Check {
	c := Check{ID: IDMetaDB, Title: titleMeta, Status: StatusFail}
	switch {
	case openErr != nil:
		c.Detail = "cannot open the meta database: " + openErr.Error()
		c.Fix = "check meta_db (SAGE_META_DB)"
		return c
	case meta == nil:
		return unknownCheck(IDMetaDB, titleMeta, "not configured: no meta_db is set "+
			"(single mode keeps pg_sage's state in the monitored database)")
	}
	qctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()
	var exists bool
	if err := meta.QueryRow(qctx, "/* pg_sage doctor */ SELECT "+
		"pg_catalog.to_regclass('sage.schema_migrations') IS NOT NULL").
		Scan(&exists); err != nil {
		c.Detail = "cannot reach the meta database: " + err.Error()
		c.Fix = "check meta_db's host, port, role and password from this host"
		return c
	}
	if !exists {
		c.Status = StatusWarn
		c.Detail = "the meta database is reachable; the sage schema is not created " +
			"yet: the sidecar creates it on its first start"
		return c
	}
	var recorded []string
	var version string
	if err := meta.QueryRow(qctx, metaLedgerSQL).Scan(&recorded, &version); err != nil {
		c.Detail = "cannot read sage.schema_migrations: " + err.Error()
		c.Fix = "GRANT SELECT ON sage.schema_migrations TO the role in meta_db"
		return c
	}
	return ledgerCheck(c, recorded, version, expected)
}

// ledgerCheck passes when every expected step is recorded and warns,
// naming the first missing steps, when some are not.
func ledgerCheck(c Check, recorded []string, version string, expected []string) Check {
	have := make(map[string]bool, len(recorded))
	for _, id := range recorded {
		have[id] = true
	}
	var missing []string
	for _, id := range expected {
		if !have[id] {
			missing = append(missing, id)
		}
	}
	by := ""
	if version != "" {
		by = ", last by pg_sage " + version
	}
	if len(missing) == 0 {
		c.Status = StatusPass
		c.Detail = fmt.Sprintf("reachable; %d migration steps recorded%s", len(recorded), by)
		return c
	}
	shown := missing[:min(len(missing), 3)]
	c.Status = StatusWarn
	c.Detail = fmt.Sprintf("reachable; %d of this build's migration steps are not applied "+
		"yet (%s)%s", len(missing), strings.Join(shown, ", "), by)
	c.Fix = "start this pg_sage build once against the meta database: it applies them"
	return c
}

// notifyCheck sends the test notification only when the operator asked.
func notifyCheck(ctx context.Context, send NotifyTest) Check {
	if send == nil {
		return unknownCheck(IDNotification, titleNotify, "not sent: pass --send-test to "+
			"send one test notification through each configured channel")
	}
	c := Check{ID: IDNotification, Title: titleNotify}
	sctx, cancel := context.WithTimeout(ctx, modelProbeTimeout)
	defer cancel()
	n, err := send(sctx)
	switch {
	case err != nil:
		c.Status = StatusFail
		c.Detail = fmt.Sprintf("test delivery failed after %d sent: %v", n, err)
		c.Fix = "check the channel's URL or key and that this host may reach it " +
			"(notification_policy.allow_private_targets for a private address)"
	case n == 0:
		c.Status = StatusWarn
		c.Detail = "no notification channels are configured in the alerting: block"
		c.Fix = "set alerting.slack_webhook_url, alerting.pagerduty_routing_key or " +
			"alerting.webhooks; channels saved in the dashboard are tested from " +
			"Admin > Notifications"
	default:
		c.Status = StatusPass
		c.Detail = fmt.Sprintf("test notification sent through %d channel(s)", n)
	}
	return c
}
