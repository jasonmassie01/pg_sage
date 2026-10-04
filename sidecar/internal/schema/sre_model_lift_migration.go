package schema

// Roadmap 2.4 (2026-10-04) "measure the model": a stored PGIncidentBench
// report keeps its held-out model lift (the model mode, whether a live
// run hit its budget, and per family the override precision,
// inconclusive-case lift and Safe Pass against the causal graph), from
// which the ledger decides each family's model-root authority. NULL for
// reports without one (every report stored before). Additive and
// idempotent on top of ddlSREBenchProvenance.
const ddlSREModelLift = `
ALTER TABLE sage.sre_eval_runs ADD COLUMN IF NOT EXISTS model_lift jsonb;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                   WHERE conname = 'sre_eval_runs_model_lift_object'
                     AND conrelid = 'sage.sre_eval_runs'::regclass) THEN
        ALTER TABLE sage.sre_eval_runs
            ADD CONSTRAINT sre_eval_runs_model_lift_object
            CHECK (model_lift IS NULL OR jsonb_typeof(model_lift) = 'object');
    END IF;
END $$;
`
