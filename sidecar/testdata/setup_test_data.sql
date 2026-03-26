-- pg_sage LLM DBA Features Test Data
-- Run against Cloud SQL PG17 test instance

-- Base tables for test scenarios
CREATE TABLE IF NOT EXISTS customers (
    customer_id SERIAL PRIMARY KEY,
    name VARCHAR(100),
    email VARCHAR(200),
    status VARCHAR(20) DEFAULT 'active',
    created_at TIMESTAMPTZ DEFAULT now()
);

CREATE TABLE IF NOT EXISTS products (
    product_id SERIAL PRIMARY KEY,
    name VARCHAR(200),
    price NUMERIC(10,2),
    category VARCHAR(50),
    created_at TIMESTAMPTZ DEFAULT now()
);

CREATE TABLE IF NOT EXISTS orders (
    order_id SERIAL PRIMARY KEY,
    customer_id INT REFERENCES customers(customer_id),
    product_id INT,
    order_date TIMESTAMPTZ DEFAULT now(),
    total_amount NUMERIC(10,2),
    notes TEXT
);

CREATE TABLE IF NOT EXISTS line_items (
    line_item_id SERIAL PRIMARY KEY,
    order_id INT REFERENCES orders(order_id),
    product_id INT REFERENCES products(product_id),
    quantity INT DEFAULT 1,
    unit_price NUMERIC(10,2)
);

-- Insert base data
INSERT INTO customers (name, email, status)
SELECT 'Customer ' || g,
       'customer' || g || '@example.com',
       CASE WHEN g % 10 = 0 THEN 'inactive' ELSE 'active' END
FROM generate_series(1, 5000) g
ON CONFLICT DO NOTHING;

INSERT INTO products (name, price, category)
SELECT 'Product ' || g,
       (random() * 100 + 1)::numeric(10,2),
       'cat_' || (g % 20)
FROM generate_series(1, 5000) g
ON CONFLICT DO NOTHING;

INSERT INTO orders (customer_id, product_id, order_date, total_amount, notes)
SELECT (random() * 4999 + 1)::int,
       (random() * 4999 + 1)::int,
       now() - (random() * 365)::int * interval '1 day',
       (random() * 500 + 10)::numeric(10,2),
       'Order notes ' || g
FROM generate_series(1, 50000) g
ON CONFLICT DO NOTHING;

INSERT INTO line_items (order_id, product_id, quantity, unit_price)
SELECT (random() * 49999 + 1)::int,
       (random() * 4999 + 1)::int,
       (random() * 5 + 1)::int,
       (random() * 100 + 1)::numeric(10,2)
FROM generate_series(1, 100000) g
ON CONFLICT DO NOTHING;

-- VACUUM TUNING TEST DATA
CREATE TABLE IF NOT EXISTS vacuum_custom (
    id SERIAL PRIMARY KEY,
    val TEXT
);
ALTER TABLE vacuum_custom SET (
    autovacuum_vacuum_scale_factor = 0.01,
    autovacuum_vacuum_threshold = 100
);
INSERT INTO vacuum_custom
SELECT g, repeat('x', 50) FROM generate_series(1, 100000) g
ON CONFLICT DO NOTHING;
UPDATE vacuum_custom SET val = repeat('y', 50) WHERE id <= 30000;
DELETE FROM vacuum_custom WHERE id BETWEEN 30001 AND 50000;

CREATE TABLE IF NOT EXISTS high_churn (
    id SERIAL PRIMARY KEY,
    counter INT DEFAULT 0,
    updated_at TIMESTAMPTZ DEFAULT now()
);
INSERT INTO high_churn (counter)
SELECT 0 FROM generate_series(1, 200000) g
ON CONFLICT DO NOTHING;
UPDATE high_churn SET counter = counter + 1, updated_at = now();
UPDATE high_churn SET counter = counter + 1, updated_at = now();

CREATE TABLE IF NOT EXISTS append_only_log (
    id BIGSERIAL PRIMARY KEY,
    event_type VARCHAR(50),
    payload JSONB,
    created_at TIMESTAMPTZ DEFAULT now()
);
INSERT INTO append_only_log (event_type, payload)
SELECT 'event_' || (g % 10), jsonb_build_object('seq', g)
FROM generate_series(1, 500000) g
ON CONFLICT DO NOTHING;

-- WAL TEST: write-heavy burst
DO $$ BEGIN
    FOR i IN 1..5 LOOP
        UPDATE orders SET notes = 'wal_test_' || i WHERE order_id <= 10000;
    END LOOP;
END $$;

-- BLOAT TEST DATA
CREATE TABLE IF NOT EXISTS severe_bloat (
    id SERIAL PRIMARY KEY,
    data TEXT
);
INSERT INTO severe_bloat
SELECT g, repeat('x', 200) FROM generate_series(1, 200000) g
ON CONFLICT DO NOTHING;
UPDATE severe_bloat SET data = repeat('y', 200) WHERE id <= 150000;
DELETE FROM severe_bloat WHERE id BETWEEN 50001 AND 150000;

CREATE TABLE IF NOT EXISTS tiny_bloat (id INT, val TEXT);
INSERT INTO tiny_bloat VALUES (1, 'a'), (2, 'b')
ON CONFLICT DO NOTHING;
DELETE FROM tiny_bloat WHERE id = 1;

-- Generate pg_stat_statements entries
DO $$ BEGIN
    FOR i IN 1..20 LOOP
        PERFORM * FROM orders ORDER BY total_amount DESC, order_date ASC LIMIT 10000;
    END LOOP;
END $$;

DO $$ BEGIN
    FOR i IN 1..50 LOOP
        PERFORM * FROM products WHERE product_id = i;
    END LOOP;
END $$;

DO $$ BEGIN
    FOR i IN 1..20 LOOP
        PERFORM * FROM orders ORDER BY order_id OFFSET i * 100 LIMIT 100;
    END LOOP;
END $$;

ANALYZE;

SELECT 'Test data loaded. Tables:' AS status;
SELECT relname, n_live_tup, n_dead_tup
FROM pg_stat_user_tables
WHERE schemaname = 'public'
ORDER BY n_live_tup DESC;
