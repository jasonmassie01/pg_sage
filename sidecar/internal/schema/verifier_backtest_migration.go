package schema

// ddlVerifierBacktest (lab-02) creates sage.verifier_backtest: one row per
// completed verifier back-test of this database (internal/verify/backtest),
// the placebo false-positive rate and its Wilson 95% upper bound, the
// inconclusive share and the detection rate on injected 30% drops, with
// the counts behind them and the whole report. lab-01c's verify.method
// auto reads the newest row. database_id is the history store's meta-db
// record id in meta mode, NULL otherwise. Retention keeps 365 days.
//
// CREATE ... IF NOT EXISTS on a new table and its index; lock_timeout
// bounds the catalog locks on the first run.
const ddlVerifierBacktest = `
SET LOCAL lock_timeout = '5s';
CREATE TABLE IF NOT EXISTS sage.verifier_backtest (
    id                 bigserial PRIMARY KEY,
    database_id        bigint,
    ran_at             timestamptz NOT NULL,
    window_from        timestamptz NOT NULL,
    window_to          timestamptz NOT NULL,
    verifier           text NOT NULL CHECK (verifier IN ('v1', 'v2')),
    placebos           integer NOT NULL CHECK (placebos >= 0),
    excluded           integer NOT NULL CHECK (excluded >= 0 AND excluded <= placebos),
    false_positives    integer NOT NULL
                           CHECK (false_positives >= 0 AND false_positives <= placebos),
    fp_rate            float8 NOT NULL CHECK (fp_rate >= 0 AND fp_rate <= 1),
    fp_wilson_upper    float8 NOT NULL CHECK (fp_wilson_upper >= 0 AND fp_wilson_upper <= 1),
    inconclusive_share float8 NOT NULL
                           CHECK (inconclusive_share >= 0 AND inconclusive_share <= 1),
    injected           integer NOT NULL CHECK (injected >= 0 AND injected <= placebos),
    detected           integer NOT NULL CHECK (detected >= 0 AND detected <= injected),
    seed               bigint NOT NULL,
    report             jsonb NOT NULL
);
CREATE INDEX IF NOT EXISTS verifier_backtest_ran_at ON sage.verifier_backtest (ran_at DESC);
`
