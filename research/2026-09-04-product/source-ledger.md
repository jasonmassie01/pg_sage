# Claim-to-source ledger

All sources accessed 2026-09-04. "Undated" means publication/update date was not
established from the visible page; crawl age is not treated as publication date.
Links below preserve source provenance for independent verification.

| Claim family | Title / publisher | Date | URL | Confidence / limitation |
|---|---|---|---|---|
| Product intent | Autonomous DBA Product Spec / local repository | 2026-04-27 | [Local source](../../specs/autonomous-dba-product-spec-2026-04-27.md) | High intent; not capability proof |
| Existing prescribed autonomy | Agent-Native Autonomy Build Spec / local repository | 2026-07-22 | [Local source](../../specs/agent-native-autonomy-build-spec.md) | High intent, checked against package code |
| Current latency verification | verify/evaluate.go / local repository | HEAD 2447ef2 | [Local source](../../sidecar/internal/verify/evaluate.go) | High current code, feature-specific validation delegated |
| Filtered vector search failure | With supabase...too many documents...no results / Stack Overflow | 2024-01-13 | https://stackoverflow.com/questions/77812490/with-supabase-i-am-doing-a-pg-vector-search-and-when-i-have-too-many-documents | Medium pain evidence; unanswered, root cause unresolved |
| Storage growth despite vacuum | Something peculiar...table keeps growing / Reddit | Relative page date 2 years ago | https://www.reddit.com/r/PostgreSQL/comments/1dpbl95/something_peculiar_table_keeps_growing_on_disk/ | Anecdotal; replies are hypotheses |
| Slot disk exhaustion | WAL slots filled storage during replication / Reddit | Relative page date about 2022 | https://www.reddit.com/r/PostgreSQL/comments/y2si1q/wal_slots_filled_storage_during_replication/ | Anecdotal, corroborated mechanism in official docs |
| Slot retention tradeoff | Log-Shipping Standby Servers / PostgreSQL | Current v18 docs, undated | https://www.postgresql.org/docs/current/warm-standby.html | High mechanism; not a safe slot-deletion prescription |
| Workload index portfolio | Index Advisor: Getting Started / pganalyze | Undated | https://pganalyze.com/docs/index-advisor/getting-started | High published capability; vendor incentive |
| Thin clones/lifecycle | DBLab Engine / PostgresAI | Undated | https://postgres.ai/docs/database-lab | High product mechanism; vendor performance claims not adopted |
| Managed PG index advisor | Index advisor overview / Google Cloud | Undated | https://docs.cloud.google.com/alloydb/docs/index-advisor-overview | High documented service; exact pricing unverified |
| Managed anomaly advice | Analyzing performance anomalies / AWS RDS | Undated | https://docs.aws.amazon.com/AmazonRDS/latest/UserGuide/devops-guru-for-rds.html | High documented scope; provider-specific |
| Automatic tuning prior art | Automatic Tuning Overview / Microsoft | Undated | https://learn.microsoft.com/en-us/azure/azure-sql/database/automatic-tuning-overview?view=azuresql | High general capability |
| Validated index lifecycle | Automatically Indexing Millions of Databases / Microsoft Research | SIGMOD 2019 | https://www.microsoft.com/en-us/research/wp-content/uploads/2019/02/autoindexing_azuredb.pdf | High described architecture; not a present-day feature availability matrix |
| Invisible index verification | The Optimizer in Oracle Database 19c / Oracle | 2019 | https://www.oracle.com/technetwork/database/bi-datawarehousing/twp-optimizer-with-oracledb-19c-5324206.pdf | High prior-art mechanism; current packaging not priced |
| HNSW knobs/filtering/quality | pgvector README / pgvector maintainers | Current fetched README, installs 0.8.6 | https://github.com/pgvector/pgvector | High mechanism; local tested version is 0.8.2 |
| DiskANN separate surface | pgvectorscale README / Timescale | Current fetched README, undated | https://github.com/timescale/pgvectorscale | High documented mechanism; benchmark claims not generalized |
| Existing HNSW autotuner | lantern_extras README / Lantern | Undated | https://github.com/lanterndata/lantern_extras#Index-Autotune | High prior art; invalidates universal novelty claim |
| Filter-aware ANN research | ACORN / original authors on arXiv | 2024 | https://arxiv.org/abs/2403.04871 | High research existence; no reproduction here |
| Filter/plan-dependent quality | Filtered ANN Search... / original authors on arXiv | 2026 | https://arxiv.org/abs/2602.11443 | Medium emerging preprint; results bounded to its evaluation |
| Branch resource accounting | synthetic-size.md / Neon engineering | Undated | https://github.com/neondatabase/neon/blob/main/docs/synthetic-size.md | High CoW semantics; no dollar estimate |

## Gap reconciliation

- Universal novelty: contradicted by Lantern and cross-engine prior art; removed.
- July MCP absence: contradicted by current code; described as historical.
- Why the Stack Overflow query fails: not established; no single-cause assertion.
- Exact competitor pricing: not needed for feature decision; no invented numbers.
- Production vector quality: requires representative LifeOS/application workload;
  synthetic fixture validates mechanics only.
- Clone/restore trust: existing adapters do not certify any particular deployed
  provider; future experiments require cost/authority and teardown evidence.

## Search log and stopping rule

Discovery searched pgvector filtering/recall on Reddit and Stack Overflow,
autovacuum and slot disk incidents, pganalyze index overhead, Azure verification,
Oracle automatic indexing, AlloyDB/AWS advisors, PostgresAI clones and vector prior
art. Follow-up opened original support threads, official extension READMEs,
provider docs and original papers; it checked current repository specs and code.
The Neon docs open failed once; original public engineering documentation filled
that factual gap. Repeated generic search variants were stopped after the first
slice had primary support and all material uncertainties were explicit.
