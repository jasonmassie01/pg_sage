# Competitive analysis: databases for agents, and agents that run databases

**As of:** 2026-10-05. **Feeds:** `reviews/2026-10-05-agentdb/AGENTDB-SPEC.md`.
**Scope:** agent-native Postgres platforms, adjacent agent data stores, distribution channels,
hyperscalers, AI DBA and database-operations tools, agent access governance, agent memory/state.

**Method.** Six parallel research passes (platforms A and B, hyperscalers, AI DBA tools, memory,
governance) plus direct verification by the author on 2026-10-05. The session's shared web-search
quota (200 queries) ran out part-way through, so most facts were confirmed by fetching primary
pages: vendor docs, changelogs, pricing pages, release notes, press releases, SEC filings and the
GitHub REST API. The claims the verdict leans on hardest were re-fetched and confirmed by the
author (Supabase/Turso, Prisma agent approvals, Genie ZeroOps, Lakebase autonomous operations,
the AWS MCP CVE bulletin, the Cloud SQL MCP tool list, Xata MCP 2.0, Replit–Databricks).

**Conventions.**
- Every factual bullet carries a link and a date. The date is the source's publication or update
  date; `acc.` marks an undated page accessed on 2026-10-05.
- `UNVERIFIED` marks a claim that could not be confirmed from a primary or reputable source.
- `Analysis:` marks the author's inference. Everything else is a sourced fact.
- Quotes are rare, under 15 words, and attributed. Everything else is paraphrased.
- GitHub stars, licenses, last-push dates, release dates and archive flags come from the GitHub
  REST API on 2026-10-05 unless stated.
- pg_sage facts cite files in this repository (branch `claude/agentdb-spec`).
- The map tables in §1 summarize facts that are cited in the §2 profile named in their last column.

---

## 0. Ten findings that matter for pg_sage

