WITH d AS (DELETE FROM sb_fixture.ledger RETURNING id) SELECT count(*) FROM d
