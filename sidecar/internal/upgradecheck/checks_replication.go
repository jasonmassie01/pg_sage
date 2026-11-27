package upgradecheck

import (
	"context"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5"
)

// U1: a table logical replication cannot identify rows of. Blue/green and
// logical upgrades replicate every table, and UPDATE/DELETE on such a
// table fails on the publisher; pg_upgrade copies it as it is. REPLICA
// IDENTITY FULL or USING INDEX identifies rows, so those tables pass;
// REPLICA IDENTITY NOTHING identifies none, even on a table with a
// primary key.
// Unlogged and temporary tables are not replicated; extension-owned
// tables are the extension's.
var identitySQL = Tag + `SELECT pg_catalog.format('%I.%I', n.nspname, c.relname),
  CASE c.relreplident WHEN 'd' THEN 'default' ELSE 'nothing' END,
  count(*) OVER ()
FROM pg_catalog.pg_class c
JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
WHERE c.relkind IN ('r', 'p') AND c.relpersistence = 'p'
  AND n.nspname NOT IN ('pg_catalog', 'information_schema')
  AND n.nspname !~ '^pg_(toast|temp_)'
  AND (c.relreplident = 'n' OR (c.relreplident = 'd'
    AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_constraint k
      WHERE k.conrelid = c.oid AND k.contype = 'p')))
  AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_depend d
    WHERE d.classid = 'pg_catalog.pg_class'::regclass AND d.objid = c.oid
      AND d.deptype = 'e')
ORDER BY 1
LIMIT $1`

func checkIdentity(ctx context.Context, q pgx.Tx, in input) (Check, error) {
	rows, total, err := collect(ctx, q, []string{"table", "replica_identity"}, identitySQL)
	if err != nil {
		return Check{}, err
	}
	out := Check{Rows: rows, Total: total, Status: Pass,
		Reason: "every replicated table has a replica identity logical replication " +
			"can use"}
	if total == 0 {
		return out, nil
	}
	if in.target.Method == PGUpgrade {
		out.Status = Warn
		out.Reason = fmt.Sprintf("%d tables have no replica identity logical replication "+
			"can use (no primary key, or REPLICA IDENTITY NOTHING); pg_upgrade copies them "+
			"as they are, but a blue/green or logical upgrade would stop at them. Add a "+
			"primary key or set REPLICA IDENTITY.", total)
		return out, nil
	}
	out.Status = Block
	out.Reason = fmt.Sprintf("%d tables have no replica identity logical replication "+
		"can use (no primary key, or REPLICA IDENTITY NOTHING); a %s "+
		"upgrade replicates them logically and stops at its prechecks. Add a primary key, "+
		"or set REPLICA IDENTITY USING INDEX (a unique, non-null index) or FULL.", total,
		in.target.Method)
	return out, nil
}

// U3: this database's logical slots and subscriptions (each fleet member
// reports its own; slots of databases pg_sage does not monitor are not
// seen). pg_upgrade from a
// source below 17 does not carry logical slots, and from below 17 it
// does not keep a subscription's table sync state. subconninfo (which
// can hold a password) is never read.
var slotsSQL = Tag + `SELECT s.slot_name::text, coalesce(s.plugin::text, ''),
  coalesce(s.database::text, ''), count(*) OVER ()
FROM pg_catalog.pg_replication_slots s
WHERE s.slot_type = 'logical' AND s.database = current_database()
ORDER BY 1
LIMIT $1`

var subscriptionsSQL = Tag + `SELECT s.subname::text, s.subenabled::text, count(*) OVER ()
FROM pg_catalog.pg_subscription s
WHERE s.subdbid = (SELECT d.oid FROM pg_catalog.pg_database d
  WHERE d.datname = current_database())
ORDER BY 1
LIMIT $1`

func checkReplication(ctx context.Context, q pgx.Tx, in input) (Check, error) {
	slots, nSlots, err := collect(ctx, q, []string{"slot", "plugin", "database"}, slotsSQL)
	if err != nil {
		return Check{}, err
	}
	subs, nSubs, err := collect(ctx, q, []string{"subscription", "enabled"},
		subscriptionsSQL)
	if err != nil {
		return Check{}, err
	}
	out := Check{Total: nSlots + nSubs, Status: Pass,
		Reason: "no logical slots or subscriptions"}
	for _, r := range slots {
		r["kind"] = "slot"
		out.Rows = append(out.Rows, r)
	}
	for _, r := range subs {
		r["kind"] = "subscription"
		if len(out.Rows) < MaxRows {
			out.Rows = append(out.Rows, r)
		}
	}
	if out.Total == 0 {
		return out, nil
	}
	out.Status, out.Reason = replicationVerdict(in, nSlots, nSubs)
	return out, nil
}

func replicationVerdict(in input, nSlots, nSubs int) (Status, string) {
	src := "PostgreSQL " + strconv.Itoa(in.src.major)
	if in.target.Method != PGUpgrade {
		return Warn, fmt.Sprintf("%d logical slots and %d subscriptions: a %s upgrade "+
			"does not move them; recreate the slots on the new primary and repoint "+
			"consumers and subscriptions at switchover.", nSlots, nSubs, in.target.Method)
	}
	if in.src.major >= 17 {
		return Pass, fmt.Sprintf("pg_upgrade from %s carries logical slots and "+
			"subscription state when the slots have consumed all WAL and the new cluster "+
			"has wal_level=logical and at least %d max_replication_slots.", src, nSlots)
	}
	if nSlots > 0 {
		return Block, fmt.Sprintf("pg_upgrade from %s does not carry logical slots: the "+
			"%d slots disappear and their consumers lose their position. Drain and drop "+
			"them before the upgrade, recreate them after, and resync consumers.",
			src, nSlots)
	}
	return Warn, fmt.Sprintf("pg_upgrade from %s does not keep the %d subscriptions' "+
		"table sync state; refresh or recreate them after the upgrade.", src, nSubs)
}
