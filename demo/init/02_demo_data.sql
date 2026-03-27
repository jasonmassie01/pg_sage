-- ============================================================
-- DEMO DATA: 7 pre-planted problems for pg_sage to find
-- ============================================================

-- 1. CUSTOMERS table — good, no issues (baseline)
CREATE TABLE customers (
    customer_id SERIAL PRIMARY KEY,
    name VARCHAR(100) NOT NULL,
    email VARCHAR(255) UNIQUE NOT NULL,
    status VARCHAR(20) DEFAULT 'active',
    created_at TIMESTAMP DEFAULT now()
);

INSERT INTO customers (name, email, status)
SELECT
    'Customer ' || i,
    'customer' || i || '@example.com',
    CASE WHEN random() < 0.9 THEN 'active' ELSE 'inactive' END
FROM generate_series(1, 50000) i;

-- 2. ORDERS table — MISSING FK INDEX on customer_id
CREATE TABLE orders (
    order_id SERIAL PRIMARY KEY,
    customer_id INT REFERENCES customers(customer_id),
    order_date TIMESTAMP DEFAULT now(),
    total_amount DECIMAL(10,2),
    status VARCHAR(20) DEFAULT 'pending'
);

INSERT INTO orders (customer_id, order_date, total_amount, status)
SELECT
    (random() * 49999 + 1)::int,
    now() - (random() * 365 || ' days')::interval,
    (random() * 500 + 10)::decimal(10,2),
    CASE (random() * 4)::int
        WHEN 0 THEN 'pending'
        WHEN 1 THEN 'shipped'
        WHEN 2 THEN 'delivered'
        ELSE 'cancelled'
    END
FROM generate_series(1, 500000) i;

-- 3. LINE_ITEMS — DUPLICATE INDEXES + UNUSED INDEX
CREATE TABLE line_items (
    item_id SERIAL PRIMARY KEY,
    order_id INT REFERENCES orders(order_id),
    product_name VARCHAR(200),
    quantity INT,
    unit_price DECIMAL(10,2)
);

INSERT INTO line_items (order_id, product_name, quantity, unit_price)
SELECT
    (random() * 499999 + 1)::int,
    'Product ' || (random() * 1000)::int,
    (random() * 10 + 1)::int,
    (random() * 100 + 1)::decimal(10,2)
FROM generate_series(1, 1000000) i;

CREATE INDEX idx_li_order_id ON line_items(order_id);
CREATE INDEX idx_li_order_id_dup ON line_items(order_id);
CREATE INDEX idx_li_product_name ON line_items(product_name);

-- 4. ORDER_EVENTS — NO PRIMARY KEY, NO INDEXES AT ALL
CREATE TABLE order_events (
    order_id INT,
    event_type VARCHAR(50),
    event_data JSONB,
    created_at TIMESTAMP DEFAULT now()
);

INSERT INTO order_events (order_id, event_type, event_data, created_at)
SELECT
    (random() * 499999 + 1)::int,
    CASE (random() * 5)::int
        WHEN 0 THEN 'created'
        WHEN 1 THEN 'payment_received'
        WHEN 2 THEN 'shipped'
        WHEN 3 THEN 'delivered'
        ELSE 'cancelled'
    END,
    jsonb_build_object('source', 'demo', 'seq', i),
    now() - (random() * 365 || ' days')::interval
FROM generate_series(1, 500000) i;

-- 5. AUDIT_LOG — Dead tuples (bloat)
CREATE TABLE audit_log (
    id SERIAL PRIMARY KEY,
    table_name VARCHAR(100),
    action VARCHAR(20),
    old_data JSONB,
    new_data JSONB,
    created_at TIMESTAMP DEFAULT now()
);

INSERT INTO audit_log (table_name, action, old_data, new_data)
SELECT
    CASE (random() * 3)::int
        WHEN 0 THEN 'customers' WHEN 1 THEN 'orders' ELSE 'line_items'
    END,
    CASE (random() * 2)::int WHEN 0 THEN 'UPDATE' ELSE 'INSERT' END,
    '{"before": true}'::jsonb,
    '{"after": true}'::jsonb
FROM generate_series(1, 100000) i;

