-- pg_sage capability demo — seed schema + data.
-- Run against a fresh `pgsage_demo` database on the health_pg container
-- (PG16 + timescaledb, has pg_stat_statements preloaded, vector, btree_gin).
--
-- Each block sets up ONE scenario that pg_sage should detect and act on.
-- The companion load generator (run_demo.py) hammers the read queries so
-- they land in pg_stat_statements for the optimizer/advisor to analyze.
--
-- Idempotent: safe to re-run (drops and recreates everything).

\set ON_ERROR_STOP on

CREATE EXTENSION IF NOT EXISTS pg_stat_statements;
CREATE EXTENSION IF NOT EXISTS vector;
CREATE EXTENSION IF NOT EXISTS btree_gin;

SELECT pg_stat_statements_reset();

DROP TABLE IF EXISTS order_items, orders, events, documents, customers CASCADE;

-- ---------------------------------------------------------------------------
-- customers — small dimension table (join target). Indexed PK only.
-- ---------------------------------------------------------------------------
CREATE TABLE customers (
    id        bigserial PRIMARY KEY,
    name      text NOT NULL,
    region    text NOT NULL,
    tier      text NOT NULL
);
INSERT INTO customers (name, region, tier)
SELECT 'customer ' || g,
       (ARRAY['NA','EU','APAC','LATAM'])[1 + (g % 4)],
       (ARRAY['free','pro','enterprise'])[1 + (g % 3)]
FROM generate_series(1, 5000) g;

-- ---------------------------------------------------------------------------
-- Scenario A — MISSING INDEX → CREATE INDEX (auto, safe).
-- Scenario H — STALE STATS  → ANALYZE (auto).
-- Scenario I — DEAD TUPLES  → VACUUM  (auto).
-- orders has 500k rows, no index on customer_id (hot filter column).
-- autovacuum disabled so stats stay stale and dead tuples accumulate,
-- guaranteeing the VACUUM + ANALYZE findings fire instead of being
-- silently cleaned up by the autovacuum daemon mid-demo.
-- ---------------------------------------------------------------------------
CREATE TABLE orders (
    id           bigserial PRIMARY KEY,
    customer_id  bigint NOT NULL,
    status       text   NOT NULL,
    total_cents  bigint NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now()
) WITH (autovacuum_enabled = false);

INSERT INTO orders (customer_id, status, total_cents, created_at)
SELECT 1 + (random() * 4999)::bigint,
       (ARRAY['pending','paid','shipped','cancelled'])[1 + (g % 4)],
       (random() * 50000)::bigint,
       now() - (random() * interval '365 days')
FROM generate_series(1, 500000) g;

-- Scenario G — DUPLICATE INDEX → DROP INDEX (auto, safe).
-- Two byte-identical indexes on orders.status; pg_sage drops one.
CREATE INDEX idx_orders_status_a ON orders (status);
CREATE INDEX idx_orders_status_b ON orders (status);

-- Churn: create dead tuples + bump n_mod_since_analyze (no autovacuum here).
UPDATE orders SET status = 'paid'      WHERE id % 5 = 0;
UPDATE orders SET total_cents = total_cents + 1 WHERE id % 7 = 0;
DELETE FROM orders WHERE id % 50 = 0;

-- ---------------------------------------------------------------------------
-- Scenario B — MISSING FK INDEX → CREATE INDEX (auto, safe).
-- order_items.order_id is a foreign key with NO supporting index — the
-- classic "unindexed FK" that makes parent deletes and joins slow.
-- ---------------------------------------------------------------------------
CREATE TABLE order_items (
    id        bigserial PRIMARY KEY,
    order_id  bigint NOT NULL REFERENCES orders (id),
    sku       text   NOT NULL,
    qty       int    NOT NULL
);
-- Sample order_id from orders that actually survived the churn above, so the
-- FK is always satisfied (~1.6 items/order, all pointing at live parents).
INSERT INTO order_items (order_id, sku, qty)
SELECT o.id,
       'SKU-' || (1 + (random() * 2000)::int),
       1 + (random() * 9)::int
FROM orders o
CROSS JOIN generate_series(1, 2) g
WHERE random() < 0.8;

-- ---------------------------------------------------------------------------
-- Scenario C — GIN INDEX FOR JSONB → CREATE INDEX USING gin (optimizer).
-- events.payload is queried with the @> containment operator; without a
-- GIN index every lookup is a full seq scan.
-- ---------------------------------------------------------------------------
CREATE TABLE events (
    id         bigserial PRIMARY KEY,
    payload    jsonb NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);
INSERT INTO events (payload, created_at)
SELECT jsonb_build_object(
           'type',    (ARRAY['click','view','purchase','signup','logout'])[1 + (g % 5)],
           'user_id', 1 + (random() * 5000)::int,
           'source',  (ARRAY['web','ios','android','api'])[1 + (g % 4)],
           'amount',  (random() * 500)::numeric(10,2)
       ),
       now() - (random() * interval '90 days')
FROM generate_series(1, 300000) g;

-- ---------------------------------------------------------------------------
-- Scenario D — HNSW INDEX FOR VECTOR → CREATE INDEX USING hnsw (optimizer).
-- documents.embedding is queried with nearest-neighbour ORDER BY <-> LIMIT,
-- but has no vector index, forcing a full scan + exact distance on every row.
-- ---------------------------------------------------------------------------
CREATE TABLE documents (
    id        bigserial PRIMARY KEY,
    title     text NOT NULL,
    embedding vector(256) NOT NULL
);

CREATE OR REPLACE FUNCTION demo_rand_vec(dim int)
RETURNS vector LANGUAGE sql VOLATILE AS $$
    SELECT (SELECT array_agg(random())::vector
            FROM generate_series(1, dim));
$$;

INSERT INTO documents (title, embedding)
SELECT 'document ' || g, demo_rand_vec(256)
FROM generate_series(1, 20000) g;

-- ---------------------------------------------------------------------------
-- Scenario E — work_mem BUMP → ALTER SYSTEM SET work_mem (memory advisor).
-- Pin this database's work_mem tiny so the big GROUP BY / sort in the load
-- generator spills to temp files, which the memory advisor flags.
-- ---------------------------------------------------------------------------
ALTER DATABASE pgsage_demo SET work_mem = '64kB';

-- Refresh planner stats for the dimension tables we DO want indexed/analyzed
-- (orders is intentionally left stale for Scenario H).
ANALYZE customers;
ANALYZE order_items;
ANALYZE events;
ANALYZE documents;

SELECT 'seed complete' AS status,
       (SELECT count(*) FROM orders)      AS orders,
       (SELECT count(*) FROM order_items) AS order_items,
       (SELECT count(*) FROM events)      AS events,
       (SELECT count(*) FROM documents)   AS documents,
       (SELECT count(*) FROM customers)   AS customers;
