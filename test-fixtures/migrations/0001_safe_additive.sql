-- migration-11 fixture: expected verdict safe (exit 0).
SET lock_timeout = '5s';
CREATE TABLE lint_fixture_orders (id bigint, customer_id bigint, note text);
CREATE INDEX CONCURRENTLY lint_fixture_orders_customer ON lint_fixture_orders (customer_id);
