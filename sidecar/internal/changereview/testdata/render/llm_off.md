<!-- pg_sage-review db=shop id=8f14e45f-ceea-467a-9575-0c9a1d8e7a01 receipt=pending -->
### pg_sage database review: REVIEW

Database shop, deterministic verdict review. pg_sage read the catalog and statistics and planned statements (plan-only); it ran nothing.

Track record on this database: safe: 6 of 6 concurred; 0 false-safe (safe, then a regressed receipt).

Model: no_model. App-code changes were not analyzed; the verdict is deterministic.

#### Warning

- **CR-20** rule_CR-20: CR-20 warning — deterministic CR-20 (evidence: stmt:1)

#### Statements

- 1. add_column at db/migrate/001.sql:1 (sql input): `ALTER TABLE public.orders ADD COLUMN note text`

#### Not determined

- app-code changes: no model is configured

#### Receipt

After this lands, pg_sage checks 1 catalog post-conditions and compares 2 watched statements before and after (Welch test). Landing: awaiting.
