\set ON_ERROR_STOP on

DO $$
BEGIN
  BEGIN
    CREATE EXTENSION IF NOT EXISTS pg_stat_statements;
  EXCEPTION WHEN OTHERS THEN
    RAISE NOTICE 'pg_stat_statements unavailable in this database: %', SQLERRM;
  END;
END $$;

DROP SCHEMA IF EXISTS sage_verify CASCADE;
CREATE SCHEMA sage_verify;
SET search_path = sage_verify, public;

CREATE SEQUENCE sage_verify.near_exhaustion_seq AS integer MINVALUE 1 MAXVALUE 100 START 1;
SELECT setval('sage_verify.near_exhaustion_seq', 95, true);

CREATE TABLE sage_verify.customers (
  id bigint PRIMARY KEY,
  region text NOT NULL,
  email varchar(255),
  profile jsonb,
  created_at timestamp NOT NULL DEFAULT now(),
  code char(12),
  active boolean NOT NULL DEFAULT true,
  small_status int NOT NULL,
  nullable_external_id text UNIQUE,
  legacy_id serial
);

CREATE TABLE sage_verify.orders (
  id bigserial PRIMARY KEY,
  customer_id bigint NOT NULL REFERENCES sage_verify.customers(id),
  status text NOT NULL,
  amount numeric(12,2) NOT NULL,
  created_at timestamp NOT NULL DEFAULT now(),
  payload jsonb,
  note text
);

CREATE TABLE sage_verify.no_primary_key (
  natural_key text,
  payload text,
  created_at timestamp
);

CREATE TABLE sage_verify.parent_int (
  id integer PRIMARY KEY
);

CREATE TABLE sage_verify.child_bigint_fk (
  id bigserial PRIMARY KEY,
  parent_id bigint REFERENCES sage_verify.parent_int(id)
);

CREATE TABLE sage_verify.bloated_table (
  id bigserial PRIMARY KEY,
  payload text
) WITH (autovacuum_enabled = false);

CREATE TABLE sage_verify.toast_heavy (
  id bigserial PRIMARY KEY,
  payload text
);

CREATE TABLE sage_verify.sort_target (
  id bigserial PRIMARY KEY,
  group_id int NOT NULL,
  score int NOT NULL,
  payload text
);

CREATE TABLE sage_verify.join_left (
  id int PRIMARY KEY,
  join_key int NOT NULL,
  payload text
);

CREATE TABLE sage_verify.join_right (
  id int PRIMARY KEY,
  join_key int NOT NULL,
  payload text
);

CREATE TABLE sage_verify.invalid_index_seed (
  id bigserial PRIMARY KEY,
  duplicate_value int NOT NULL
);

DO $$
DECLARE
  cols text;
BEGIN
  SELECT string_agg(format('c%s integer', g), ', ')
    INTO cols
    FROM generate_series(1, 55) AS g;
  EXECUTE format('CREATE TABLE sage_verify.wide_table (id bigserial PRIMARY KEY, %s)', cols);
END $$;

INSERT INTO sage_verify.customers (
  id, region, email, profile, created_at, code, active, small_status,
  nullable_external_id
)
SELECT g,
       'region-' || (g % 5),
       'user' || g || '@example.com',
       jsonb_build_object('customer_id', g, 'tier', g % 3, 'flags', jsonb_build_array(g % 2, g % 5)),
       now() - (g || ' minutes')::interval,
       lpad((g % 999)::text, 12, '0'),
       (g % 2 = 0),
       g % 3,
       CASE WHEN g % 100 = 0 THEN NULL ELSE 'ext-' || g END
  FROM generate_series(1, 5000) AS g;

INSERT INTO sage_verify.orders (customer_id, status, amount, created_at, payload, note)
SELECT ((g - 1) % 5000) + 1,
       CASE WHEN g % 3 = 0 THEN 'open' WHEN g % 3 = 1 THEN 'paid' ELSE 'cancelled' END,
       (g % 1000) + 0.42,
       now() - (g || ' seconds')::interval,
       jsonb_build_object('customer_id', ((g - 1) % 5000) + 1, 'bucket', g % 10),
       repeat('n', 200)
  FROM generate_series(1, 20000) AS g;

INSERT INTO sage_verify.no_primary_key
SELECT 'nk-' || g, repeat('p', 100), now()
  FROM generate_series(1, 2000) AS g;

INSERT INTO sage_verify.parent_int
SELECT g FROM generate_series(1, 1000) AS g;

INSERT INTO sage_verify.child_bigint_fk (parent_id)
SELECT ((g - 1) % 1000) + 1 FROM generate_series(1, 2000) AS g;

INSERT INTO sage_verify.bloated_table (payload)
SELECT repeat(md5(g::text), 50) FROM generate_series(1, 10000) AS g;
DELETE FROM sage_verify.bloated_table WHERE id <= 8000;

INSERT INTO sage_verify.toast_heavy (payload)
SELECT repeat(md5(g::text), 12000) FROM generate_series(1, 200) AS g;

INSERT INTO sage_verify.sort_target (group_id, score, payload)
SELECT g % 25, (random() * 1000000)::int, repeat('s', 100)
  FROM generate_series(1, 15000) AS g;

INSERT INTO sage_verify.join_left
SELECT g, g % 500, repeat('l', 50) FROM generate_series(1, 5000) AS g;

INSERT INTO sage_verify.join_right
SELECT g, g % 500, repeat('r', 50) FROM generate_series(1, 5000) AS g;

INSERT INTO sage_verify.invalid_index_seed (duplicate_value)
SELECT 1 FROM generate_series(1, 100) AS g;

DO $$
DECLARE
  cols text;
BEGIN
  SELECT string_agg('0', ', ')
    INTO cols
    FROM generate_series(1, 55);
  EXECUTE format('INSERT INTO sage_verify.wide_table SELECT g, %s FROM generate_series(1, 25) AS g', cols);
END $$;

CREATE INDEX idx_verify_orders_status_a ON sage_verify.orders(status);
CREATE INDEX idx_verify_orders_status_b ON sage_verify.orders(status);
CREATE INDEX idx_verify_orders_created_at ON sage_verify.orders(created_at);
CREATE INDEX idx_verify_orders_created_at_amount ON sage_verify.orders(created_at, amount);
CREATE INDEX idx_verify_customers_active ON sage_verify.customers(active);
CREATE INDEX idx_verify_customers_region_unused ON sage_verify.customers(region);
CREATE INDEX idx_verify_sort_target_group ON sage_verify.sort_target(group_id);
CREATE INDEX idx_verify_join_right_key ON sage_verify.join_right(join_key);

\set ON_ERROR_STOP off
CREATE UNIQUE INDEX CONCURRENTLY idx_verify_invalid_unique
  ON sage_verify.invalid_index_seed(duplicate_value);
\set ON_ERROR_STOP on

ANALYZE sage_verify.customers;
ANALYZE sage_verify.orders;
ANALYZE sage_verify.no_primary_key;
ANALYZE sage_verify.parent_int;
ANALYZE sage_verify.child_bigint_fk;
ANALYZE sage_verify.bloated_table;
ANALYZE sage_verify.toast_heavy;
ANALYZE sage_verify.sort_target;
ANALYZE sage_verify.join_left;
ANALYZE sage_verify.join_right;
ANALYZE sage_verify.invalid_index_seed;
ANALYZE sage_verify.wide_table;