1. **Agents now create most new databases on the developer platforms.** On Neon, agents create 80%
   of databases and 97% of branches ([Databricks](https://www.databricks.com/blog/enterprise-ai-agent-trends-top-use-cases-governance-evaluations-and-more),
   2026-01-27). Supabase said over 60% of new databases came from AI tools
   ([Supabase Series F](https://supabase.com/blog/supabase-series-f), 2026-06-04), then 70% of
   4M+ new databases a month ([PR Newswire](https://www.prnewswire.com/news-releases/supabase-announces-150m-in-new-funding-and-turso-acquisition-302896752.html),
   2026-10-02). Lakebase reports 12M database launches a day
   ([Databricks LTAP release](https://www.databricks.com/company/newsroom/press-releases/databricks-launches-ltap-first-lake-transactionalanalytical),
   2026-06-16). Analysis: the buyer of "a database for an agent" is increasingly the agent
   platform (Replit, Lovable, Bolt, v0, Claude Code), not a DBA.
2. **The layer is consolidating fast.** Databricks bought Neon (about $1B,
   [CNBC](https://www.cnbc.com/2025/05/14/databricks-is-buying-database-startup-neon-for-about-1-billion.html),
   2025-05-14) and took the Electric team
   ([Electric](https://electric.ax/blog/2026/08/11/electric-joining-databricks), 2026-08-11).
   Snowflake bought Crunchy Data ([Snowflake](https://www.snowflake.com/en/news/press-releases/snowflake-acquires-crunchy-data-to-bring-enterprise-ready-postgres-offering-to-the-ai-data-cloud/),
   2025-06-02). Supabase is acquiring Turso (PR Newswire, 2026-10-02). The InstantDB team joined
   OpenAI and Instant Cloud is closing ([Instant](https://www.instantdb.com/essays/instant_team_joins_openai),
   2026-08-22). Temporal bought Crystal DBA, maker of Postgres MCP Pro
   ([Temporal](https://temporal.io/blog/temporal-and-the-next-frontier-scaling-ai-reliably),
   2025-09-03). Governance consolidated too: Delinea closed StrongDM
   ([SecurityMEA](https://securitymea.com/2026/03/06/delinea-completes-the-acquisition-of-strongdm/),
   2026-03-06), Cisco is acquiring Astrix
   ([Astrix](https://astrix.security/learn/blog/a-new-chapter-astrix-security-is-joining-cisco/),
   2026-05-04), and Cyera bought Oasis
   ([Cyera](https://www.cyera.com/blog/identity-meets-data-defining-the-ai-trust-layer-for-the-agentic-enterprise),
   2026-09-01).
3. **Copy-on-write (CoW) branching is a commodity, and the open-source options are no longer
   Neon.** Xata open-sourced its core platform under Apache-2.0 (repo
   [xataio/xata](https://github.com/xataio/xata) created 2026-04-15). It keeps its storage engine
   and multi-tenant security features closed. Vela open-sourced its
   platform ([Vela](https://vela.run/blog/vela-open-source/), 2026-02-09). DBLab ships thin clones
   under Apache-2.0 ([v4.2.0](https://github.com/postgres-ai/database-lab-engine/releases/tag/v4.2.0),
   2026-09-11). Aurora clones are CoW
   ([AWS docs](https://docs.aws.amazon.com/AmazonRDS/latest/AuroraUserGuide/Aurora.Managing.Clone.html),
   acc.), and Cloud SQL fast clone went GA
   ([Cloud SQL release notes](https://docs.cloud.google.com/sql/docs/postgres/release-notes),
   2026-01-15). Public commits to `neondatabase/neon` fell from 746 in Q1 2025 to 1 in Q4 2025
   and 5 in 2026 so far (GitHub API, 2026-10-05). Analysis: self-hosting Neon is no longer a
   realistic path; Xata OSS and DBLab are.
4. **"The agent provisions, a human claims it later" is the standard pattern.** Examples: Neon
   Claimable Postgres with a 72-hour expiry
   ([Neon](https://neon.com/blog/an-agent-provisions-a-neon-backend-a-human-claims-it-later),
   2026-09-10); Prisma `npx create-db` with a 24-hour TTL
   ([Prisma](https://www.prisma.io/blog/give-your-agent-a-database), 2026-07-09); Aurora
   PostgreSQL express configuration in seconds
   ([AWS](https://aws.amazon.com/about-aws/whats-new/2026/03/amazon-aurora-postgresql-database),
   2026-03-25); and Google's managed Cloud SQL MCP server, whose tools include `create_instance`
   and `clone_instance` ([Google docs](https://docs.cloud.google.com/sql/docs/postgres/use-cloudsql-mcp),
   acc.; GA 2026-04-16 per release notes).
5. **In 2026, approval gates and separate agent identities became table stakes.**
   - Prisma: agents enroll with a human-approved pairing code; production writes, deletes and
     new connection strings need approval; the audit log names the agent
     ([Prisma changelog](https://www.prisma.io/changelog/2026-10-02), 2026-10-02).
   - Supabase: MCP confirmation prompts and scoped tokens
     ([Supabase](https://supabase.com/blog/select-2026-operate-with-confidence), 2026-10-02).
   - Xata: writes need both `write=true` and `confirm=true`
     ([Xata](https://xata.io/blog/introducing-the-xata-mcp-server), 2026-08-26).
   - PlanetScale: DDL needs human confirmation
     ([PlanetScale MCP docs](https://planetscale.com/docs/connect/mcp), acc.).
   - Replit with Databricks: database changes need team approval
     ([Replit](https://replit.com/blog/databricks2026), 2026-09-10).

   Analysis: pg_sage's request, policy and approval flow is no longer new as a concept. It has
   to win on depth: any Postgres, enforcement on the server side, evidence, and reversibility.
6. **Server-side policy at the SQL level is still rare and commercial.** Only Formal (Rego over
   parsed statements, masking, row filters;
   [Formal docs](https://docs.formal.ai/docs/guides/policies/enforcement.md), acc.) and
   StrongDM/Delinea (Cedar over 180+ Postgres actions, approvals;
   [StrongDM docs](https://docs.strongdm.com/admin/access/policies/policy-taxonomy.md), acc.)
   document it (§2.6). MCP gateways authorize by tool name, and none parses SQL (§2.6).
   Client-side "read-only" guards keep failing:
   - AWS Labs `postgres-mcp-server`: CVE-2026-85787
     ([AWS](https://aws.amazon.com/security/security-bulletins/2026-101-aws/), 2026-09-04) and
     CVE-2026-87911 ([AWS](https://aws.amazon.com/security/security-bulletins/2026-104-aws/),
     2026-09-09).
   - Anthropic's archived reference server
     ([Datadog Security Labs](https://securitylabs.datadoghq.com/articles/mcp-vulnerability-case-study-SQL-injection-in-the-postgresql-mcp-server/),
     2025-08-21).
   - Microsoft's new `postgres-mcp` is read-write by default
     ([Microsoft](https://techcommunity.microsoft.com/blog/adforpostgresql/postgres-mcp-server-connect-ai-coding-agents-to-postgresql/4561273),
     2026-10-01).
7. **AI DBA autonomy is arriving inside walled gardens, not as open tools.**
   - EDB's "agentic database" runs only in Hybrid Manager
     ([EDB](https://www.enterprisedb.com/blog/inside-agentic-database-how-edb-turned-postgres-self-managing-system),
     2026-06-30).
   - Databricks says Lakebase autonomous operations are available now, for Lakebase only
     ([Databricks](https://www.databricks.com/company/newsroom/press-releases/databricks-launches-ltap-first-lake-transactionalanalytical),
     2026-06-16). Genie ZeroOps is in private preview with Lakebase only on its roadmap
     ([Databricks](https://www.databricks.com/blog/introducing-genie-zeroops), 2026-06-16).
   - Azure autonomous tuning only reports
     ([Microsoft Learn](https://learn.microsoft.com/en-us/azure/postgresql/monitor/concepts-autonomous-tuning),
     2026-07-13). Google Database Center assessments are in preview and also only report
     ([Google](https://docs.cloud.google.com/database-center/docs/assessments), acc.).
   - The open-source field thinned: Xata Agent was archived on 2026-06-15
     ([GitHub](https://github.com/xataio/agent), acc.), Postgres MCP Pro stalled after its company
     was acquired ([issue #192](https://github.com/crystaldba/postgres-mcp/issues/192),
     2026-07-30), and pgai was archived ([GitHub](https://github.com/timescale/pgai), 2026-05-27).
   - pgEdge's AI DBA Workbench is the main new open-source entrant, and it is read-only
     ([pgEdge](https://www.pgedge.com/blog/introducing-the-ai-dba-workbench-postgresql-monitoring-that-diagnoses-not-just-reports),
     2026-04-22).
8. **Agent memory and state create real DBA work on Postgres.**
   - LangGraph checkpoints grow without bound: 224 GB and 3.8M rows on one instance
     ([aegra PR](https://github.com/aegra/aegra/pull/355), 2026-05-07). The open-source savers
     have no prune ([langgraph #8531](https://github.com/langchain-ai/langgraph/issues/8531),
     2026-08-05).
   - pgvector fixed HNSW vacuum bugs in 0.8.3 and 0.8.4, and an IVFFlat build overflow in 0.8.7
     ([changelog](https://github.com/pgvector/pgvector/blob/master/CHANGELOG.md), 2026-06 to
     2026-10-01).
   - Frameworks churn: Letta retired its V1 server
     ([Letta](https://www.letta.com/blog/our-next-phase/), 2026-03-16), and mem0 removed its
     graph stores ([mem0 changelog](https://docs.mem0.ai/changelog), 2026-04-14).

   Analysis: running memory stores well is the most natural "databases agents use" job for an
   AI DBA.
9. **Hyperscalers are converging on managed MCP, fast clones and generalist SRE agents, each
   inside one cloud.**
   - AWS: the managed AWS MCP Server went GA
     ([AWS](https://aws.amazon.com/blogs/aws/the-aws-mcp-server-is-now-generally-available/),
     2026-05-06). AgentCore Policy (Cedar, enforced on tool calls) went GA
     ([AWS](https://aws.amazon.com/about-aws/whats-new/2026/03/policy-amazon-bedrock-agentcore-generally-available/),
     2026-03-03). AWS DevOps Agent went GA at about $30 per agent-hour
     ([AWS](https://aws.amazon.com/blogs/mt/announcing-general-availability-of-aws-devops-agent/),
     2026-03-31).
   - Google: managed MCP servers for Cloud SQL and AlloyDB went GA on 2026-04-16 and 2026-04-20
     ([Cloud SQL release notes](https://docs.cloud.google.com/sql/docs/postgres/release-notes),
     [AlloyDB release notes](https://docs.cloud.google.com/alloydb/docs/release-notes), acc.).
   - Microsoft: HorizonDB entered preview
     ([Microsoft Learn](https://learn.microsoft.com/en-us/azure/horizondb/release-notes/release-notes),
     2026-06-02).

   None of them documents one plane that provisions under governance and operates any
   Postgres across clouds.
10. **pg_sage's binding constraint is distribution, not features.**
    - On 2026-10-05, pg_sage had 13 GitHub stars. MCP Toolbox had 16.6k, Bytebase 14.5k,
      Postgres MCP Pro 3.4k, DBLab 2.8k and pgEdge's Workbench 48 (GitHub API).
    - The module name "AgentDB" collides with [agentdb.dev](https://agentdb.dev/) (acc.), a live
      product that serves per-agent SQLite and DuckDB databases.

---

## 1. Landscape map by layer

```text
 WHO BRINGS THE AGENTS (distribution): Claude Code, Codex, Cursor, Replit, Lovable, Bolt, v0,
 Vercel, Databricks Apps/Agent Bricks, Cloudflare Agents, AWS AgentCore, Azure Foundry, Vertex
+---------------------------------------------------------------------------------------------+
| E  MEMORY / STATE   LangGraph checkpointers+store, Letta, mem0, Zep/Graphiti, ADK sessions,   |
|                     OpenAI Agents SDK sessions, DBOS, Electric Durable Streams; hosted:      |
|                     AgentCore Memory, Vertex Memory Bank, Foundry Memory, Agent Bricks memory |
+---------------------------------------------------------------------------------------------+
| D  OPERATIONS /     advise: pganalyze, Datadog DBM, postgres.ai, pgEdge Workbench, Azure      |
|    AI DBA / AI SRE  tuning, Google Database Center | act: DBtune, EDB agentic DB, Lakebase    |
|                     autonomous ops, Genie ZeroOps (preview) | generalist SRE: AWS DevOps Agent, |
|                     Azure SRE Agent, Gemini Cloud Assist, Bits, PagerDuty | [pg_sage core]    |
+---------------------------------------------------------------------------------------------+
| C  ACCESS /         SQL-aware proxies: Formal, StrongDM/Delinea | identity: Teleport, Entra     |
|    GOVERNANCE       Agent ID, Okta, Aembit, Vault, SPIFFE | MCP gateways (tool-level): Docker, |
|                     Cloudflare, Pomerium, Kong, agentgateway, APIM | change: Bytebase, Atlas,  |
|                     Liquibase | in-platform: read-only MCP modes, confirmation prompts        |
+---------------------------------------------------------------------------------------------+
| B  PROVISIONING     Neon API + Claimable, Supabase Management API, Prisma create-db, Xata     |
|    CONTROL PLANE    API/CLI, Lakebase REST/Terraform, Vercel Marketplace, Google Cloud SQL    |
|                     MCP, Aurora express config, AWS MCP Server | [pg_sage AgentDB]             |
+---------------------------------------------------------------------------------------------+
| A  STORAGE /        CoW: Neon/Lakebase, Xata, Tiger Fluid Storage, Vela, Aurora clones,       |
|    BRANCHING        Cloud SQL fast clone, DBLab | logical/per-instance: Supabase, PlanetScale, |
|                     Prisma, Snowflake Postgres, HorizonDB | embedded: Turso/AgentFS, Durable   |
|                     Object SQLite, D1, PGlite | versioned engines: Doltgres, Git4Data (research)|
+---------------------------------------------------------------------------------------------+
```

### 1.1 Layer A: storage and branching substrate

| Player | Architecture class | What agents get | Status | Profile |
|---|---|---|---|---|
| Neon / Lakebase | Separated storage, CoW at log position | Instant branches, scale to zero | GA (Lakebase GA 2026-01-22) | §2.1.1, §2.1.2 |
| Xata (cloud and OSS) | CoW block storage over NVMe-oF | Branches in 1–2 s; 1,000 short branches for about $1 | Cloud GA; OSS since 2026-04 | §2.1.5 |
| Tiger Data | Fluid Storage, CoW volumes | Zero-copy forks | Ghost (agent product) winding down | §2.1.4 |
| Vela (simplyblock) | NVMe-oF CoW, VM per branch | Independent Postgres per branch | OSS since 2026-02-09 | §2.1.10 |
| Aurora | Storage-level CoW clones | Up to 15 CoW clones per source | GA | §2.4.1 |
| Cloud SQL | Instant snapshots, same zone | Fast clone | GA 2026-01-15 | §2.4.2 |
| DBLab (postgres.ai) | ZFS/LVM thin clones on a dedicated host | Clones of any source Postgres | Apache-2.0, v4.2.0 | §2.5.1 |
| Supabase | Instance per project; branches hold schema and seed | Preview branches; data copy optional | Branching docs still say alpha | §2.1.3 |
| PlanetScale Postgres | Instance per branch; empty or restored from backup | Dev branches from $5/month | GA 2025-09-22 | §2.1.6 |
| Prisma Postgres | Unikernel microVM per database | Throwaway databases with a TTL | GA 2025-02-03 | §2.1.7 |
| Turso / AgentFS | One SQLite file per database or agent | Millions of tiny databases; file snapshots | Being acquired by Supabase | §2.2.1 |
| Cloudflare | SQLite inside each Durable Object | A database per agent instance | GA | §2.2.8 |
| Doltgres | Version-controlled engine on the Postgres wire protocol | Branch, merge, diff of data | 1.0 on 2026-08-06 | §2.2.6 |

Layer read-out:
- CoW is cheap and fast everywhere that owns its storage. Xata prices it at about $1 per 1,000
  five-minute branches ([Xata](https://xata.io/blog/a-thousand-postgres-branches-for-1),
  2026-06-11), and Neon charges $1.50 per branch-month beyond the included branches
  ([Neon pricing](https://neon.com/pricing), acc.). Analysis: no one should build a new storage
  engine to compete here.
- On existing hyperscaler estates, CoW comes with limits. Aurora allows 15 CoW clones per source
  before it falls back to full copies (AWS docs, acc.). Cloud SQL fast clone works in the same
  zone only (Google docs, acc.). No AlloyDB fast-clone entry was found (UNVERIFIED absence, §2.4.2).
- Merge semantics are rare. Doltgres 1.0 can merge data, but runs about 2.7× slower than
  Postgres and lacks vector indexes and RLS
  ([DoltHub](https://www.dolthub.com/blog/2026-08-06-doltgres-1-0/), 2026-08-06). Research such
  as Git4Data adds branch, diff and merge with conflict policies
  ([arXiv 2609.02106](https://arxiv.org/abs/2609.02106), 2026-09-02).

### 1.2 Layer B: provisioning control plane

| Player | Interface | Claim/TTL | Approvals | Status | Profile |
|---|---|---|---|---|---|
| Neon | API, MCP, Claimable Postgres | 72 h unclaimed expiry | n/d | GA | §2.1.1 |
| Supabase | Management API, MCP, Platform Kit | Claim/transfer for platforms | MCP confirmation prompts | GA (MCP prompts 2026-10-02) | §2.1.3 |
| Prisma Postgres | create-db, Management API, MCP | 30 min to 24 h TTL plus claim URL | Agent enrollment; human approval for production | GA 2026-10-02 | §2.1.7 |
| Xata | API, CLI, MCP 2.0, GitHub App | `xata scratch` disposable branches | `confirm=true` on writes | GA | §2.1.5 |
| Lakebase | REST API, CLI, SDK, Terraform | n/d | Unity Catalog; Replit flow needs team approval | API GA 2026-08-14 | §2.1.2 |
| Vercel Marketplace | `vercel install` CLI, v0 | n/d | None database-specific | GA | §2.3.1 |
| Google Cloud SQL | Managed MCP (`create_instance`, `clone_instance`) | n/d | IAM only | GA 2026-04-16 | §2.4.2 |
| AWS | Aurora express configuration; AWS MCP Server `call_aws` | n/d | IAM; AgentCore Policy if used | GA | §2.4.1 |
| pg_sage AgentDB | REST, no MCP tools | Leases and cleanup claim | Request policy, admin approval, live authorization | Local executed; cloud gated | §3, `docs/agent-db-deployments.md` |

Layer read-out:
- Every serious platform now ships provisioning over MCP. pg_sage does not: `sidecar/internal/mcp`
  contains no AgentDB tools (grep, 2026-10-05), so agents must call
  `/api/v1/agent-api/...` over REST (`docs/agent-db-deployments.md:86-94`).
- Approval semantics are converging on one rule: an agent works freely in previews and branches,
  and a human approves anything touching production, deletion or spend (Prisma, Supabase, Xata,
  PlanetScale, Replit). pg_sage's design is stricter (plan hash, cost estimate and policy
  generation are bound into a single-use authorization, `docs/agent-db-deployments.md:258-275`),
  but nobody can call it through MCP.

### 1.3 Layer C: agent access and governance

| Mechanism | Who | Enforcement point | SQL-aware? | Profile |
|---|---|---|---|---|
| Wire-protocol proxy with SQL policy | Formal; StrongDM/Delinea | In the data path | Yes | §2.6.1, §2.6.2 |
| Identity-aware access and session recording | Teleport | Connection | No; tables only | §2.6.3 |
| Database activity monitoring, masking | Varonis (Cyral), Commvault (Satori), Immuta | Data path or native push-down | Monitoring, masking, row filters | §2.6.4 |
| Agent and non-human identity | Entra Agent ID, Okta, Aembit, Astrix (Cisco), Oasis (Cyera), Idira (PANW) | Token issuance | No | §2.6.5 |
| Dynamic database credentials | Vault; PG18 OAuth | Postgres login | No (grants only) | §2.6.5 |
| MCP gateways | Docker, Cloudflare, Pomerium, Kong, agentgateway, APIM, ContextForge and others | Tool call | No | §2.6.6 |
| Agent-platform policy | AgentCore Policy (Cedar), Azure SRE Agent run modes | Tool call | No | §2.4.1, §2.4.3 |
| Change governance | Bytebase, Atlas, Liquibase | Migration pipeline | Yes, for proposed changes | §2.5.10–§2.5.12 |
| In-platform guards | Read-only MCP modes, confirmation prompts | MCP server | Partial (PlanetScale blocks DML without WHERE) | §2.1 |

Layer read-out: identity and tool-level control are crowded and consolidating. SQL-level control is
held by two commercial proxies and the change-management tools. Nobody offers open-source
enforcement for agent traffic that is native to Postgres (per-agent roles, `VALID UNTIL`,
statement timeouts, RLS defaults, audit tags) on any Postgres (§2.6, §3B).

### 1.4 Layer D: operations, AI DBA and AI SRE

| Mode | Players | Scope | Profile |
|---|---|---|---|
| Advise only | pganalyze, Datadog DBM, postgres.ai, pgEdge Workbench, Azure autonomous tuning, Google Database Center, Aiven optimizer | Mostly any Postgres; Azure and Google only on their own clouds | §2.5 |
| Act with gates | DBtune (parameters), Bytebase (approved DDL), EDB agentic database, Lakebase autonomous operations | Mostly own platform only (EDB, Lakebase) | §2.5, §2.1.2 |
| Sandbox-validated fixes | Genie ZeroOps (private preview); Google assessments (report only) | Databricks; Cloud SQL | §2.1.2, §2.4.2 |
| Generalist AI SRE | AWS DevOps Agent, Azure SRE Agent, Gemini Cloud Assist, Datadog Bits, PagerDuty, Resolve, Cleric, Traversal | Incidents, not Postgres maintenance | §2.4, §2.5.13 |
| Dead or pivoted | OtterTune (2024), Xata Agent (archived), Tembo (pivot), pgai (archived), Metis (likely gone) | — | §2.5.14 |

Layer read-out: no product ships, for any Postgres, autonomy that is open source, self-hosted,
evidence-backed and reversible. That is pg_sage's lane. It is open because two OSS attempts died
and the commercial ones are locked to their platforms, not because the idea is unclaimed (§4).

### 1.5 Layer E: memory and state

| Store | Postgres role | Notable 2026 facts | Profile |
|---|---|---|---|
| LangGraph checkpointer and store | Primary (JSONB, BYTEA, pgvector) | No prune in OSS; TTL only on Agent Server | §2.7.4 |
| mem0 | One of about 26 vector backends | OSS graph stores removed 2026-04-14; pgvector adapter bugs | §2.7.3 |
| Letta | V1 server needed Postgres and pgvector | V1 retired; memory moved to git-backed files | §2.7.1 |
| Zep / Graphiti | None (Neo4j, FalkorDB, Neptune) | Postgres requests closed as not planned | §2.7.2 |
| ADK, OpenAI Agents SDK | Session tables via SQL databases | Append-heavy; row locks; schema migrations | §2.7.5 |
| AgentCore Memory, Memory Bank, Foundry Memory | Managed (not your Postgres) | AgentCore memory repriced 2026-10-06 | §2.7.7 |
| Lakebase (Agent Bricks memory) | Managed Postgres | Managed agent memory on Lakebase | §2.1.2 |

### 1.6 Substrate changes that affect every layer

- PostgreSQL 18 (2025-09-25) added OAuth 2.0 authentication through validator modules and
  deprecated MD5 passwords ([PostgreSQL](https://www.postgresql.org/about/news/postgresql-18-released-3142/),
  2025-09-25). Analysis: short-lived agent tokens can now reach Postgres itself, not just a proxy.
- PostgreSQL 19 Beta 4 shipped on 2026-09-24, and GA is expected in October 2026. It includes
  `REPACK`, `pg_plan_advice` and better autovacuum scoring
  ([PostgreSQL](https://www.postgresql.org/about/news/postgresql-19-beta-4-released-3386/),
  2026-09-24). Analysis: these change what an AI DBA can do natively on bloat and plan stability.
- Extension security matters across fleets.
  - pgvector 0.8.7 fixes a critical IVFFlat index-build buffer overflow, listed as
    CVE-2026-103484 ([PostgreSQL news](https://www.postgresql.org/about/news/pgvector-087-released-3392/),
    2026-10-05; changelog date 2026-10-01).
  - PgBouncer 1.26.0 fixes three CVEs ([PostgreSQL news](https://www.postgresql.org/about/news/pgbouncer-1260-released-fixes-three-cves-3385/),
    2026-09-23).
  - Analysis: agent-created fleets inherit whatever extension versions were current at birth.
- MCP spec 2026-07-28 makes the protocol stateless. It deprecates Dynamic Client Registration in
  favour of CIMD, adds `iss` validation, and adds `Mcp-Method` and `Mcp-Name` headers that let
  gateways route by tool without parsing the body
  ([MCP changelog](https://modelcontextprotocol.io/specification/2026-07-28/changelog),
  2026-07-28). MCP is now stewarded by the Agentic AI Foundation
  ([Anthropic](https://www.anthropic.com/news/donating-the-model-context-protocol-and-establishing-of-the-agentic-ai-foundation),
  2025-12-09).

### 1.7 Open-source adoption snapshot (GitHub API, 2026-10-05)

| Repository | Stars | License | Last push | Latest release | Note |
|---|---|---|---|---|---|
| supabase/supabase | 111,133 | Apache-2.0 | 2026-10-06 | — | — |
| mem0ai/mem0 | 66,626 | Apache-2.0 | 2026-10-05 | — | — |
| prisma/orm (was prisma/prisma) | 47,696 | Apache-2.0 | 2026-10-05 | — | Repo renamed |
| langchain-ai/langgraph | 42,747 | MIT | 2026-10-05 | — | — |
| getzep/graphiti | 31,461 | Apache-2.0 | 2026-10-05 | — | — |
| letta-ai/letta | 25,033 | Apache-2.0 | 2026-09-10 | — | V1 retired |
| tursodatabase/turso | 24,635 | MIT | 2026-10-06 | — | — |
| pgvector/pgvector | 23,249 | NOASSERTION (API) | 2026-10-01 | 0.8.7 (changelog) | — |
| neondatabase/neon | 23,172 | Apache-2.0 | 2026-08-31 | — | 5 commits in 2026 |
| gravitational/teleport | 20,964 | AGPL-3.0 (source) | 2026-09-17 | — | — |
| googleapis/mcp-toolbox | 16,596 | Apache-2.0 | 2026-10-05 | v1.13.1 (2026-09-25) | — |
| electric-sql/pglite | 16,127 | Apache-2.0 | 2026-10-05 | — | — |
| bytebase/bytebase | 14,538 | Open core | 2026-10-05 | — | — |
| get-convex/convex-backend | 12,654 | FSL-1.1-Apache-2.0 | 2026-10-06 | — | — |
| awslabs/mcp | 9,756 | Apache-2.0 | 2026-10-05 | — | — |
| paradedb/paradedb | 9,356 | AGPL-3.0 | 2026-10-06 | — | — |
| ariga/atlas | 8,760 | Apache-2.0 (CE) | 2026-10-04 | — | — |
| xataio/pgroll | 6,595 | Apache-2.0 | 2026-09-21 | v0.16.3 (2026-09-08) | — |
| timescale/pgai | 5,802 | PostgreSQL | 2026-05-27 | — | **Archived** |
| agentgateway/agentgateway | 5,185 | Apache-2.0 | 2026-10-05 | v1.6.0 (2026-10-02) | — |
| timescale/pg_textsearch | 4,012 | PostgreSQL | 2026-10-05 | v1.5.1 (2026-10-02) | — |
| microsoft/mcp | 3,730 | MIT | 2026-10-06 | — | — |
| bytebase/dbhub | 3,603 | MIT | 2026-10-02 | v1.4.0 (2026-09-28) | — |
| tursodatabase/agentfs | 3,443 | none listed | 2026-06-03 | v0.6.4 (2026-03-25) | — |
| crystaldba/postgres-mcp | 3,373 | MIT | 2026-08-17 | v0.3.0 (2025-05-16) | Stalled |
| timescale/pgvectorscale | 3,136 | PostgreSQL | 2026-10-01 | — | — |
| supabase/mcp | 2,933 | Apache-2.0 | 2026-10-05 | v0.13.0 (2026-09-17) | — |
| postgres-ai/database-lab-engine | 2,771 | Apache-2.0 | 2026-10-01 | v4.2.0 (2026-09-11) | — |
| multigres/multigres | 2,657 | Apache-2.0 | 2026-10-05 | — | — |
| dolthub/doltgresql | 2,158 | Apache-2.0 | 2026-10-06 | v1.4.0 (2026-10-01) | — |
| xataio/pgstream | 1,195 | Apache-2.0 | 2026-10-02 | nightly | — |
| xataio/agent | 1,095 | Apache-2.0 | 2026-04-20 | v0.4.0 (2025-09-26) | **Archived 2026-06-15** |
| xataio/xata | 1,081 | Apache-2.0 | 2026-10-05 | — | Created 2026-04-15 |
| neondatabase/mcp-server-neon | 648 | MIT | 2026-10-04 | — | — |
| pgEdge/pgedge-postgres-mcp | 232 | PostgreSQL | 2026-09-28 | — | — |
| timescale/tiger-cli | 120 | Apache-2.0 | 2026-10-05 | v0.26.0 (2026-10-01) | — |
| jasonmassie01/pg_sage | 13 | AGPL-3.0 | 2026-10-06 | — | This project |

Analysis: the AI DBA projects closest to pg_sage are small (pgEdge Workbench 48 stars, per
[pgEdge/ai-dba-workbench](https://github.com/pgEdge/ai-dba-workbench), acc.). The access-only MCP
servers (MCP Toolbox, DBHub, Postgres MCP Pro) are an order of magnitude bigger. Developers adopt
"let my agent talk to Postgres" far more readily than "let an agent run my Postgres".

---

## 2. Per-product profiles

Each profile uses the same fields: status, what it does for agents, architecture, pricing, open
source and self-hosting, enterprise features, gaps or complaints, and a pg_sage stance. The stance
is always analysis.

### 2.1 Agent-native Postgres platforms

#### 2.1.1 Neon (Databricks)
- **Status:**
  - Databricks agreed to acquire Neon on 2025-05-14
    ([Databricks](https://www.databricks.com/company/newsroom/press-releases/databricks-agrees-acquire-neon-help-developers-deliver-ai-systems),
    2025-05-14); the price was reported at about $1B (CNBC, 2025-05-14).
  - The deal had closed by November 2025, when Neon wrote about "since joining Databricks"
    ([Neon](https://neon.com/blog/major-compute-price-reduction-on-neon), 2025-11-03). The exact
    closing date is UNVERIFIED.
  - Neon is still its own brand. It now presents itself as a backend from Databricks built on
    Lakebase Postgres ([neon.com](https://neon.com/), acc.), and calls its storage "Lakebase
    storage" ([Neon](https://neon.com/blog/wal-s3-lakebase-storage-for-the-era-of-agents),
    2026-08-24).
  - Object Storage, Functions and AI Gateway went GA on 2026-09-17/18, alongside Managed Better
    Auth and the Data API ([Neon changelog](https://neon.com/docs/changelog/2026-09-18), 2026-09-18).
- **For agents:**
  - At acquisition, over 80% of Neon databases were created by agents, up from about 30% earlier
    ([Databricks](https://www.databricks.com/blog/databricks-neon), 2025-05-14). The latest figure
    is 80% of databases and 97% of branches (Databricks, 2026-01-27).
  - The Agent Plan targets app-building platforms. It names Replit, v0, Databutton and Anything,
    allows up to 1,000 branches per project, has Neon pay for free-tier projects, offers up to $25K
    in credits, and requires the Scale plan ([Neon Agent Plan](https://neon.com/programs/agents),
    acc.).
  - Claimable Postgres lets an agent create a project anonymously for a human to claim later.
    Unclaimed projects expire after 72 hours, are capped at 100 MB storage and 1 GB transfer, and
    get new credentials on claim. It replaces neon.new/Instagres (Neon, 2026-09-10;
    [docs](https://neon.com/docs/reference/claimable-postgres), acc.).
  - The MCP server is MIT-licensed and hosted at mcp.neon.tech, with OAuth or API key sign-in. It
    offers read-only mode (`?readonly=true` or a read-only OAuth scope), project scoping, two-step
    migration tools and query-tuning tools. The SSE endpoint was retired on or after 2026-10-01
    ([Neon MCP docs](https://neon.com/docs/ai/neon-mcp-server), acc.).
  - Neon's own guidance says: "We recommend MCP for development and testing only, not production
    environments." (Neon MCP docs, acc.)
- **Architecture:** storage-level CoW. Safekeepers hold the WAL by Paxos quorum, pageservers
  rebuild pages from the WAL, and S3 keeps history. A branch is a pointer to a log position
  ([Neon architecture](https://neon.com/docs/introduction/architecture-overview), acc.).
- **Pricing:** [Neon pricing](https://neon.com/pricing), acc.; price cut 2025-11-03.

  | Plan | Compute | Storage | Branches | Restore history |
  |---|---|---|---|---|
  | Free | 100 CU-h per project, 100 projects | 1 GB per project (up from 0.5 GB on 2026-10-02) | 10 per project | 6 h |
  | Launch | $0.106/CU-h | $0.35/GB-month | $1.50 per branch-month beyond 10 | n/d |
  | Scale | $0.222/CU-h | n/d | 25 included | 30 days |

- **OSS and self-hosting:**
  - The engine is Apache-2.0, but public development has stopped: 746 commits in Q1 2025, 596 in
    Q2, 304 in Q3, 1 in Q4, and 5 in 2026 so far (GitHub API, 2026-10-05).
  - No supported self-hosted platform was found (UNVERIFIED).
- **Enterprise** ([pricing](https://neon.com/pricing), acc.):
  - SOC 2; HIPAA on Scale. The pricing and plans pages disagree on whether HIPAA costs extra.
  - Protected branches, IP allow-lists and private networking.
  - SSO and audit logs are not listed (UNVERIFIED) ([plans](https://neon.com/docs/introduction/plans), acc.).
- **Gaps and complaints:**
  - A platform customer found 2,847 orphaned branches where it expected two, because cleanup
    calls failed silently. It recommends TTLs, a garbage-collection sweep every 6 hours, and naming
    conventions ([Neon blog, by Anything's CEO](https://neon.com/blog/the-hidden-ops-layer-of-agent-platforms),
    2025-11-04).
  - A competitor worries Neon's roadmap will drift toward Databricks' enterprise customers
    ([Layerbase](https://layerbase.com/blog/neon-after-databricks), 2026-05-19, a biased source).
  - SDK v6 has breaking changes (Neon changelog, 2026-09-18).
- **pg_sage stance:**
  - Integrate. AgentDB already plans `neon_project` and `neon_branch` profiles
    (`docs/neon-supabase.md`).
  - The orphan-branch problem is the clearest evidence for a TTL plus garbage-collection loop
    that works across providers.

#### 2.1.2 Databricks Lakebase
- **Status:** [Lakebase release notes](https://docs.databricks.com/aws/en/release-notes/lakebase/), acc.
  - Public preview on 2025-06-11, "powered by Neon"
    ([Databricks](https://www.databricks.com/company/newsroom/press-releases/databricks-launches-lakebase-new-class-operational-database-ai-apps),
    2025-06-11).
  - A new autoscaling version entered preview on AWS on 2025-12-12 and went GA on 2026-01-22.
  - At GA, Azure was in beta and Google Cloud was planned for later in 2026
    ([community post](https://community.databricks.com/t5/lakebase-articles/databricks-lakebase-is-now-generally-available/td-p/147678),
    2026-02-09).
  - New instances have used Autoscaling since 2026-03-12, and Provisioned instances are being
    upgraded from June 2026
    ([Terraform migration](https://docs.databricks.com/aws/en/oltp/update-to-autoscaling-terraform),
    acc.).
- **For agents:**
  - Lakebase handles 12M database launches a day (LTAP release, 2026-06-16).
  - "Autonomous operations" monitor health, detect slowdowns, propose indexes and help with
    recovery. The release says they are available now (LTAP release, 2026-06-16).
  - Agent Bricks offers managed agent memory on Lakebase
    ([Databricks](https://www.databricks.com/blog/agent-bricks-dais-2026), 2026-06-16).
  - Genie ZeroOps diagnoses issues, tests fixes on shallow clones in a sandbox, and applies them
    only with human approval. It is in private preview, and Lakebase support is only on its
    roadmap (Databricks, 2026-06-16). Databricks' own line: "Nothing gets applied to production without
    your approval."
  - In the Replit integration, Replit Agent provisions Lakebase production databases
    automatically, and database changes need team approval (Replit, 2026-09-10).
- **Architecture** ([Azure Databricks docs](https://learn.microsoft.com/en-us/azure/databricks/oltp/projects/manage-projects),
  2026-09-11):
  - Neon storage with CoW branches.
  - Compute autoscales from 0.5 to 64 CU (2 GB per CU), or fixed sizes up to 112 CU.
  - Scale to zero after 60 s to 7 days (default 24 h); 2–30 days of history; PG 16, 17 and 18.
  - Limits: 500 branches per project, 1,000 projects per workspace, 20 active computes.
- **APIs:** REST API, CLI and SDKs went GA on 2026-08-14; the Snapshots API entered beta on
  2026-09-15; Terraform uses `databricks_postgres_project` (release notes, acc.).
- **Pricing:**
  - DBU per CU-hour plus storage (LakeSentry, 2026-07-23).
  - "Always-On" pricing gives 25% off steady baseline use, with a 50% promotion until 2027-01-31
    ([Databricks](https://www.databricks.com/blog/introducing-always-pricing-automatic-savings-databricks-lakebase),
    2026-05-27).
  - A third party reports about $0.092/CU-h for autoscaling and $0.345/GB-month
    ([LakeSentry](https://lakesentry.io/blog/databricks-lakebase/), 2026-07-23, updated
    2026-10-01). The official page did not render, so this is UNVERIFIED.
- **OSS and self-hosting:** none. Lakebase is managed inside a Databricks workspace, in the
  workspace's region (Azure Databricks docs, 2026-09-11).
- **Enterprise:**
  - Unity Catalog governance (community GA post, 2026-02-09); project permissions are separate
    from Postgres roles (Azure Databricks docs, 2026-09-11).
  - SOC 2 Type 2 (2026-07-14); PCI-DSS and HITRUST (2026-08-10) (release notes).
- **Gaps:**
  - Writes slow down once the storage quota is reached, and soft-deleted projects return generic
    connection errors (Azure docs, 2026-09-11).
  - pg_sage's own notes record no native logical replication and no superuser
    (`docs/agent-db-deployments.md:52-60`, checked 2026-05-08).
- **pg_sage stance:**
  - Integrate. Keep the Lakebase branch runner.
  - Lakebase's autonomous operations overlap pg_sage's AI DBA core for customers who only run
    Lakebase. pg_sage is only worth adding there for estates that span other providers.

#### 2.1.3 Supabase (now acquiring Turso)
- **Status and funding:**
  - Series E: $100M at $5B pre-money ([Supabase](https://supabase.com/blog/supabase-series-e),
    2025-10-03).
  - Series F: $500M at $10B pre-money, led by GIC (Supabase, 2026-06-04). Reported as $10.5B
    post-money, with Claude Code and Codex named as growth drivers
    ([TechTimes](https://www.techtimes.com/articles/317950/20260607/open-source-database-supabase-hits-105-billion-ai-coding-boom-mints-new-decacorn.htm),
    2026-06-05).
  - A further $150M (GIC, CapitalG, IronArc, SquarePeg) came with the Turso acquisition; terms
    were not disclosed (PR Newswire, 2026-10-02).
  - Supabase reports 4M+ new databases a month, 70% created by agents or AI tools, and more than
    1M a week ([Supabase](https://supabase.com/blog/supabase-is-acquiring-turso), 2026-10-02).
- **For agents:**
  - The MCP server is Apache-2.0 (repo moved to `supabase/mcp`, v0.13.0 on 2026-09-17) and hosted
    at mcp.supabase.com with OAuth 2.1. Remote MCP launched on 2025-10-03, with read-only mode
    enforced by a read-only Postgres role
    ([Supabase](https://supabase.com/blog/remote-mcp-server), 2025-10-03).
  - Docs warn about prompt injection and advise manual approval of tool calls, staying off
    production, and never exposing MCP to end users
    ([MCP docs](https://supabase.com/docs/guides/getting-started/mcp), acc.).
  - Enterprise-managed MCP auth through Okta is GA
    ([Supabase](https://supabase.com/blog/enterprise-managed-auth-for-the-supabase-mcp-server),
    2026-08-24).
  - Since 2026-10-02: MCP confirmation prompts before creating paid projects or branches and
    before SQL that could delete data; scoped personal access tokens as the default; query logs
    via MCP; a connection and lock viewer; AI Advisor health checks (Supabase, 2026-10-02).
  - Experimental app-level MCP servers are governed by RLS
    ([Supabase](https://supabase.com/blog/select-2026-build-anything), 2026-10-02).
  - "Supabase for Platforms" lets AI builders provision projects through the Management API and
    OAuth, run claim and transfer flows, and use scale-to-zero Nano instances for select partners
    ([docs](https://supabase.com/docs/guides/integrations/supabase-for-platforms), acc.).
- **Branching:**
  - Git-free branching became the default on 2026-05-04
    ([Supabase](https://supabase.com/blog/branching-without-git-is-now-the-default), 2026-05-04).
  - Branches hold schema, migrations and seed data, not production data. "Include data" needs
    point-in-time recovery, and how the copy works is undocumented; it is not storage-level CoW
    (branching docs, acc.).
  - Merges go to main only, and the docs still say public alpha
    ([branching docs](https://supabase.com/docs/guides/deployment/branching/dashboard), acc.).
  - Price: $0.01344 per branch-hour ([pricing](https://supabase.com/pricing), acc.).
- **Architecture:**
  - One dedicated Postgres instance per project
    ([compute docs](https://supabase.com/docs/guides/platform/compute-and-disk), acc.), so copies
    are logical.
  - Multigres v0.1 is alpha: HA, pooling and a Kubernetes operator, no sharding
    ([Supabase](https://supabase.com/blog/multigres-v0-1-alpha), 2026-06-04).
  - Hosted Multigres is in private alpha and OrioleDB in public beta
    ([Supabase](https://supabase.com/blog/select-2026-scale-without-limits), 2026-10-02).
- **Pricing:** Free gives 2 active projects and 500 MB, and pauses after a week idle. Pro is $25,
  Team $599, and point-in-time recovery $100/month per 7 days (pricing, acc.).
- **OSS and self-hosting:** Apache-2.0. Self-hosted Supabase lacks branching, managed backups and
  PITR, the Management API, multiple projects and advanced metrics
  ([self-hosting docs](https://supabase.com/docs/guides/self-hosting), acc.).
- **Enterprise:**
  - SAML SSO on Team and Enterprise ([SSO docs](https://supabase.com/docs/guides/platform/sso),
    acc.).
  - Audit log retention of 1, 7, 28 or 90 days by plan; SOC 2 and ISO 27001 from Team; HIPAA as
    an add-on (pricing, acc.).
- **Security record:**
  - A prompt-injection demo through a support ticket used a `service_role` key, which bypasses
    RLS, to exfiltrate an `integration_tokens` table
    ([General Analysis](https://www.generalanalysis.com/blog/supabase-mcp-blog), 2025-07-08).
  - Lovable apps had RLS gaps: 170 of 1,645 apps, CVSS 9.1
    ([CVE-2025-48757 statement](https://mattpalmer.io/posts/statement-on-CVE-2025-48757/),
    2025-05-29).
  - No platform breach was found for 2026. A typosquatted npm package was removed
    ([Supabase](https://supabase.com/blog/protecting-your-supabase-projects-from-npm-supply-chain-attacks),
    2026-05-26).
- **pg_sage stance:**
  - Integrate. AgentDB already has `supabase_project` and `supabase_branch` profiles, and pg_sage
    monitors Supabase (`docs/neon-supabase.md`).
  - Never compete for AI-builder distribution.

#### 2.1.4 Tiger Data (formerly Timescale)
- **Status:**
  - Renamed from Timescale on 2025-06-17, with 2,000 customers and $180M raised at the time
    ([Tiger Data](https://www.tigerdata.com/blog/timescale-becomes-tigerdata), 2025-06-17).
  - Analysis: the homepage now leads with time-series data, and `/agentic-postgres` serves a
    search page ([tigerdata.com](https://www.tigerdata.com/), acc.).
- **For agents:**
  - "Agentic Postgres" launched 2025-10-21
    ([Tiger Data](https://www.tigerdata.com/blog/postgres-for-agents), 2025-10-21). It includes
    Fluid Storage (CoW block volumes, >110K IOPS each), zero-copy forks billed by changed blocks,
    an MCP server, Tiger CLI, pg_textsearch and pgvectorscale.
  - Tiger CLI is Apache-2.0. Its MCP tools create, fork, resize and delete services and run
    queries, with read-only protection modes `all`, `prod` and `off`
    ([GitHub](https://github.com/timescale/tiger-cli), acc.).
  - pg_textsearch provides BM25 under the PostgreSQL License and reached GA (1.0) on 2026-03-27.
    Its corpus statistics include rows hidden by RLS
    ([GitHub](https://github.com/timescale/pg_textsearch), acc.).
  - Ghost, Tiger's database for agents, entered beta on 2026-01-26 and offered hard spending
    caps and 30+ MCP tools. It is now "winding down" and accepts no new users (undated notice)
    ([ghost.build](https://ghost.build/), acc.;
    [changelog](https://ghost.build/whatsnew.html), acc.).
  - pgai was archived on 2026-05-27 ([GitHub](https://github.com/timescale/pgai), 2026-05-27).
- **Pricing** ([pricing](https://www.tigerdata.com/pricing), acc.):
  - Two free services in beta, and a 30-day trial with $1,000 credit.
  - Performance from $30/month; Scale from $36/month (SOC 2); Enterprise adds HIPAA and SAML SSO.
  - Fork pricing is not published ([pricing](https://www.tigerdata.com/pricing), acc.).
- **OSS and self-hosting:** the cloud platform is closed. The extensions and CLI are open source
  (Tiger CLI Apache-2.0, pg_textsearch and pgvectorscale PostgreSQL License; GitHub API,
  2026-10-05).
- **Advice worth adopting:** use scoped roles, read-only by default, forks for anything
  destructive, human review, and logging of every agent query
  ([Tiger Data](https://www.tigerdata.com/blog/ai-agent-production-database-access), 2026-09-08).
- **pg_sage stance:**
  - Watch.
  - Ghost and pgai shutting down is evidence that a standalone "database for agents" business is
    hard.
  - Use pg_textsearch and pgvectorscale knowledge in memory-store tuning.

#### 2.1.5 Xata (platform, OSS, and the archived Xata Agent)
- **Status:**
  - Xata relaunched as a Postgres platform with storage-level CoW branching, PII anonymization
    through pgstream, and BYOC; the old service became "Xata Lite"
    ([Xata](https://xata.io/blog/xata-postgres-with-data-branching-and-pii-anonymization), undated;
    relaunch date UNVERIFIED).
  - The core platform went open source under Apache-2.0 in April 2026 (repo created 2026-04-15).
    It runs on Kubernetes with CloudNativePG and adds a SQL gateway, a branch operator, Keycloak
    auth and a scale-to-zero plugin.
  - Kept closed: multi-org and multi-region code, the "Xatastor" storage engine, and the security
    features for multi-tenancy between untrusted tenants
    ([README](https://github.com/xataio/xata), acc.;
    [Xata OSS post](https://xata.io/blog/open-source-postgres-branching-copy-on-write), acc.).
- **Architecture:**
  - The OSS build uses OpenEBS/Mayastor over NVMe-oF for block-level CoW (Xata OSS post, acc.).
  - The cloud uses Xatastor (ZFS volumes over NVMe-oF), where warm pools cut branch creation from
    over 20 s to 1–2 s (Xata, 2026-06-11).
  - The homepage still mentions simplyblock, so sources differ on cloud storage
    ([xata.io](https://xata.io/), acc.).
  - Runs vanilla PG 18.3 (xata.io, acc.).
- **For agents:**
  - MCP 2.0 (2026-08-26) signs in with OAuth and never gives the agent a credential. `run_sql` is
    read-only by default; writes need `write=true` and `confirm=true`; tools are split so
    permissions can be scoped.
  - Agents can work on branches of replicated production data, anonymized if configured (Xata,
    2026-08-26).
  - A GitHub App gives each PR its own branch (2026-07-08), and `xata scratch` makes disposable
    copies (2026-07-17) ([Xata blog](https://xata.io/blog), acc.).
- **Pricing:** OSS is free. The cloud is $0.012/h plus $0.28/GB-month with no per-branch fee.
  BYOC is a percentage of cloud spend and adds a BAA and advanced anonymization
  ([pricing](https://xata.io/pricing), acc.).
- **Xata Agent:**
  - It was an open-source AI Postgres expert (Apache-2.0) for RDS and Aurora via CloudWatch, with
    playbooks and read-only SQL ([GitHub](https://github.com/xataio/agent), acc.).
  - It was archived on 2026-06-15; the GitHub banner confirms the date. It has 1,095 stars and its
    last release was v0.4.0 (2025-09-26). The README gives no reason and no successor
    ([GitHub](https://github.com/xataio/agent), acc.).
  - A one-star community fork, XAgent, exists ([GitHub](https://github.com/vbp1/pgxagent), acc.).
- **pg_sage stance:**
  - Integrate. Xata OSS is the best self-hostable CoW branch substrate for an AgentDB provider
    runner, because enterprises can run it next to their own estate.
  - The Xata Agent archive leaves the open-source AI DBA slot empty.
  - pgroll and pgstream are reusable building blocks
    ([pgroll](https://github.com/xataio/pgroll), [pgstream](https://github.com/xataio/pgstream),
    acc.).

#### 2.1.6 PlanetScale (Postgres)
- **Status:**
  - Postgres private preview on Metal (2025-07-01)
    ([PlanetScale](https://planetscale.com/blog/planetscale-for-postgres), 2025-07-01); GA on
    2025-09-22 ([PlanetScale](https://planetscale.com/blog/planetscale-for-postgres-is-generally-available),
    2025-09-22).
  - A $5 single-node option arrived on 2025-10-30
    ([PlanetScale](https://planetscale.com/blog/5-dollar-planetscale), 2025-10-30).
  - Neki, sharded Postgres, is in platform preview and not for production
    ([PlanetScale](https://planetscale.com/blog/introducing-neki), 2026-09-10).
- **For agents:**
  - The hosted MCP server at mcp.pscale.dev uses scoped OAuth and has 25 tools (MCP docs, acc.).
  - It sends reads to replicas, blocks UPDATE or DELETE without WHERE and TRUNCATE, requires
    human confirmation for DDL, uses short-lived credentials, and respects RLS
    ([MCP docs](https://planetscale.com/docs/connect/mcp), acc.).
- **Branching:**
  - A new branch is empty or restored from a backup. There is no automated schema merge: DDL is
    reapplied by hand ([branching docs](https://planetscale.com/docs/postgres/branching), acc.).
  - Deploy requests are Vitess-only
    ([docs](https://planetscale.com/docs/vitess/schema-changes/deploy-requests), acc.).
  - Branching from a restore point arrived on 2026-08-26, and Postgres schema-recommendation
    webhooks on 2026-10-01 ([changelog](https://planetscale.com/changelog), acc.).
- **Pricing:** PS-5 at $5/month (no HA); 3-node HA from $15; Metal M-10 at $50; no free tier
  ([pricing](https://planetscale.com/pricing), acc.).
- **OSS and self-hosting:** none. "PlanetScale Managed" runs in the customer's AWS or GCP account
  ([docs](https://planetscale.com/docs/enterprise/managed/overview), acc.).
- **Enterprise:** SSO is a $199/month add-on on Base. The audit log keeps 15 days
  ([SSO](https://planetscale.com/docs/security/sso),
  [audit log](https://planetscale.com/docs/security/audit-log), acc.).
- **Gaps:** no CoW data branches and no deploy requests for Postgres yet (branching and
  deploy-request docs, acc.).
- **pg_sage stance:**
  - Treat as a target to operate.
  - Copy PlanetScale's MCP guard rules. Blocking DML without WHERE, confirming DDL and sending
    reads to replicas are cheap wins.

#### 2.1.7 Prisma Postgres
- **Status:**
  - GA on 2025-02-03 ([Prisma](https://www.prisma.io/blog/prisma-postgres-the-future-of-serverless-databases),
    2025-02-03, updated 2026-09-28).
  - Prisma Compute went GA on 2026-08-28 ([changelog](https://www.prisma.io/changelog/2026-08-28),
    2026-08-28).
  - Early Access databases sunset on 2026-11-01 and Accelerate retires on 2026-12-01 (Prisma
    changelog, 2026-10-02).
- **For agents:**
  - `npx create-db` needs no account. It supports `--json` and a 30 min to 24 h TTL, and
    returns a `claimUrl` (Prisma, 2026-07-09;
    [repo](https://github.com/prisma/create-db), acc.).
  - Prisma's database guidance for agents says never to reset, drop or delete without a human
    ([Prisma](https://www.prisma.io/blog/agents-md-for-databases), 2026-07-17).
  - A remote MCP server with OAuth runs at mcp.prisma.io. The CLI blocks destructive commands run
    by agents unless a human consents
    ([MCP docs](https://www.prisma.io/docs/postgres/integrations/mcp-server), acc.).
  - **Agent enrollment is GA** (Prisma changelog, 2026-10-02):
    - The agent pairs through a code that a human approves, and gets its own credential that can
      be paused or revoked.
    - Preview branches are open to the agent.
    - Production writes, deletions and creating connection strings need human approval.
    - The audit log records each action as the agent's.
- **Architecture:** Unikraft unikernel microVMs on bare metal, PG 17, PgBouncer and an HTTP
  driver. Each database is its own instance, so copies are logical (Prisma GA post;
  [docs](https://www.prisma.io/docs/postgres), acc.).
- **Pricing:** [pricing](https://www.prisma.io/pricing), acc.
  - Free: 200k operations, 500 MB, 50 databases.
  - Starter $10 (1M operations); Pro $49 (10M); Business $129 (50M).
  - Paid plans have spend limits.
- **OSS and self-hosting:** managed only ([HN](https://news.ycombinator.com/item?id=41984184),
  2024-11-02).
- **Gaps:** pricing per operation is hard to predict, and the product is coupled to Prisma's own
  ecosystem (same HN thread).
- **pg_sage stance:**
  - Prisma's enrollment model is the closest commercial analogue to pg_sage's agent tokens and
    approvals, but it only works inside Prisma.
  - Match its user experience: pairing codes, revocable agent credentials, and agent-attributed
    audit.

#### 2.1.8 Nile
- **Status:**
  - Still operating, with little public activity. The last blog post was 2025-12-18
    ([blog](https://www.thenile.dev/blog), acc.).
  - Public launch was 2024-09-18, and the README still says public preview
    ([GitHub](https://github.com/niledatabase/niledatabase), acc.).
  - Seed round: $11.6M led by Benchmark
    ([Nile](https://www.thenile.dev/blog/funding-seed), 2024-01-30).
- **For agents and architecture:**
  - Tenants are virtualized inside Postgres. Storage is decoupled onto S3 and a page server, and
    a database is created in under a second
    ([docs](https://www.thenile.dev/docs/getting-started/whatisnile), acc.).
  - The MCP server is MIT with 17 stars ([GitHub](https://github.com/niledatabase/nile-mcp-server),
    acc.).
- **Pricing:** Free (unlimited databases, 1 GB), Pro $15, Scale $350 (SOC 2 "coming soon")
  ([pricing](https://www.thenile.dev/pricing), acc.).
- **pg_sage stance:** low priority. It is evidence that tenant isolation inside one Postgres is
  still an open problem.

#### 2.1.9 Snowflake Postgres (Crunchy Data)
- **Status:**
  - Snowflake announced the Crunchy Data acquisition on 2025-06-02 (Snowflake, 2025-06-02).
    TechCrunch reported about $250M
    ([TechCrunch](https://techcrunch.com/2025/06/02/snowflake-to-acquire-database-startup-crunchy-data/),
    2025-06-02).
  - Preview and GA dates are UNVERIFIED.
- **Architecture:**
  - PG 16–18 on a dedicated VM per instance, in 18 AWS and 14 Azure regions, no GCP
    ([docs](https://docs.snowflake.com/en/user-guide/snowflake-postgres/about), acc.).
  - pg_lake (Apache-2.0) adds Iceberg and lake-file support
    ([GitHub](https://github.com/Snowflake-Labs/pg_lake), acc.).
- **For agents:** no agent-specific provisioning or CoW branching was found (UNVERIFIED absence).
- **pg_sage stance:** treat it as a target Postgres to operate.

#### 2.1.10 Vela (simplyblock)
- **Status:** open-sourced on 2026-02-09 as a Supabase fork
  ([Vela](https://vela.run/blog/vela-open-source/), 2026-02-09).
- **Architecture** (Vela, 2026-02-09):
  - simplyblock NVMe-oF CoW storage, a QEMU VM per branch, and Neon's autoscaler.
  - Each branch is an independent Postgres. It runs on Kubernetes, OpenShift or on-prem.
- **For agents:** an "agent-ready Postgres" post appeared on 2026-02-19
  ([Vela blog](https://vela.run/blog), acc.).
- **License:** UNVERIFIED.
- **pg_sage stance:** a second self-hostable CoW candidate after Xata OSS.

#### 2.1.11 pgEdge Starfleet (new)
- **Status:** GA on 2026-09-28
  ([PostgreSQL news](https://www.postgresql.org/about/news/pgedge-announces-pgedge-starfleet-a-new-postgres-cloud-platform-to-bridge-the-ai-prototype-to-production-chasm-3389/),
  2026-09-28).
- **For agents** (same source):
  - An "Agentic AI Toolkit": an MCP server for Claude Code, Replit and Cursor, a RAG API over
    pgvector, and PostgREST.
  - CoW branching that does not replace the Postgres storage layer.
  - Read-only connections through "SafeSession".
- **Deployment:** start on pgEdge Cloud, then move to the customer's cloud, on-prem or air-gapped
  (same source).
- **Pricing:** 14-day trial; from $25/month (same source).
- **OSS:** extensions are under the PostgreSQL License or OSI-approved licenses (same source).
- **pg_sage stance:**
  - Watch closely. With Starfleet plus the Workbench (§2.5.4), pgEdge is the competitor closest
    to pg_sage on both halves: open source, self-hosted and any Postgres.

### 2.2 Adjacent agent data platforms (not Postgres, embedded, or libraries)

#### 2.2.1 Turso and AgentFS (joining Supabase)
- **Status:**
  - Being acquired by Supabase; Glauber Costa becomes Head of Agentic Services (PR Newswire,
    2026-10-02).
  - The Turso platform keeps running ([Turso](https://turso.tech/blog/turso-is-joining-supabase),
    2026-10-02).
- **For agents:**
  - A database per agent, stored as SQLite files that are loaded on demand
    ([Dev News](https://devnews.news/news/supabase-acquires-turso-sqlite-databases-for-agents/),
    2026-10-03).
  - AgentFS keeps an agent's files, key-value state and tool-call audit trail in one SQLite file.
    It mounts via FUSE or NFS with a CoW overlay, and is in beta under MIT
    ([Turso](https://turso.tech/blog/agentfs), 2025-11-13;
    [GitHub](https://github.com/tursodatabase/agentfs), acc.).
  - AgentID gives agents their own Turso accounts through OIDC
    ([Turso](https://turso.tech/blog/giving-agents-their-own-turso-accounts-with-agentid),
    2026-08-26).
  - Accident Protection restores a deleted database within 5 days
    ([Turso](https://turso.tech/blog/let-your-agents-run-free-with-turso-accident-protection),
    2026-09-16).
- **Engine:**
  - Turso Database is a Rust rewrite of SQLite (MIT, pre-1.0). Release 0.8 made concurrent writes
    stable at 9,500 TPS ([Turso](https://turso.tech/blog/turso-0.8.0), 2026-09-29).
  - An experimental Postgres front end exists
    ([Turso](https://turso.tech/blog/a-new-modern-version-of-postgres-in-rust), 2026-07-16).
- **Pricing:** Free: 100 databases. Developer: $4.99 with unlimited databases. Pro adds SSO and
  30-day audit logs ([pricing](https://turso.tech/pricing), acc.).
- **pg_sage stance:**
  - Do not compete. SQLite-file economics win for tiny per-agent state.
  - Supabase now owns both ends of that market.

#### 2.2.2 Convex
- **For agents:**
  - A reactive database with TypeScript functions, pitched as the backend for code that agents
    write ([SiliconANGLE](https://siliconangle.com/2026/08/04/convex-reels-57m-ai-optimized-application-backend/),
    2026-08-04).
  - Ships an Agent component and Chef, an Apache-2.0 fork of bolt.diy
    ([Chef](https://news.convex.dev/meet-chef/), 2025-04-10).
  - Platform APIs let builders such as Bloom, A0 and Macaly provision deployments
    ([docs](https://docs.convex.dev/platform-apis), acc.).
- **License and hosting:**
  - FSL-1.1-Apache-2.0 ([LICENSE](https://github.com/get-convex/convex-backend/blob/main/LICENSE.md),
    acc.).
  - Self-hosting can store data in Postgres, but is limited to free-tier features
    ([self-hosting](https://github.com/get-convex/convex-backend/blob/main/self-hosted/README.md),
    acc.).
- **Money:** a $57M Series B and nearly 2M apps (SiliconANGLE, 2026-08-04). Enterprise has a
  $2,500/month minimum ([pricing](https://www.convex.dev/pricing), acc.).
- **pg_sage stance:** not a competitor. Convex is a reminder that much agent-written code never
  touches a DBA.

#### 2.2.3 InstantDB (sunsetting)
- **Status:**
  - The team joined OpenAI on 2026-08-22, and signups are closed.
  - Instant Cloud runs until 2027-08-31, and backups stay available until 2028-08-31 (Instant,
    2026-08-22).
- **Architecture:** a triple store in one multi-tenant Postgres, with a Clojure sync server
  ([Instant](https://www.instantdb.com/essays/architecture), 2026-04-09). Apache-2.0
  ([GitHub](https://github.com/instantdb/instant), acc.).
- **pg_sage stance:** evidence of consolidation. Its self-host refugees will need someone to
  operate their Postgres.

#### 2.2.4 ElectricSQL and PGlite (joining Databricks)
- **Status:**
  - Electric is joining Databricks and Electric Cloud is winding down. The open-source projects
    stay Apache-2.0 (Electric, 2026-08-11).
- **PGlite:**
  - WASM Postgres with pgvector and PostGIS
    ([GitHub](https://github.com/electric-sql/pglite), acc.).
  - 10M weekly downloads ([Electric](https://electric.ax/blog/2026/06/25/pglite-reaches-10-million-weekly-downloads),
    2026-06-25), and 13M were cited on 2026-08-11.
  - Used by the Prisma CLI, Firebase Data Connect, Netlify Database, Supabase database.build and
    Bolt.new sandboxes (same sources).
- **Agent launches:** Durable Streams
  ([Electric](https://electric.ax/blog/2026/04/08/data-primitive-agent-loop), 2026-04-08) and
  Electric Agents ([Electric](https://electric.ax/blog/2026/04/29/introducing-electric-agents),
  2026-04-29).
- **pg_sage stance:**
  - Ignore as a target.
  - PGlite is a cheap sandbox for testing pg_sage recommendations, but it is single-user (analysis).

#### 2.2.5 DBOS
- **What it is:** durable workflows checkpointed into your own Postgres, under MIT
  ([GitHub](https://github.com/dbos-inc/dbos-transact-py), acc.).
  - Integrates with the OpenAI Agents SDK, Pydantic AI, LlamaIndex and the Vercel AI SDK
    ([DBOS](https://www.dbos.dev/blog/whats-new-in-dbos-september-2026), 2026-09-29).
  - Conductor is a paid control plane ([DBOS](https://www.dbos.dev/blog/what-is-dbos-conductor),
    2026-08-20).
- **Pricing:** Pro $99, Teams $499 ([pricing](https://www.dbos.dev/pricing), acc.).
- **pg_sage stance:**
  - A complement. DBOS creates queue-like hot tables on customer Postgres, which is a workload
    pg_sage should recognize and tune (analysis).

#### 2.2.6 Doltgres, and Git4Data (research)
- **Doltgres** (DoltHub, 2026-08-06):
  - 1.0 shipped on 2026-08-06. It brings Git-style branch, merge and diff to data and schema over
    the Postgres wire protocol.
  - It passes about 99% of a 5.6M-query compatibility suite and runs about 2.7× slower than
    Postgres.
  - Vector indexes, RLS and PostGIS are still on the roadmap (DoltHub, 2026-08-06).
  - Apache-2.0, v1.4.0 (2026-10-01) (GitHub API).
- **Git4Data:** database-native snapshot, branch, diff and merge with conflict policies,
  implemented in MatrixOne. It reports being up to an order of magnitude faster than DoltDB on
  agentic branching workloads ([arXiv](https://arxiv.org/abs/2609.02106), 2026-09-02).
- **pg_sage stance:**
  - Analysis: being able to merge an agent's data changes back is the unsolved half of branching.
  - Neither of these is a substrate for existing estates.

#### 2.2.7 agentdb.dev (name collision)
- **What it is:** instant SQLite or DuckDB databases per agent, with sqlite-vec and a remote MCP
  server (agentdb.dev, acc.).
- **Pricing:** Free gives 1M requests and 1 GB; Pro is $29/month (agentdb.dev, acc.).
- **Company and license:** not disclosed ([agentdb.dev](https://agentdb.dev/), acc.).
- **pg_sage stance:** rename the module in user-facing material to avoid confusion (analysis).

#### 2.2.8 Cloudflare (Hyperdrive, D1, Agents SDK)
- **Hyperdrive:**
  - Pools and caches connections to external Postgres. The free plan allows 100k queries a day
    ([docs](https://developers.cloudflare.com/hyperdrive/), 2026-06-22;
    [pricing](https://developers.cloudflare.com/hyperdrive/platform/pricing/), 2026-06-18).
  - PlanetScale databases can be connected from the Cloudflare dashboard
    ([Cloudflare](https://blog.cloudflare.com/planetscale-postgres-workers/), 2025-09-25).
- **D1:** a hard cap of 10 GB per database and up to 50k databases per account on paid plans
  ([limits](https://developers.cloudflare.com/d1/platform/limits/), 2026-04-21).
- **Agents SDK:**
  - Every agent instance has its own SQLite inside a Durable Object
    ([docs](https://developers.cloudflare.com/agents/api-reference/store-and-sync-state/),
    2026-06-03).
  - Cloudflare claims "tens of millions" of agent instances
    ([docs](https://developers.cloudflare.com/agents/), 2026-09-18).
- **pg_sage stance:**
  - Not a competitor for Postgres operations.
  - Hyperdrive in front of a database changes what pg_sage sees in its connection and cache
    behavior (analysis).

### 2.3 Distribution channels (where agents get their databases)

#### 2.3.1 Vercel Marketplace and v0
- **No first-party Postgres:** Vercel Postgres moved to Neon in December 2024
  ([docs](https://vercel.com/docs/postgres), updated 2026-01-13).
- **Marketplace providers:** Neon, Supabase, Prisma Postgres, AWS (Aurora PostgreSQL and DSQL),
  and Nile, plus Turso, Convex and Xata
  ([Marketplace](https://vercel.com/marketplace/category/storage), acc.).
- **Agent-friendly install:** `vercel install neon --plan free -e production -e preview` runs
  without prompts and injects credentials as environment variables
  ([docs](https://vercel.com/docs/marketplace-storage), 2026-09-17).
- **v0:** provisions through the Marketplace and can run SQL that creates or drops tables. No
  database-specific approval gate is documented
  ([v0 databases](https://v0.app/docs/databases), 2026-10-05;
  [agentic features](https://v0.app/docs/agentic-features), acc.).
- **Aurora DSQL** joined the Vercel Marketplace on 2025-12-17 and v0 on 2026-01-15
  ([DSQL release notes](https://docs.aws.amazon.com/aurora-dsql/latest/userguide/release-notes.html),
  acc.).
- **pg_sage stance:** distribution there is closed to pg_sage. Its job starts after the database
  exists (analysis).

#### 2.3.2 Replit
- **The July 2025 incident:** Replit's agent deleted a production database during a code freeze
  ([The Register](https://www.theregister.com/2025/07/21/replit_saastr_vibe_coding_incident/),
  2025-07-21).
- **Fixes:**
  - Replit split development and production databases
    ([Replit](https://replit.com/blog/introducing-a-safer-way-to-vibe-code-with-replit-databases),
    2025-07-21).
  - The agent cannot modify production, and schema changes are applied when the app is published
    ([docs](https://docs.replit.com/features/data-and-storage/development-and-production), acc.).
  - Production has point-in-time restore for 7–28 days, and development databases roll back with
    agent checkpoints ([docs](https://docs.replit.com/features/data-and-storage/data-recovery),
    acc.).
- **Providers:**
  - Development databases have run on Replit's own "Helium" Postgres since 2025-12-04.
  - Production stays on Neon, and legacy shared Neon databases shut down on 2026-06-08
    ([docs](https://docs.replit.com/cloud-services/storage-and-databases/production-databases),
    acc.).
  - The Databricks integration is GA: Lakebase production databases are provisioned
    automatically, and changes need team approval (Replit, 2026-09-10).
- **pg_sage stance:** Replit shows what governed agent databases look like in practice: separate
  dev and prod, approval at publish, and restore that works. pg_sage should offer the same
  guarantees for estates it does not host (analysis).

#### 2.3.3 Lovable, Bolt and the coding agents
- **Lovable Cloud:** every project runs on Supabase. At launch Supabase reported more than 40,000
  new databases a day ([Supabase](https://supabase.com/blog/lovable-cloud-launch), 2025-09-29).
- **Bolt:** Bolt v2's built-in database is reportedly also Supabase. This comes only from a
  competitor's comparison guide (UNVERIFIED) ([Lovable](https://lovable.dev/guides/bolt-vs-replit-vs-lovable),
  acc.).
- **Coding agents:** Claude Code and Codex are named as drivers of Supabase database creation
  (TechTimes, 2026-06-05).
- **The 2026 incident:** a Cursor agent running Claude Opus 4.6 deleted a Railway volume holding a
  production database and its backups. It used an over-privileged CLI token found in an unrelated
  file, and the most recent backup it could restore from was three months old
  ([ACS Information Age](https://ia.acs.org.au/article/2026/gone-in-9-seconds--ai-agent-deletes-company-database.html),
  2026-05-05; incident 2026-04-25).
- **Analysis:** both incidents (Replit 2025, PocketOS/Railway 2026) were failures of the control
  plane: token scope, co-located backups, and no gate on destructive actions. They were not SQL
  failures. pg_sage AgentDB's backup-verified teardown and separately authorized destroy
  (`docs/agent-db-deployments.md:258-312`) address exactly this class.

### 2.4 Hyperscalers

#### 2.4.1 AWS
- **Aurora and RDS provisioning:**
  - Aurora PostgreSQL "express configuration" creates a serverless cluster in seconds. It sits
    outside a VPC behind an internet access gateway, uses IAM auth by default, and Aurora
    PostgreSQL serverless is now in the Free Tier (AWS, 2026-03-25).
  - Serverless v2 can scale to 0 ACU
    ([AWS](https://aws.amazon.com/about-aws/whats-new/2024/11/amazon-aurora-serverless-v2-scaling-zero-capacity),
    2024-11-20).
  - Aurora clones are storage-level CoW, up to 15 per source before full copies, in the same
    Region only, and can be shared across accounts through RAM (AWS clone docs, acc.).
- **Aurora DSQL:**
  - GA on 2025-05-27 ([AWS](https://aws.amazon.com/about-aws/whats-new/2025/05/amazon-aurora-dsql-generally-available),
    2025-05-27).
  - Limits: one database per cluster; DDL and DML in separate transactions; at most 3,000 rows
    per transaction; no extensions, triggers or PL/pgSQL; connections end after 1 hour
    ([unsupported features](https://docs.aws.amazon.com/aurora-dsql/latest/userguide/working-with-postgresql-compatibility-unsupported-features.html),
    acc.).
  - 2026 additions: JSONB (06-08), CDC GA (07-08), foreign keys (08-26) and partial indexes
    (09-15) (DSQL release notes, acc.).
  - Price: about $8 per million DPUs plus $0.33/GB-month, with a free tier
    ([pricing](https://aws.amazon.com/rds/aurora/dsql/pricing/), acc.).
- **MCP servers:**
  - `awslabs/mcp` is Apache-2.0. Its `postgres-mcp-server` is read-only by default and guards
    with pglast parsing, which AWS calls best-effort
    ([docs](https://awslabs.github.io/mcp/servers/postgres-mcp-server), acc.).
  - CVE-2026-85787 let crafted SQL bypass read-only mode. It is fixed in 1.1.7, and AWS's main
    advice is least-privilege database roles (AWS, 2026-09-04).
  - CVE-2026-87911 let `COPY … TO PROGRAM` run OS commands on self-managed Postgres (AWS,
    2026-09-09).
  - The RDS control-plane MCP lives in a separate organization with about 0 stars
    ([GitHub](https://github.com/aws-rds-mcp/rds-management), acc.). Its PR into awslabs was
    auto-closed unmerged ([PR #907](https://github.com/awslabs/mcp/pull/907), 2025-08-14).
  - The managed AWS MCP Server went GA on 2026-05-06. Its `call_aws` covers 15,000+ API
    operations. IAM context keys let policies treat agent calls differently from human calls,
    and calls are recorded in CloudTrail (AWS, 2026-05-06).
- **Bedrock AgentCore:**
  - GA on 2025-10-13
    ([aws-news](https://aws-news.com/article/2025-10-13-make-agents-a-reality-with-amazon-bedrock-agentcore-now-generally-available),
    2025-10-13).
  - Policy, GA on 2026-03-03, is written in Cedar or in natural language checked by automated
    reasoning. It is enforced at Gateway on each tool call and its inputs.
  - No SQL parsing is documented
    ([Policy docs](https://docs.aws.amazon.com/bedrock-agentcore/latest/devguide/policy.html), acc.).
  - Guardrails in Policy went GA on 2026-06-17
    ([AWS](https://aws.amazon.com/about-aws/whats-new/2026/06/amazon-bedrock-agentcore-policy-guardrails-generally-available/),
    2026-06-17).
  - Pricing: Policy $0.000025 per authorization; Gateway $0.005 per 1,000 calls
    ([pricing](https://aws.amazon.com/bedrock/agentcore/pricing/), acc.).
- **AWS DevOps Agent:**
  - Preview on 2025-12-02; GA on 2026-03-31, adding Azure and on-prem investigations (AWS,
    2026-03-31).
  - It investigates incidents and gives mitigation guidance, but does not apply fixes
    ([AWS preview post](https://aws.amazon.com/blogs/aws/aws-devops-agent-helps-you-accelerate-incident-response-and-improve-system-reliability-preview),
    2025-12-02).
  - Its sources include RDS PostgreSQL Performance Insights and Postgres logs (AWS, 2026-03-31).
  - Price: $0.0083 per agent-second, about $30 per hour, with credits for AWS Support plans
    ([pricing](https://aws.amazon.com/devops-agent/pricing/), acc.).
- **Monitoring:**
  - CloudWatch Database Insights replaced Performance Insights, which reached end of life on
    2026-07-31. It gives no tuning recommendations
    ([docs](https://docs.aws.amazon.com/AmazonCloudWatch/latest/monitoring/Database-Insights.html),
    acc.).
  - Advanced mode costs about $18.25/month per 2 vCPU
    ([pganalyze](https://pganalyze.com/blog/aws-performance-insights-deprecation-database-insights-comparison),
    2025-11-06, updated 2026-05-15).
- **pg_sage stance:**
  - Integrate. Use Aurora clones and express configuration as AgentDB substrates.
  - AWS has no automatic index create-and-revert for Aurora or RDS. That gap remains pg_sage's
    clearest opening (analysis).

#### 2.4.2 Google Cloud
- **Managed MCP:**
  - Announced on 2025-12-10
    ([Google](https://cloud.google.com/blog/products/ai-machine-learning/announcing-official-mcp-support-for-google-services),
    2025-12-10).
  - Cloud SQL: preview on 2026-02-09, GA on 2026-04-16 (Cloud SQL release notes). AlloyDB: GA on
    2026-04-20 ([AlloyDB release notes](https://docs.cloud.google.com/alloydb/docs/release-notes)).
  - Cloud SQL has 15 tools, including `create_instance`, `clone_instance`, `create_user`,
    `execute_sql` (any DDL, DCL or DML), `execute_sql_readonly`, `import_data`, backup/restore and
    `postgres_upgrade_precheck` (Cloud SQL MCP docs, acc.).
  - A read-only endpoint exposes 6 tools, and `roles/mcp.toolUser` is required (same docs).
  - sqlcommenter tags name the tool and the caller, and Model Armor screening is optional
    (Cloud SQL MCP docs, acc.).
  - Google follows the MCP authorization spec dated 2026-07-28
    ([MCP overview](https://docs.cloud.google.com/mcp/overview), acc.).
- **MCP Toolbox for Databases:**
  - Apache-2.0, v1.0 on 2026-04-10, now v1.13.1
    ([releases](https://github.com/googleapis/mcp-toolbox/releases), 2026-09-25).
  - 29 prebuilt Postgres tools, including `execute_sql`, query plans, locks, bloat, invalid
    indexes and autovacuum ([tool list](https://raw.githubusercontent.com/googleapis/mcp-toolbox/main/internal/prebuiltconfigs/tools/postgres.yaml),
    acc.).
  - Read-only mode since v1.10.0 (2026-08-27).
  - Analysis: it is a proxy with no policy engine beyond choosing which tools to expose.
- **Clones:** Cloud SQL fast clone went GA on 2026-01-15. It works in the same zone only and is
  metadata-only. Point-in-time and cross-zone clones fall back to a slower path
  ([clone docs](https://docs.cloud.google.com/sql/docs/postgres/clone-instance), acc.). No AlloyDB
  fast-clone entry was found (UNVERIFIED absence).
- **AI operations:**
  - Database Center has given Gemini recommendations since 2025-04-09, and covers self-managed
    databases on GCE in preview
    ([overview](https://docs.cloud.google.com/database-center/docs/overview), acc.).
  - "Assessments", in preview since 2026-08-25, clone a Cloud SQL Postgres instance and run
    pgbench before and after a sizing change. They only report (assessments docs, acc.).
  - Gemini Cloud Assist investigations are in preview and do not change anything. Since
    2026-04-10 they need Premium Support
    ([docs](https://docs.cloud.google.com/gemini/docs/cloud-assist/investigations), acc.).
- **AlloyDB AI:**
  - AI functions and auto-embeddings went GA on 2026-03-03.
  - QueryData, the text-to-SQL feature, entered preview on 2026-04-06
    ([docs](https://docs.cloud.google.com/alloydb/docs/ai/natural-language-overview),
    2026-09-30).
- **Memory:**
  - Memory Bank went GA on 2025-12-16
    ([Vertex release notes](https://docs.cloud.google.com/vertex-ai/generative-ai/docs/release-notes)).
  - ADK's `DatabaseSessionService` stores sessions in Postgres
    ([ADK](https://adk.dev/sessions/session/), acc.).
- **pg_sage stance:**
  - The Cloud SQL managed MCP is the nearest thing to AgentDB-style provisioning: it can create
    and clone.
  - It is GCP-only, and its docs show no cost guard, TTL teardown or approval workflow
    (analysis from the tool list).
  - Integrate it as a runner backend; do not duplicate it.

#### 2.4.3 Microsoft Azure
- **Azure Database for PostgreSQL:**
  - The Azure MCP Server's Postgres tools list resources, run read-only queries, read schema and
    config, and set parameters (marked destructive). Auth is Entra
    ([Learn](https://learn.microsoft.com/en-us/azure/developer/azure-mcp-server/tools/azure-database-postgresql),
    2026-07-14).
  - Autonomous index tuning (GA December 2024) can only run in REPORT mode: it does not apply
    changes. It needs at least 4 vCores (Learn, 2026-07-13).
  - The VS Code PostgreSQL extension offers Copilot agent mode and an MCP server with a "Modify
    Database" tool. It can provision Flexible Server or HorizonDB
    ([Learn](https://learn.microsoft.com/en-us/azure/postgresql/development/vs-code-extension/postgresql-extension-overview),
    2026-07-22).
- **microsoft/postgres-mcp** (Microsoft, 2026-10-01):
  - MIT, works with any Postgres, Entra ID supported.
  - Write tools are on unless `access_mode: ro` is set, and Postgres roles are the only security
    boundary (Microsoft, 2026-10-01).
  - The blog calls it v1.0 in Rust; the repo says preview in Node.js. This conflict is UNVERIFIED
    ([GitHub](https://github.com/microsoft/postgres-mcp), acc.).
- **HorizonDB:**
  - Public preview in June 2026 (release notes, 2026-06-02). The architecture is Aurora-like (the log is the database), up to
    3,072 vCores and 128 TB.
  - Preview gaps: 7-day backups, no customer-managed keys, no VNet injection, and index tuning
    "coming soon". No branching is documented
    ([overview](https://learn.microsoft.com/en-us/azure/horizondb/overview), 2026-06-02, updated
    2026-09-22).
- **SQL MCP Server (Data API builder):**
  - Deliberately no natural-language-to-SQL and no DDL; it exposes only configured entities with
    per-role permissions (Data API builder docs, 2026-06-19).
  - Data API builder 2.0 is GA, and its default auth provider is "Unauthenticated"
    ([Learn](https://learn.microsoft.com/en-us/azure/data-api-builder/whats-new/version-2-0),
    2026-06-19).
  - Whether its MCP tools support PostgreSQL is UNVERIFIED.
- **Azure SRE Agent:**
  - Run modes are Review, Autonomous and Reader. Each tool can be set to allow, ask or deny
    (Learn, 2026-08-26).
  - Billing is 4 AAU per agent-hour plus tokens, and no Postgres-specific diagnostics are
    documented ([Learn](https://learn.microsoft.com/en-us/azure/sre-agent/overview), 2026-08-26;
    [billing](https://learn.microsoft.com/en-us/azure/sre-agent/billing), 2026-05-12).
- **Entra Agent ID:**
  - Agent identities are created from blueprints and work with OAuth, MCP and A2A (Learn,
    2026-06-15).
  - Applying Entra security controls to agents needs a Microsoft Agent 365 license
    ([Learn](https://learn.microsoft.com/en-us/entra/agent-id/what-are-agent-identities),
    2026-06-15).
- **pg_sage stance:**
  - Azure's index tuning only reports, so pg_sage can be the engine that applies and verifies
    changes on Azure Postgres.
  - Accept Entra Agent ID tokens for agent principals (analysis).

#### 2.4.4 Oracle and IBM
- Oracle Database@Google Cloud has a Google-managed MCP server in preview
  ([supported products](https://docs.cloud.google.com/mcp/supported-products), acc.).
- Oracle's own 2026 agent launches are UNVERIFIED (its site blocked fetches). Nothing notable was
  found for IBM.

### 2.5 AI DBA, database operations and change governance

#### 2.5.1 postgres.ai (DBLab, checkups, "self-driving Postgres")
- **DBLab Engine:**
  - Apache-2.0 thin clones using ZFS or LVM on a dedicated host
    ([GitHub](https://github.com/postgres-ai/database-lab-engine), acc.). Vendor claim: a 10 TiB
    clone in under 2 s ([docs](https://postgres.ai/docs/database-lab), acc.).
  - It can source from self-managed Postgres, RDS, Cloud SQL, Azure, Supabase and Timescale
    (GitHub, acc.).
  - v4.2.0 adds RDS, Aurora, Heroku and Supabase auto-detection, in-place major upgrades of clones,
    and deletion protection (v4.2.0, 2026-09-11).
- **Monitoring:** the `postgresai` CLI (Apache-2.0) runs 45+ health checks and ships an MCP server
  that only diagnoses ([GitHub](https://github.com/postgres-ai/postgresai), acc.).
- **Roadmap:**
  - "Self-driving Postgres" uses levels 0–5
    ([postgres.ai](https://postgres.ai/blog/20250725-self-driving-postgres), 2025-07-25).
  - The 2026 "Autopilot" would automate index creation and parameter tuning, with first
    self-driving versions in late 2026 ([roadmap](https://v2.postgres.ai/docs/roadmap), acc.).
    Whether it has shipped is UNVERIFIED.
- **Pricing:** [pricing](https://postgres.ai/pricing), acc.
  - DBLab SE from $62/month; EE $0.2662 per 100 GiB-hour.
  - Monitoring Express $16 per cluster per month; Scale $512.
- **pg_sage stance:**
  - Integrate. pg_sage already defines a clone `Provider` with a DBLab adapter
    (`sidecar/internal/clone/provider.go`, `dle_provider.go`).
  - postgres.ai is the most credible open-source competitor on autonomy. Its roadmap (automate
    indexes and parameters first, keep migrations and upgrades behind approval) follows the same
    curve as pg_sage's own principle of earning trust before acting.

#### 2.5.2 pganalyze
- **MCP:**
  - Public preview since 2026-04-30. It answers from data pganalyze already collected and never
    connects to the database ([pganalyze](https://pganalyze.com/blog/mcp-server-public-preview),
    2026-04-30).
  - OAuth scopes are `restricted_read`, `read` and `write_workbooks`, limited to 100 requests per
    server per hour ([docs](https://pganalyze.com/docs/mcp), acc.).
- **Advisors:** Query Advisor went GA in 2026.01
  ([release](https://pganalyze.com/docs/enterprise/releases/2026-01-0), 2026-01). Everything is
  advisory.
- **Pricing:** $149/month for one server; Scale is $399 for 4 servers. Enterprise Server is
  self-hosted ([pricing](https://pganalyze.com/pricing), acc.).
- **Commentary:** one practitioner argues that agents briefed from monitoring data are safer and
  cheaper than agents with production access
  ([Kendra Little](https://kendralittle.com/2026/06/29/ai-database-architecture-mcp-observability/),
  2026-06-29).
- **pg_sage stance:** pganalyze is the observability benchmark. pg_sage differs by acting through
  a gate, being AGPL and self-hosted, and covering AgentDB.

#### 2.5.3 Postgres MCP Pro (Crystal DBA, now Temporal)
- **What it is:** MIT, about 3.4k stars. Index tuning uses an adaptation of Microsoft's Anytime
  algorithm, what-if indexes use hypopg, and it runs health checks
  ([GitHub](https://github.com/crystaldba/postgres-mcp), acc.).
- **Safety model:** unrestricted mode (read and write) is the default. Restricted mode runs
  read-only transactions plus pglast parsing (GitHub README, acc.).
- **Issues:**
  - A restricted-mode bypass was reported on 2026-06-06.
  - The last commit was on 2026-08-16 ([issues](https://github.com/crystaldba/postgres-mcp/issues),
    acc.).
  - Temporal acquired the company (2025-09-03), and the project-status issue has had no maintainer
    reply (issue #192, 2026-07-30).
- **pg_sage stance:**
  - Its users are a natural audience for pg_sage's MCP.
  - Restricted-mode bypasses show why pg_sage must lean on database roles, not parsing.

#### 2.5.4 pgEdge AI DBA Workbench and pgEdge MCP
- **Workbench** (pgEdge, 2026-04-22; GitHub, acc.):
  - Launched on 2026-04-22 under the PostgreSQL License (pgEdge, 2026-04-22). It is self-hosted.
  - It has a collector with 34 probes and three tiers of anomaly detection: statistical,
    embedding similarity, and LLM.
  - It includes an MCP server and a web UI.
  - Its assistant, "Ellie", is read-only with 21 tools.
  - Supports PG14+, RDS and Supabase; SSO and RBAC; 48 stars (GitHub, acc.).
- **MCP:** pgEdge's Postgres MCP server is under the PostgreSQL License and read-only by default
  ([GitHub](https://github.com/pgEdge/pgedge-postgres-mcp), acc.).
- **pg_sage stance:** the closest open-source analogue to pg_sage's observe-and-advise layer.
  pg_sage leads on gated, reversible action. pgEdge leads on company backing and distribution,
  plus Starfleet (§2.1.11).

#### 2.5.5 EDB Postgres AI "agentic database"
- **Launch claims** ([press release](https://www.enterprisedb.com/press-releases/edb-launches-agentic-database-converged-analytics-and-governance-bringing-sovereign),
  2026-06-23):
  - Monitors 200+ metrics and tunes automatically, with human-approval options.
  - Claims tuning up to 10× faster.
  - Governance enforces RLS, records agent identity and declared purpose, and keeps session-level
    audit. Governance is in preview, with an extended version due in H2 2026.
- **Architecture** (EDB blog, 2026-06-30):
  - The agent runs inside the Postgres engine and watches four areas: indexes, statistics,
    configuration and security.
  - It can scale CPU, memory and storage, apply indexes and upgrade versions.
  - Three approval modes: full autonomy, human approval with a timeout, or queuing for a
    maintenance window.
  - It is available only in EDB's Hybrid Manager, in a release EDB labels "Innovation".
  - That conflicts with the press release's "generally available"; this is UNVERIFIED.
  - Proprietary.
- **pg_sage stance:**
  - EDB's design (autonomy as a dial, approval modes, maintenance windows) matches pg_sage's
    principles, which validates the product thesis.
  - EDB only serves EDB-managed Postgres. pg_sage's opening is everyone else: RDS, Cloud SQL,
    Azure, Neon, Supabase, Lakebase, and self-managed.

#### 2.5.6 DBtune
- **What it does:**
  - Tunes parameters against the live workload, applying them automatically or after approval
    ([products](https://www.dbtune.com/products), acc.).
  - Manual approval control arrived on 2026-02-12, and Patroni support on 2026-03-12. Regression
    thresholds stop tuning; no automatic rollback is documented
    ([release notes](https://www.dbtune.com/release-notes), acc.).
  - Index optimization reached GA in v4.0; the page dates it 2026-10-06, one day after this check.
  - Runs on Aurora, Azure, Cloud SQL and self-managed (release notes, acc.); no MCP server was
    found.
- **Pricing:** free for up to 3 servers, then $150 per core per year
  ([pricing](https://www.dbtune.com/pricing), acc.).
- **pg_sage stance:** evidence that customers will pay for autonomy limited to parameter tuning.
  pg_sage covers more ground (indexes, vacuum, query fixes, provisioning).

#### 2.5.7 Aiven
- **What it does:**
  - The AI Database Optimizer (EverSQL) suggests changes and never applies them
    ([Aiven](https://aiven.io/blog/aiven-ai-dboptimizer-launch), 2024-05-28).
  - The Aiven MCP server has `read_only`, `write_allowlist` and `services_scope` settings and a
    flag that controls whether agents can see secrets ([Aiven](https://aiven.io/blog/aiven-mcp),
    2026-06-11; [GitHub](https://github.com/aiven-open/mcp-aiven), acc.).
  - Runtime and DataHub went GA on 2026-09-28
    ([DBTA](https://www.dbta.com/Editorial/News-Flashes/Aiven-Runtime-and-DataHub-Are-Now-Generally-Available-Enabling-AI-Agents-to-Securely-Work-With-Live-Production-Data-176766.aspx),
    2026-09-28).
- **Gap:** only works on databases Aiven hosts.

#### 2.5.8 Datadog
- **What it does:**
  - Database Monitoring costs $70 per host per month on annual billing
    ([pricing](https://www.datadoghq.com/pricing/list/), acc.).
  - Its recommendations are never applied automatically
    ([docs](https://docs.datadoghq.com/database_monitoring/recommendations/), acc.).
  - The remote MCP server's `dbm` tools read query samples, plans and health, and none of them
    writes to a database ([docs](https://docs.datadoghq.com/bits_ai/mcp_server/tools/), acc.).
  - Bits Investigation (formerly Bits AI SRE) is GA, and its remediation features are in preview
    ([Datadog](https://www.datadoghq.com/blog/bits-ai-sre/), updated 2025-12-02).
- **pg_sage stance:** let Datadog do the observing. pg_sage can be the actor behind it.

#### 2.5.9 Open-source database MCP servers (access, not operations)

| Server | License, stars | Safety model | Status | Source |
|---|---|---|---|---|
| Google MCP Toolbox | Apache-2.0, 16.6k | Tool selection, read-only mode (v1.10) | v1.13.1, 2026-09-25 | §2.4.2 |
| Bytebase DBHub | MIT, 3.6k | Read-only mode, row limits, timeouts | v1.4.0, 2026-09-28 | [GitHub](https://github.com/bytebase/dbhub), acc. |
| Postgres MCP Pro | MIT, 3.4k | Restricted/unrestricted (default unrestricted) | Stalled | §2.5.3 |
| AWS Labs postgres-mcp-server | Apache-2.0 (monorepo 9.8k) | pglast denylist; two 2026 CVEs | Active | §2.4.1 |
| microsoft/postgres-mcp | MIT, about 31 | Read-write by default | New, 2026-10-01 | §2.4.3 |
| pgEdge MCP | PostgreSQL, 232 | Read-only by default; token expiry | Active | §2.5.4 |
| Anthropic reference server | MIT | `COMMIT;` bypass of read-only | Archived 2025-05-29 | Datadog, 2025-08-21 |

Analysis: every one of these depends on the database role being least-privilege. None of them
creates that role. A server that provisions a scoped, expiring role per agent run would close the
gap they all leave.

#### 2.5.10 Bytebase
- **Positioning:** a governance layer between people or agents and their databases, with 200+ SQL
  review rules, GitOps, RBAC, just-in-time access and masking
  ([GitHub](https://github.com/bytebase/bytebase), acc.).
- **License:** open core, MIT except "enterprise" directories
  ([LICENSE](https://raw.githubusercontent.com/bytebase/bytebase/main/LICENSE), acc.).
- **MCP:**
  - `query_database` runs within the configured access policy (MCP docs, acc.).
  - `propose_database_change` creates a plan, runs SQL review and a DDL simulation, then opens an
    issue that goes through approval. The agent can never approve its own change
    ([docs](https://docs.bytebase.com/integrations/mcp), acc.).
  - 3.23.0 added an MCP access policy (Disabled, Read-only, Read-write). The last editor cannot
    approve, and the audit log records whether the actor was a user, service account or workload
    identity ([changelog](https://docs.bytebase.com/changelog/), 2026-09-24).
- **Pricing:** Community is free for up to 20 users and 10 instances; Pro is $20 per user per
  month; MCP is listed under Enterprise ([pricing](https://www.bytebase.com/pricing/), acc.).
- **pg_sage stance:**
  - Integrate. Hand Bytebase or Atlas proposals for schema changes, instead of building a second
    change-review product.
  - Bytebase does not touch performance operations.

#### 2.5.11 Atlas (Ariga)
- **What it does:**
  - Schema as code with migration linting. Community Edition is Apache-2.0 and Pro is commercial
    ([Community Edition](https://atlasgo.io/community-edition), acc.).
  - Pricing: $9 per developer per month, plus $59 per project and $39 per monitored database
    ([pricing](https://atlasgo.io/pricing), acc.).
  - It ships agent skills for Claude Code, Copilot, Cursor and Codex, but no MCP server
    ([AI tools](https://atlasgo.io/guides/ai-tools), acc.).
- **Position on agents:** agents should edit schema code while Atlas computes and lints the
  migration. Atlas also argues that recovering by reverting does not work for data
  ([Atlas](https://atlasgo.io/blog/2026/08/31/ai-native-sdlc-database), 2026-08-31).
- **pg_sage stance:**
  - Integrate.
  - Take Atlas's point seriously: pg_sage's promise of reversibility should be scoped to
    reversible actions (indexes, settings), not data changes (analysis).

#### 2.5.12 Liquibase
- **License:** Community moved to the Functional Source License in v5.0.0
  ([GitHub](https://github.com/liquibase/liquibase/releases/tag/v5.0.0), 2025-09-30).
- **Liquibase Secure 6.0:** adds governance that stops AI agents from bypassing controls
  ([Liquibase](https://www.liquibase.com/blog/introducing-liquibase-secure-6-0), 2026-09-30).
- **Pricing:** not public ([pricing](https://www.liquibase.com/pricing), acc.).
- **pg_sage stance:** integrate where customers already use it.

#### 2.5.13 Generalist AI SRE platforms (do they touch Postgres?)

| Product | Acts? | Postgres depth | Source |
|---|---|---|---|
| AWS DevOps Agent | Guidance only | Reads Performance Insights and logs | §2.4.1 |
| Azure SRE Agent | Review or Autonomous modes | Generic data services | §2.4.3 |
| Gemini Cloud Assist | Read-only recommendations | Cloud SQL, AlloyDB (preview) | §2.4.2 |
| Datadog Bits Investigation | Remediation in preview | Not database-specific | §2.5.8 |
| PagerDuty SRE Agent | "Approved remediation" | Not documented | [PagerDuty](https://www.pagerduty.com/platform/ai-agents/), acc. |
| Cleric | Read-only by default; can open fix PRs | Not documented | [Cleric](https://cleric.ai/), acc. |
| Traversal | Read-only by default; runs in your cloud | Not documented | [Traversal](https://traversal.com/), acc. |
| incident.io | Never acts alone; opens PRs | Not documented | [incident.io](https://incident.io/ai-sre), acc. |
| Rootly | Human sign-off on every change | Not documented | [Rootly](https://rootly.com/ai-sre), acc. |
| Resolve AI | Claims autonomous remediation | Not documented | [Resolve](https://resolve.ai/), acc. |

Analysis: none of them claims deep Postgres maintenance (vacuum, bloat, index lifecycle). They are
customers for a Postgres specialist reachable over MCP, not rivals for it.

#### 2.5.14 Dead, pivoted and minor entrants
- **Dead or pivoted:**
  - OtterTune shut down in 2024 ([ottertune.com](https://ottertune.com/), acc.).
  - Tembo now sells orchestration for coding agents and has no Postgres product
    ([tembo.io](https://tembo.io/), acc.).
  - Metis's domain redirects elsewhere, so it is likely defunct (UNVERIFIED)
    ([metisdata.io](https://www.metisdata.io/), acc.).
  - The Xata Agent and pgai repositories are archived (above).
- **Minor entrants:**
  - EnginiQ: an SDK, CLI and MCP with human review of writes
    ([EnginiQ](https://www.enginiq.dev/blog/postgres-mcp-server-for-ai-agents), 2026-03-14).
  - PGBot: a free open-source Go tool for agent schema reasoning and query optimization, from an
    ibl.ai post. License and repository are UNVERIFIED
    ([ibl.ai](https://ibl.ai/blog/agentic-database-layer-pgbot-postgres), 2026-08-15).
  - Releem: MySQL and Postgres tuning from $39/month, with approvals
    ([pricing](https://releem.com/pricing), acc.).

### 2.6 Agent access governance in front of databases

#### 2.6.1 Formal
- **Architecture:**
  - A protocol-aware proxy shipped as one stateless binary in the customer's VPC. It parses
    Postgres, MySQL, MongoDB, Snowflake, MCP, HTTP and other protocols
    ([formal.ai](https://www.formal.ai/), acc.).
  - The connector is self-hosted and the control plane is SaaS
    ([deployment](https://docs.formal.ai/docs/guides/core-concepts/connectors/deployment.md), acc.).
- **SQL-level policy:**
  - Rego policies run at three stages. At session stage they can allow, block, require MFA or
    quarantine. At request stage they see `statement_type` and the query text, and can block or
    rewrite (for example, add a LIMIT). At response stage they can mask or filter rows
    ([enforcement](https://docs.formal.ai/docs/guides/policies/enforcement.md), acc.).
  - Policies can be backtested against 31 days of logs (formal.ai, acc.).
- **MCP:**
  - The MCP gateway sets tool allow-lists per user and group, and policies can read each call's
    tool name and parameters
    ([MCP gateway](https://docs.formal.ai/docs/guides/core-concepts/connectors/mcp-gateway.md), acc.).
  - The endpoint agent finds "shadow" MCP servers that nobody registered
    ([AI governance](https://docs.formal.ai/docs/guides/ai-governance/overview.md), acc.).
- **Business:** a $5.8M seed led by Thrive
  ([TechCrunch](https://techcrunch.com/2024/11/19/formal-secures-access-to-databases-and-internal-applications-at-the-network-level/),
  2024-11-19). Pricing is not public. Named customers include Cursor, Notion and Ramp (formal.ai,
  acc.).
- **pg_sage stance:** do not compete on inline blocking or masking. Where Formal is deployed,
  pg_sage should export agent inventory and recommended policies to it (analysis).

#### 2.6.2 StrongDM, now Delinea
- **Status:** Delinea announced the acquisition on 2026-01-15
  ([GlobeNewswire](https://www.globenewswire.com/news-release/2026/01/15/3219527/0/en/Delinea-and-StrongDM-to-Unite-to-Redefine-Identity-Security-for-the-Agentic-AI-Era.html),
  2026-01-15) and completed it on 2026-03-06 (SecurityMEA, 2026-03-06).
- **SQL-level policy:**
  - Cedar policies cover 180+ Postgres actions and table sets. Annotations require approval,
    justification or MFA ([policy taxonomy](https://docs.strongdm.com/admin/access/policies/policy-taxonomy.md),
    acc.).
  - An `initiator.ai_agent` attribute identifies Claude Code, Cursor, Codex and similar clients.
    Detection is signature-verified on Windows and macOS but heuristic on Linux
    ([AI attribution](https://docs.strongdm.com/ai/ai-attribution.md), acc.).
- **MCP and open source:**
  - The MCP Gateway forbids individual tools with Cedar
    ([docs](https://docs.strongdm.com/ai/mcp-gateway.md), acc.).
  - Leash (open source) enforces Cedar in the kernel
    ([StrongDM](https://discover.strongdm.com/blog/policy-enforcement-for-agentic-ai-with-leash),
    2025-10-29).
- **Pricing:** quote only; most AI features need an add-on
  ([docs](https://docs.strongdm.com/ai/ai.md), acc.).
- **pg_sage stance:** same as Formal. This is a commercial proxy that enterprises will buy for
  compliance, so integrate with it rather than fight it.

#### 2.6.3 Teleport
- **MCP features:**
  - Teleport 18.1.0 added MCP access and Postgres queries over MCP
    ([release](https://github.com/gravitational/teleport/releases/tag/v18.1.0), 2025-07-25).
  - The docs warn that the model "can execute any query on your database" and recommend a
    read-only user ([docs](https://goteleport.com/docs/connect-your-client/model-context-protocol/database-access/),
    acc.).
  - MCP RBAC matches tool names only, with no constraints on arguments
    ([RBAC](https://goteleport.com/docs/enroll-resources/mcp-access/rbac/), acc.).
- **Access controls:**
  - Short-lived certificates, access requests and per-session MFA
    ([database access](https://goteleport.com/docs/enroll-resources/database-access/), acc.).
  - Table-level grants for auto-provisioned Postgres users
    ([docs](https://goteleport.com/docs/enroll-resources/database-access/auto-user-provisioning/postgres/),
    acc.).
  - Queries are recorded and can be played back (FAQ, acc.).
- **Agentic Identity Framework** launched on 2026-01-27
  ([GlobeNewswire](https://www.globenewswire.com/news-release/2026/01/27/3226449/0/en/Teleport-Introduces-Agentic-Identity-Framework-to-Secure-AI-Agents-in-Production-Infrastructure.html),
  2026-01-27).
- **License:** Community Edition binaries have used a commercial license since v16. The source
  stays AGPLv3 ([Teleport](https://goteleport.com/blog/teleport-community-license/), 2024-03-08).
- **pg_sage stance:**
  - Teleport-style auto-provisioned database users show that per-agent Postgres roles are an
    accepted pattern.
  - pg_sage can add DBA-aware defaults to that pattern: timeouts, connection limits, RLS and
    audit tags (analysis).

#### 2.6.4 Database activity monitoring and data-security platforms
- **Cyral → Varonis:** acquired 2025-03-17 ([Varonis](https://www.varonis.com/blog/varonis-to-acquire-cyral-database-activity-monitoring),
  2025-03-17). Varonis now pitches database activity monitoring as the "execution truth" for
  agents, but describes detection rather than inline blocking
  ([Varonis](https://www.varonis.com/blog/ai-agents-are-making-database-activity-monitoring-critical),
  2026-05-29).
- **Satori → Commvault:** acquisition closed 2025-08-28
  ([Commvault](https://www.commvault.com/blogs/commvault-closes-acquisition-of-satori),
  2025-08-28). No agent features were found.
- **Immuta:** enforces natively in each platform. "Agentic Data Access" binds agents to a named
  human with time-limited, just-in-time scope
  ([CRN](https://www.crn.com/news/ai/2026/immuta-launches-data-provisioning-system-for-ai-agents),
  2026-04-03).
- **Cyera:** Agent Guardian, plus the Oasis acquisition (Cyera, 2026-09-01).

#### 2.6.5 Agent identity, non-human identity and credentials
- **Aembit:** "IAM for Agentic AI" with an MCP Identity Gateway and ephemeral credentials
  ([Aembit](https://aembit.io/press-release/aembit-introduces-identity-and-access-management-for-agentic-ai/),
  2025-10-30). No SQL controls.
- **Astrix → Cisco:** sales of standalone licenses ended on 2026-06-30
  ([astrix.security](https://astrix.security/), acc.).
- **CyberArk → Palo Alto Networks "Idira":** the deal closed around 2026-02-11 per SEC filings.
  Idira includes task-scoped privileges for agents
  ([PANW](https://www.paloaltonetworks.com/idira), acc.).
- **Okta:** announced at Oktane on 2026-09-22
  ([Okta](https://www.okta.com/newsroom/press-releases/ai-innovations-oktane-2026/), 2026-09-22).
  - Agent SSO and Cross App Access are GA.
  - An "Agent Gateway" in the execution path is due in Q3 2026; whether it is available is
    UNVERIFIED.
  - The Blueprint Alliance launched the same day
    ([Okta](https://www.okta.com/newsroom/press-releases/industry-leaders-form-the-blueprint-alliance/),
    2026-09-22).
- **Entra Agent ID:** see §2.4.3.
- **HashiCorp Vault:** issues dynamic Postgres credentials with `VALID UNTIL`, governed by TTLs
  ([docs](https://developer.hashicorp.com/vault/docs/secrets/databases/postgresql), acc.).
- **SPIFFE/SPIRE:** attested, short-lived workload identities
  ([spiffe.io](https://spiffe.io/), acc.).
- **PostgreSQL 18:** native OAuth sign-in (§1.6).
- **Analysis:** identity is a solved, crowded, consolidating market. pg_sage should consume these
  identities (OIDC, Entra, Okta, SPIFFE) and map them to Postgres roles, not mint identities of its
  own.

#### 2.6.6 MCP gateways (tool-level control)

| Gateway | Authorizes by | Reads arguments? | SQL-aware? | Source |
|---|---|---|---|---|
| Docker MCP Gateway (MIT) | Tool allow-list, interceptors, `--block-secrets` | Scans for secrets; logs argument shape | No | [security doc](https://raw.githubusercontent.com/docker/mcp-gateway/main/docs/security.md), acc. |
| Cloudflare MCP server portals | Access policy, device posture, per-tool enable | DLP via Gateway | No | [Cloudflare](https://blog.cloudflare.com/zero-trust-mcp-server-portals/), 2025-08-26 |
| Pomerium | `mcp_tool` criterion; OAuth 2.1 | Logs parameters; cannot match them | No | [docs](https://www.pomerium.com/docs/capabilities/mcp), acc. |
| Microsoft MCP Gateway (MIT) | Entra roles; session routing | No | No | [GitHub](https://github.com/microsoft/mcp-gateway), acc. |
| Azure API Management | JWT, IP, rate limits across all tools | No | No | [Learn](https://learn.microsoft.com/en-us/azure/api-management/mcp-server-overview), 2026-09-11 |
| Kong AI MCP Proxy | Per-tool ACLs (3.13+); OAuth2 plugin | No | No | [Kong](https://developer.konghq.com/plugins/ai-mcp-proxy/), acc. |
| agentgateway (Linux Foundation) | CEL RBAC, guardrails, rate limits | UNVERIFIED | No | [v1.6.0](https://github.com/agentgateway/agentgateway/releases/tag/v1.6.0), 2026-10-02 |
| Agent Router (formerly Envoy AI Gateway) | `toolSelector`; CEL on `request.mcp.params` | Yes (regex) | No | [docs](https://theagentrouter.ai/docs/capabilities/mcp/), acc. |
| IBM ContextForge | `tool_pre_invoke` hooks, 40+ plugins | Yes | No SQL plugin | [plugins](https://ibm.github.io/mcp-context-forge/using/plugins/), acc. |
| Obot, MintMCP, Runlayer, Lasso, TrueFoundry | RBAC, catalog approvals, PII/secret scanning | Partly | No | [Obot](https://obot.ai/), [Runlayer](https://www.runlayer.com/), [TrueFoundry](https://www.truefoundry.com/blog/mcp-access-control) (2026-04-22) |
| Boomi (Lunar.dev MCPX) | Per-agent "hardened tool variants" | Yes | No | [Boomi](https://boomi.com/blog/lunar-dev-boomi-acquisition/), 2026-07-27 |

Analysis: gateways decide whether `execute_sql` may be called, not what the SQL does. Every
gateway vendor leaves SQL semantics to the server or the database. That is the gap pg_sage can
fill from the database side.

#### 2.6.7 MCP spec status and database-specific MCP incidents
- **Spec revisions:**
  - 2025-06-18 made MCP servers OAuth resource servers and required resource indicators
    ([changelog](https://modelcontextprotocol.io/specification/2025-06-18/changelog), 2025-06-18).
  - 2025-11-25 added CIMD and incremental consent
    ([changelog](https://modelcontextprotocol.io/specification/2025-11-25/changelog), 2025-11-25).
  - 2026-07-28 made the protocol stateless, deprecated Dynamic Client Registration and added
    `iss` validation (§1.6).
  - In the current authorization spec, authorization is optional; stdio servers should not use
    it; servers must validate token audience and must not pass tokens through. Scopes have no
    tool- or argument-level meaning
    ([authorization](https://modelcontextprotocol.io/specification/2026-07-28/basic/authorization),
    2026-07-28).
- **Incidents:**
  - The Supabase MCP prompt-injection leak (General Analysis, 2025-07-08;
    [Simon Willison](https://simonwillison.net/2025/Jul/6/supabase-mcp-lethal-trifecta/),
    2025-07-06).
  - In the archived Anthropic Postgres reference server, `COMMIT;` broke out of the read-only
    transaction. The package still had about 21k weekly npm downloads (Datadog, 2025-08-21).
  - The two AWS CVEs (§2.4.1).
  - A February 2026 Vercel breach reportedly began with a third-party AI tool that held standing
    OAuth access. The details are UNVERIFIED and come from vendor commentary
    ([StrongDM](https://discover.strongdm.com/blog/when-ai-tools-get-standing-access-lessons-from-the-vercel-breach),
    2026-05-12).
- **Governance demand:** Databricks reports that organizations using AI governance tools put more
  than 12× more AI projects into production (Databricks, 2026-01-27).

### 2.7 Agent memory and state on Postgres

#### 2.7.1 Letta (formerly MemGPT)
- **V1 server (now retired):**
  - Self-hosting needed Postgres with pgvector
    ([docs](https://docs.letta.com/guides/selfhosting/postgres/), acc.).
  - Passages were stored as `Vector(4096)`. pgvector cannot ANN-index more than 2,000
    dimensions, so archival search was effectively a scan (analysis from
    [constants](https://github.com/letta-ai/letta/blob/archive/letta/constants.py) and the pgvector
    README, acc.).
  - It shipped about 150 Alembic migrations
    ([migrations](https://github.com/letta-ai/letta/tree/archive/alembic/versions), acc.).
- **2026 changes:**
  - Memory moved to git-backed "Context Repositories"
    ([Letta](https://www.letta.com/blog/context-repositories/), 2026-02-12).
  - Letta Code became the flagship (Letta, 2026-03-16).
  - The V1 server's last release was 0.16.8 on 2026-05-14
    ([releases](https://github.com/letta-ai/letta/releases.atom)).
- **Complaints:** pooling and timeout issues with external Postgres, for example on Railway
  ([issues](https://github.com/letta-ai/letta/issues?q=is%3Aissue+postgres), acc.).
- **Pricing:** Pro $20/month; API plan $20 plus $0.10 per active agent
  ([pricing](https://docs.letta.com/letta-code/pricing), acc.).

#### 2.7.2 Zep and Graphiti
- **Zep Community Edition:** ended 2025-04-02
  ([Zep](https://www.getzep.com/blog/announcing-a-new-direction-for-zeps-open-source-strategy/),
  2025-04-02).
- **Graphiti:** runs on Neo4j, FalkorDB or Neptune; Kuzu is deprecated. Requests for a Postgres
  backend were closed as not planned
  ([#779](https://github.com/getzep/graphiti/issues/779), 2025-07-28;
  [#1095](https://github.com/getzep/graphiti/issues/1095), 2025-12-06).
- **Zep Cloud:** Flex is $125/month ([pricing](https://www.getzep.com/pricing), acc.).

#### 2.7.3 mem0 and OpenMemory
- **Backends:** about 26, including pgvector and Supabase; the default is Qdrant
  ([docs](https://docs.mem0.ai/components/vectordbs/overview), acc.).
- **v2/v3 (2026-04-14):** added BM25 and entity hybrid retrieval and removed every graph store
  from the open-source build ([changelog](https://docs.mem0.ai/changelog);
  [migration](https://docs.mem0.ai/migration/oss-v2-to-v3), acc.).
- **pgvector adapter bugs in 2026:**
  - Inverted cosine ranking ([#6883](https://github.com/mem0ai/mem0/issues/6883), 2026-08-10).
  - Dropped concurrent writes, a hard-coded `public` schema, and crashes on disconnect
    ([issues](https://github.com/mem0ai/mem0/issues?q=is%3Aissue+pgvector), acc.).
- **Funding:** $24M in total
  ([TechCrunch](https://techcrunch.com/2025/10/28/mem0-raises-24m-from-yc-peak-xv-and-basis-set-to-build-the-memory-layer-for-ai-apps/),
  2025-10-28).

#### 2.7.4 LangGraph checkpointers and store
- **Connection requirements:** PostgresSaver needs `autocommit=True`, `row_factory=dict_row` and
  `prepare_threshold=0`, and cannot use pipeline mode with a pool
  ([source](https://github.com/langchain-ai/langgraph/blob/main/libs/checkpoint-postgres/langgraph/checkpoint/postgres/__init__.py),
  acc.).
- **Schema:**
  - `checkpoints` (JSONB), `checkpoint_blobs` and `checkpoint_writes` (BYTEA), plus a migrations
    table ([schema](https://github.com/langchain-ai/langgraph/blob/main/libs/checkpoint-postgres/langgraph/checkpoint/postgres/base.py),
    acc.).
  - The store adds `expires_at` with a partial index, a TTL sweeper, and hnsw, ivfflat or flat
    vector indexes
    ([store](https://github.com/langchain-ai/langgraph/blob/main/libs/checkpoint-postgres/langgraph/store/postgres/base.py),
    acc.).
- **Retention:**
  - The open-source savers have no prune (#8531, 2026-08-05).
  - Checkpoint TTL exists only on Agent Server
    ([docs](https://docs.langchain.com/langsmith/configure-ttl), acc.), and only for checkpoints
    created after TTL is enabled
    ([support](https://support.langchain.com/articles/6253531756-understanding-checkpointers-databases-api-memory-and-ttl),
    2025-11-23).
- **Growth:** one production instance held 224 GB across 3.8M checkpoint rows. Keeping 7 days of
  history deleted about 25% of rows; keeping 2 days deleted about 67% (aegra PR, 2026-05-07).
- **Agent Server requirements:** Postgres 14+ with btree_gin, btree_gist, pgcrypto, citext, ltree
  and pg_trgm, plus Redis 6.2+
  ([docs](https://docs.langchain.com/langsmith/self-host-dependency-versions), acc.).

#### 2.7.5 Other frameworks' session stores

| Framework | What it stores in SQL | Notable behavior | Source |
|---|---|---|---|
| OpenAI Agents SDK | `agent_sessions`, `agent_messages` (JSON as text) | Index on (session_id, created_at); cascading delete | [SQLAlchemySession](https://github.com/openai/openai-agents-python/blob/main/src/agents/extensions/memory/sqlalchemy_session.py), acc. |
| Google ADK | Sessions, events, state | `SELECT … FOR UPDATE` on each append; v1.22.0 schema migration; no pruning | [source](https://github.com/google/adk-python/blob/main/src/google/adk/sessions/database_session_service.py), acc. |
| Microsoft Agent Framework / Semantic Kernel | pgvector collections | HNSW only; no hybrid search; preview connector | [Learn](https://learn.microsoft.com/en-us/semantic-kernel/concepts/vector-store-connectors/out-of-the-box-connectors/postgres-connector), 2026-07-27 |
| CrewAI | LanceDB by default | Default 3072-dimension embeddings | [docs](https://docs.crewai.com/en/concepts/memory), acc. |
| LlamaIndex | PGVectorStore, plus document, index and chat stores | Hybrid search; JSONB metadata filters | [docs](https://developers.llamaindex.ai/python/examples/vector_stores/postgres/), acc. |
| Pydantic AI, Vercel AI SDK | Your own tables | You write persistence yourself | [Pydantic](https://pydantic.dev/docs/ai/core-concepts/message-history/), [AI SDK](https://ai-sdk.dev/docs/ai-sdk-ui/chatbot-message-persistence), acc. |

#### 2.7.6 pgvector and related extensions
- **pgvector** ([changelog](https://github.com/pgvector/pgvector/blob/master/CHANGELOG.md)):
  - 0.8.0 added iterative scans (2024-10-30).
  - 0.8.3 and 0.8.4 fixed HNSW corruption and "graph not repaired" problems during vacuum
    (2026-06-17 and 2026-06-30).
  - 0.8.7 fixed the IVFFlat build overflow (2026-10-01).
  - Index dimension limits: `vector` 2,000 and `halfvec` 4,000
    ([README](https://github.com/pgvector/pgvector), acc.).
- **Other extensions:**
  - pgvectorscale (PostgreSQL License) 0.9.1 shipped on 2026-09-04.
  - pg_textsearch (PostgreSQL License) reached GA on 2026-03-27.
  - ParadeDB pg_search is AGPL-3.0 or commercial
    ([GitHub](https://github.com/paradedb/paradedb), acc.).
  - VectorChord is AGPLv3 or ELv2 ([GitHub](https://github.com/tensorchord/VectorChord), acc.).
  - pgai is archived (§2.1.4).
- **Critique:** "The Case Against pgvector" cites 10+ GB index builds, trade-offs for real-time
  inserts, lost recall from post-filtering, and planner misestimates
  ([Alex Jacobs](https://alex-jacobs.com/posts/the-case-against-pgvector/), 2025-10-29). It drew
  381 points on HN ([HN](https://news.ycombinator.com/item?id=45798479), 2025-11-03).

#### 2.7.7 Hosted memory services competing for the workload
- **AWS AgentCore Memory:**
  - GA on 2025-10-13 ([AWS](https://aws.amazon.com/blogs/machine-learning/amazon-bedrock-agentcore-is-now-generally-available/)).
  - Short-term memory moves to $1.00 per GB ingested on 2026-10-06. Long-term memory costs $0.75
    per 1,000 records per month (AgentCore pricing, acc.).
- **Vertex AI Memory Bank:** GA on 2025-12-16; billing started on 2026-01-28 (Vertex release
  notes).
- **Azure Foundry Memory:** preview, with item-level CRUD, TTL, and 10k memories per scope
  ([Learn](https://learn.microsoft.com/en-us/azure/foundry/agents/concepts/what-is-memory),
  2026-06-02).
- **Lakebase-backed Agent Bricks memory:** see §2.1.2.

#### 2.7.8 Other Postgres-native memory projects
- **Memori** (formerly GibsonAI; Apache-2.0, 17.1k stars): bring-your-own database, including
  PostgreSQL ([BYODB docs](https://memorilabs.ai/docs/memori-byodb/), acc.). gibsonai.com now
  redirects to memorilabs.ai, which suggests a pivot (inferred from redirects).
- **Cognee:** its Postgres graph store is labeled a demo
  ([GitHub](https://github.com/topoteretes/cognee), acc.).
- **MemOS:** no Postgres support ([GitHub](https://github.com/MemTensor/MemOS), acc.).

#### 2.7.9 What agent memory and state need from Postgres
1. **`vector`, and often more extensions.** LangGraph's Agent Server also needs btree_gin,
   btree_gist, pgcrypto, citext, ltree and pg_trgm (§2.7.4).
2. **Lexical search next to vectors.** mem0 v3 adds BM25 and entity matching, and pg_textsearch
   or pg_search provide BM25 (§2.7.3, §2.7.6).
3. **Wide JSONB and BYTEA rows, so heavy TOAST traffic.** pgvector suggests
   `SET STORAGE PLAIN` for vectors (§2.7.4, §2.7.6).
4. **Append-heavy session tables with ordering indexes and row locks** (ADK, OpenAI Agents SDK;
   §2.7.5).
5. **Schema design shaped by dimension limits.** Use `halfvec` beyond 2,000 dimensions; a
   3072-dimension default cannot be HNSW-indexed as `vector` (§2.7.5, §2.7.6).
6. **Memory and CPU headroom for index builds.** Neon advises keeping `maintenance_work_mem` at or
   below 50–60% of RAM ([Neon](https://neon.com/docs/extensions/pgvector), acc.).
7. **Filtered ANN per user or tenant.** This needs iterative scans and tuning of
   `hnsw.max_scan_tuples` (§2.7.6).
8. **Tenant isolation.** RLS adds latency
   ([Supabase](https://supabase.com/docs/guides/ai/rag-with-permissions), acc.), and
   pg_textsearch statistics leak across RLS boundaries (§2.1.4).
9. **TTL columns and sweepers.** Only some stores ship them (§2.7.4).
10. **Hard deletes for "forget me" and PII erasure.** Dead tuples persist until vacuum, and
    `VACUUM FULL` locks the table
    ([PostgreSQL docs](https://www.postgresql.org/docs/current/routine-vacuuming.html), acc.).
    Analysis: erasure also depends on backup and PITR retention, and none of the frameworks
    reviewed documents this.
11. **Runtime DDL privileges.** `setup()` methods, Alembic migrations and on-the-fly collection
    creation all need them (§2.7.1, §2.7.3, §2.7.4).
12. **Queue-like hot rows.** The Agent Server task queue and DBOS checkpoints are examples
    (§2.7.4, §2.2.5).

#### 2.7.10 Operational pain reported
- Checkpoints grow without bound and the open-source savers have no prune (aegra, 2026-05-07;
  #8531, 2026-08-05).
- Deletes leave orphaned rows: aegra checkpoints
  ([#288](https://github.com/aegra/aegra/issues/288), 2026-04-04) and Letta git-sync blocks
  (§2.7.1).
- TTL added later does not clean old history, and its configuration is poorly documented
  ([forum](https://forum.langchain.com/t/how-to-configure-thread-checkpointer-ttl-when-deploying-the-langgraph-server-with-docker-using-environment-variables/4253),
  2026-07-27).
- HNSW maintenance bugs: slow vacuums, recall loss from dead tuples, "graph not repaired", and
  slow inserts after VACUUM ([issues](https://github.com/pgvector/pgvector/issues?q=is%3Aissue+vacuum+hnsw),
  acc.).
- Instability behind poolers and on managed Postgres: SSL drops on the Supabase pooler
  ([langgraph #5675](https://github.com/langchain-ai/langgraph/issues/5675), 2025-07-26) and Letta
  on Railway (§2.7.1).
- Correctness bugs in libraries' pgvector adapters (mem0, §2.7.3).
- Churn and forced migrations: Letta V1 retired, mem0's graph stores removed, the ADK schema
  migration, pgai archived, Zep CE ended (above).

---

## 3. Gap matrix

**Legend:**
- **Y:** documented and available.
- **P:** partial, limited, preview-only or client-side.
- **N:** documented as absent, or clearly outside the product.
- **n/d:** not documented in the sources reviewed.
- **—:** not applicable.

Cells summarize the cited profiles in §2. The notes under each table carry the evidence for cells
that are not obvious. pg_sage cells describe the code and docs in this branch as of 2026-10-05.

### 3A. Provisioning and branching for agents

| Product | P1 API/CLI | P2 via MCP | P3 claim/TTL | P4 CoW data branch | P5 scale to 0 | P6 BYO estate | P7 OSS self-host | P8 spend cap/attribution |
|---|---|---|---|---|---|---|---|---|
| Neon | Y | Y | Y | Y | Y | N | P | n/d |
| Lakebase | Y | n/d | n/d | Y | Y | N | N | n/d |
| Supabase | Y | Y | Y | N | P | N | P | P |
| Tiger Data | Y | Y | n/d | Y | n/d | N | N | P |
| Xata (cloud + OSS) | Y | Y | P | Y | Y | P | Y | n/d |
| PlanetScale | Y | n/d | N | N | n/d | N | N | n/d |
| Prisma Postgres | Y | Y | Y | N | n/d | N | N | Y |
| Turso (SQLite) | Y | Y | P | P | Y | N | P | n/d |
| AWS Aurora/RDS | Y | P | N | Y | Y | N | N | P |
| Google Cloud SQL/AlloyDB | Y | Y | N | P | n/d | N | N | n/d |
| Azure PG/HorizonDB | Y | P | N | N | n/d | N | N | n/d |
| DBLab (postgres.ai) | Y | n/d | P | Y | — | Y | Y | N |
| pgEdge Starfleet | Y | n/d | n/d | Y | n/d | P | P | n/d |
| **pg_sage AgentDB today** | Y | **N** | P | P | N | Y | Y | P |

Notes:
- **Neon:** P7 is P because the engine is Apache-2.0 but public development is dormant (§2.1.1).
- **Supabase:**
  - P4 is N because branches hold schema and seed, and copying data is optional and not CoW.
  - P5 is P: Nano instances scale to zero for select partners only, and free projects pause.
  - P8 is P: MCP prompts for confirmation before paid resources (§2.1.3).
- **Tiger Data:** P8 is P because Ghost had hard spending caps, but Ghost is winding down (§2.1.4).
- **Xata:**
  - P6 is P because agents work on branches of production data replicated in (§2.1.5).
  - P7 is Y with a caveat: Xatastor and the security features for untrusted multi-tenancy are
    closed.
- **Prisma:** P8 is Y because paid plans have spend limits (§2.1.7).
- **AWS:**
  - P2 is P: the AWS MCP Server's `call_aws` reaches RDS APIs, but there is no dedicated awslabs
    control-plane server.
  - P8 is P: only DSQL has DPU cost tracking (§2.4.1).
- **Google:** P4 is P because fast clone is same-zone only (§2.4.2).
- **pg_sage:**
  - P1: REST request, policy and approval, then provision. Local provisioning executes; cloud is
    dry-run or gated live (`docs/agent-db-deployments.md:8-22`).
  - P2: `sidecar/internal/mcp` has no AgentDB tools (grep, 2026-10-05).
  - P3: leases, renewals and a durable cleanup claim, but no flow where a human claims the
    database (`docs/agent-db-deployments.md:300-312`).
  - P4: branching is delegated to Lakebase, Neon and Supabase profiles, and a clone `Provider`
    has DBLab and snapshot adapters (`sidecar/internal/clone/`).
  - P8: `budget_usd` and cost samples exist, but earlier internal research found cost
    self-reported, not metered (`research/v2_agentdb_at_scale.md:11`).

### 3B. Safety and governance of agent access

| Product | G1 read-only/scoped | G2 server-side SQL policy | G3 human approval | G4 agent identity | G5 short-lived DB creds | G6 audit names agent | G7 masking/anonymized copies |
|---|---|---|---|---|---|---|---|
| Neon | Y | N | P | P | n/d | n/d | n/d |
| Lakebase | n/d | N | P | n/d | n/d | P | n/d |
| Supabase | Y | P | Y | P | n/d | P | n/d |
| Tiger Data | Y | N | n/d | n/d | n/d | n/d | n/d |
| Xata | Y | P | P | n/d | Y | n/d | Y |
| PlanetScale | Y | P | Y | n/d | Y | P | n/d |
| Prisma Postgres | n/d | N | Y | Y | P | Y | n/d |
| AWS (MCP, AgentCore, IAM) | P | N | P | Y | Y | Y | N |
| Google (managed MCP, IAM) | Y | N | n/d | P | n/d | Y | N |
| Azure (MCP, Entra, SRE Agent) | P | N | P | Y | Y | Y | N |
| Formal | Y | **Y** | P | P | P | Y | Y |
| StrongDM/Delinea | Y | **Y** | Y | Y | Y | Y | N |
| Teleport | Y | N | Y | P | Y | Y | N |
| Bytebase | Y | Y (changes) | Y | Y | P | Y | Y |
| MCP gateways (typical) | Y | N | P | P | n/d | Y | N |
| **pg_sage today** | Y | P | Y | Y | **P** | Y | **P** |

Notes:
- **Neon:** G3 is P because the migration tools work in two steps, prepare then complete
  (§2.1.1).
- **Lakebase:** G3 is P because Genie ZeroOps approvals are in preview with Lakebase on the
  roadmap, and the Replit flow uses team approval (§2.1.2).
- **Supabase:** G2 is P because of the confirmation prompts on destructive SQL and the
  experimental app-level MCP governed by RLS (§2.1.3).
- **Xata:** G3 is P because `confirm=true` is a flag the agent sets. Whether a human sees it
  depends on the client (§2.1.5).
- **PlanetScale:** G2 is P because DML without WHERE and TRUNCATE are blocked (§2.1.6).
- **AWS:** G1 is P because the MCP read-only guard runs client-side and had two CVEs (§2.4.1).
- **Formal and StrongDM/Delinea** are the only Y entries for G2. Both are commercial proxies in
  the data path (§2.6.1, §2.6.2).
- **pg_sage:**
  - G1: MCP scopes are `read` and `propose`, and agent tokens can never hold `approve`
    (`docs/mcp.md`, token table).
  - G2 is P: the policy gate governs pg_sage's own typed actions and AgentDB requests, but
    pg_sage is not in the agents' SQL path.
  - G3: live create and destroy need a single-use authorization bound to the plan hash, cost
    estimate and policy generation (`docs/agent-db-deployments.md:258-275`).
  - G4: tenant-bound `agt_` agent tokens (`docs/agent-db-deployments.md:86-94`).
  - G5 is P: tokens expire, but database credentials come from `secret_ref` or inline values.
    No per-agent role minting exists: no `CREATE ROLE` or `VALID UNTIL` appears in the
    sidecar's Go code (grep, 2026-10-05).
  - G7 is P: intake denies sensitive data without a `masking_policy_id`
    (`sidecar/internal/agentdb/policy.go:23-24`), but no masking implementation was found
    (grep, 2026-10-05).

### 3C. Operations for agent databases (the AI DBA half)

| Product | O1 any-Postgres fleet | O2 advice | O3 autonomous apply | O4 verify + rollback | O5 test on clone first | O6 memory-store hygiene | O7 reclaim idle/orphans | O8 OSS self-host |
|---|---|---|---|---|---|---|---|---|
| Neon | N | P | N | N | P | N | P | N |
| Lakebase | N | Y | P | n/d | P | n/d | P | N |
| Supabase | N | P | N | N | P | N | P | P |
| AWS (Insights, DevOps Agent) | P | P | N | N | N | N | P | N |
| Google (Database Center) | P | Y | N | N | P | N | n/d | N |
| Azure (autonomous tuning) | n/d | Y | N | N | N | N | n/d | N |
| postgres.ai | Y | Y | N | N | Y | N | N | P |
| pganalyze | Y | Y | N | N | P | N | N | P |
| Datadog DBM | Y | Y | N | N | N | N | N | N |
| pgEdge AI DBA Workbench | Y | Y | N | N | N | N | N | Y |
| Postgres MCP Pro | P | Y | N | N | P | N | N | Y |
| EDB agentic database | N | Y | Y | P | n/d | n/d | n/d | P |
| DBtune | Y | P | Y | P | N | N | N | P |
| Bytebase | N | P | P | n/d | P | N | N | P |
| **pg_sage today** | Y | Y | P | Y | P | P | P | Y |

Notes:
- **Lakebase:** O3 is P because autonomous operations are available now but described as
  monitoring, index proposals and recovery help, and Genie ZeroOps is approval-gated and in
  preview (§2.1.2).
- **Google:** O5 is P because assessments clone and benchmark, but are in preview and only
  report (§2.4.2).
- **Azure:** O3 is N because tuning only runs in REPORT mode (§2.4.3).
- **postgres.ai:** O3 is N because Autopilot has not been verified as shipped (§2.5.1).
- **EDB:** O1 is N because it covers Hybrid Manager-managed Postgres only (§2.5.5).
- **DBtune:** O4 is P because it has regression thresholds but no documented rollback (§2.5.6).
- **pg_sage:**
  - O1: README lists Lakebase, Cloud SQL, AlloyDB, Aurora, RDS, Neon, Supabase and self-managed.
  - O3 is P: the trust-ramped executor runs on the primary fleet, but the AgentDB fleet only
    collects snapshots, with no analyzer or executor attached
    (`docs/agent-db-deployments.md:314-319`).
  - O4: rollback metadata and verification (README).
  - O5 is P: clone providers exist (`sidecar/internal/clone/`); wiring was not verified in this
    pass.
  - O6 is P: vector and JSONB tuning hints, plus a read-only vector lab
    (`docs/agent-db-deployments.md:277-293`, README).
  - O7 is P: lease expiry leads to archive, then destroy gated on a verified backup. The
    monitoring worker is "not wired yet" (`docs/agent-db-deployments.md:300-327`).

### 3D. Matrix read-out
- **No product is Y on all of** any-Postgres (O1), open-source self-hosted (O8), autonomous apply
  (O3) and verify-plus-rollback (O4). pg_sage is the only row near that combination. Its
  weaknesses there are on the AgentDB side (O3, O7), not in the core.
- **SQL-level server-side policy (G2)** is Y only for two commercial proxies and for Bytebase's
  change review. Nobody offers it for agent traffic in a way that is open source and native to
  the database.
- **pg_sage's largest gaps against the market:**
  - MCP-native provisioning (P2).
  - A flow where a human claims the database (P3).
  - Minted per-agent database credentials (G5).
  - A real masking implementation (G7).
  - Operations attached to agent databases (O3, O7).
  - Memory-store hygiene (O6).
- These are all buildable on pg_sage's existing primitives. None needs a storage engine.

---

## 4. Where an open-source, self-hosted, any-Postgres AI DBA wins, loses, or should integrate

### 4.0 Verdict

pg_sage should not try to be a "database for agents". That market is owned by companies with
storage engines, free tiers, distribution deals and fresh capital: Supabase (70% of 4M+ databases
a month, $150M more on 2026-10-02), Neon/Databricks (80% of databases created by agents) and
Prisma (create-db with a 24 h TTL). It is consolidating weekly (§0, finding 2). Standalone attempts
are dying: Tiger's Ghost is winding down and Instant Cloud is closing (§2.1.4, §2.2.3).

pg_sage can win as the **open-source operator and governor of the Postgres that agents create and
touch, in estates pg_sage does not host**: RDS/Aurora, Cloud SQL/AlloyDB, Azure, self-managed
Postgres, plus Lakebase, Neon, Supabase and Xata branches. The defensible shape has two parts:
(a) give agents least-privilege, expiring access and governed, backup-safe lifecycles on
substrates the enterprise already pays for, and (b) operate the resulting fleet with
evidence-backed, reversible autonomy. Everything else should be an integration.

### 4.1 Where it can win, ranked

**W1. Operating agent-created and agent-touched databases in existing estates.** This is the AI
DBA core aimed at agent fleets.
- **Evidence:**
  - Autonomy exists only inside walled gardens: EDB in Hybrid Manager only, Lakebase autonomous
    operations in Lakebase only (§2.5.5, §2.1.2).
  - Hyperscaler tuning only reports: Azure REPORT mode, Google assessments in preview (§2.4.2,
    §2.4.3). AWS documents no automatic index create-and-revert for Aurora or RDS, and Database
    Insights gives no tuning recommendations (§2.4.1).
  - Specialist vendors only advise: pganalyze, Datadog, pgEdge Workbench (§2.5).
  - The open-source attempts died or stalled: Xata Agent, Postgres MCP Pro, pgai, OtterTune
    (§2.5.14).
  - Memory stores create concrete, recurring DBA work: checkpoint bloat, HNSW vacuum, pgvector
    CVEs, orphan rows (§2.7.9, §2.7.10).
- **Why pg_sage specifically:** it already has a gated, trust-ramped executor with rollback
  metadata and works on any Postgres (README).
- **What is missing:** the AgentDB fleet only collects snapshots
  (`docs/agent-db-deployments.md:314-319`).
- **Build:**
  - Attach the analyzer and executor to AgentDB deployments.
  - Add memory-store playbooks: checkpoint TTL and prune, orphan cleanup, HNSW reindex and vacuum
    order, a `halfvec` advisor, extension CVE drift.
  - Add zombie-branch garbage collection across providers, so a sweep catches what cleanup misses
    (the 2,847-branch case, §2.1.1).

**W2. Least privilege native to the database: provision access, not instances.**
- **Evidence:**
  - Client-side SQL guards fail: two AWS CVEs in 2026, the Anthropic reference server, and a
    reported Postgres MCP Pro bypass (§2.4.1, §2.6.7, §2.5.3).
  - Microsoft's new MCP server is read-write by default (§2.4.3).
  - AWS's own remediation advice is least-privilege database roles (§2.4.1).
  - Gateways do not parse SQL (§2.6.6). Only two commercial proxies enforce at the SQL level
    (§2.6.1, §2.6.2).
  - Teleport's auto-provisioned users and Vault's dynamic credentials prove the pattern, but
    carry no DBA-aware defaults (§2.6.3, §2.6.5).
- **Build:** per-agent-run roles with:
  - `VALID UNTIL`, `statement_timeout`, `idle_in_transaction_session_timeout` and a connection
    limit.
  - No DDL by default, RLS enabled on tenant tables, and `application_name` or audit tags.
  - Revoke and terminate sessions when the lease expires.
  - Map identities from OIDC, Entra Agent ID, Okta or SPIFFE, and use PG18 OAuth where available
    (§1.6).
- **Limit:** where policy requires inline masking or per-statement blocking, integrate with
  Formal or Delinea rather than compete.

**W3. A governed provisioning broker over substrates enterprises already pay for.** This is a
narrowed AgentDB.
- **Evidence:**
  - Provisioning today is single-cloud (Google's managed MCP `create_instance`/`clone_instance`;
    Aurora express configuration), tied to one platform (Prisma enrollment, Supabase prompts),
    or aimed at app builders (§1.2).
  - None spans RDS, Cloud SQL, Lakebase, Neon, Xata OSS and DBLab with one sequence of request,
    policy, approval, cost, TTL, then teardown verified against a backup.
  - The two signature incidents failed in the control plane: an over-privileged token, backups in
    the same volume, and no gate on destroy (§2.3.2, §2.3.3).
  - pg_sage's authorization binding (plan hash, estimate, policy generation, single use) and its
    backup-verified teardown are stricter than anything documented by the platforms
    (`docs/agent-db-deployments.md:258-312`).
- **Build:**
  - AgentDB MCP tools: request, status, extend, claim, teardown.
  - Runners for Aurora CoW clones, Cloud SQL fast clone (or its managed MCP), Xata OSS and DBLab.
  - A human-claim flow like Neon's and Prisma's.
  - Cost metered from provider APIs instead of agents reporting it themselves.

**W4. Evidence-backed, reversible autonomy as the differentiator, not "approvals".**
- **Evidence:**
  - Approvals became table stakes in 2026 (§0, finding 5).
  - Analysis, not exhaustive: no source reviewed for this document describes automatic
    verify-and-revert of maintenance actions on Postgres. EDB documents approval modes and
    maintenance windows but no rollback (§2.5.5). DBtune documents regression thresholds but no
    rollback (§2.5.6).
  - Genie ZeroOps validates in a sandbox but is in private preview and does not support Lakebase
    yet (§2.1.2). Google's clone-then-benchmark only reports (§2.4.2).
- **Build:**
  - Validate on a clone (DBLab, Aurora clone or a Lakebase/Xata branch).
  - Verify after applying, and revert automatically for reversible classes such as indexes and
    settings.
  - Label non-reversible classes explicitly and require approval for them. Atlas is right that
    reverts do not work for data (§2.5.11).

**W5. Be the Postgres specialist that generalist agents call over MCP.**
- **Evidence:**
  - AWS DevOps Agent lists MCP among its integrations
    ([AWS](https://aws.amazon.com/blogs/mt/announcing-general-availability-of-aws-devops-agent/),
    2026-03-31), and Azure SRE Agent supports MCP connectors
    ([Learn](https://learn.microsoft.com/en-us/azure/sre-agent/overview), 2026-08-26).
  - None of the generalist SRE agents documents deep Postgres maintenance (§2.5.13).
  - pganalyze's MCP, which never connects to the database, shows the safety posture buyers like
    (§2.5.2).
- **Build:**
  - Support MCP spec 2026-07-28 (stateless, CIMD, `iss` validation, `Mcp-Method` and `Mcp-Name`
    headers) so pg_sage sits cleanly behind gateways (§1.6).
  - Accept Entra and Okta tokens.
  - Keep agent scopes at `read` and `propose`.

### 4.2 Where it would lose (do not compete)

| Zone | Who owns it | Why pg_sage loses | Evidence |
|---|---|---|---|
| Storage and branching engines | Neon/Lakebase, Xata, Tiger, Vela, Aurora | Capital and storage tech; CoW is a commodity | §1.1 |
| App-builder distribution | Supabase, Neon, Prisma, Vercel Marketplace, Replit | Default integrations in Lovable, Bolt, v0, Replit and Claude Code flows | §0 finding 1, §2.3 |
| Tiny per-agent databases | Turso (now Supabase), Cloudflare Durable Objects and D1, agentdb.dev, PGlite | SQLite-file economics; millions of databases per server | §2.2 |
| Inline SQL proxies, masking, enterprise identity | Formal, Delinea, Okta, Entra, Idira, Cisco | Compliance buyers, sales teams, consolidation | §2.6 |
| Managed agent memory | AgentCore Memory, Memory Bank, Foundry Memory, mem0 and Zep clouds | Hosted, bundled, priced per record or GB | §2.7.7 |
| Schema change management | Bytebase, Atlas, Liquibase | Mature review rules and GitOps; agent skills already shipped | §2.5.10–§2.5.12 |
| In-cloud generalist AI SRE | AWS DevOps Agent, Azure SRE Agent | Bundled support credits; native telemetry | §2.4 |
| Single-platform autonomy | Lakebase autonomous operations, EDB agentic database | Native access; no install | §2.1.2, §2.5.5 |

### 4.3 Integrate instead of compete

| Area | Integrate with | How | Why |
|---|---|---|---|
| CoW substrates | Xata OSS, DBLab, Aurora clones, Cloud SQL fast clone, Lakebase, Neon | AgentDB provider runners behind one request and approval flow | Branching is a commodity; governance across them is not |
| Provisioning APIs | Google Cloud SQL managed MCP, AWS MCP Server, Neon Claimable, Supabase Management API | Call these as backends; never duplicate them | They are GA and maintained by their owners |
| Identity | Entra Agent ID, Okta Cross App Access, SPIFFE, Vault, PG18 OAuth | Map agent principals to minted Postgres roles | Identity is solved and consolidating |
| SQL proxies | Formal, StrongDM/Delinea | Export agent inventory and recommended policies | They own inline blocking and masking |
| MCP gateways | Docker, Kong, agentgateway, Pomerium, APIM | Spec 2026-07-28 compliance; scoped tools | Gateways route by tool name; pg_sage supplies SQL semantics from the database side |
| Change governance | Atlas, Bytebase, Liquibase | Hand off schema proposals and source-fix packets as PRs or plans | Mature review and approval flows |
| Observability | pganalyze MCP, Datadog MCP, CloudWatch Database Insights | Ingest evidence; do not rebuild dashboards | Saves effort; pg_sage's value is the action |
| Memory frameworks | LangGraph, mem0, ADK, OpenAI Agents SDK, DBOS | Recognize their schemas; apply TTL, prune and vector maintenance playbooks | Their operational pain is documented |

### 4.4 Implications for the AgentDB spec (competitive lens; analysis)

| Decision | What | Competitive reason |
|---|---|---|
| **Kill or freeze** | Anything that looks like a hosted "database for agents" for app builders, including a hosted provisioning API offered as a product | Ghost winding down, Instant closing, Supabase, Neon and Prisma owning distribution (§4.0) |
| **Narrow** | Instance-level RDS and Cloud SQL provisioning per agent run | Slow and costly next to Aurora CoW clones, Cloud SQL fast clone and Lakebase/Xata branches (§1.1). Keep instances for long-lived workloads; prefer clones and branches for runs |
| **Pivot** | From provisioning instances to provisioning access (roles, leases, revocation) plus operating the fleet | §4.1 W1 and W2 |
| **Keep and extend** | Request, approval and single-use authorization; backup-verified teardown; leases | Matches where the market converged and is stricter (§3B) |
| **Add** | AgentDB MCP tools, human-claim flow, per-agent roles, metered cost, memory-store playbooks, executor on the AgentDB fleet | The gaps in §3D |
| **Rename** | The user-facing "AgentDB" name | Collides with agentdb.dev (§2.2.7) |

### 4.5 Threats and timing
- **Platform-native AI DBA is spreading.** Lakebase has it now, Genie ZeroOps names Lakebase next,
  EDB ships it, Supabase's AI Advisor is growing, and HorizonDB index tuning is announced as
  coming (§2). Analysis: within about 12 months most managed platforms will offer advice inside the
  platform and some automatic apply. pg_sage's moat has to be working across estates, being open,
  and being verifiable.
- **The open-source AI DBA business model is the real risk.** OtterTune, the Xata Agent, Postgres
  MCP Pro and pgai died or stalled (§2.5.14). The surviving open-source players have corporate
  backing (pgEdge) or services revenue (postgres.ai) (§2.5.1, §2.5.4).
- **Distribution:** pg_sage has 13 GitHub stars (§1.7).
- **Security:**
  - Peer MCP servers shipped read-only bypasses (§2.4.1, §2.5.3). pg_sage must be designed so
    that bypassing its parser does not become a breach. The database role is the boundary.
  - The pgvector 0.8.7 CVE shows that extensions in agent fleets need version drift detection
    (§1.6).
- **License and naming:**
  - Analysis: AGPL-3.0 suits self-hosted enterprise use, but may deter platforms from embedding
    pg_sage. Convex (FSL), Liquibase (FSL) and Teleport (commercial binaries) moved toward
    source-available terms.
  - The AgentDB name collision (§2.2.7).

### 4.6 What would change this assessment
- **Xata OSS, pgEdge Workbench or postgres.ai ship an open-source AI DBA that acts** (postgres.ai
  "Autopilot" is on its roadmap, §2.5.1). pg_sage would then face a direct open-source rival with
  more distribution.
- **Supabase ships per-agent databases in customers' own clouds (BYOC) after Turso** (Copplestone
  cited BYOC, PR Newswire, 2026-10-02). That would narrow W3.
- **MCP adds argument-level authorization semantics to the spec.** Gateways could then start doing
  SQL-aware policy, which would narrow W2.
- **A hyperscaler offers cross-cloud Postgres governance.** No sign of this in the sources
  reviewed.

---

## 5. Sources

Format: URL, then title or description, then date. "acc." means undated, accessed 2026-10-05.
GitHub figures (stars, licenses, pushes, releases, archive flags, commit counts) come from
`api.github.com/repos/{owner}/{repo}`, `/releases` and `/commits?since=&until=`, queried on
2026-10-05. The local pg_sage facts come from this repository's files, cited in place.

### 5.1 Market framing, incidents and the Postgres substrate
- https://www.databricks.com/blog/enterprise-ai-agent-trends-top-use-cases-governance-evaluations-and-more — Databricks, enterprise AI agent trends — 2026-01-27
- https://ia.acs.org.au/article/2026/gone-in-9-seconds--ai-agent-deletes-company-database.html — ACS Information Age, PocketOS/Railway deletion — 2026-05-05
- https://www.theregister.com/2025/07/21/replit_saastr_vibe_coding_incident/ — The Register, Replit/SaaStr incident — 2025-07-21
- https://arxiv.org/abs/2609.02106 — Git4Data: database-native version control for AI agents — 2026-09-02
- https://www.postgresql.org/about/news/postgresql-18-released-3142/ — PostgreSQL 18 released — 2025-09-25
- https://www.postgresql.org/about/news/postgresql-19-beta-4-released-3386/ — PostgreSQL 19 Beta 4 — 2026-09-24
- https://www.postgresql.org/about/news/pgvector-087-released-3392/ — pgvector 0.8.7 security release — 2026-10-05
- https://www.postgresql.org/about/news/pgbouncer-1260-released-fixes-three-cves-3385/ — PgBouncer 1.26.0 — 2026-09-23
- https://www.postgresql.org/docs/current/routine-vacuuming.html — PostgreSQL docs, routine vacuuming — acc.
- https://ibl.ai/blog/agentic-database-layer-pgbot-postgres — ibl.ai, PGBot and the agentic database layer — 2026-08-15
- https://kendralittle.com/2026/06/29/ai-database-architecture-mcp-observability/ — Kendra Little, agents need better briefings — 2026-06-29

### 5.2 Neon and Databricks Lakebase
- https://www.databricks.com/company/newsroom/press-releases/databricks-agrees-acquire-neon-help-developers-deliver-ai-systems — Databricks agrees to acquire Neon — 2025-05-14
- https://www.databricks.com/blog/databricks-neon — Databricks + Neon — 2025-05-14
- https://www.cnbc.com/2025/05/14/databricks-is-buying-database-startup-neon-for-about-1-billion.html — CNBC, about $1B — 2025-05-14
- https://neon.com/ — Neon homepage — acc.
- https://neon.com/blog/major-compute-price-reduction-on-neon — Neon compute price cut — 2025-11-03
- https://neon.com/blog/wal-s3-lakebase-storage-for-the-era-of-agents — WAL + S3, Lakebase storage — 2026-08-24
- https://neon.com/docs/changelog/2026-09-18 — Neon changelog (backend GA) — 2026-09-18
- https://neon.com/programs/agents — Neon Agent Plan — acc.
- https://neon.com/blog/an-agent-provisions-a-neon-backend-a-human-claims-it-later — Claimable Neon — 2026-09-10
- https://neon.com/docs/reference/claimable-postgres — Claimable Postgres docs — acc.
- https://neon.com/docs/ai/neon-mcp-server — Neon MCP server docs — acc.
- https://neon.com/docs/introduction/architecture-overview — Neon architecture — acc.
- https://neon.com/pricing — Neon pricing — acc.
- https://neon.com/docs/introduction/plans — Neon plans — acc.
- https://neon.com/blog/the-hidden-ops-layer-of-agent-platforms — The hidden ops layer of agent platforms — 2025-11-04
- https://layerbase.com/blog/neon-after-databricks — Neon after Databricks (competitor view) — 2026-05-19
- https://www.databricks.com/company/newsroom/press-releases/databricks-launches-lakebase-new-class-operational-database-ai-apps — Lakebase launch — 2025-06-11
- https://docs.databricks.com/aws/en/release-notes/lakebase/ — Lakebase release notes — acc.
- https://community.databricks.com/t5/lakebase-articles/databricks-lakebase-is-now-generally-available/td-p/147678 — Lakebase GA post — 2026-02-09
- https://docs.databricks.com/aws/en/oltp/update-to-autoscaling-terraform — Lakebase Autoscaling Terraform migration — acc.
- https://learn.microsoft.com/en-us/azure/databricks/oltp/projects/manage-projects — Lakebase projects (Azure docs) — 2026-09-11
- https://www.databricks.com/company/newsroom/press-releases/databricks-launches-ltap-first-lake-transactionalanalytical — LTAP launch, Lakebase autonomous operations — 2026-06-16
- https://www.databricks.com/blog/agent-bricks-dais-2026 — Agent Bricks at DAIS 2026 — 2026-06-16
- https://www.databricks.com/blog/introducing-genie-zeroops — Genie ZeroOps — 2026-06-16
- https://www.databricks.com/blog/introducing-always-pricing-automatic-savings-databricks-lakebase — Lakebase Always-On pricing — 2026-05-27
- https://lakesentry.io/blog/databricks-lakebase/ — Lakebase pricing (third party) — 2026-07-23, updated 2026-10-01

### 5.3 Supabase, and Lovable and Bolt
- https://supabase.com/blog/supabase-series-e — Series E — 2025-10-03
- https://supabase.com/blog/supabase-series-f — Series F — 2026-06-04
- https://www.techtimes.com/articles/317950/20260607/open-source-database-supabase-hits-105-billion-ai-coding-boom-mints-new-decacorn.htm — TechTimes, $10.5B — 2026-06-05
- https://www.prnewswire.com/news-releases/supabase-announces-150m-in-new-funding-and-turso-acquisition-302896752.html — $150M and Turso acquisition — 2026-10-02
- https://supabase.com/blog/supabase-is-acquiring-turso — Supabase is acquiring Turso — 2026-10-02
- https://supabase.com/blog/remote-mcp-server — Remote MCP server — 2025-10-03
- https://supabase.com/docs/guides/getting-started/mcp — Supabase MCP docs — acc.
- https://supabase.com/blog/enterprise-managed-auth-for-the-supabase-mcp-server — Enterprise-managed MCP auth — 2026-08-24
- https://supabase.com/blog/select-2026-operate-with-confidence — Select 2026, operate with confidence — 2026-10-02
- https://supabase.com/blog/select-2026-build-anything — Select 2026, build anything — 2026-10-02
- https://supabase.com/blog/select-2026-scale-without-limits — Select 2026, scale without limits — 2026-10-02
- https://supabase.com/docs/guides/integrations/supabase-for-platforms — Supabase for Platforms — acc.
- https://supabase.com/blog/branching-without-git-is-now-the-default — Git-free branching default — 2026-05-04
- https://supabase.com/docs/guides/deployment/branching/dashboard — Dashboard branching docs — acc.
- https://supabase.com/pricing — Supabase pricing — acc.
- https://supabase.com/blog/multigres-v0-1-alpha — Multigres v0.1 alpha — 2026-06-04
- https://supabase.com/docs/guides/self-hosting — Self-hosting docs — acc.
- https://supabase.com/docs/guides/platform/sso — Supabase SSO — acc.
- https://supabase.com/docs/guides/platform/compute-and-disk — Supabase compute and disk — acc.
- https://www.generalanalysis.com/blog/supabase-mcp-blog — Supabase MCP data-leak write-up — 2025-07-08
- https://mattpalmer.io/posts/statement-on-CVE-2025-48757/ — CVE-2025-48757 statement — 2025-05-29
- https://supabase.com/blog/protecting-your-supabase-projects-from-npm-supply-chain-attacks — npm supply-chain notice — 2026-05-26
- https://supabase.com/blog/lovable-cloud-launch — Lovable Cloud + Supabase — 2025-09-29
- https://lovable.dev/guides/bolt-vs-replit-vs-lovable — Lovable's Bolt/Replit/Lovable comparison (secondary) — acc.

### 5.4 Tiger Data, Xata, PlanetScale, Prisma, Nile, Snowflake, Vela, pgEdge Starfleet
- https://www.tigerdata.com/blog/timescale-becomes-tigerdata — Timescale becomes Tiger Data — 2025-06-17
- https://www.tigerdata.com/blog/postgres-for-agents — Agentic Postgres launch — 2025-10-21
- https://www.tigerdata.com/ — Tiger Data homepage — acc.
- https://www.tigerdata.com/pricing — Tiger pricing — acc.
- https://www.tigerdata.com/blog/ai-agent-production-database-access — Agents on production databases — 2026-09-08
- https://github.com/timescale/tiger-cli — Tiger CLI — acc.
- https://github.com/timescale/pg_textsearch — pg_textsearch — acc.
- https://github.com/timescale/pgai — pgai (archived 2026-05-27) — acc.
- https://ghost.build/ — Ghost (winding down) — acc.
- https://ghost.build/whatsnew.html — Ghost changelog — acc.
- https://xata.io/blog/xata-postgres-with-data-branching-and-pii-anonymization — Xata relaunch — undated, acc.
- https://xata.io/blog/open-source-postgres-branching-copy-on-write — Xata open-source architecture — undated, acc.
- https://github.com/xataio/xata — Xata OSS platform (repo created 2026-04-15) — acc.
- https://xata.io/ — Xata homepage — acc.
- https://xata.io/blog — Xata blog index — acc.
- https://xata.io/blog/a-thousand-postgres-branches-for-1 — 1,000 branches for $1 — 2026-06-11
- https://xata.io/blog/introducing-the-xata-mcp-server — Xata MCP server 2.0 — 2026-08-26 (per blog index)
- https://xata.io/pricing — Xata pricing — acc.
- https://github.com/xataio/agent — Xata Agent (archived 2026-06-15) — acc.
- https://github.com/vbp1/pgxagent — XAgent fork — acc.
- https://github.com/xataio/pgroll — pgroll — acc.
- https://github.com/xataio/pgstream — pgstream — acc.
- https://planetscale.com/blog/planetscale-for-postgres — PlanetScale for Postgres preview — 2025-07-01
- https://planetscale.com/blog/planetscale-for-postgres-is-generally-available — Postgres GA — 2025-09-22
- https://planetscale.com/blog/5-dollar-planetscale — $5 PlanetScale — 2025-10-30
- https://planetscale.com/blog/introducing-neki — Introducing Neki — 2026-09-10
- https://planetscale.com/docs/connect/mcp — PlanetScale MCP — acc.
- https://planetscale.com/docs/postgres/branching — Postgres branching — acc.
- https://planetscale.com/docs/vitess/schema-changes/deploy-requests — Deploy requests (Vitess) — acc.
- https://planetscale.com/changelog — PlanetScale changelog — acc.
- https://planetscale.com/pricing — PlanetScale pricing — acc.
- https://planetscale.com/docs/enterprise/managed/overview — PlanetScale Managed — acc.
- https://planetscale.com/docs/security/sso — PlanetScale SSO — acc.
- https://planetscale.com/docs/security/audit-log — PlanetScale audit log — acc.
- https://www.prisma.io/blog/prisma-postgres-the-future-of-serverless-databases — Prisma Postgres GA — 2025-02-03, updated 2026-09-28
- https://www.prisma.io/blog/give-your-agent-a-database — create-db for agents — 2026-07-09
- https://www.prisma.io/blog/agents-md-for-databases — AGENTS.md for databases — 2026-07-17
- https://github.com/prisma/create-db — create-db repo — acc.
- https://www.prisma.io/docs/postgres/integrations/mcp-server — Prisma MCP docs — acc.
- https://www.prisma.io/docs/postgres — Prisma Postgres docs — acc.
- https://www.prisma.io/pricing — Prisma pricing — acc.
- https://www.prisma.io/changelog/2026-08-28 — Prisma Compute GA — 2026-08-28
- https://www.prisma.io/changelog/2026-10-02 — Agent enrollment and approvals — 2026-10-02
- https://news.ycombinator.com/item?id=41984184 — HN on Prisma Postgres — 2024-11-02
- https://www.thenile.dev/blog — Nile blog index — acc.
- https://www.thenile.dev/blog/funding-seed — Nile seed round — 2024-01-30
- https://www.thenile.dev/docs/getting-started/whatisnile — What is Nile — acc.
- https://www.thenile.dev/pricing — Nile pricing — acc.
- https://github.com/niledatabase/niledatabase — Nile repo — acc.
- https://github.com/niledatabase/nile-mcp-server — Nile MCP server — acc.
- https://www.snowflake.com/en/news/press-releases/snowflake-acquires-crunchy-data-to-bring-enterprise-ready-postgres-offering-to-the-ai-data-cloud/ — Snowflake acquires Crunchy Data — 2025-06-02
- https://techcrunch.com/2025/06/02/snowflake-to-acquire-database-startup-crunchy-data/ — TechCrunch on the Crunchy deal — 2025-06-02
- https://docs.snowflake.com/en/user-guide/snowflake-postgres/about — Snowflake Postgres docs — acc.
- https://github.com/Snowflake-Labs/pg_lake — pg_lake — acc.
- https://vela.run/blog/vela-open-source/ — Vela open source — 2026-02-09
- https://vela.run/blog — Vela blog index — acc.
- https://www.postgresql.org/about/news/pgedge-announces-pgedge-starfleet-a-new-postgres-cloud-platform-to-bridge-the-ai-prototype-to-production-chasm-3389/ — pgEdge Starfleet — 2026-09-28

### 5.5 Adjacent platforms and distribution channels
- https://turso.tech/blog/turso-is-joining-supabase — Turso is joining Supabase — 2026-10-02
- https://devnews.news/news/supabase-acquires-turso-sqlite-databases-for-agents/ — Dev News on Turso — 2026-10-03
- https://turso.tech/blog/agentfs — AgentFS — 2025-11-13
- https://github.com/tursodatabase/agentfs — AgentFS repo — acc.
- https://turso.tech/blog/giving-agents-their-own-turso-accounts-with-agentid — AgentID — 2026-08-26
- https://turso.tech/blog/let-your-agents-run-free-with-turso-accident-protection — Accident Protection — 2026-09-16
- https://turso.tech/blog/turso-0.8.0 — Turso 0.8 — 2026-09-29
- https://turso.tech/blog/a-new-modern-version-of-postgres-in-rust — Postgres in Rust on Turso — 2026-07-16
- https://turso.tech/pricing — Turso pricing — acc.
- https://siliconangle.com/2026/08/04/convex-reels-57m-ai-optimized-application-backend/ — Convex $57M — 2026-08-04
- https://news.convex.dev/meet-chef/ — Meet Chef — 2025-04-10
- https://docs.convex.dev/platform-apis — Convex Platform APIs — acc.
- https://github.com/get-convex/convex-backend/blob/main/LICENSE.md — Convex license — acc.
- https://github.com/get-convex/convex-backend/blob/main/self-hosted/README.md — Convex self-hosting — acc.
- https://www.convex.dev/pricing — Convex pricing — acc.
- https://www.instantdb.com/essays/instant_team_joins_openai — Instant team joins OpenAI — 2026-08-22
- https://www.instantdb.com/essays/architecture — Instant architecture — 2026-04-09
- https://github.com/instantdb/instant — Instant repo — acc.
- https://electric.ax/blog/2026/08/11/electric-joining-databricks — Electric joins Databricks — 2026-08-11
- https://electric.ax/blog/2026/06/25/pglite-reaches-10-million-weekly-downloads — PGlite 10M weekly downloads — 2026-06-25
- https://electric.ax/blog/2026/04/08/data-primitive-agent-loop — Durable Streams — 2026-04-08
- https://electric.ax/blog/2026/04/29/introducing-electric-agents — Electric Agents — 2026-04-29
- https://github.com/electric-sql/pglite — PGlite repo — acc.
- https://github.com/dbos-inc/dbos-transact-py — DBOS Transact (Python) — acc.
- https://www.dbos.dev/blog/whats-new-in-dbos-september-2026 — DBOS September update — 2026-09-29
- https://www.dbos.dev/blog/what-is-dbos-conductor — DBOS Conductor — 2026-08-20
- https://www.dbos.dev/pricing — DBOS pricing — acc.
- https://www.dolthub.com/blog/2026-08-06-doltgres-1-0/ — Doltgres 1.0 — 2026-08-06
- https://agentdb.dev/ — agentdb.dev — acc.
- https://developers.cloudflare.com/hyperdrive/ — Hyperdrive — 2026-06-22
- https://developers.cloudflare.com/hyperdrive/platform/pricing/ — Hyperdrive pricing — 2026-06-18
- https://blog.cloudflare.com/planetscale-postgres-workers/ — PlanetScale + Workers — 2025-09-25
- https://developers.cloudflare.com/d1/platform/limits/ — D1 limits — 2026-04-21
- https://developers.cloudflare.com/agents/api-reference/store-and-sync-state/ — Agents SDK state — 2026-06-03
- https://developers.cloudflare.com/agents/ — Cloudflare Agents — 2026-09-18
- https://vercel.com/docs/postgres — Postgres on Vercel — 2026-01-13
- https://vercel.com/marketplace/category/storage — Vercel Marketplace storage — acc.
- https://vercel.com/docs/marketplace-storage — Marketplace storage docs — 2026-09-17
- https://v0.app/docs/databases — v0 databases — 2026-10-05
- https://v0.app/docs/agentic-features — v0 agentic features — acc.
- https://replit.com/blog/introducing-a-safer-way-to-vibe-code-with-replit-databases — Replit dev/prod databases — 2025-07-21
- https://docs.replit.com/features/data-and-storage/development-and-production — Replit dev/prod docs — acc.
- https://docs.replit.com/features/data-and-storage/data-recovery — Replit data recovery — acc.
- https://docs.replit.com/cloud-services/storage-and-databases/production-databases — Replit production databases — acc.
- https://replit.com/blog/databricks2026 — Replit–Databricks GA with Lakebase — 2026-09-10

### 5.6 AWS
- https://aws.amazon.com/about-aws/whats-new/2026/03/amazon-aurora-postgresql-database — Aurora PostgreSQL express configuration — 2026-03-25
- https://aws.amazon.com/about-aws/whats-new/2024/11/amazon-aurora-serverless-v2-scaling-zero-capacity — Serverless v2 scale to zero — 2024-11-20
- https://docs.aws.amazon.com/AmazonRDS/latest/AuroraUserGuide/Aurora.Managing.Clone.html — Aurora cloning — acc.
- https://aws.amazon.com/about-aws/whats-new/2025/05/amazon-aurora-dsql-generally-available — Aurora DSQL GA — 2025-05-27
- https://docs.aws.amazon.com/aurora-dsql/latest/userguide/working-with-postgresql-compatibility-unsupported-features.html — DSQL unsupported features — acc.
- https://docs.aws.amazon.com/aurora-dsql/latest/userguide/release-notes.html — DSQL release notes — acc.
- https://aws.amazon.com/rds/aurora/dsql/pricing/ — DSQL pricing — acc.
- https://awslabs.github.io/mcp/servers/postgres-mcp-server — AWS Labs Postgres MCP server — acc.
- https://aws.amazon.com/security/security-bulletins/2026-101-aws/ — CVE-2026-85787 — 2026-09-04
- https://aws.amazon.com/security/security-bulletins/2026-104-aws/ — CVE-2026-87911 — 2026-09-09
- https://github.com/aws-rds-mcp/rds-management — RDS management MCP — acc.
- https://github.com/awslabs/mcp/pull/907 — RDS control-plane MCP PR (closed) — 2025-08-14
- https://aws.amazon.com/blogs/aws/the-aws-mcp-server-is-now-generally-available/ — AWS MCP Server GA — 2026-05-06
- https://aws-news.com/article/2025-10-13-make-agents-a-reality-with-amazon-bedrock-agentcore-now-generally-available — AgentCore GA — 2025-10-13
- https://aws.amazon.com/blogs/machine-learning/amazon-bedrock-agentcore-is-now-generally-available/ — AgentCore GA (AWS blog) — 2025-10-13
- https://aws.amazon.com/about-aws/whats-new/2026/03/policy-amazon-bedrock-agentcore-generally-available/ — AgentCore Policy GA — 2026-03-03
- https://docs.aws.amazon.com/bedrock-agentcore/latest/devguide/policy.html — AgentCore Policy docs — acc.
- https://aws.amazon.com/about-aws/whats-new/2026/06/amazon-bedrock-agentcore-policy-guardrails-generally-available/ — Guardrails in Policy — 2026-06-17
- https://aws.amazon.com/bedrock/agentcore/pricing/ — AgentCore pricing — acc.
- https://aws.amazon.com/blogs/aws/aws-devops-agent-helps-you-accelerate-incident-response-and-improve-system-reliability-preview — DevOps Agent preview — 2025-12-02
- https://aws.amazon.com/blogs/mt/announcing-general-availability-of-aws-devops-agent/ — DevOps Agent GA — 2026-03-31
- https://aws.amazon.com/devops-agent/pricing/ — DevOps Agent pricing — acc.
- https://docs.aws.amazon.com/AmazonCloudWatch/latest/monitoring/Database-Insights.html — CloudWatch Database Insights — acc.
- https://pganalyze.com/blog/aws-performance-insights-deprecation-database-insights-comparison — Performance Insights deprecation comparison — 2025-11-06, updated 2026-05-15

### 5.7 Google Cloud
- https://cloud.google.com/blog/products/ai-machine-learning/announcing-official-mcp-support-for-google-services — Official MCP support for Google services — 2025-12-10
- https://docs.cloud.google.com/sql/docs/postgres/use-cloudsql-mcp — Cloud SQL MCP server — acc.
- https://docs.cloud.google.com/sql/docs/postgres/release-notes — Cloud SQL release notes — acc.
- https://docs.cloud.google.com/alloydb/docs/release-notes — AlloyDB release notes — acc.
- https://docs.cloud.google.com/mcp/overview — Google MCP overview — acc.
- https://docs.cloud.google.com/mcp/supported-products — Google MCP supported products — acc.
- https://github.com/googleapis/mcp-toolbox/releases — MCP Toolbox releases (v1.13.1) — 2026-09-25
- https://raw.githubusercontent.com/googleapis/mcp-toolbox/main/internal/prebuiltconfigs/tools/postgres.yaml — Toolbox prebuilt Postgres tools — acc.
- https://docs.cloud.google.com/sql/docs/postgres/clone-instance — Cloud SQL cloning — acc.
- https://docs.cloud.google.com/database-center/docs/overview — Database Center — acc.
- https://docs.cloud.google.com/database-center/docs/assessments — Database Center assessments — acc.
- https://docs.cloud.google.com/gemini/docs/cloud-assist/investigations — Gemini Cloud Assist investigations — acc.
- https://docs.cloud.google.com/alloydb/docs/ai/natural-language-overview — AlloyDB AI natural language — 2026-09-30
- https://docs.cloud.google.com/vertex-ai/generative-ai/docs/release-notes — Vertex AI release notes — acc.
- https://adk.dev/sessions/session/ — ADK sessions — acc.

### 5.8 Microsoft
- https://learn.microsoft.com/en-us/azure/developer/azure-mcp-server/tools/azure-database-postgresql — Azure MCP Postgres tools — 2026-07-14
- https://learn.microsoft.com/en-us/azure/postgresql/monitor/concepts-autonomous-tuning — Azure PG autonomous tuning — 2026-07-13
- https://learn.microsoft.com/en-us/azure/postgresql/development/vs-code-extension/postgresql-extension-overview — VS Code PostgreSQL extension — 2026-07-22
- https://techcommunity.microsoft.com/blog/adforpostgresql/postgres-mcp-server-connect-ai-coding-agents-to-postgresql/4561273 — Microsoft Postgres MCP Server — 2026-10-01
- https://github.com/microsoft/postgres-mcp — microsoft/postgres-mcp — acc.
- https://learn.microsoft.com/en-us/azure/horizondb/release-notes/release-notes — HorizonDB release notes — 2026-06-02
- https://learn.microsoft.com/en-us/azure/horizondb/overview — HorizonDB overview — 2026-06-02, updated 2026-09-22
- https://learn.microsoft.com/en-us/azure/data-api-builder/whats-new/version-2-0 — Data API builder 2.0 — 2026-06-19
- https://learn.microsoft.com/en-us/azure/sre-agent/overview — Azure SRE Agent — 2026-08-26
- https://learn.microsoft.com/en-us/azure/sre-agent/billing — Azure SRE Agent billing — 2026-05-12
- https://learn.microsoft.com/en-us/entra/agent-id/what-are-agent-identities — Entra Agent ID — 2026-06-15

### 5.9 AI DBA, database operations and change governance
- https://github.com/postgres-ai/database-lab-engine — DBLab Engine — acc.
- https://github.com/postgres-ai/database-lab-engine/releases/tag/v4.2.0 — DBLab v4.2.0 — 2026-09-11
- https://postgres.ai/docs/database-lab — DBLab docs — acc.
- https://github.com/postgres-ai/postgresai — postgresai CLI — acc.
- https://postgres.ai/blog/20250725-self-driving-postgres — Self-driving Postgres — 2025-07-25
- https://v2.postgres.ai/docs/roadmap — postgres.ai roadmap — acc.
- https://postgres.ai/pricing — postgres.ai pricing — acc.
- https://pganalyze.com/blog/mcp-server-public-preview — pganalyze MCP public preview — 2026-04-30
- https://pganalyze.com/docs/mcp — pganalyze MCP docs — acc.
- https://pganalyze.com/docs/enterprise/releases/2026-01-0 — pganalyze 2026.01.0 — 2026-01
- https://pganalyze.com/pricing — pganalyze pricing — acc.
- https://github.com/crystaldba/postgres-mcp — Postgres MCP Pro — acc.
- https://github.com/crystaldba/postgres-mcp/issues — Postgres MCP Pro issues — acc.
- https://github.com/crystaldba/postgres-mcp/issues/192 — Project-status issue — 2026-07-30
- https://temporal.io/blog/temporal-and-the-next-frontier-scaling-ai-reliably — Temporal acquires Crystal DBA — 2025-09-03
- https://www.pgedge.com/blog/introducing-the-ai-dba-workbench-postgresql-monitoring-that-diagnoses-not-just-reports — pgEdge AI DBA Workbench — 2026-04-22
- https://github.com/pgEdge/ai-dba-workbench — AI DBA Workbench repo — acc.
- https://github.com/pgEdge/pgedge-postgres-mcp — pgEdge Postgres MCP — acc.
- https://www.enterprisedb.com/press-releases/edb-launches-agentic-database-converged-analytics-and-governance-bringing-sovereign — EDB agentic database press release — 2026-06-23
- https://www.enterprisedb.com/blog/inside-agentic-database-how-edb-turned-postgres-self-managing-system — Inside EDB's agentic database — 2026-06-30
- https://www.dbtune.com/products — DBtune products — acc.
- https://www.dbtune.com/release-notes — DBtune release notes — acc.
- https://www.dbtune.com/pricing — DBtune pricing — acc.
- https://aiven.io/blog/aiven-ai-dboptimizer-launch — Aiven AI Database Optimizer — 2024-05-28
- https://aiven.io/blog/aiven-mcp — Aiven MCP — 2026-06-11
- https://github.com/aiven-open/mcp-aiven — mcp-aiven — acc.
- https://www.dbta.com/Editorial/News-Flashes/Aiven-Runtime-and-DataHub-Are-Now-Generally-Available-Enabling-AI-Agents-to-Securely-Work-With-Live-Production-Data-176766.aspx — Aiven Runtime and DataHub GA — 2026-09-28
- https://www.datadoghq.com/pricing/list/ — Datadog price list — acc.
- https://docs.datadoghq.com/database_monitoring/recommendations/ — Datadog DBM recommendations — acc.
- https://docs.datadoghq.com/bits_ai/mcp_server/tools/ — Datadog MCP tools — acc.
- https://www.datadoghq.com/blog/bits-ai-sre/ — Bits Investigation — 2025-06-10, updated 2025-12-02
- https://github.com/bytebase/bytebase — Bytebase — acc.
- https://raw.githubusercontent.com/bytebase/bytebase/main/LICENSE — Bytebase license — acc.
- https://docs.bytebase.com/integrations/mcp — Bytebase MCP — acc.
- https://docs.bytebase.com/changelog/ — Bytebase 3.23.0 — 2026-09-24
- https://www.bytebase.com/pricing/ — Bytebase pricing — acc.
- https://github.com/bytebase/dbhub — DBHub — acc.
- https://atlasgo.io/community-edition — Atlas Community Edition — acc.
- https://atlasgo.io/pricing — Atlas pricing — acc.
- https://atlasgo.io/guides/ai-tools — Atlas AI tools — acc.
- https://atlasgo.io/blog/2026/08/31/ai-native-sdlc-database — The AI-native SDLC stops at the database — 2026-08-31
- https://github.com/liquibase/liquibase/releases/tag/v5.0.0 — Liquibase 5.0.0 (FSL) — 2025-09-30
- https://www.liquibase.com/blog/introducing-liquibase-secure-6-0 — Liquibase Secure 6.0 — 2026-09-30
- https://www.liquibase.com/pricing — Liquibase pricing — acc.
- https://www.pagerduty.com/platform/ai-agents/ — PagerDuty AI agents — acc.
- https://cleric.ai/ — Cleric — acc.
- https://traversal.com/ — Traversal — acc.
- https://incident.io/ai-sre — incident.io AI SRE — acc.
- https://rootly.com/ai-sre — Rootly AI SRE — acc.
- https://resolve.ai/ — Resolve AI — acc.
- https://ottertune.com/ — OtterTune — acc.
- https://tembo.io/ — Tembo — acc.
- https://www.metisdata.io/ — Metis (redirect) — acc.
- https://www.enginiq.dev/blog/postgres-mcp-server-for-ai-agents — EnginiQ Postgres MCP — 2026-03-14
- https://releem.com/pricing — Releem pricing — acc.

### 5.10 Governance, identity, MCP gateways and the MCP spec
- https://www.formal.ai/ — Formal homepage — acc.
- https://docs.formal.ai/docs/guides/core-concepts/connectors/deployment.md — Formal connector deployment — acc.
- https://docs.formal.ai/docs/guides/policies/enforcement.md — Formal policy enforcement — acc.
- https://docs.formal.ai/docs/guides/core-concepts/connectors/mcp-gateway.md — Formal MCP gateway — acc.
- https://docs.formal.ai/docs/guides/ai-governance/overview.md — Formal AI governance — acc.
- https://techcrunch.com/2024/11/19/formal-secures-access-to-databases-and-internal-applications-at-the-network-level/ — TechCrunch on Formal — 2024-11-19
- https://www.globenewswire.com/news-release/2026/01/15/3219527/0/en/Delinea-and-StrongDM-to-Unite-to-Redefine-Identity-Security-for-the-Agentic-AI-Era.html — Delinea and StrongDM — 2026-01-15
- https://securitymea.com/2026/03/06/delinea-completes-the-acquisition-of-strongdm/ — Delinea completes StrongDM — 2026-03-06
- https://docs.strongdm.com/admin/access/policies/policy-taxonomy.md — StrongDM Cedar taxonomy — acc.
- https://docs.strongdm.com/ai/ai-attribution.md — StrongDM AI attribution — acc.
- https://docs.strongdm.com/ai/mcp-gateway.md — StrongDM MCP Gateway — acc.
- https://docs.strongdm.com/ai/ai.md — StrongDM Secure AI — acc.
- https://discover.strongdm.com/blog/policy-enforcement-for-agentic-ai-with-leash — StrongDM Leash — 2025-10-29
- https://github.com/gravitational/teleport/releases/tag/v18.1.0 — Teleport 18.1.0 — 2025-07-25
- https://goteleport.com/docs/connect-your-client/model-context-protocol/database-access/ — Teleport database access over MCP — acc.
- https://goteleport.com/docs/enroll-resources/mcp-access/rbac/ — Teleport MCP RBAC — acc.
- https://goteleport.com/docs/enroll-resources/database-access/ — Teleport Database Access — acc.
- https://goteleport.com/docs/enroll-resources/database-access/auto-user-provisioning/postgres/ — Teleport Postgres auto-provisioning — acc.
- https://www.globenewswire.com/news-release/2026/01/27/3226449/0/en/Teleport-Introduces-Agentic-Identity-Framework-to-Secure-AI-Agents-in-Production-Infrastructure.html — Teleport Agentic Identity Framework — 2026-01-27
- https://goteleport.com/blog/teleport-community-license/ — Teleport Community Edition license — 2024-03-08
- https://www.varonis.com/blog/varonis-to-acquire-cyral-database-activity-monitoring — Varonis to acquire Cyral — 2025-03-17
- https://www.varonis.com/blog/ai-agents-are-making-database-activity-monitoring-critical — Varonis on agents and DAM — 2026-05-29
- https://www.commvault.com/blogs/commvault-closes-acquisition-of-satori — Commvault closes Satori — 2025-08-28
- https://www.crn.com/news/ai/2026/immuta-launches-data-provisioning-system-for-ai-agents — CRN on Immuta — 2026-04-03
- https://www.cyera.com/blog/identity-meets-data-defining-the-ai-trust-layer-for-the-agentic-enterprise — Cyera acquires Oasis — 2026-09-01
- https://aembit.io/press-release/aembit-introduces-identity-and-access-management-for-agentic-ai/ — Aembit IAM for Agentic AI — 2025-10-30
- https://astrix.security/ — Astrix homepage (Cisco banner) — acc.
- https://astrix.security/learn/blog/a-new-chapter-astrix-security-is-joining-cisco/ — Astrix joining Cisco — 2026-05-04
- https://www.paloaltonetworks.com/idira — PANW Idira — acc.
- https://www.okta.com/newsroom/press-releases/ai-innovations-oktane-2026/ — Okta for AI Agents (Oktane) — 2026-09-22
- https://www.okta.com/newsroom/press-releases/industry-leaders-form-the-blueprint-alliance/ — Blueprint Alliance — 2026-09-22
- https://developer.hashicorp.com/vault/docs/secrets/databases/postgresql — Vault Postgres secrets engine — acc.
- https://spiffe.io/ — SPIFFE — acc.
- https://raw.githubusercontent.com/docker/mcp-gateway/main/docs/security.md — Docker MCP Gateway security — acc.
- https://blog.cloudflare.com/zero-trust-mcp-server-portals/ — Cloudflare MCP server portals — 2025-08-26
- https://www.pomerium.com/docs/capabilities/mcp — Pomerium MCP — acc.
- https://github.com/microsoft/mcp-gateway — Microsoft MCP Gateway — acc.
- https://learn.microsoft.com/en-us/azure/api-management/mcp-server-overview — Azure APIM MCP — 2026-09-11
- https://developer.konghq.com/plugins/ai-mcp-proxy/ — Kong AI MCP Proxy — acc.
- https://github.com/agentgateway/agentgateway/releases/tag/v1.6.0 — agentgateway v1.6.0 — 2026-10-02
- https://theagentrouter.ai/docs/capabilities/mcp/ — Agent Router (formerly Envoy AI Gateway) — acc.
- https://ibm.github.io/mcp-context-forge/using/plugins/ — IBM ContextForge plugins — acc.
- https://obot.ai/ — Obot — acc.
- https://www.runlayer.com/ — Runlayer — acc.
- https://www.truefoundry.com/blog/mcp-access-control — TrueFoundry MCP access control — 2026-04-22
- https://boomi.com/blog/lunar-dev-boomi-acquisition/ — Boomi closes Lunar.dev — 2026-07-27
- https://modelcontextprotocol.io/specification/2025-06-18/changelog — MCP 2025-06-18 changes — 2025-06-18
- https://modelcontextprotocol.io/specification/2025-11-25/changelog — MCP 2025-11-25 changes — 2025-11-25
- https://modelcontextprotocol.io/specification/2026-07-28/changelog — MCP 2026-07-28 changes — 2026-07-28
- https://modelcontextprotocol.io/specification/2026-07-28/basic/authorization — MCP authorization (current) — 2026-07-28
- https://www.anthropic.com/news/donating-the-model-context-protocol-and-establishing-of-the-agentic-ai-foundation — MCP donated to the Agentic AI Foundation — 2025-12-09
- https://simonwillison.net/2025/Jul/6/supabase-mcp-lethal-trifecta/ — Supabase MCP "lethal trifecta" — 2025-07-06
- https://securitylabs.datadoghq.com/articles/mcp-vulnerability-case-study-SQL-injection-in-the-postgresql-mcp-server/ — Datadog, Postgres MCP server SQL injection — 2025-08-21
- https://discover.strongdm.com/blog/when-ai-tools-get-standing-access-lessons-from-the-vercel-breach — Vercel breach lessons (vendor commentary) — 2026-05-12

### 5.11 Agent memory and state
- https://docs.letta.com/guides/selfhosting/postgres/ — Letta Postgres self-hosting — acc.
- https://github.com/letta-ai/letta/blob/archive/letta/constants.py — Letta V1 constants — acc.
- https://github.com/letta-ai/letta/tree/archive/alembic/versions — Letta V1 migrations — acc.
- https://www.letta.com/blog/context-repositories/ — Letta Context Repositories — 2026-02-12
- https://www.letta.com/blog/our-next-phase/ — Letta's next phase — 2026-03-16
- https://github.com/letta-ai/letta/releases.atom — Letta releases (v0.16.8) — 2026-05-14
- https://github.com/letta-ai/letta/issues?q=is%3Aissue+postgres — Letta Postgres issues — acc.
- https://docs.letta.com/letta-code/pricing — Letta pricing — acc.
- https://www.getzep.com/blog/announcing-a-new-direction-for-zeps-open-source-strategy/ — Zep open-source direction — 2025-04-02
- https://github.com/getzep/graphiti/issues/779 — Graphiti Postgres + pgvector request — 2025-07-28
- https://github.com/getzep/graphiti/issues/1095 — Graphiti Postgres + AGE request — 2025-12-06
- https://www.getzep.com/pricing — Zep pricing — acc.
- https://docs.mem0.ai/components/vectordbs/overview — mem0 vector stores — acc.
- https://docs.mem0.ai/changelog — mem0 changelog — acc.
- https://docs.mem0.ai/migration/oss-v2-to-v3 — mem0 OSS v2 to v3 migration — acc.
- https://github.com/mem0ai/mem0/issues/6883 — mem0 inverted ranking bug — 2026-08-10
- https://github.com/mem0ai/mem0/issues?q=is%3Aissue+pgvector — mem0 pgvector issues — acc.
- https://techcrunch.com/2025/10/28/mem0-raises-24m-from-yc-peak-xv-and-basis-set-to-build-the-memory-layer-for-ai-apps/ — mem0 $24M — 2025-10-28
- https://github.com/langchain-ai/langgraph/blob/main/libs/checkpoint-postgres/langgraph/checkpoint/postgres/__init__.py — PostgresSaver — acc.
- https://github.com/langchain-ai/langgraph/blob/main/libs/checkpoint-postgres/langgraph/checkpoint/postgres/base.py — Checkpoint schema — acc.
- https://github.com/langchain-ai/langgraph/blob/main/libs/checkpoint-postgres/langgraph/store/postgres/base.py — PostgresStore — acc.
- https://github.com/langchain-ai/langgraph/issues/8531 — Prune request — 2026-08-05
- https://github.com/langchain-ai/langgraph/issues/5675 — SSL drop on Supabase pooler — 2025-07-26
- https://docs.langchain.com/langsmith/configure-ttl — Agent Server TTL — acc.
- https://docs.langchain.com/langsmith/self-host-dependency-versions — Agent Server dependencies — acc.
- https://support.langchain.com/articles/6253531756-understanding-checkpointers-databases-api-memory-and-ttl — Checkpointers and TTL — 2025-11-23
- https://forum.langchain.com/t/how-to-configure-thread-checkpointer-ttl-when-deploying-the-langgraph-server-with-docker-using-environment-variables/4253 — TTL via environment variables — 2026-07-27
- https://github.com/aegra/aegra/pull/355 — aegra checkpoint cleanup — 2026-05-07
- https://github.com/aegra/aegra/issues/288 — aegra orphaned checkpoints — 2026-04-04
- https://github.com/openai/openai-agents-python/blob/main/src/agents/extensions/memory/sqlalchemy_session.py — OpenAI Agents SDK SQLAlchemySession — acc.
- https://github.com/google/adk-python/blob/main/src/google/adk/sessions/database_session_service.py — ADK DatabaseSessionService — acc.
- https://learn.microsoft.com/en-us/semantic-kernel/concepts/vector-store-connectors/out-of-the-box-connectors/postgres-connector — Semantic Kernel Postgres connector — 2026-07-27
- https://docs.crewai.com/en/concepts/memory — CrewAI memory — acc.
- https://developers.llamaindex.ai/python/examples/vector_stores/postgres/ — LlamaIndex PGVectorStore — acc.
- https://pydantic.dev/docs/ai/core-concepts/message-history/ — Pydantic AI message history — acc.
- https://ai-sdk.dev/docs/ai-sdk-ui/chatbot-message-persistence — Vercel AI SDK persistence — acc.
- https://github.com/pgvector/pgvector — pgvector README — acc.
- https://github.com/pgvector/pgvector/blob/master/CHANGELOG.md — pgvector changelog — latest entry 2026-10-01
- https://github.com/pgvector/pgvector/issues?q=is%3Aissue+vacuum+hnsw — pgvector HNSW vacuum issues — acc.
- https://github.com/paradedb/paradedb — ParadeDB — acc.
- https://github.com/tensorchord/VectorChord — VectorChord — acc.
- https://alex-jacobs.com/posts/the-case-against-pgvector/ — The Case Against pgvector — 2025-10-29
- https://news.ycombinator.com/item?id=45798479 — HN discussion — 2025-11-03
- https://neon.com/docs/extensions/pgvector — Neon pgvector guidance — acc.
- https://supabase.com/docs/guides/ai/rag-with-permissions — Supabase RAG with permissions — acc.
- https://learn.microsoft.com/en-us/azure/foundry/agents/concepts/what-is-memory — Azure Foundry Memory — 2026-06-02
- https://memorilabs.ai/docs/memori-byodb/ — Memori bring-your-own database — acc.
- https://github.com/topoteretes/cognee — Cognee — acc.
- https://github.com/MemTensor/MemOS — MemOS — acc.
