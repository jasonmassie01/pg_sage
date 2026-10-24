<!-- pg_sage-review db=shop id=8f14e45f-ceea-467a-9575-0c9a1d8e7a01 receipt=pending -->
### pg_sage database review: RISKY

Database shop, deterministic verdict risky. pg_sage read the catalog and statistics and planned statements (plan-only); it ran nothing.

Track record on this database: safe: 6 of 6 concurred; 0 false-safe (safe, then a regressed receipt).

Model: ok, 0 tokens; 0 dropped for missing or ungrounded citations.

#### Critical

- **CR-03** column_still_referenced: orders.email is still read by 1 hot statement — queryid -42 ran 2000 times this week &lt;script&gt;alert(1)&lt;/script&gt; second line (evidence: stmt:1, queryid:-42)

#### Warning

- **CR-02** rule_CR-02: CR-02 warning — deterministic CR-02 (evidence: stmt:1)

#### Statements

- 1. add_column at db/migrate/001.sql:1 (sql input): `ALTER TABLE public.orders ADD COLUMN note text`

#### Receipt

After this lands, pg_sage checks 1 catalog post-conditions and compares 2 watched statements before and after (Welch test). Landing: awaiting.
