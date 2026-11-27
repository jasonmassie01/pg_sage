-- migration-11 fixture: expected verdict review (exit 1) without table
-- statistics: a plain CREATE INDEX blocks writes for the whole build.
CREATE INDEX lint_fixture_orders_note ON lint_fixture_orders (note);
