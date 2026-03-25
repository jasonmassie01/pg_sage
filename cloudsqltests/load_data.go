// +build ignore

package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/jackc/pgx/v5"
)

func main() {
	url := os.Getenv("SAGE_DATABASE_URL")
	if url == "" {
		log.Fatal("SAGE_DATABASE_URL not set")
	}

	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		log.Fatalf("connect: %v", err)
	}
	defer conn.Close(ctx)

	steps := []struct {
		name string
		sql  string
	}{
		{"create customers", `
CREATE TABLE IF NOT EXISTS customers (
    customer_id SERIAL PRIMARY KEY,
    email VARCHAR(255) NOT NULL UNIQUE,
    name VARCHAR(200) NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    status VARCHAR(20) NOT NULL DEFAULT 'active'
)`},
		{"create customers index", `CREATE INDEX IF NOT EXISTS idx_customers_status ON customers (status) WHERE status = 'active'`},
		{"create products", `
CREATE TABLE IF NOT EXISTS products (
    product_id SERIAL PRIMARY KEY,
    sku VARCHAR(50) NOT NULL UNIQUE,
    name VARCHAR(300) NOT NULL,
    price NUMERIC(12,2) NOT NULL CHECK (price > 0),
    category_id INT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
)`},
		{"create order_events", `
CREATE TABLE IF NOT EXISTS order_events (
    order_id INT,
    event_type VARCHAR(50),
    event_data JSONB,
    created_at TIMESTAMPTZ DEFAULT now()
)`},
		{"create orders", `
CREATE TABLE IF NOT EXISTS orders (
    order_id SERIAL PRIMARY KEY,
    customer_id INT NOT NULL REFERENCES customers(customer_id),
    product_id INT NOT NULL REFERENCES products(product_id),
    quantity INT NOT NULL DEFAULT 1,
    total_amount NUMERIC(12,2) NOT NULL,
    order_date TIMESTAMPTZ NOT NULL DEFAULT now(),
    status VARCHAR(20) NOT NULL DEFAULT 'pending',
    shipping_address TEXT,
    notes TEXT
)`},
		{"create line_items", `
CREATE TABLE IF NOT EXISTS line_items (
    line_item_id SERIAL PRIMARY KEY,
    order_id INT NOT NULL REFERENCES orders(order_id),
    product_id INT NOT NULL REFERENCES products(product_id),
    quantity INT NOT NULL,
    unit_price NUMERIC(12,2) NOT NULL,
    discount_pct NUMERIC(5,2) DEFAULT 0
)`},
		{"create line_items indexes", `
CREATE INDEX IF NOT EXISTS idx_li_order ON line_items (order_id);
CREATE INDEX IF NOT EXISTS idx_li_order_product ON line_items (order_id, product_id);
CREATE INDEX IF NOT EXISTS idx_li_product ON line_items (product_id);
CREATE INDEX IF NOT EXISTS idx_li_product_dup ON line_items (product_id);
CREATE INDEX IF NOT EXISTS idx_li_discount ON line_items (discount_pct);
CREATE INDEX IF NOT EXISTS idx_orders_status ON orders (status)`},
		{"create audit_log", `
CREATE TABLE IF NOT EXISTS audit_log (
    log_id SERIAL PRIMARY KEY,
    table_name VARCHAR(100), operation VARCHAR(10),
    old_data JSONB, new_data JSONB, query_text TEXT,
    user_name VARCHAR(100), ip_address INET,
    created_at TIMESTAMPTZ DEFAULT now()
)`},
		{"create ticket_seq", `CREATE SEQUENCE IF NOT EXISTS ticket_seq MAXVALUE 10000 START 9990`},
		{"advance ticket_seq", `SELECT nextval('ticket_seq') FROM generate_series(1,5)`},
		{"create events partitioned", `
CREATE TABLE IF NOT EXISTS events (
    event_id BIGSERIAL, event_type VARCHAR(50) NOT NULL,
    payload JSONB, created_at TIMESTAMPTZ NOT NULL DEFAULT now()
) PARTITION BY RANGE (created_at)`},
		{"create events partitions", `
CREATE TABLE IF NOT EXISTS events_2025_q1 PARTITION OF events FOR VALUES FROM ('2025-01-01') TO ('2025-04-01');
CREATE TABLE IF NOT EXISTS events_2025_q2 PARTITION OF events FOR VALUES FROM ('2025-04-01') TO ('2025-07-01');
CREATE TABLE IF NOT EXISTS events_2025_q3 PARTITION OF events FOR VALUES FROM ('2025-07-01') TO ('2025-10-01');
CREATE TABLE IF NOT EXISTS events_2025_q4 PARTITION OF events FOR VALUES FROM ('2025-10-01') TO ('2026-01-01');
CREATE TABLE IF NOT EXISTS events_2026_q1 PARTITION OF events FOR VALUES FROM ('2026-01-01') TO ('2026-04-01')`},
		{"load customers (50K)", `
INSERT INTO customers (email, name, status)
SELECT 'user'||g||'@example.com', 'Customer '||g,
       CASE WHEN g%20=0 THEN 'inactive' ELSE 'active' END
FROM generate_series(1,50000) g
ON CONFLICT DO NOTHING`},
		{"load products (5K)", `
INSERT INTO products (sku, name, price, category_id)
SELECT 'SKU-'||lpad(g::text,6,'0'), 'Product '||g, (random()*500+1)::numeric(12,2), (g%50)+1
FROM generate_series(1,5000) g
ON CONFLICT DO NOTHING`},
		{"load orders (500K)", `
INSERT INTO orders (customer_id, product_id, quantity, total_amount, order_date, status)
SELECT (random()*49999+1)::int, (random()*4999+1)::int, (random()*10+1)::int,
       (random()*1000+10)::numeric(12,2), now()-(random()*365)::int*interval '1 day',
       (ARRAY['pending','shipped','delivered','returned','cancelled'])[floor(random()*5+1)::int]
FROM generate_series(1,500000)`},
		{"load line_items (1M)", `
INSERT INTO line_items (order_id, product_id, quantity, unit_price, discount_pct)
SELECT (random()*499999+1)::int, (random()*4999+1)::int, (random()*5+1)::int,
       (random()*200+5)::numeric(12,2),
       CASE WHEN random()<0.1 THEN (random()*30)::numeric(5,2) ELSE 0 END
FROM generate_series(1,1000000)`},
		{"load order_events (500K)", `
INSERT INTO order_events (order_id, event_type, event_data, created_at)
SELECT (random()*499999+1)::int,
       (ARRAY['created','updated','shipped','delivered','refunded'])[floor(random()*5+1)::int],
       jsonb_build_object('source','api','version',(random()*3+1)::int),
       now()-(random()*365)::int*interval '1 day'
FROM generate_series(1,500000)`},
		{"load audit_log (100K)", `
INSERT INTO audit_log (table_name, operation, old_data, new_data, query_text, user_name, ip_address)
SELECT (ARRAY['orders','customers','products','line_items'])[floor(random()*4+1)::int],
       (ARRAY['INSERT','UPDATE','DELETE'])[floor(random()*3+1)::int],
       jsonb_build_object('id',g,'data',repeat('x',200)),
       jsonb_build_object('id',g,'data',repeat('y',200)),
       'SELECT '||repeat('column_name, ',50)||' FROM big_table WHERE id = '||g,
       'app_user', ('10.0.'||(g%256)||'.'||(g%256))::inet
FROM generate_series(1,100000) g`},
		{"load events (200K)", `
INSERT INTO events (event_type, payload, created_at)
SELECT (ARRAY['pageview','click','purchase','signup','logout'])[floor(random()*5+1)::int],
       jsonb_build_object('user_id',(random()*50000+1)::int,'page','/page/'||g),
       '2025-01-01'::timestamptz + (random()*450)::int * interval '1 day'
FROM generate_series(1,200000)`},
		{"create dead tuples (orders)", `UPDATE orders SET notes = 'updated' WHERE order_id <= 50000`},
		{"create dead tuples (order_events)", `DELETE FROM order_events WHERE ctid IN (SELECT ctid FROM order_events LIMIT 100000)`},
		{"run slow query workload", `
DO $$
BEGIN
    FOR i IN 1..20 LOOP
        PERFORM count(*) FROM orders WHERE customer_id = i;
        PERFORM * FROM customers c WHERE EXISTS (
            SELECT 1 FROM orders o WHERE o.customer_id = c.customer_id AND o.total_amount > 500
        ) LIMIT 10;
        PERFORM o.order_id, c.name, p.name FROM orders o
            JOIN customers c ON c.customer_id = o.customer_id
            JOIN products p ON p.product_id = o.product_id
            WHERE o.order_date > now() - interval '30 days' LIMIT 100;
        PERFORM * FROM orders ORDER BY total_amount DESC LIMIT 100;
        PERFORM count(*) FROM order_events WHERE order_id = i;
        PERFORM status, count(*), avg(total_amount) FROM orders GROUP BY status;
        PERFORM * FROM customers WHERE customer_id = i;
        PERFORM * FROM products WHERE product_id = i;
    END LOOP;
END $$`},
		{"analyze all tables", `ANALYZE`},
	}

	for i, step := range steps {
		start := time.Now()
		fmt.Printf("[%d/%d] %s... ", i+1, len(steps), step.name)
		_, err := conn.Exec(ctx, step.sql)
		if err != nil {
			fmt.Printf("ERROR: %v\n", err)
			// Continue on non-fatal errors (e.g. partition already exists)
			continue
		}
		fmt.Printf("OK (%.1fs)\n", time.Since(start).Seconds())
	}

	// Verify row counts
	fmt.Println("\n=== Row Count Verification ===")
	tables := []string{"customers", "products", "orders", "line_items", "order_events", "audit_log", "events"}
	for _, t := range tables {
		var count int64
		err := conn.QueryRow(ctx, "SELECT count(*) FROM "+t).Scan(&count)
		if err != nil {
			fmt.Printf("  %s: ERROR %v\n", t, err)
			continue
		}
		fmt.Printf("  %s: %d\n", t, count)
	}

	// Verify pg_stat_statements
	var ssCount int64
	err = conn.QueryRow(ctx, "SELECT count(*) FROM pg_stat_statements WHERE queryid IS NOT NULL").Scan(&ssCount)
	if err != nil {
		fmt.Printf("\n  pg_stat_statements: ERROR %v\n", err)
	} else {
		fmt.Printf("\n  pg_stat_statements entries: %d\n", ssCount)
	}

	fmt.Println("\nPhase 1 data loading complete.")
}
