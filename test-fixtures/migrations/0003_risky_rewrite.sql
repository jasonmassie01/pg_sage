-- migration-11 fixture: expected verdict risky (exit 2): a type change that
-- rewrites the table under ACCESS EXCLUSIVE.
ALTER TABLE lint_fixture_orders ALTER COLUMN customer_id TYPE numeric(20,0);