UPDATE audit_log SET action = 'MODIFIED' WHERE id % 3 = 0;
DELETE FROM audit_log WHERE id % 5 = 0;

-- 6. NEAR-EXHAUSTED SEQUENCE
CREATE SEQUENCE demo_sequence AS INTEGER MAXVALUE 2147483647 START WITH 2147483600;
SELECT nextval('demo_sequence') FROM generate_series(1, 10);

-- 7. PARTITIONED TABLE
CREATE TABLE events (
    id BIGSERIAL,
    event_type VARCHAR(50),
    payload JSONB,
    created_at TIMESTAMP NOT NULL DEFAULT now()
) PARTITION BY RANGE (created_at);

CREATE TABLE events_2024 PARTITION OF events
    FOR VALUES FROM ('2024-01-01') TO ('2025-01-01');
CREATE TABLE events_2025 PARTITION OF events
    FOR VALUES FROM ('2025-01-01') TO ('2026-01-01');
CREATE TABLE events_2026 PARTITION OF events
    FOR VALUES FROM ('2026-01-01') TO ('2027-01-01');

INSERT INTO events (event_type, payload, created_at)
SELECT
    'type_' || (random() * 10)::int,
    jsonb_build_object('demo', true, 'seq', i),
    '2025-01-01'::timestamp + (random() * 365 || ' days')::interval
FROM generate_series(1, 200000) i;

-- ============================================================
-- SLOW QUERY WORKLOAD
-- ============================================================

DO $$
BEGIN
    FOR i IN 1..20 LOOP
        PERFORM count(*) FROM orders WHERE customer_id = (random() * 49999 + 1)::int;
        PERFORM count(*) FROM orders o JOIN customers c ON o.customer_id = c.customer_id
        WHERE c.status = 'active' AND o.total_amount > 100;
        PERFORM count(*) FROM order_events WHERE order_id = (random() * 499999 + 1)::int;
        PERFORM order_id FROM orders ORDER BY order_date DESC LIMIT 100;
        PERFORM count(*) FROM (
            SELECT 1 FROM customers c1 CROSS JOIN customers c2 LIMIT 1000
        ) sub;
        PERFORM count(*) FROM customers WHERE EXTRACT(YEAR FROM created_at) = 2025;
        PERFORM * FROM line_items WHERE order_id = (random() * 499999 + 1)::int LIMIT 10;
        PERFORM count(*) FROM orders WHERE customer_id IN (
            SELECT customer_id FROM customers WHERE status = 'inactive'
        );
    END LOOP;
END $$;

SELECT count(*) FROM orders WHERE customer_id = 12345;
SELECT count(*) FROM orders WHERE customer_id = 67890;
SELECT count(*) FROM orders o JOIN customers c ON o.customer_id = c.customer_id WHERE c.status = 'active' AND o.total_amount > 100;
SELECT count(*) FROM order_events WHERE order_id = 54321;
SELECT order_id FROM orders ORDER BY order_date DESC LIMIT 100;
SELECT count(*) FROM customers WHERE EXTRACT(YEAR FROM created_at) = 2025;
SELECT * FROM line_items WHERE order_id = 12345 LIMIT 10;

ANALYZE;

-- Transfer ownership to sage_agent so executor can CREATE/DROP indexes
ALTER TABLE customers OWNER TO sage_agent;
ALTER TABLE orders OWNER TO sage_agent;
ALTER TABLE line_items OWNER TO sage_agent;
ALTER TABLE order_events OWNER TO sage_agent;
ALTER TABLE audit_log OWNER TO sage_agent;
ALTER TABLE events OWNER TO sage_agent;
ALTER TABLE events_2024 OWNER TO sage_agent;
ALTER TABLE events_2025 OWNER TO sage_agent;
ALTER TABLE events_2026 OWNER TO sage_agent;
ALTER SEQUENCE customers_customer_id_seq OWNER TO sage_agent;
ALTER SEQUENCE orders_order_id_seq OWNER TO sage_agent;
ALTER SEQUENCE line_items_item_id_seq OWNER TO sage_agent;
ALTER SEQUENCE audit_log_id_seq OWNER TO sage_agent;
ALTER SEQUENCE events_id_seq OWNER TO sage_agent;
ALTER SEQUENCE demo_sequence OWNER TO sage_agent;
