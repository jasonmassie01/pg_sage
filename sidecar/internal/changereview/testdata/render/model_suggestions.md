<!-- pg_sage-review db=shop id=8f14e45f-ceea-467a-9575-0c9a1d8e7a01 receipt=pending -->
### pg_sage database review: REVIEW

Database shop, deterministic verdict safe, raised to review by model findings. pg_sage read the catalog and statistics and planned statements (plan-only); it ran nothing.

Track record on this database: safe: 6 of 6 concurred; 0 false-safe (safe, then a regressed receipt).

Model: ok, 4200 tokens; 2 dropped for missing or ungrounded citations.

#### Warning

- **model** n_plus_one: Order.where in a loop — 50 queries per request (evidence: stmt:2)

  _suggested by pg_sage's model — verify_

````ruby
orders = Order.where(id: ids).to_a
# ``` fence inside code
````


#### Statements

- 1. add_column at db/migrate/001.sql:1 (sql input): `ALTER TABLE public.orders ADD COLUMN note text`
- 2. select at app/controllers/orders_controller.rb:12 (model extracted, confidence 0.80), new shape: `SELECT * FROM orders WHERE id = $1`

2 model claims or findings were dropped (uncited, ungrounded or outside the diff).

#### Receipt

After this lands, pg_sage checks 1 catalog post-conditions and compares 2 watched statements before and after (Welch test). Landing: awaiting.
