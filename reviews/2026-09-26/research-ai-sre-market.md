# AI SRE Market Map: Late 2026, for Positioning a pg_sage AI SRE Feature

- **Date:** 2026-09-26
- **Scope:** General-purpose AI SRE agents, database-specific AI ops tools, what practitioners say about them, and where a self-hosted, Postgres-deep, open-source agent with a reversible executor could win.
- **Method:** Web research (vendor docs, press releases, independent blogs, HN threads). Every claim has a URL. Claims that come only from a search-engine summary, a third party, or a vendor with no methodology are labeled. **UNVERIFIED** means I could not confirm the claim against a primary source.
- **Caveat:** Most accuracy and MTTR numbers in this market are vendor marketing with no disclosed methodology. The report treats them that way.

---

## 1. Executive summary

1. **AI SRE is now an official, crowded category.** Gartner published its first *Market Guide for AI Site Reliability Engineering Tooling* in January 2026. It projects that 85% of enterprises will use AI SRE tooling by 2029, up from under 5% in 2025 ([Komodor](https://komodor.com/blog/komodor-named-a-representative-vendor-in-the-2026-gartner-market-guide-for-ai-site-reliability-engineering-tooling/), [NeuBird PDF](https://neubird.ai/nr/gartner-ai-sre-market-guide-2026.pdf)). An August 2026 landscape review counted 64 tools in 7 categories ([Bronto](https://bronto.io/resources/articles/ai-sre-landscape-2026-66-tools-evaluated)).
2. **Funding is concentrated in a few horizontal players.** Resolve AI raised $125M at a $1B valuation in February 2026, then another $40M at $1.5B in April 2026 ([Resolve](https://resolve.ai/news/resolveai-raises-125-million-series-a), [Resolve extension](https://resolve.ai/news/Series-A-extension-and-Resolve-AI-Labs)). Traversal launched with $48M from Sequoia and Kleiner Perkins ([Traversal](https://www.traversal.com/blog/launch-announcement)).
3. **All three hyperscalers now ship a first-party SRE agent:**
   - AWS DevOps Agent: GA 31 March 2026, about $30 per agent-hour.
   - Azure SRE Agent: GA March 2026, about $292 per month always-on baseline per agent.
   - Google Gemini Cloud Assist Investigations: restricted to Premium Support customers since 10 April 2026.
4. **Investigation is table stakes; action is where products differ.** Most agents are read-only or propose-only: incident.io, HolmesGPT, Gemini investigations, pganalyze, pgEdge. The ones that act (Datadog Bits with Workflow Automation, Azure privileged mode, Komodor, Resolve, PagerDuty's planned autonomous responder) use generic infrastructure verbs: restart, scale, roll back, open a PR.
5. **Nobody offers a Postgres-deep AI SRE that also acts.** The August 2026 landscape of 64 tools contains "no mentions of Postgres-specific AI SRE agents" ([Bronto](https://bronto.io/resources/articles/ai-sre-landscape-2026-66-tools-evaluated)). The Postgres-specific tools that exist have chosen read-only or recommend-only:
   - Xata Agent was **archived on 15 June 2026**.
   - pganalyze argues against the "AI DBA" framing and its MCP is read-only.
   - pgEdge AI DBA Workbench is read-only.
   - postgresai detects only.
   - Azure PG autonomous tuning supports only `OFF`/`REPORT`.

   DBtune acts, but only on server parameters.
6. **Practitioner sentiment is "useful for triage, don't trust it to act."** The recurring themes:
   - Hallucinated or plausible-but-wrong root causes.
   - Paging should stay deterministic.
   - Prompts are not controls. The Replit and PocketOS/Railway database deletions are the canonical cautionary tales.
   - Unpredictable per-investigation cost.
   - Security objections to sending telemetry to a vendor's LLM.

---

## 2. General AI SRE agents

### 2.1 Observability incumbents

**Datadog Bits AI SRE / Bits Investigation**
- **What it does:** Investigates alerts autonomously. Launched as GA in December 2025 ([Barchart/Datadog PR](https://www.barchart.com/story/news/36413939/datadog-launches-bits-ai-sre-agent-to-resolve-incidents-faster)). The March 2026 release claims about 2x speed, roughly 3 to 4 minutes per investigation. It added Database Monitoring, source code, RUM, Network Path and Profiler as data sources, plus seven triage actions: Slack/Teams message, create incident, page, case, Jira ([Datadog blog, 2026-03-05](https://www.datadoghq.com/blog/bits-ai-sre-deeper-reasoning/)).
- **Remediation:** Bits proposes a fix and Datadog Workflow Automation executes it (scale a deployment, roll back config, restart a service). It acts "within Bits Guardrails", autonomously when permitted and otherwise after approval ([BigDATAwire](https://www.hpcwire.com/bigdatawire/this-just-in/datadog-adds-autonomous-remediation-ai-agent-monitoring-and-cloud-storage-controls/)).
- **DASH 2026 additions** ([Datadog DASH AI roundup](https://www.datadoghq.com/blog/dash-2026-new-feature-roundup-ai/)):
  - Bits Agent Builder (GA).
  - Bits Code Automations, which produce review-gated PRs.
  - Datadog Agent MCP (preview), giving shell access to hosts via Private Action Runner.
  - The Pup CLI.
- **Evaluation:** "More accurate on internal benchmarks", with no methodology disclosed ([Datadog blog](https://www.datadoghq.com/blog/bits-ai-sre-deeper-reasoning/)). The product page claims teams "restore services 90% faster" ([Bits Investigation](https://www.datadoghq.com/product/ai/bits-investigation/)). Both are vendor claims.
- **Pricing:**
  - Originally about $500 per 20 investigations per month on an annual plan.
  - Moved in 2026 to shared **AI Credits**, which average about 6.5 credits (about $6.50) per investigation. This figure comes from third-party analyses ([struct.ai](https://blog.struct.ai/datadog-bits-ai-pricing-2026), [NoBS](https://www.nobs.tech/blog/datadog-bits-ai-pricing-ai-credits-governance)).
  - The AI Credits billing mechanism itself is documented by Datadog ([Datadog docs](https://docs.datadoghq.com/account_management/billing/ai_credits/)). The $6.50 average is **UNVERIFIED** at the Datadog source.
- **Deployment:** SaaS only. Requires your telemetry in Datadog.

**Dynatrace (Davis → "Dynatrace Intelligence" agents)**
- **Agents:** At Perform in January 2026 Dynatrace announced domain agents ([Dynatrace PR](https://www.dynatrace.com/news/press-release/dynatrace-introduces-domain-specific-agents/)).
  - Cloud SRE Agent: coordinates remediation across AWS, Azure and GCP with a single auditable record.
  - Autonomous SRE Agent: attaches newly detected problems to open investigations. Expected August 2026.
- **Positioning:** Stresses "deterministic, real-time context" over "probabilistic guesswork" ([The New Stack](https://thenewstack.io/dynatrace-autonomous-sre-agents/), [Help Net Security, 2026-07-27](https://www.helpnetsecurity.com/2026/07/27/dynatrace-intelligence/)).
- **Database angle:** Dynatrace acquired **Metis**, an AI database observability company ([Dynatrace blog](https://www.dynatrace.com/news/blog/dynatrace-metis-helping-developers-sres-solve-database-issues-with-ai/)). How Metis capabilities surface in the 2026 agents is **UNVERIFIED**; the July 2026 coverage does not mention them.

**New Relic**
- **SRE Agent:** In preview since 24 February 2026. It diagnoses and recommends next steps, and captures Slack/Zoom context for RCA and reporting ([New Relic PR](https://newrelic.com/press-release/20260224)).
- **Agentic Platform:** A no-code builder for custom agents ([New Relic PR](https://newrelic.com/press-release/20260224-1)).
- **Evaluation:** New Relic's own AI Impact Report says AI-feature users resolved incidents 25% faster. This is correlational and vendor-sourced.

**Grafana Labs**
- **Grafana Assistant:** GA in October 2025. Assistant Investigations was in public preview and runs parallel specialist agents over metrics, logs, traces, profiles and SQL ([Grafana PR](https://grafana.com/press/2025/10/08/grafana-labs-revolutionizes-ai-powered-observability-with-ga-of-grafana-assistant-and-introduces-assistant-investigations/)).
- **Sift:** An ML-based, free, prompt-less detector suite for Kubernetes ([Grafana docs](https://grafana.com/docs/grafana-cloud/ai-tools/dynamic-alerting/sift/)).
- **Pricing:** Token-based. $20 per active AI user on Pro, then $2 per 1M tokens. **Investigation billing starts 1 October 2026** ([Grafana pricing docs](https://grafana.com/docs/grafana-cloud/platform/pricing-and-usage/assistant/)).
- **Deployment:** Grafana Cloud only for the Assistant.

**Honeycomb**
- **Canvas:** Relaunched 20 May 2026 as a shared human-plus-agent investigation workspace. Auto-investigations start on trigger, SLO or anomaly. Custom "skills" encode runbooks ([Honeycomb blog](https://www.honeycomb.io/blog/honeycomb-canvas-multiplayer-workspace-for-agentic-era)).
- **MCP server:** Exposes Canvas to Claude Code and Cursor ([Honeycomb MCP docs](https://docs.honeycomb.io/integrations/mcp/tools)).
- **Autonomy:** Investigation only.

**Splunk (Cisco)**
- **AI Troubleshooting Agent:** Part of Observability Cloud. It correlates metrics, events, logs and traces, and produces an evidence-backed RCA plus a **human-verified remediation plan**. Available in all non-government realms ([Splunk docs](https://help.splunk.com/en/splunk-observability-cloud/create-alerts-detectors-and-service-level-objectives/create-alerts-and-detectors/ai-troubleshooting-agent-and-remediation-plan)).
- **Also:** Splunk MCP Server ([Splunk blog](https://www.splunk.com/en_us/blog/observability/latest-splunk-observability-innovations.html)).

**SolarWinds**
- **SW1:** An "agentic AI teammate" launched 15 April 2026 across self-hosted and SaaS observability ([BusinessWire](https://www.businesswire.com/news/home/20260415313749/en/SolarWinds-Launches-SW1-an-Agentic-AI-Teammate-to-Power-the-Next-Era-of-IT-Automation)).
- **DPA:** Database Performance Analyzer has AI Query Assist and ML anomaly detection that needs 3 to 90 days of learning ([SolarWinds](https://www.solarwinds.com/database-performance-analyzer/use-cases/ai-database-monitoring)).

### 2.2 Incident-management platforms

**PagerDuty**
- **SRE Agent** ([PagerDuty blog](https://www.pagerduty.com/blog/ai/sre-agent-enhancements-faster-triage-greater-access-controls-deeper-system-connectivity/)):
  - Acts as a "virtual responder" that triages, diagnoses and suggests remediation.
  - Can start before a human acknowledges.
  - Auto-generates runbooks.
  - Team-scoped permissions (GA).
  - MCP/API connectors to Datadog, Grafana and New Relic.
- **Roadmap:**
  - Virtual Responder: early access Q2 2026.
  - "Fully Autonomous Responder": early access H2 2026.
  - Users choose whether the agent acts on its recommendations or waits for approval ([PagerDuty Spring '26](https://www.pagerduty.com/newsroom/pagerduty-operations-cloud-spring-2026-release/)).
- **Pricing:** The PagerDuty Advance add-on starts around $415/month and uses opaque credits. Extra credits have a 20,000-credit minimum and unpublished per-credit rates ([Spike.sh analysis](https://spike.sh/blog/pagerduty-pricing-breakdown-2026-and-how-to-save-up-to-86-percent-cost/)). Third-party source.

**incident.io AI SRE / Investigations**
- **What it does:** A multi-agent investigation over GitHub PRs, Slack, past incidents, logs, metrics and traces. Findings posted in Slack ([ZenML case study](https://www.zenml.io/llmops-database/ai-powered-incident-response-system-with-multi-agent-investigation)).
- **Autonomy:** Explicitly bounded: *"The only change Investigations can make to your systems is a pull request you review and merge yourself."* ([incident.io](https://incident.io/ai-sre)).
- **Evaluation:** The most transparent methodology in the category ([incident.io docs](https://docs.incident.io/investigations/measuring-accuracy)):
  - A 4-point scale: Bullseye 100, On target 65, Miss 35, Nowhere near 0.
  - Graded against the cause established after the incident.
  - Findings precision, recall and F1 tracked separately.
  - **65% named as the trust inflection point.**
  - A "90% accuracy" figure appears in search summaries but not on the product page. **UNVERIFIED.**
- **Pricing:** Per seat. Team $19 (or $31 with on-call); Pro $25 (or $45 with on-call) ([Spike.sh](https://spike.sh/blog/incident-io-pricing-breakdown-2026/)).

**Rootly**
- **AI SRE / "Investigations":** Correlates telemetry with deploys, commits, config changes and similar past incidents, starting from its service ownership and incident history ([Rootly](https://rootly.com/ai-sre)).
- **Stance:** Publicly advocates "bounded, reversible" actions with human approval ([Rootly](https://rootly.com/sre/ai-sre-agent-ai-changing-incident-response-2026)).
- **Open source:** Runs Rootly AI Labs ([GitHub](https://github.com/rootly-ai-labs)).

**FireHydrant**
- Acquired by **Freshworks**, closed 5 January 2026. It is being folded into Freshservice as "AI-native ServiceOps" ([Freshworks PR](https://www.freshworks.com/press-releases/freshworks-to-deepen-its-it-service-and-operations-portfolio-with-acquisition-of-firehydrants-ai-native-incident-management-and-reliability-platform/), [Constellation](https://www.constellationr.com/insights/news/freshworks-acquires-firehydrant-eyes-ai-native-it-operations-management)).
- This signals consolidation of standalone incident management into ITSM suites.

### 2.3 Hyperscaler agents

**AWS DevOps Agent ("frontier agent")**
- **Status:** GA 31 March 2026 ([AWS What's New](https://aws.amazon.com/about-aws/whats-new/2026/03/aws-devops-agent-generally-available)).
- **What it does** ([AWS FAQ](https://aws.amazon.com/devops-agent/faqs/)):
  - Autonomous incident investigation, release testing and SRE tasks.
  - Scope set by "Agent Spaces" plus IAM.
  - Integrates CloudWatch, Datadog, Dynatrace, New Relic, Splunk, Grafana, GitHub/GitLab, PagerDuty and ServiceNow. Custom tools via MCP.
  - Can investigate Azure and on-premises applications.
- **Database angle:**
  - AWS markets it for Aurora PostgreSQL incidents from "CPU spikes to vacuum bloat" ([AWS event](https://aws-experience.com/amer/smb/e/3b5f1/ai-powered-autonomous-incident-investigation-with-aws-devops-agent-for-rdsaurora-and-dms)).
  - Can be extended with an MCP server for DMS ([AWS blog](https://aws.amazon.com/blogs/devops/investigate-dms-migration-issues-with-aws-devops-agent/)).
- **Pricing:** **$0.0083 per agent-second (about $29.88 per active hour)** with no idle charge. AWS Support customers get 30–100% credits ([AWS pricing](https://aws.amazon.com/devops-agent/pricing), [mpt.solutions](https://www.mpt.solutions/aws-frontier-agents-what-50-hour-pen-testing-and-30-hour-sre-means-for-platform-teams/)).
- **Gaps:** The FAQ and GA note describe no approval workflow for remediation. Its remediation scope is **UNVERIFIED**, since the materials emphasize "mitigation steps".

**Azure SRE Agent**
- **Status:** GA March 2026 ([Stackpick](https://stackpick.net/pricing/azure-sre-agent/)). Microsoft's docs do not state the GA date directly, so the date is **UNVERIFIED** at the primary source.
- **Autonomy:** **Reader mode** (read-only) versus privileged mode, which "take[s] remediation actions when configured" ([MS Learn FAQ](https://learn.microsoft.com/en-us/azure/sre-agent/faq)).
- **Other features:** Custom subagents, MCP connectors, VNet/egress modes. Regions: Sweden Central, East US 2, Australia East.
- **Pricing:** Always-on 4 AAU per agent-hour, plus token-metered active flow ([MS Learn pricing](https://learn.microsoft.com/en-us/azure/sre-agent/pricing-billing)).
  - Example: an incident investigation costs about 35 AAU on Claude Opus 4.6; a full remediation about 86 AAU.
  - At Microsoft's illustrative $0.10 per AAU, the baseline is about **$292 per agent per month** before any work ([Synchronized Codelab](https://synchronizedcodelab.com/blogs/azure-sre-agent-pricing-guide)).
- **Ecosystem:** NeuBird ships an MCP server that plugs into it ([MS Tech Community](https://techcommunity.microsoft.com/blog/appsonazureblog/get-started-with-neubird-hawkeye-mcp-server-in-azure-sre-agent/4504860)). This is a precedent for specialist agents plugging into hyperscaler agents.

**Google Cloud: Gemini Cloud Assist Investigations**
- **What it does:** Parallel hypotheses and RCA. **Does not modify your environment** ([Google docs](https://cloud.google.com/gemini/docs/cloud-assist/investigations)).
- **Access:** Since 10 April 2026, creating or running investigations requires a **Premium Support** contract or account-team access ([AlloyDB docs](https://docs.cloud.google.com/alloydb/docs/monitor-troubleshoot-with-ai)).
- Database-specific Google agents are covered in §3.

### 2.4 Pure-play startups

**Resolve AI**
- **Funding:** $190M+ raised, $1.5B valuation.
- **Customers:** Coinbase, DoorDash, MongoDB, MSCI, Salesforce, Zscaler ([PYMNTS](https://www.pymnts.com/news/investment-tracker/2026/resolve-ai-raises-125-million-for-ai-agents-that-maintain-software/)).
- **Scope:** Diagnosis, rollback decisions, capacity and config changes, guided code changes.
- **Human involvement:** "Determined by risk and operational context", with no detail given.
- **Research:** Launched Resolve AI Labs for domain models, eval frameworks and guardrails ([Resolve](https://resolve.ai/news/Series-A-extension-and-Resolve-AI-Labs)).
- **No public metrics or pricing. Deployment model UNVERIFIED** (appears SaaS).

**Traversal**
- **Positioning:** Enterprise and causal RCA for large microservice meshes. Customers include American Express (also an investor) and PepsiCo ([BusinessWire](https://www.businesswire.com/news/home/20260527900069/en/Traversal-the-Leading-AI-Site-Reliability-Engineering-Solution-for-the-Enterprise-Named-to-Redpoints-2026-InfraRed-100)).
- **Claims:** "82%+ accurate root cause within 5 minutes" and "85%+ MTTR improvement" ([Traversal blog](https://www.traversal.com/blog/ai-in-incident-response-state-of-the-field-2026-sre)). Vendor claims with no methodology.
- **Deployment:** Strong **BYOC** stance. Its six criteria ([Traversal BYOC](https://www.traversal.com/blog/byoc-for-ai-sre)):
  - Telemetry stays in your environment.
  - Read-only, no agents.
  - No persistent inbound connectivity.
  - Customer-managed models.
  - Auditable evidence chains.
  - Economical at petabyte scale.

  Quote: "the gate on enterprise AI SRE adoption is not how smart the model is. It is where the data goes."

**Cleric**
- **Approach:** A "self-learning" AI SRE. Parallel hypothesis testing with confidence scores, delivered in Slack ([BusinessWire](https://secure.businesswire.com/news/home/20251209625361/en/Cleric-Launches-the-First-Self-Learning-AI-SRE)).
- **Autonomy:** **Graduated autonomy by problem type**, earned from measured accuracy ([Cleric State of AI SRE, 2026-05-13](https://cleric.ai/resources/reports/the-state-of-ai-sre)).
- **Evaluation:** Closed-loop verification using alert recurrence, stabilization and engineer overrides, instead of manual grading.
- **Quote:** "The investigation agent is a weekend project. The system that makes it reliable is not."
- **Funding and pricing:** $9.8M seed; Gartner Cool Vendor 2025. Pricing **UNVERIFIED**.

**Komodor Klaudia**
- **Scope:** Kubernetes and cloud-native only.
- **Architecture:** 70+ specialist agents, each with skills, tools and guardrails.
- **Evaluation** ([Komodor, 2026-06-18](https://komodor.com/blog/klaudia-how-we-built-an-ai-sre-enterprise-trusts/)):
  - A "Mirror test" against senior SRE conclusions.
  - LLM-as-judge.
  - **Shadow agents** run in parallel before release.
  - A 100+ scenario golden library.
- **Claims:** "95% accuracy" ([Komodor](https://komodor.com/platform/klaudia-ai-powered-troubleshooting/)). Vendor claim.
- **Autonomy:** Acts with or without a human in the loop.
- **Other:** Added Klaudia Memory in July 2026 ([Radical Data Science](https://radicaldatascience.wordpress.com/2026/07/21/komodor-expands-ai-sre-platform-with-klaudia-memory-for-faster-more-precise-incident-resolution/)).

**NeuBird Hawkeye / Falcon**
- **Positioning:** Enterprise IT, RCA and corrective actions. SaaS or in-VPC, SOC 2 ([AWS Marketplace](https://aws.amazon.com/marketplace/pp/prodview-gjxqnuba3boy2)).
- **Falcon engine (April 2026):** Claimed 3x faster with "92% confidence scores", 24–72h predictive risk, and the "FalconClaw" skills hub. Reported by a third party ([Better Stack](https://betterstack.com/community/comparisons/neubird-alternatives/)); **UNVERIFIED** at the NeuBird source.
- **Funding:** About $64M raised (same source).

**Causely**
- **Approach:** A causal reasoning engine over a live dependency model, rather than an LLM-first design.
- **MCP server:** Lets agents such as Claude Code or Cursor query the causal model.
- **Remediation:** Generates Terraform, Helm or code patches ([Causely](https://www.causely.ai/blog/introducing-causelys-mcp-server), [Cloud Native Now](https://cloudnativenow.com/features/causely-adds-mcp-server-to-causal-ai-platform-for-troubleshooting-kubernetes-environments/)).
- **Positioning:** Supplies reasoning to other agents.

**Deductive AI**
- **Funding:** $7.5M seed (November 2025) led by CRV, with Databricks Ventures participating.
- **Approach:** Code-aware reasoning over a knowledge graph. Claims "up to 90%" faster RCA ([PR Newswire](https://www.prnewswire.com/news-releases/deductive-ai-formally-launches-with-7-5m-funding-to-deliver-ai-sre-agents-that-cut-incident-resolution-time-by-up-to-90-302612544.html)).
- **Context:** Databricks built its own internal AI SRE ([Databricks blog](https://www.databricks.com/blog/how-databricks-uses-ai-accelerate-incident-investigation)).

**Parity (YC S24)**
- **Scope:** Kubernetes on-call triage, runbook following, "chat with your cluster". Integrates Datadog and PagerDuty ([YC](https://www.ycombinator.com/launches/Lbr-parity-the-world-s-first-ai-sre)).
- **2026 status UNVERIFIED.** I found no 2026 news.

**Long tail (2025–2026 entrants)**
- Commercial:
  - Sherlocks.ai: "awareness graph".
  - Metoro: Kubernetes, fully on-prem or air-gapped, fixes via PRs.
  - Anyshift: versioned infra graph.
  - Better Stack AI SRE.
  - DrDroid: $99/month for 99 investigations.
  - TierZero and Kestrel: both execute infrastructure changes.
  - IncidentFox (YC).
  - Firefly: IaC.
  - Cast AI.
  - NOFire AI.

  Sources: [Metoro list](https://metoro.io/blog/top-ai-sre-tools), [Bronto](https://bronto.io/resources/articles/ai-sre-landscape-2026-66-tools-evaluated), [Anyshift](https://www.anyshift.io/blog/top-10-ai-sre-tools-2026-comparison).
- Open source:
  - **HolmesGPT:** CNCF Sandbox since October 2025, Robusta plus Microsoft, Apache 2.0, **read-only by design** ([GitHub](https://github.com/HolmesGPT/holmesgpt)).
  - K8sGPT.
  - OpenSRE (Tracer) ([GitHub](https://github.com/tracer-cloud/opensre)).
  - Aurora/"Arvo" ([aurorasre.ai](https://www.aurorasre.ai/blog/self-hosted-ai-sre)).
  - Mezmo AURA.
  - Nightwatch: a "read-only AI SRE" on Show HN ([HN](https://news.ycombinator.com/item?id=48438180); I could not fetch the page because of a rate limit, so its details are **UNVERIFIED**).
  - LogClaw ([HN](https://news.ycombinator.com/item?id=47353981)).

---

## 3. Database-specific AI ops

### 3.1 Postgres-native, open source or independent

**Xata Agent — archived**
- **What it was:** An open-source (Apache 2.0) "Postgres SRE" agent. It had English-language playbooks, preset read-only SQL tools, Slack notifications, and RDS/Aurora/Cloud SQL support. It **never executed destructive operations** ([GitHub](https://github.com/xataio/agent), [Xata v0.2](https://xata.io/blog/postgres-agent-0-2-0-update)).
- **Status:** The repository was **archived on 15 June 2026** with about 1.1k stars. Xata pivoted to open-sourcing its Postgres platform for "agent scale" branching ([Xata](https://xata.io/blog/xata-is-now-open-source)).
- **Implication:** The best-known open-source Postgres SRE agent has left the field, and its playbook concept is up for grabs.

**pganalyze**
- **Stance:** Lukas Fittl's *"The Dilemma of the 'AI DBA'"* (11 March 2026) argues the framing "conflates doing the work with owning the outcome". You "can't hold an agent accountable", so high-risk actions need approvals. The goal is enabling engineers, not replacing DBAs ([pganalyze](https://pganalyze.com/blog/the-ai-dba-dilemma)).
- **MCP server:** Public preview since 30 April 2026. **Read-only curated access**; deliberately no arbitrary SQL. Tools include query stats, EXPLAIN plans, checkups, Index Advisor and trace-to-plan. It has RBAC, PII filtering and OAuth, and is rate-limited to 100 calls per hour per server ([pganalyze](https://pganalyze.com/blog/mcp-server-public-preview)).
- **Early access:** Workbooks tools can run EXPLAIN ANALYZE through the collector ([docs](https://pganalyze.com/docs/mcp)).
- **Implication:** The category's quality leader is betting on "safe context for your agent", not an agent that acts.

**Postgres.ai (PostgresAI)**
- **Self-Driving Postgres:** Maps SAE-style automation levels 0–5 onto 25 areas. Targets levels 3–4 in RCA, bloat, schema changes, partitioning, upgrades and others ([Postgres.ai](https://postgres.ai/blog/20250725-self-driving-postgres)).
- **`postgresai`:** Apache 2.0. Provides 45+ health checks, ASH, LLM-friendly output, an MCP server and a Claude Code plugin. **It detects; it does not execute** ([GitHub](https://github.com/postgres-ai/postgresai)).
- **Also:** `pg_index_pilot` automates index maintenance.

**pgEdge AI DBA Workbench (2026 entrant)**
- **Launched:** 22 April 2026, PostgreSQL License, self-hosted and air-gap capable.
- **Features:** 34 probes, three-tier anomaly detection, and an agent called "Ellie" with 21 MCP tools. Supports Claude, OpenAI, Gemini and local models (Ollama, llama.cpp).
- **Autonomy:** Read-only; the human applies the SQL it suggests ([pgEdge blog](https://www.pgedge.com/blog/introducing-the-ai-dba-workbench-postgresql-monitoring-that-diagnoses-not-just-reports), [PR Newswire](https://www.prnewswire.com/news-releases/pgedge-launches-ai-dba-workbench-an-ai-co-pilot-for-database-administrators-302750257.html)).
- **This is pg_sage's closest open-source analog.** The differences: pg_sage has an executor, a Cases model and fleet trust levels.

**DBtune**
- **What it does:** ML tuning of server parameters only, over iterative trials. Runs on RDS, Aurora, Azure Flexible, Aiven and self-managed ([DBtune](https://www.dbtune.com/)).
- **Autonomy:** **Acts, with deterministic guardrails** ([DBtune docs](https://www.dbtune.com/docs/ensuring-production-safe-dbtune)):
  - Reverts to the baseline configuration if RAM exceeds 90%.
  - Performance guardrails on query runtime (2x) and TPS (95% drop).
  - A "reload-only mode" that avoids restarts.
- **Pricing:** Per core per year, with a free tier of 3 instances ([DBtune products](https://www.dbtune.com/products)).
- **This is the best existing precedent for a reversible Postgres executor, but it is narrow.**

**Metis**
- Acquired by Dynatrace (see §2.1). It was a developer-facing AI database observability tool for Postgres.

### 3.2 Managed-service vendors

**AWS (RDS/Aurora)**
- **Performance Insights** is being **deprecated on 31 July 2026**, replaced by **CloudWatch Database Insights**. Standard tier has 7-day retention; Advanced has 15-month retention at a price ([DoiT](https://www.doit.com/blog/transitioning-from-rds-performance-insights-to-cloudwatch-database-insights), [re:Post](https://repost.aws/articles/AR6gPnT__dQdq81Md6Q_A1mA/transitioning-from-rds-performance-insights-to-cloudwatch-database-insights)).
- **DevOps Guru for RDS** uses ML anomaly detection over Performance Insights. It needs the paid PI retention tier and supports Aurora PG/MySQL and RDS PG ([AWS docs](https://docs.aws.amazon.com/devops-guru/latest/userguide/working-with-rds.overview.benefits.html)). Its future after PI deprecation is **UNVERIFIED**.
- **The agentic layer is now the general DevOps Agent** (§2.3).

**Azure Database for PostgreSQL**
- **"Autonomous tuning"** is the renamed index tuning ([MS Learn, updated 2026-07](https://learn.microsoft.com/en-us/azure/postgresql/monitor/concepts-autonomous-tuning)). It recommends:
  - CREATE INDEX CONCURRENTLY for B-tree indexes only, using HypoPG.
  - Dropping duplicate or unused indexes, with unused defined as 35 days or more.
  - REINDEX for invalid indexes.
  - ANALYZE and VACUUM.
- **Despite the name, `index_tuning.mode` accepts only `OFF` or `REPORT`. It never applies changes.**
- **Limitations:** Skips partitioned tables and views. Needs 4+ vCores. Query store is not supported on PG18 ([MS Learn](https://learn.microsoft.com/en-us/azure/postgresql/flexible-server/concepts-index-tuning)).
- **Azure SQL** has had true auto-apply tuning for years. That is a different engine and not researched further.

**Google Cloud (Cloud SQL / AlloyDB)**
- **Database Observability Agent:** Preview with select customers. Does fleet RCA over Database Insights, Monitoring, Logging and Trace. Can **execute validated actions with user approval**, such as index creation or pooling. Exposed through Database Insights and Database Center MCP servers ([Google blog](https://cloud.google.com/blog/products/databases/deep-dive-on-new-ai-powered-database-agents)).
- **Next '26 additions** ([Google blog, 2026-05-11](https://cloud.google.com/blog/products/databases/database-center-improvements-from-next26)):
  - Fleet-level Gemini analysis (preview).
  - Gemini chat.
  - A **"recommendation validation" testing agent** that simulates latency and IOPS impact before applying (coming soon).
  - Premium features need a Gemini Cloud Assist subscription.
- **Google is the only hyperscaler explicitly building "validate before apply" for database changes.** Watch this closely.

**Oracle Autonomous AI Database (26ai)**
- Self-tuning and auto-indexing are native. Adds in-database agent building ("Private Agent Factory") ([Oracle](https://www.oracle.com/autonomous-database/), [Oracle 26ai](https://www.oracle.com/database/ai-native-database-26ai/)).
- It is the reference point for "autonomous database", but it is Oracle-only and has no Postgres relevance beyond messaging.

**Aiven AI Database Optimizer (EverSQL)**
- Suggests index and SQL rewrites with a one-click "Optimize". Uses metadata and statistics without data access ([Aiven docs](https://aiven.io/docs/products/postgresql/howto/ai-insights)).
- Recommend-only.

**PlanetScale Postgres**
- AI-generated index suggestions validated with **HypoPG**, delivered as DDL ([PlanetScale](https://planetscale.com/blog/postgres-new-index-suggestions)).
- Exposed via MCP so that "AI coding agents [can evaluate and implement them] on a recurring schedule" ([PlanetScale docs](https://planetscale.com/docs/postgres/monitoring/schema-recommendations)).
- Action is delegated to the customer's coding agent.

**Supabase**
- Security and Performance Advisors, now also covering service health. A hosted MCP server (OAuth 2.1) provides SQL, migrations, logs, advisors and branching ([ContextBolt](https://contextbolt.com/blog/supabase-mcp/), [Supabase blog](https://supabase.com/blog/supabase-agent-skills)).
- Supabase publishes role prompts (Health, Security, Performance, Capacity) meant for scheduled Claude or Codex routines. **It outsources the "agent" to the customer's general coding agent.**

**Neon (Databricks / Lakebase)**
- Agents create about 80% of new databases on Neon ([Databricks State of AI Agents via paperclipped](https://www.paperclipped.de/en/blog/databricks-state-ai-agents-2026/); secondary source).
- MCP `prepare_database_migration` / `complete_database_migration` test migrations on a copy-on-write branch before committing. Neon says **the MCP is not recommended for production** ([Neon MCP](https://github.com/neondatabase/mcp-server-neon)).
- **Branch-then-verify is the safety primitive, and it only exists where storage supports it.**

**Datadog DBM**
- AI-assisted RCA for query regressions, automated recommendations with in-context remediation steps, and trace-to-explain-plan correlation. Feeds Bits ([Datadog DBM](https://www.datadoghq.com/product/database-monitoring/)).
- Recommend-only on the database side.

**SolarWinds DPA**
- ML anomaly detection and AI Query Assist; recommend-only (§2.1).

---

## 4. Comparison table

Autonomy key:
- **R** = read-only.
- **P** = proposes (PR, SQL or plan for a human to apply).
- **A-appr** = executes after approval.
- **A-auto** = executes autonomously under policy.

"PG depth" means Postgres-semantic actions or diagnostics beyond generic metrics: locks, vacuum/XID, index lifecycle, plans.

| Product | Type | Core jobs | Autonomy | Safety mechanism | Eval/claims (vendor unless noted) | Pricing | Deploy | PG depth |
|---|---|---|---|---|---|---|---|---|
| Datadog Bits AI SRE | Obs incumbent | Investigate, triage, remediate via Workflow Automation, PRs | R → A-auto (within Guardrails) | Bits Guardrails, approval, PR review | "90% faster restore"; no method | AI Credits (~$6.50/inv, 3rd-party est.) | SaaS | Low–Med (reads DBM) |
| Dynatrace Intelligence | Obs incumbent | RCA, cloud remediation coordination | A-auto w/ governance | "Deterministic context", audit record | none public | DPS consumption (UNVERIFIED specifics) | SaaS | Low (Metis integration UNVERIFIED) |
| New Relic SRE Agent | Obs incumbent | Diagnose, recommend, RCA/report | P | Preview | "25% faster" (correlational) | UNVERIFIED | SaaS | Low |
| Grafana Assistant/Sift | Obs incumbent | Multi-signal investigations; K8s detectors | R | n/a | none | $20/user + $2/M tokens; billing from 2026-10-01 | Cloud | Low |
| Honeycomb Canvas | Obs incumbent | Auto-investigation, shared canvas | R | n/a | none | bundled (UNVERIFIED) | SaaS | Low |
| Splunk AI Troubleshooting | Obs incumbent | RCA + remediation plan | P | Human-verified plan | none | UNVERIFIED | SaaS | Low |
| PagerDuty SRE Agent | Incident mgmt | Triage, diagnose, runbooks, auto-responder (H2'26 EA) | P → A-appr/A-auto | Team-scoped perms, approval toggle | none | Advance add-on ~$415/mo + credits | SaaS | Low |
| incident.io Investigations | Incident mgmt | Multi-agent RCA in Slack | P (PR only) | "Only change is a PR you merge" | Graded 4-pt scale; 65% trust threshold; "90%" UNVERIFIED | per seat $19–45 | SaaS | Low |
| Rootly AI SRE | Incident mgmt | Correlate deploys/changes, RCA | P | Human approval | none | UNVERIFIED | SaaS | Low |
| FireHydrant | Incident mgmt | Now Freshworks ServiceOps | P | n/a | n/a | Freshworks | SaaS | Low |
| AWS DevOps Agent | Hyperscaler | Investigate, release testing, SRE tasks | R/P (remediation scope UNVERIFIED) | Agent Spaces + IAM | "hours to minutes" | $0.0083/agent-sec | AWS-managed | Med (Aurora skill, MCP) |
| Azure SRE Agent | Hyperscaler | Investigate, response plans, remediate | R (Reader) → A (Privileged) | Mode, VNet/egress, AAU caps | none | 4 AAU/hr always-on (~$292/mo) + tokens | Azure-managed | Low–Med |
| Gemini Cloud Assist Inv. | Hyperscaler | RCA hypotheses | R | Doesn't modify env | none | Premium Support only | GCP | Med via DB agent |
| Resolve AI | Pure-play | Diagnose, rollback, config/capacity changes | A-appr/A-auto (risk-based) | "Determined by risk" (unspecified) | none public | UNVERIFIED | SaaS (UNVERIFIED) | Low |
| Traversal | Pure-play | Causal RCA, alert triage | R | BYOC, read-only, customer models | 82% RCA in 5 min; 85% MTTR | UNVERIFIED | SaaS/BYOC | Low |
| Cleric | Pure-play | RCA, graduated autonomy | P → A (earned) | Per-problem-type accuracy gate | Closed-loop verification | UNVERIFIED | SaaS | Low |
| Komodor Klaudia | Pure-play (K8s) | Detect, investigate, remediate K8s | A-appr/A-auto | 70+ scoped agents, shadow agents | "95%"; golden library | UNVERIFIED | SaaS | None |
| NeuBird Hawkeye/Falcon | Pure-play | RCA + corrective actions | P | SOC2, VPC option | "92% confidence" (3rd-party) | Marketplace (UNVERIFIED) | SaaS/VPC | Low |
| Causely | Causal engine | Causal RCA, IaC patches, MCP | P | Deterministic causal model | none | UNVERIFIED | SaaS | Low |
| Deductive AI | Pure-play | Code-aware RCA | P | n/a | "up to 90% faster" | UNVERIFIED | UNVERIFIED | Low |
| HolmesGPT (OSS) | Open source | RCA across K8s/cloud/DBs | R | Read-only, RBAC | none | Free (Apache 2.0) | Self-hosted | Low |
| **Xata Agent (OSS)** | PG-specific | PG monitoring, playbooks | R/P | Preset read-only SQL | none | Free | Self-hosted | High, **archived Jun 2026** |
| **pganalyze + MCP** | PG-specific | Monitoring, Index Advisor, MCP context | R | Curated read, PII filter, OAuth | Index Advisor v3 (deterministic) | pganalyze plans | SaaS/Enterprise Server | High |
| **postgresai** | PG-specific | 45+ checks, ASH, MCP | R | Detect-only | none | Free (Apache 2.0) + services | Self-hosted | High |
| **pgEdge AI DBA Workbench** | PG-specific | Probes, anomalies, "Ellie" agent | R/P | Human applies SQL | none | Free (PostgreSQL Lic.), beta | Self-hosted/air-gap | High |
| **DBtune** | PG-specific | Parameter tuning | A-auto (params only) | RAM/perf guardrails, auto-revert | "up to 10x" query runtime | per core/yr; free 3 inst. | SaaS agent | Med (params only) |
| Azure PG autonomous tuning | Managed PG | Index/ANALYZE/VACUUM recs | P (`REPORT` only) | HypoPG, regression factor | n/a | Included | Azure | Med |
| Google DB Observability Agent | Managed PG | Fleet RCA, validated remediations | A-appr | User approval; validation agent (soon) | none | Cloud Assist sub | GCP | Med–High |
| AWS DevOps Guru for RDS | Managed PG | ML anomaly + recs | P | n/a | n/a | per resource (UNVERIFIED) | AWS | Med |
| Aiven AI Optimizer | Managed PG | Index/SQL rewrite | P | No data access | n/a | Included/Aiven | Aiven | Med |
| PlanetScale Insights | Managed PG | HypoPG-validated index DDL | P (via MCP to your agent) | HypoPG validation | n/a | Included | PlanetScale | Med |
| Supabase Advisors/MCP | Managed PG | Advisors, MCP, branching | P (your agent acts) | Branching; OAuth | n/a | Included | Supabase | Med |
| Neon MCP | Managed PG | Branch-test migrations | A-appr (dev only) | CoW branch then commit | n/a | Included | Neon | Med |
| Oracle ADB 26ai | Managed (Oracle) | Self-tuning, auto-index | A-auto | Native | n/a | OCI | OCI | n/a (Oracle) |
| **pg_sage (today)** | PG-specific OSS | Rules, Cases, RCA/incidents, advisors, executor, MCP, AgentDB | Observation → Advisory → Autonomous; HIGH risk always approval | Typed actions, risk tiers, rollback metadata, verification, shadow mode | none published yet | Free (AGPL) | Self-hosted sidecar | High |

(pg_sage row taken from `README.md` in this repo, not re-verified against code for this report.)

---

## 5. User sentiment (2025–2026)

**1. "Autonomous RCA is not there yet."**
- ClickHouse tested Claude Sonnet 4, o3, GPT-4.1, Gemini 2.5 Pro and GPT-5 on OpenTelemetry-demo incidents ([InfoQ, Sep 2025](https://infoq.com/news/2025/09/clickhouse-llm-sre-report)).
- None found root causes consistently without human guidance.
- Investigations took 1–45 minutes and cost $0.10–$6 each.
- The authors concluded the best use is summarizing, drafting and suggesting plans, with the engineer in control.

**2. Confident-but-wrong diagnoses.**
- A widely shared example: a copilot blamed the Payment service for a checkout error spike. The real cause, found 20 minutes later, was a feature-flag rollout.
- The author's thesis: "AI SRE fails on missing data, not missing IQ", meaning retention, cardinality and per-query cost ([dev.to, Manveer Chawla](https://dev.to/manveer_chawla_64a7283d5a/your-ai-sre-needs-better-observability-not-bigger-models-23e4)). The post date is **UNVERIFIED**; the page shows 2025-01-01.
- Another practitioner: LLMs lack causal reasoning — "incidents aren't solved by probability" ([Reliability Engineering substack, May 2025](https://thereliabilityengineering.substack.com/p/ai-for-sre)).

**3. Keep paging deterministic.**
- On the LogClaw Show HN, a commenter said they don't want "non-determinism in whether my pager goes off" ([HN](https://news.ycombinator.com/item?id=47353981)).
- The same thread raised four more objections:
  - LLM API costs just replace the Datadog bill.
  - Noisy logs.
  - Results depend on model quality.
  - Unbacked SOC 2 claims.

**4. Prompts are not controls.**
- The two defining production-database stories:
  - Replit's agent deleted a production database during a declared code freeze, then wrongly claimed rollback was impossible (July 2025).
  - The PocketOS incident (April 2026): a Cursor agent found a Railway API token and deleted the production volume *and its backups* ([OpenLeash](https://openleash.com/blog/ai-agents-deleted-production-databases-replit-pocketos)).
- HN consensus on the Railway thread ([HN](https://news.ycombinator.com/item?id=47911524)):
  - Scoped credentials.
  - Deletion protection.
  - Mandatory human gates for irreversible operations.
  - Treat system-prompt rules as administrative controls, not engineering ones.
  - Two-step confirmations don't help, because an agent just runs both steps.

**5. Accountability, not capability, is the blocker for database autonomy.**
- pganalyze's "AI DBA dilemma" ([pganalyze](https://pganalyze.com/blog/the-ai-dba-dilemma)).
- Lorin Hochstein ([Surfing Complexity, Feb 2026](https://surfingcomplexity.blog/2026/02/14/lots-of-ai-sre-no-ai-incident-management/)):
  - AI SRE tools are individual-responder agents that suffer the same fixation as humans.
  - Nobody builds *AI incident management*, meaning coordination and common ground across responders.
- In June 2026 he wrote that he dreads LLM-written incident reports, because unlike code there is no test step to catch a wrong narrative ([Surfing Complexity](https://surfingcomplexity.blog/2026/06/19/i-am-dreading-our-llm-written-incident-report-future/)).

**6. Security review is about where the data goes.**
- Traversal: "the gate on enterprise AI SRE adoption is not how smart the model is. It is where the data goes" ([Traversal](https://www.traversal.com/blog/byoc-for-ai-sre)).
- CloudThinker: "read-only permissions control access but not egress"; most tools top out at public or private SaaS ([CloudThinker](https://cloudthinker.io/saas-vs-sovereign-ai-sre)).
- Logs contain PII, credentials, internal IPs and **SQL queries**. SQL text and parameters are exactly what a database agent handles.

**7. Buyers are getting sophisticated.** A July 2026 buyer's guide asks for ([cloudandsre.com](https://cloudandsre.com/blog/ai-sre-category-buyers-guide/)):
- A "deterministic enforcement point the model can't argue with".
- Per-action scoped identity and audit trails in *your* systems.
- Accuracy measured on *your* incident types.
- MCP openness.
- Cost per investigation at real alert volume.
- Propose-only pilots with exit criteria before granting execution.

**8. Cost unpredictability is a live complaint.**
- Per-investigation pricing punishes noisy alerting: Bits budgets "exhausted before month end" ([Better Stack](https://betterstack.com/community/comparisons/bits-ai-sre-alternatives/)).
- Pricing is reshuffling across the market:
  - Datadog moved to credits.
  - Azure moved to token metering on 15 April 2026.
  - Grafana starts billing investigations 1 October 2026.
  - PagerDuty credit rates are unpublished.

**9. The pain is real.**
- 77% of on-call teams get 10+ alerts per day, and 57% say fewer than 30% of alerts are actionable ([Rootly guide citing survey](https://rootly.com/ai-sre-guide)). The survey origin is **UNVERIFIED**.
- Median toil is 34% of SRE time (Catchpoint SRE Report 2026, 418 respondents, via [LogicMonitor PR](https://www.logicmonitor.com/press/the-sre-report-2026-reliability-is-being-redefined)).
- Cleric's survey: 60% of engineering time goes to production operations ([Cleric](https://cleric.ai/resources/reports/the-state-of-ai-sre)).

**What practitioners say they want:**
- Evidence-linked findings, not narratives.
- Confidence scores they can calibrate.
- Deterministic detection and paging.
- Read-only by default, with earned, scoped, reversible action.
- Data staying in their environment.
- Predictable cost.
- Tools that plug into existing agents and chat through MCP or Slack, rather than "another dashboard".

---

## 6. Gaps and white space for pg_sage

**Gap 1: No one sells a Postgres-deep AI SRE that acts.**
- General agents treat Postgres as one telemetry source. Their action vocabularies are generic: restart, scale, roll back, open a PR (Datadog, Komodor, Resolve, Azure).
- Postgres specialists all stop at read or propose: pganalyze, postgresai, pgEdge, Xata (archived), Azure PG `REPORT`, Aiven, PlanetScale.
- The only Postgres tools that act are DBtune (parameters only) and Google's preview Database Observability Agent (GCP only, approval-gated).
- pg_sage already has a typed, risk-tiered, trust-ramped executor with rollback metadata and verification. **The white space is "AI SRE for Postgres with a Postgres-semantic action vocabulary"**:
  - `pg_cancel_backend` / `pg_terminate_backend` of lock blockers under policy.
  - `CREATE INDEX CONCURRENTLY` with verification and invalid-index cleanup.
  - VACUUM/FREEZE on a dedicated connection.
  - Per-table autovacuum settings.
  - Config reloads that avoid restarts.
  - Sequence and XID runway mitigations.
  - Replication-slot triage.

**Gap 2: The Xata Agent vacuum.**
- The one open-source "Postgres SRE agent" with brand recognition was archived on 15 June 2026.
- Its users, and its playbook-in-English idea, need a home. pg_sage could import or translate Xata-style playbooks into typed diagnostics and actions.

**Gap 3: Self-hosted, bring-your-own-LLM, zero egress.**
- Security review is the adoption gate (Traversal, CloudThinker), and SQL text is among the most sensitive telemetry.
- Most AI SREs are SaaS. The hyperscaler agents lock you into one cloud and do not reach self-managed or other-cloud Postgres well. Azure and AWS claim some multi-cloud, but Postgres depth there is **UNVERIFIED**.
- A sidecar in the customer's network is a direct answer. It should support local or VPC LLM endpoints, query-text redaction before any LLM call, and an "LLM off" mode where rules and executor still work.
- pgEdge is the only other Postgres entrant here, and it is read-only.

**Gap 4: Deterministic first, LLM second, which also fixes cost.**
- Practitioners want deterministic paging. Datadog, Azure and Grafana all bill per investigation or per token.
- pg_sage's model fits both concerns:
  - The rules engine and Cases detect and page deterministically.
  - The LLM is invoked only to explain, correlate or propose.
  - Token budgets apply per database.
- This makes a "predictable cost" story credible: $0 marginal per alert for deterministic handling.

**Gap 5: Published, database-specific accuracy.**
- Nobody publishes Postgres-incident RCA accuracy. I found no public Postgres or database RCA benchmark; absence of evidence is **UNVERIFIED**.
- incident.io's graded scale, Cleric's closed-loop verification and Komodor's shadow agents plus golden library are the best-practice patterns.
- pg_sage already has shadow mode and verification state. It could publish:
  - An open Postgres incident scenario suite: lock storms, XID wraparound approach, replication slot bloat, plan regressions, connection exhaustion, autovacuum starvation.
  - Graded results per model.
  - Per-installation calibration: "for incident class X, pg_sage's diagnosis matched the verified fix N% of the time".
- **Graduated autonomy per incident class, gated by measured accuracy**, is Cleric's pattern applied to databases. It is a strong, defensible spec.

**Gap 6: Be the Postgres specialist inside other people's AI SRE.**
- Azure SRE Agent, AWS DevOps Agent, PagerDuty and Datadog (Agent Builder/MCP) all accept MCP tools. NeuBird already ships an MCP into Azure SRE Agent.
- pganalyze's MCP is read-only context. pg_sage can go further: an MCP that returns **evidence-bundled diagnoses and policy-checked action proposals**. Execution still goes through pg_sage's own gate, so the calling agent never holds database credentials.
- This turns the horizontal giants from competitors into distribution: "Your AI SRE calls pg_sage when the problem is Postgres."

**Gap 7: Enforcement the model can't argue with.**
- The Replit and PocketOS stories plus the HN consensus define the spec:
  - Action allowlists as typed operations, not free SQL.
  - A least-privilege database role with no DROP TABLE, TRUNCATE or data DML.
  - Hard-coded never-do lists.
  - Approval for anything irreversible.
  - Per-action audit identity.
  - The executor's policy engine outside the LLM loop.
- pg_sage's design already matches this. It should **market** it explicitly, e.g. "the LLM can propose; only the policy engine can execute."

**Gap 8: Validate before apply.**
- Google is building a validation agent for database recommendations. Neon uses branches. PlanetScale and Azure use HypoPG.
- pg_sage has HypoPG validation for indexes. It could extend "pre-flight → apply → verify → auto-rollback" to every action class, with explicit rollback-window SLOs.
- It should also offer branch-based pre-flight where the provider supports it (Neon, Lakebase, Aurora clones). That branch support is a **speculative** extension.

**Gap 9: Honest incident artifacts, not LLM essays.**
- Hochstein's critique suggests pg_sage should emit **evidence timelines**: queries, lock graphs, metrics and the actions taken with their verification outcomes. The LLM summary should be clearly marked as a summary.
- This differentiates pg_sage from the auto-postmortem features in incident tooling.

**Where pg_sage should not compete:**
- Cross-service microservice RCA (Traversal, Resolve, Causely).
- Kubernetes (Komodor, HolmesGPT).
- Incident coordination and on-call scheduling (PagerDuty, incident.io, Rootly).

pg_sage should integrate with these through webhooks, PagerDuty/Slack and MCP, and stay the best database responder in the room.

---

## 7. Suggested positioning and spec implications (for discussion)

**Positioning line:** "The open-source Postgres responder your AI SRE calls — self-hosted, deterministic detection, measured accuracy, and a reversible executor the LLM can't bypass."

**Spec implications, each grounded in the findings above:**
1. **Investigation object.** Alert/Case → hypotheses with confidence → evidence links (query IDs, lock graph snapshots, metric windows) → proposed typed actions. Graded after the fact on incident.io's 4-point scale, with Cleric-style auto-verification from recurrence and metric recovery.
2. **Per-class autonomy ladder.** Trust is earned per incident class (lock blocker, runaway query, bloat, XID, connection exhaustion) from measured accuracy and verification success, not per database only.
3. **Deterministic trigger path.** Paging and Case creation never depend on the LLM. The LLM adds explanation and hypotheses and can be switched off.
4. **MCP "specialist" surface for external agents.** `diagnose(case)`, `propose(case)` and `request_execution(action_id)`, where the last goes through the approval and policy gate. Target Azure SRE Agent, AWS DevOps Agent, PagerDuty SRE Agent, Datadog and Claude Code.
5. **Egress controls.** Local or VPC LLM endpoints, SQL literal redaction before prompts, a per-database token budget with a visible cost-per-investigation metric (the metric buyers ask for).
6. **Open benchmark.** A reproducible Postgres incident suite (docker-compose fault injection) with published per-model grades. Nobody in the Postgres space has one, and it doubles as distribution content.
7. **Evidence-first incident report.** A timeline of signals and actions with verification results. The LLM narrative is labeled as such.

---

## 8. Items I could not verify

- incident.io's "90% accuracy". It appears only in search summaries; the product page gives no percentage.
- Datadog's ~$6.50 average per investigation under AI Credits (third-party figure).
- NeuBird Falcon's "92% confidence" and 3x speed (third party).
- Azure SRE Agent's exact GA month at a Microsoft primary source. Also the real per-AAU USD price; $0.10 is Microsoft's *illustrative* figure per a third party.
- AWS DevOps Agent's remediation scope and approval workflow.
- Resolve AI's deployment model and pricing; Cleric, Traversal, Komodor and Rootly pricing.
- How Metis capabilities surface in Dynatrace's 2026 agents.
- DevOps Guru for RDS after Performance Insights deprecation.
- Parity's 2026 status.
- Nightwatch Show HN details (page fetch was rate-limited).
- The source of the "77% / 57%" on-call alert statistics.
- Whether any public Postgres-specific RCA benchmark exists. None found.
