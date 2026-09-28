package schema

// ddlExplainCachePlanHash stores the stable plan fingerprint
// (internal/planhash) of each captured plan. query_store samples copy
// the latest non-null fingerprint for their queryid, so a plan flip is
// visible in the per-queryid time series (Sage SRE M0). Rows captured
// before this migration keep NULL (unknown plan shape).
const ddlExplainCachePlanHash = `
ALTER TABLE sage.explain_cache
    ADD COLUMN IF NOT EXISTS plan_hash text;
`
