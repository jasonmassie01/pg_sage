package collector

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// SubscriptionInfo is one logical replication subscription of this
// database: pg_subscription joined to its leader apply worker in
// pg_stat_subscription (market-13). The connection string column, which
// holds the publisher's credentials, is never read.
type SubscriptionInfo struct {
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`
	// WorkerPID is the apply worker's pid; nil when no worker runs.
	WorkerPID          *int32     `json:"worker_pid,omitempty"`
	ReceivedLSN        *string    `json:"received_lsn,omitempty"`
	LatestEndLSN       *string    `json:"latest_end_lsn,omitempty"`
	LatestEndTime      *time.Time `json:"latest_end_time,omitempty"`
	LastMsgReceiptTime *time.Time `json:"last_msg_receipt_time,omitempty"`
	// ApplyErrorCount and SyncErrorCount come from
	// pg_stat_subscription_stats (PG15+); nil before.
	ApplyErrorCount *int64 `json:"apply_error_count,omitempty"`
	SyncErrorCount  *int64 `json:"sync_error_count,omitempty"`
	// Conflicts are the PG18+ confl_* counters by column name; nil before.
	Conflicts map[string]int64 `json:"conflicts,omitempty"`
}

// The subscriptions statement reads this database's subscriptions (fleet
// mode: one row set per database, through subdbid) and each one's leader
// apply worker: relid IS NULL leaves out table synchronization workers, a
// NULL leader_pid (PG16+, read by name) parallel apply workers. Only named
// pg_subscription columns are read: the connection string column is not
// readable by normal roles and holds credentials.
const subscriptionsSelect = sageTag + `
SELECT sub.subname, sub.subenabled, w.pid, w.received_lsn::text,
       w.latest_end_lsn::text, w.latest_end_time, w.last_msg_receipt_time`

const subscriptionsFrom = `
  FROM pg_subscription sub
  LEFT JOIN LATERAL (
       SELECT st.pid, st.received_lsn, st.latest_end_lsn, st.latest_end_time,
              st.last_msg_receipt_time
         FROM pg_stat_subscription st
        WHERE st.subid = sub.oid AND st.relid IS NULL
          AND (to_jsonb(st) ->> 'leader_pid') IS NULL
        ORDER BY st.pid NULLS LAST
        LIMIT 1) w ON true`

const subscriptionsWhere = `
 WHERE sub.subdbid = (SELECT oid FROM pg_database WHERE datname = current_database())
 ORDER BY sub.subname
 LIMIT 200`

// subscriptionsStatsCols reads the PG15+ error counters, and the PG18
// conflict counters by name through to_jsonb (NULL before PG18).
const subscriptionsStatsCols = `,
       ss.apply_error_count, ss.sync_error_count,
       (SELECT jsonb_object_agg(e.key, e.value)
          FROM jsonb_each(to_jsonb(ss)) e
         WHERE e.key LIKE 'confl\_%') AS conflicts`

const subscriptionsStatsJoin = `
  LEFT JOIN pg_stat_subscription_stats ss ON ss.subid = sub.oid`

// subscriptionsSQL is the statement for server_version_num version: before
// PG15 there is no pg_stat_subscription_stats, so it is not named.
func subscriptionsSQL(version int) string {
	if version < 150000 {
		return subscriptionsSelect + `,
       NULL::int8, NULL::int8, NULL::jsonb` + subscriptionsFrom + subscriptionsWhere
	}
	return subscriptionsSelect + subscriptionsStatsCols + subscriptionsFrom +
		subscriptionsStatsJoin + subscriptionsWhere
}

// collectSubscriptions reads this database's subscriptions; none is an
// empty, non-nil slice.
func (c *Collector) collectSubscriptions(ctx context.Context) ([]SubscriptionInfo, error) {
	rows, err := c.catalogQuery(ctx, subscriptionsSQL(c.Capabilities().PGVersionNum))
	if err != nil {
		return nil, err
	}
	return scanSubscriptionRows(rows)
}

func scanSubscriptionRows(rows pgx.Rows) ([]SubscriptionInfo, error) {
	defer rows.Close()
	subs := []SubscriptionInfo{}
	for rows.Next() {
		var s SubscriptionInfo
		if err := rows.Scan(&s.Name, &s.Enabled, &s.WorkerPID, &s.ReceivedLSN,
			&s.LatestEndLSN, &s.LatestEndTime, &s.LastMsgReceiptTime,
			&s.ApplyErrorCount, &s.SyncErrorCount, &s.Conflicts); err != nil {
			return nil, err
		}
		subs = append(subs, s)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return subs, nil
}

// replicationHealthSteps are the market-13 categories: subscriptions every
// cycle, the CDC heartbeats only when some are configured. They run after
// the replication read, whose slot list places each heartbeat.
func (c *Collector) replicationHealthSteps() []catalogStep {
	steps := []catalogStep{{name: "subscriptions",
		run: func(ctx context.Context, s *Snapshot) (err error) {
			s.Subscriptions, err = c.collectSubscriptions(ctx)
			return err
		}, reset: func(s *Snapshot) { s.Subscriptions = nil }}}
	if len(c.heartbeats()) > 0 {
		steps = append(steps, catalogStep{name: "cdc_heartbeats",
			run: func(ctx context.Context, s *Snapshot) error {
				s.CDCHeartbeats = c.collectCDCHeartbeats(ctx, s)
				return nil
			}, reset: func(s *Snapshot) { s.CDCHeartbeats = nil }})
	}
	return steps
}
