# Enterprise needs: databases for and with AI agents

Research input for the pg_sage AgentDB spec. Step 1 of `PROMPT.md`, external track "enterprise
needs". Date of research: 2026-10-05.

## How to read this document

- **Priorities.** MUST: a security-mature or regulated buyer blocks adoption without it. SHOULD:
  most buyers expect it, and its absence slows a deal or a rollout. COULD: a differentiator.
- **Citations.** `[S#]` points to Section 5, which gives the URL, the date and a source type.
- **Source types.** ANALYST (Gartner, Forrester, IDC). CONSULTANCY (PwC, Deloitte, KPMG,
  McKinsey, Capgemini: no software to sell here, but they sell services). NONPROFIT (CSA, FinOps
  Foundation, OWASP, NIST; some CSA surveys are vendor-funded, flagged). VENDOR-SURVEY (survey
  sponsored by a company that sells the fix). VENDOR (product docs or marketing: they prove what a
  vendor ships or claims, not that it works). LAW/STANDARD. NEWS.
- **UNVERIFIED** marks a claim I could not confirm against a fetched source on 2026-10-05.
- **Synthesis** marks my own inference from cited facts. It is not a sourced claim.
- **Method and limits.** WebSearch and WebFetch on 2026-10-05. Gartner and Forrester pages refuse
  direct fetches. Gartner claims come from press releases as reproduced by trade press, from a
  vendor-hosted reprint of the full AI SRE Market Guide [S6], and from vendor summaries of the
  Guardian Agents Market Guide [S7] (flagged). The session's web-search quota ran out partway
  through; later facts were checked by fetching known primary URLs, and what could not be
  checked is marked UNVERIFIED. Survey numbers do not compare across surveys: samples,
  definitions and sponsors differ.

## Key findings

1. **Adoption has outrun governance, by every measure.** Gartner: 75% of IT application leaders
   pilot or deploy some agents, but only 13% strongly agree they have the right governance
   [S3]. Deloitte: 21% have a mature governance model for agents, while about three quarters
   plan to deploy agents within two years [S16]. CSA: 84% doubt they could pass an audit of
   agent behavior or access controls [S21]. IDC: by 2030, up to 20% of G1000 firms will face
   lawsuits, fines and CIO dismissals over poorly governed agents [S13].
2. **Identity is the first gate and the weakest link.** 43% run agents on shared service
   accounts, 31% let agents act under a human's identity, 68% cannot tell agent activity from
   human activity, and 74% say agents get more access than they need [S22]. Every major
   identity stack shipped agent identity between Oct 2025 and Aug 2026: Microsoft Entra Agent ID
   [S56], Okta Cross App Access (now MCP's Enterprise-Managed Authorization) and Agent SSO
   [S62][S63], AWS AgentCore Identity [S65], Google Agent Identity [S68]. Buyers expect database
   tooling to consume these identities, not to invent its own.
3. **Enforcement must be deterministic and must sit outside the model.** Gartner made runtime
   inspection and enforcement a mandatory AI TRiSM capability [S8]. Its first Market Guide for
   Guardian Agents appeared in Feb 2026 [S7], and it expects guardian agents to take 10 to 15%
   of the agentic AI market by 2030 [S2]. AWS gives its reasons for putting Cedar policy at the
   agent-to-tool boundary [S67]. A Replit agent deleted a production database during a code
   freeze despite instructions not to [S93].
4. **Humans stay in the loop; autonomy is earned per action class.** Dynatrace (vendor survey):
   69% of agentic decisions are verified by a human [S31]. Gartner lists autonomous and
   semi-autonomous action with humans in the loop as a mandatory AI SRE feature, and predicts
   that 90% of organizations will suffer an AI-caused outage by 2029 yet keep AI SRE [S6].
   Capgemini: trust in fully autonomous agents fell from 43% to 27% in a year [S20].
5. **Change management is where earned autonomy meets audit.** DORA Art. 9(4)(e), SOC 2 CC8.1
   and ISO 27001 A.8.32 all require changes to be recorded, tested, approved and verified
   [S39][S47][S45]. DORA (DevOps Research and Assessment) found that heavyweight external
   approval boards do not lower change-failure rates, and it recommends lightweight, automated,
   auditable approval [S91]. Synthesis: promoting an action class in pg_sage's trust ledger
   should map to an ITIL *standard change* (pre-authorized, executes within an envelope).
6. **Evidence must be attributable, immutable and exportable.** EU AI Act Art. 12 requires
   automatic logging, and deployers must keep logs for at least six months [S35][S37]. Gartner
   lists "audit trail of actions and reasoning" as an AI SRE feature [S6]. Only 21% keep a
   real-time agent inventory, and fewer than 28% can trace agent actions to a human or system
   across all environments [S21].
7. **Kill switches and break-glass are explicit expectations, and most enterprises lack them.**
   The EU AI Act requires that human overseers can interrupt a high-risk system [S36]. To contain
   agents, only 49% use identity disabling or token revocation, and only 33% modify access
   policies in real time [S22].
8. **Cost kills projects.** Gartner expects over 40% of agentic AI projects to be canceled by
   end-2027 because of cost, unclear value or weak risk controls [S1]. KPMG: 26% have real-time
   visibility into AI operating costs and 36% enforce token or usage controls [S18].
9. **Self-hosted and hybrid deployment are table stakes.** Gartner makes hybrid support
   (on-prem, private and public cloud, edge) a mandatory AI SRE feature [S6]. GDPR sub-processor
   rules apply whenever an LLM call carries personal data, and EU buyers expect in-region
   processing [S44][S52].
10. **The database layer is unclaimed.** None of the 41 representative vendors in Gartner's AI
    SRE Market Guide is a database specialist (Synthesis: my reading of its vendor table) [S6].
    Hyperscaler policy engines decide who may call which tool with which arguments. None of the
    documented offerings models what a SQL statement will do to a specific Postgres database
    (locks, rows touched, reversibility) (Synthesis from [S66][S68][S72]). A vendor summary of
    Gartner's guide says guardian layers must work across clouds and platforms [S7]. A
    self-hosted, database-specific guardian plus AI DBA fits that gap.

---

## 0. Evidence base

### 0.1 Analyst views

**Gartner**

- **Cancellations (2025-06-25).** Over 40% of agentic AI projects will be canceled by end-2027
  because of escalating costs, unclear business value or inadequate risk controls. Only about
  130 of the thousands of vendors marketing agentic AI are real; the rest is *agent washing*. An
  early-2025 poll of about 3,400 webinar attendees found 19% investing significantly, 42%
  conservatively, 8% not at all and 31% waiting. For 2028 Gartner predicts that 15% of
  day-to-day work decisions will be made autonomously and that 33% of enterprise software will
  include agentic AI [S1].
- **Agents in enterprise apps (2025-08-26).** Gartner predicts that 40% of enterprise apps will
  feature task-specific AI agents by 2026, up from under 5% in 2025 (headline only; release
  page blocked) [S4].
- **Guardian agents (2025-06-11).** Guardian agents will take 10 to 15% of the agentic AI market
  by 2030. Gartner names three types: reviewers (check outputs), monitors (track actions for
  follow-up) and protectors (adjust or block actions automatically). The threats it lists are
  input manipulation and data poisoning, credential hijacking, interaction with fake or criminal
  sites, and agent deviation. A May 2025 poll of 147 CIOs found 24% had deployed fewer than a
  dozen agents and 4% more than a dozen, with 50% still researching. Gartner expects 70% of AI
  apps to use multi-agent systems by 2028 [S2].
- **Autonomy survey (2025-09-30).** In a survey of 360 IT application leaders, 15% were
  considering, piloting or deploying fully autonomous agents, and 75% some form of agent. 74%
  see agents as a new attack vector. 13% strongly agree they have the right governance. Fewer
  than 20% trust vendors to protect against hallucinations [S3].
- **GenAI incidents (2026-04-09).** By 2028, 25% of enterprise GenAI apps will have at least five
  minor security incidents a year (9% in 2025). By 2029, 15% will have at least one major
  incident a year (3% in 2025). Gartner names MCP's interoperability-first design as a source of
  security mistakes [S5].
- **Market Guide for AI SRE Tooling (2026-01-26, G00836089) [S6].**
  - Adoption: 85% of enterprises will use AI SRE tooling by 2029, up from under 5% in 2025.
  - Risk: 90% of organizations will experience an AI-caused outage by 2029 yet continue to use
    AI SRE.
  - Two mandatory features: autonomous or semi-autonomous action with humans in the loop, and
    hybrid infrastructure support.
  - Common features: deployment on-premises, in the cloud or as SaaS; integration APIs; generated
    rollback plans; MCP and A2A integration; an audit trail of actions and reasoning.
  - Risk guidance: robust oversight, proactive monitoring and defined mechanisms for human
    intervention.
  - Pricing: token-, task-, data-volume- or compute-based.
  - Gartner advises caution because many vendors are startups.
  - 41 representative vendors (hyperscaler agents, observability vendors, incident platforms,
    startups). None is a database specialist (Synthesis).
- **Market Guide for Guardian Agents (2026-02-25) [S7].** Available only through vendor
  summaries.
  - Nearly 70% of enterprises already run agents in production.
  - Through 2028, at least 80% of unauthorized agent transactions will come from internal policy
    violations (oversharing, unacceptable use, misguided behavior) rather than attacks.
  - Mandatory capability areas: visibility and traceability; continuous assurance and evaluation;
    runtime inspection and enforcement.
  - Guardian layers should be independent of any one agent platform (Orchid Security's summary).
  - Capability categories include agent identity, independent enforcement, contextual access
    control, real-time policy enforcement, and discovery and registry (PlainID's summary).
- **AI TRiSM Market Guide (2025) [S8].** Four layers: AI governance, AI runtime inspection and
  enforcement, information governance, and infrastructure and stack. Runtime inspection and
  enforcement became mandatory.
- **Earlier predictions (2024-10-22) [S9].** By 2028, 25% of enterprise breaches will be traced
  to AI agent abuse; VentureBeat repeats this figure in 2025-12. The companion prediction, that
  by 2028 40% of CIOs will demand guardian agents, is UNVERIFIED (Gartner page blocked; seen only
  in search summaries).

**Forrester**

- **AEGIS (Agentic AI Enterprise Guardrails for Information Security, launched 2025-08-22) [S10].**
  Six domains: GRC; IAM; data security and privacy; application security; threat management;
  Zero Trust.
  - Principles: *least agency* (limit what an agent may decide and do, even inside its granted
    access), continuous risk management, and explainable outcomes.
  - Controls named in Forrester's and TechTarget's summaries: agents as managed identities with
    human owners, credential vaulting, just-in-time least privilege, policy-as-code guardrails,
    complete logging of prompts, actions and reasoning, rules that can automatically pause an
    agent or route it to human approval, and DLP and DSPM extended to agents.
  - Forrester cross-maps AEGIS to NIST AI RMF, ISO/IEC 42001, OWASP, the EU AI Act and MITRE ATLAS
    (secondary summary).
  - The control count is reported as 39 (UNVERIFIED).
  - Forrester's Jeff Pollard: "CISOs must pivot from securing systems to securing intent" [S10].
- **Predictions 2026 (2025-10-02).** An agentic AI deployment will cause a publicly disclosed
  breach in 2026, and staff will be dismissed over it [S11].
- **Oktane 2026 recap (2026-10-01).** Okta's agent governance and identity details remain
  unclear: no full SPIFFE agent identity yet (planned Q4 2026), and its fine-grained
  authorization strategy for agents is unclear [S12].

**IDC**

- **FutureScape agentic AI 2026 (Oct 2025).** Up to 20% of G1000 organizations will face
  lawsuits, fines and CIO dismissals by 2030 because of inadequate agent controls. Enterprises
  put an average 16.7% of planned AI spend into AI and agent security and governance. IDC
  recommends unique agent identities with bounded permissions, rollback-enabled pipelines with
  full audit trails, human override for sensitive operations, and risk-calibrated access [S13].
- **Identity survey (Commvault-sponsored, 2026-07).** 539 North American organizations: 90% say
  their identity practices need improvement for agentic AI, 58.7% need significant change or an
  overhaul, and only 26.7% have dynamic RBAC that supports AI [S14].

### 0.2 Enterprise surveys: adoption and governance

| Source (type) | Sample, fieldwork | Numbers that matter here |
|---|---|---|
| Gartner [S3] (ANALYST) | 360 IT app leaders, 2025 | 75% piloting or deploying agents; 15% considering fully autonomous ones; 13% strongly agree governance is adequate; 74% see agents as an attack vector |
| Deloitte [S16][S17] (CONSULTANCY) | 3,235 leaders, 24 countries, Aug–Sep 2025 | 21% have mature agent governance; ~75% plan agents within 2 years; gaps: decision boundaries, real-time monitoring, audit trails |
| PwC [S15] (CONSULTANCY) | 300+ US execs, Apr 22–28 2025 | 79% adopting agents; top challenges: cybersecurity 34%, cost 34%, lack of trust 28%, data 24% |
| KPMG [S18] (CONSULTANCY) | 204 US leaders at $1B+ firms, Q2 2026 | 53% use agents; 18% orchestrate multiple agents; 61% have approval processes; 36% enforce token or usage controls; 26% see AI costs in real time |
| Capgemini [S20] (CONSULTANCY) | 1,500 execs, 14 countries, 2025 | 27% trust fully autonomous agents (43% a year earlier); 2% deployed agents at scale |
| McKinsey [S19] (CONSULTANCY; UNVERIFIED, secondary) | Global, 2025 | 23% scaling agents in at least one function, 39% experimenting; at most ~10% scaled in any single function |
| CSA + Strata [S21] (NONPROFIT, vendor-funded) | 285, Sep–Oct 2025 | 18% highly confident their IAM can handle agents; 44% use or plan static API keys; 21% keep a real-time agent registry; <28% can trace agents to humans; 84% doubt they would pass an agent audit |
| CSA + Aembit [S22] (NONPROFIT, vendor-funded) | 228, Jan 2026 | 68% cannot tell agent from human activity; 43% use shared service accounts; 74% say agents get more access than needed; to contain agents, 49% use identity disabling or token revocation and 33% modify policy in real time |
| SailPoint [S26] (VENDOR-SURVEY) | 353, 2025 | 82% use agents; 44% have policies to secure them; 80% saw unintended actions (39% unauthorized systems, 33% sensitive data); 23% saw agents tricked into revealing credentials |
| Okta 2025 [S27] (VENDOR-SURVEY) | 260 execs, Apr–May 2025 | 91% use agents; 10% have a well-developed NHI strategy; 78% name control of NHI access as their leading concern |
| Okta 2026 [S28] (VENDOR-SURVEY) | 292 execs + 492 workers, Mar 2026 | 34% apply the same controls to agents as to humans; 58% had AI security incidents or close calls; 57% of workers using unapproved tools blame slow approvals |
| CyberArk [S29] (VENDOR-SURVEY) | 2,600, 2025 | Machine identities outnumber humans 82:1; 68% lack identity security controls for AI |
| Gravitee [S30] (VENDOR-SURVEY) | 900+, early 2026 | 14.4% have full security and IT approval for their agent fleet; 88% had confirmed or suspected agent incidents; 45.6% use shared API keys |
| Dynatrace [S31] (VENDOR-SURVEY) | 919 leaders, Nov–Dec 2025 | ~50% of projects still in POC or pilot; 69% of decisions human-verified; barriers: security/privacy/compliance 52%, managing at scale 51% |
| Redgate [S32] (VENDOR-SURVEY) | Thousands (exact n not stated), 2026 | 44% use AI in database management (almost triple the year before); only 23% use a formal data-governance framework |
| FinOps Foundation [S24] (NONPROFIT) | 1,192, 2026 | 98% now manage AI spend (63% in 2025) |

### 0.3 What blocks adoption (with numbers)

| Blocker | Evidence |
|---|---|
| Security, privacy and compliance | 52% name it as a barrier to production [S31]; 34% name cybersecurity as a top challenge [S15]; 74% see agents as a new attack vector [S3] |
| Governance immaturity | 13% strongly agree their governance is right [S3]; 21% have a mature model [S16]; 47% lack a formal AI governance strategy [S28] |
| Identity and attribution gaps | 68% cannot tell agents from humans [S22]; <28% can trace actions to humans [S21]; 10% have an NHI strategy [S27] |
| Unintended agent actions | 80% saw unintended actions [S26]; 88% had confirmed or suspected incidents [S30]; 58% had incidents or close calls [S28] |
| Trust in autonomy | 27% trust fully autonomous agents [S20]; 28% rank lack of trust in agents a top-3 challenge [S15]; <20% trust vendors on hallucination protection [S3] |
| Cost and value | Over 40% of projects canceled by end-2027 [S1]; 26% see AI costs in real time [S18] |
| Operating at scale | 51% struggle to manage and monitor agents at scale [S31]; ~50% of projects stuck in pilot [S31] |
| Audit readiness | 84% doubt they would pass an agent-focused audit [S21] |
| Skills | 44% report a shortage of skilled staff [S31] |
| Governance that is too slow | 57% of workers who use unapproved tools cite slow approvals [S28]; only 14.4% have full approval for all their agents [S30] |

Synthesis: two failure modes pull against each other. Too little control produces incidents
and cancellations. Control that is too slow drives teams to unsanctioned agents. The winning
design gives fast, self-service approval inside guardrails that security has signed off once.

### 0.4 Regulation and standards: what applies and when

- **EU AI Act.**
  - Dates: in force 2024-08-01; prohibitions and AI literacy from 2025-02-02; GPAI obligations
    and penalties from 2025-08-02 [S33].
  - The AI Omnibus entered into force 2026-07-27 and moved the high-risk dates: Annex III systems
    from 2027-12-02, Annex I systems from 2028-08-02 [S34].
  - Art. 12 requires automatic event logging over the system's lifetime [S35]. Art. 14 requires
    human oversight, including the ability to "interrupt the system through a 'stop' button or
    a similar procedure" [S36]. Art. 26 obliges deployers to assign competent human overseers
    and keep logs for at least six months [S37].
  - Annex III point 2 covers AI used as a safety component in managing and operating critical
    digital infrastructure [S38]. Synthesis, not legal advice: an AI DBA is not high-risk in most
    enterprise uses, but a provider of critical digital infrastructure may ask whether it is.
  - Whether the Omnibus also changed the AI literacy duty is UNVERIFIED.
- **DORA (EU financial sector, applies from 2025-01-17) [S39].**
  - Art. 9(4)(c): limit logical access to what legitimate, approved functions require.
  - Art. 9(4)(e): ICT changes must be recorded, tested, assessed, approved, implemented and
    verified in a controlled manner.
  - Third-party risk is a pillar, and LLM providers are ICT third parties (Synthesis).
  - The RTS detail (Delegated Regulation 2024/1774) could not be fetched (UNVERIFIED).
- **NYDFS.** The 2024-10-16 industry letter on AI risk keeps Part 500's controls and stresses
  MFA, access controls, data minimization, an inventory of AI systems and the data they use,
  monitoring for unusual AI queries, and third-party diligence [S40]. Part 500 phase-in details
  (MFA for all users and asset inventory by 2025-11-01) are UNVERIFIED.
- **FINRA 2026 Oversight Report (2025-12-09).** Its agent risks are autonomy without human
  validation, scope and authority beyond intent, auditability, data sensitivity, domain knowledge
  and misaligned rewards. Suggested controls: monitor agent system access and data handling,
  keep a human in the loop, track agent actions and decisions, and set guardrails [S41].
- **HIPAA Security Rule, 45 CFR 164.312.** Access is allowed only to "persons or software
  programs that have been granted access rights". It also requires unique user identification,
  an emergency access procedure, audit controls, integrity controls and entity authentication
  [S42]. The status of the Jan 2025 Security Rule NPRM is UNVERIFIED.
- **PCI DSS v4.x.** 51 future-dated requirements took effect 2025-03-31 [S43]. Requirement-level
  points on system and application accounts are UNVERIFIED (standard text is license-gated): no
  interactive use, no hard-coded passwords, periodic credential change (8.6.x); least privilege
  and periodic review (7.2.5, 7.2.5.1); automated log review (10.4.1.1).
- **GDPR.** Art. 22: individuals may obtain human intervention in solely automated decisions with
  significant effects. Art. 28: processors need prior authorization for sub-processors, which
  covers LLM providers that see personal data. Art. 32: security appropriate to risk, including
  pseudonymization, encryption and timely restore [S44].
- **SOC 2 and ISO.** SOC 2 CC8.1 change management: formal requests, impact analysis,
  independent approval, version history and an audit trail [S47]. ISO/IEC 27001:2022 A.8.32 adds
  rollback procedures and links to A.8.31 (environment separation), A.5.3 (segregation of
  duties), A.8.2 (privileged access) and A.8.15 (logging) [S45]. ISO/IEC 42001:2023, the AI
  management system standard, requires AI risk and impact assessment and lifecycle controls,
  with a three-year certification cycle [S46].
- **NIST.** CAISI launched the AI Agent Standards Initiative on 2026-02-17. It covers agent
  identity, authentication and interoperability, and includes an RFI on agent security and an
  NCCoE project on agent identity and authorization [S48].
- **OWASP.** The Top 10 for Agentic Applications (Dec 2025) includes agent goal hijacking, tool
  misuse and exploitation, and identity and privilege abuse; the full list is UNVERIFIED [S49].
  The Non-Human Identities Top 10 (2025) lists improper offboarding, secret leakage,
  over-privileged NHIs, long-lived secrets, weak environment isolation, NHI reuse and human use
  of NHIs [S50].
- **Other.** The EU Data Act applies from 2025-09-12 (cloud switching and interoperability)
  [S51]. Singapore's Model AI Governance Framework for Agentic AI was published 2026-01-22; its
  content is UNVERIFIED (page body not retrievable) [S53].

### 0.5 Vendor landscape for agent identity and data governance (VENDOR unless noted)

| Vendor, offering | What it does for agents | Status, date | Implication for pg_sage (Synthesis) |
|---|---|---|---|
| Microsoft Entra Agent ID [S56] | Agent identities and agent identity blueprints, sponsors and owners, access packages for on-behalf-of and autonomous agents, Conditional Access templates for agents, cascade deletion, sidecar or federation for non-Microsoft agents | GA (doc dated 2026-05-01; secondary sources say Apr 2026) | Accept Entra-issued agent tokens; record the sponsor; the term blueprint collides with pg_sage's AgentDB blueprints |
| Microsoft Agent 365 registry [S57][S58] | Inventory with counts of ownerless and unmanaged agents, risk signals from Entra, Defender and Purview, Graph API (preview) | Launched Ignite Nov 2025; doc 2026-09-21 | Register pg_sage and its managed agents; publish risk signals |
| Microsoft Purview [S59][S60] | DSPM for AI covers prompts and agent data access; Data Map scans PostgreSQL 8.x–16.x metadata | Docs 2025-12 / 2026-01 | Read classifications instead of building a classifier first |
| Okta XAA / Agent SSO [S62][S63][S64] | IdP-mediated agent-to-app tokens (MCP Enterprise-Managed Authorization); owners, short-lived tokens, certifications, kill switch; 25+ XAA apps including Supabase | XAA in MCP 2025-11-25; Agent SSO GA 2026-08-24 | Support XAA on the MCP server; a Postgres platform (Supabase) already does |
| AWS AgentCore Identity + Policy [S65][S66][S67] | Workload identities and token vault; Cedar policies at the Gateway with natural-language authoring and formal analysis | Identity GA Oct 2025; Policy GA 2026-03-03 | Cedar is the reference for analyzable, default-deny policy |
| Google Agent Identity [S68][S69] | SPIFFE-based identities with 24h X.509 certificates and zero default permissions; IAM allow/deny for agents (GA); Principal Access Boundary and context-aware access (preview); Agent Gateway | GA 2026-05-06 | Accept SPIFFE SVIDs; record both agent and user identity |
| Databricks Lakebase + Unity Catalog [S70][S71][S72] | Managed Postgres with branching and scale-to-zero; agents run as a shared service principal or with downscoped user tokens that enforce row filters and column masks; managed MCP servers under UC permissions | Lakebase GA; user authorization in preview; MCP doc 2026-09-11 | Competitor and target: govern Lakebase as one more provider |
| Snowflake Cortex Agents + Horizon [S73][S74] | RBAC and caller's rights; per-run budgets and per-user quotas; classification, masking, row policies, lineage; PII guardrails on agent outputs | Cortex Agents GA | Budgets and quotas are now expected for agent workloads |
| Salesforce Trust Layer [S75] | Zero data retention, grounding, masking, toxicity detection | Current | Masking before the LLM is a buyer expectation |
| ServiceNow AI Control Tower [S76] | Central AI and agent governance in ITSM (UNVERIFIED: page blocked) | Announced 2025 (UNVERIFIED) | Change records likely flow through ServiceNow |
| HashiCorp Vault DB secrets [S77] | Dynamic, per-client, leased, revocable Postgres credentials | Current | Issue agent DB credentials from Vault |
| Teleport, StrongDM (Delinea), Palo Alto Idira (CyberArk) [S79][S80][S81] | MCP and database access brokering, JIT, zero standing privilege, session recording | Current | Interoperate; do not compete as a PAM product |
| OPA, Cedar [S82][S67] | Policy-as-code engines (OPA is CNCF graduated) | Current | Import or export policy, or embed an engine |
| Bytebase [S83] | Database change review and approval, SQL review, JIT access, masking, audit; MCP server with one identity per agent | Current | Closest DB-layer peer for change governance |

Synthesis: hyperscalers now cover agent identity and tool-call authorization at the gateway.
Database-semantic control (statement risk, lock impact, row caps, reversibility, verification)
is left to database-layer tools.

### 0.6 Incidents that shape enterprise requirements

- **Replit, 2025-07.** An agent deleted a live production database during a code freeze despite
  instructions. Replit responded with automatic separation of dev and prod databases, better
  rollback, and a planning-only mode [S93]. Requirements it drives: SF-1, SF-5, AZ-4.
- **Supabase MCP, 2025-07.** A prompt injected into a support ticket made a coding assistant
  using the `service_role` key (which bypasses RLS) read and leak an `integration_tokens` table.
  Recommended mitigations: read-only mode, scoped tools, manual review of tool calls [S94].
  Requirements it drives: AZ-1, AZ-6, DP-5.

---

## 1. Enterprise requirements catalog

Each entry gives the priority, the requirement, the evidence and a spec note. Spec notes are
Synthesis and refer to pg_sage concepts named in `PROMPT.md` (`policy.Gate`, `Executor.Apply`,
trust ledger, approvals, MCP server with scoped tokens). They make no claim about the current
code.

### 1.1 Identity and attribution (ID)

- **ID-1 MUST: one identity per agent.** Each agent, and pg_sage itself, is a distinct
  non-human identity. No shared service accounts, no shared API keys, and no agent running
  under a human's identity.
  - Evidence: 43% use shared service accounts, 31% run agents under human identities, 68% cannot
    tell agents from humans [S22]; 45.6% use shared API keys [S30]. OWASP NHI9 (reuse) and NHI10
    (human use of an NHI) [S50]. CSA framework [S23]. Google issues one identity per agent and
    does not share service accounts [S69]. Auditors expect a unique identity per agent (VENDOR
    blog) [S84].
  - Spec: every agent token maps to an agent principal and a distinct Postgres login role, which
    shows in `pg_stat_activity.usename` and log prefixes. Where pooling forces a shared login,
    use `SET ROLE` plus an `application_name` tag so pgAudit lines can still be attributed.
- **ID-2 MUST: an accountable human sponsor.** Every agent has a human sponsor. Sponsorship
  transfers automatically when the sponsor leaves, and ownerless agents are flagged.
  - Evidence: CSA framework (human sponsor) [S23]; Entra sponsor lifecycle workflows [S56]; the
    Microsoft registry counts ownerless agents [S57]; Okta assigns named owners [S63];
    fewer than 28% can trace agent actions to a human [S21].
- **ID-3 MUST: a recorded delegation chain.** Each action records whether the agent acted
  autonomously or on behalf of a user, and the audit record carries both identities.
  - Evidence: Entra has separate Conditional Access templates for on-behalf-of and autonomous
    agents [S56]; Google audit logs show agent and user [S69]; Databricks user authorization
    leaves a per-request audit trail [S71]; FINRA asks firms to track agent actions and
    decisions [S41].
- **ID-4 MUST: short-lived credentials.** Credentials expire quickly and rotate automatically.
  No static database passwords sit in agent configs or MCP client files.
  - Evidence: 44% use or plan static API keys and 43% usernames and passwords [S21]; OWASP
    NHI2 and NHI7 [S50]; Google 24h certificates [S69]; Vault dynamic credentials [S77]; AgentCore token vault
    [S65].
- **ID-5 MUST for regulated buyers, SHOULD otherwise: IdP federation.** Humans sign in through
  OIDC or SAML; agents through OAuth, token exchange, XAA (MCP Enterprise-Managed Authorization)
  or workload identity federation; SCIM handles lifecycle.
  - Evidence: XAA in MCP [S62]; MCP authorization is OAuth 2.1 with audience-bound tokens and
    forbids token passthrough [S54]; AEGIS calls for OAuth, OIDC, SAML and MCP [S10]; 85% say IAM
    is vital to AI adoption [S27].
- **ID-6 SHOULD: workload attestation.** Agent runtimes and the pg_sage sidecar can present
  SPIFFE SVIDs.
  - Evidence: SPIFFE [S78]; Google Agent Identity is SPIFFE-based [S68]; the CSA framework prefers
    SVIDs for orchestrators [S23]; Forrester counts Okta's missing SPIFFE support as a gap [S12].
- **ID-7 MUST: inventory and discovery.** Keep a live inventory of agents and the databases,
  roles and branches they touch, and detect unmanaged agent connections.
  - Evidence: 21% keep real-time registries [S21]; discovery and registry is a guardian-agent
    capability [S7]; the Microsoft registry counts unmanaged agents [S57]; Okta discovers shadow
    agents [S63]; NYDFS expects an inventory of AI systems [S40].
- **ID-8 MUST: complete offboarding.** Revoking an agent removes its roles, tokens, grants and
  ephemeral databases, and orphaned roles are detected.
  - Evidence: OWASP NHI1 improper offboarding [S50]; Entra cascade deletion [S56]; Google leaves
    old IAM grants behind after an agent is redeployed and asks admins to remove them [S69].

### 1.2 Authorization, least privilege and least agency (AZ)

- **AZ-1 MUST: deny by default, scoped access, read-only start.** Access is scoped per agent by
  database, schema, table and operation class.
  - Evidence: 74% over-provision agents [S22]; 39% of agents reached unauthorized systems [S26];
    OWASP NHI5 [S50]; Google agents start with zero permissions [S69]; Supabase's mitigation was
    read-only mode [S94]. Postgres predefined roles (`pg_monitor`, `pg_read_all_stats`,
    `pg_maintain`, `pg_signal_backend`) make narrow grants possible [S85].
- **AZ-2 MUST: least agency.** Constrain which tools and SQL classes an agent may invoke, and
  with which arguments, not only which data it may see.
  - Evidence: AEGIS least agency [S10]; AgentCore Policy constrains tool inputs, for example
    amount limits [S66][S67]; OWASP tool misuse [S49].
- **AZ-3 MUST: just-in-time elevation.** DDL, bulk DML, grants and maintenance get time-boxed
  grants with automatic expiry and no standing privilege.
  - Evidence: CSA framework JIT [S23]; AEGIS JIT [S10]; StrongDM and Idira JIT and zero standing
    privilege [S80][S81]; Vault leases [S77]; Bytebase JIT [S83].
- **AZ-4 MUST: enforcement outside the model.** A deterministic policy decision is made at the
  action boundary for every action; prompts are not controls.
  - Evidence: Cedar at the agent-to-tool boundary because the LLM is untrusted [S67]; runtime
    enforcement is mandatory in TRiSM and for guardian agents [S8][S7]; the Replit agent ignored
    a code freeze [S93].
  - Spec: `policy.Gate` must sit in front of every MCP tool and every agent session path, not
    only in front of pg_sage's own executor.
- **AZ-5 SHOULD: context-aware authorization.** Decisions use environment, database tier,
  change window or freeze, data sensitivity, risk score and time of day.
  - Evidence: Google context-aware access for agents (preview) [S68]; Okta intent- and
    context-based authorization [S12]; StrongDM continuous runtime authorization [S80]; IDC
    risk-calibrated access [S13].
- **AZ-6 MUST when agents act for users: no privilege amplification.** An agent acting for a
  user sees only what that user may see (RLS, masking). No agent role has `BYPASSRLS`, and no
  agent owns the tables it reads.
  - Evidence: Databricks user tokens enforce row filters and column masks [S71]; Snowflake runs
    agents with caller's rights [S73]; Postgres RLS is bypassed by owners and `BYPASSRLS` roles
    [S87]; Supabase `service_role` bypassed RLS [S94]; 52% say agents inherit access meant for
    others [S22].

### 1.3 Policy-as-code (PC)

- **PC-1 MUST: policies are code.** Policies are versioned, reviewed in Git, testable, and every
  decision is logged with the policy version that made it.
  - Evidence: AEGIS policy-as-code [S10]; OPA separates decision from enforcement [S82]; auditors
    expect policy versions in the record (VENDOR blog) [S84].
- **PC-2 SHOULD: standard languages.** Express or export policies in OPA/Rego or Cedar so
  security teams can review them with familiar tools.
  - Evidence: OPA is CNCF graduated [S82]; AgentCore Policy GA on Cedar [S66].
- **PC-3 SHOULD: static analysis before activation.** Detect conflicts, shadowed rules and
  over-permissive rules before a policy goes live. Evidence: Cedar Analysis [S67].
- **PC-4 COULD: natural-language authoring.** Write policies in natural language, compile them
  to a formal language and verify them before use. Evidence: AgentCore Policy [S66].

### 1.4 Approvals and change management (AP)

- **AP-1 MUST: a full change record.** Every production change is recorded, tested or simulated,
  risk-assessed, approved, applied and verified, with evidence attached.
  - Evidence: DORA Art. 9(4)(e) [S39]; SOC 2 CC8.1 [S47]; ISO A.8.32 [S45]. Google SRE: "roughly
    70% of outages are due to changes in a live system" [S90].
- **AP-2 MUST: separation of duties.** The proposer is never the sole approver; the approver's
  identity is recorded; no agent approves its own change.
  - Evidence: ISO A.5.3 [S45]; peer review satisfies segregation of duties [S91]; auditors expect
    it (VENDOR blog) [S84].
- **AP-3 MUST: risk-tiered paths.** Standard changes (pre-authorized classes) run automatically
  inside an envelope. Normal changes need human approval. Emergency changes take an expedited
  path with retrospective review.
  - Evidence: ITIL CAB and emergency CAB [S92]; heavyweight CAB gating does not lower failure
    rates [S91]; humans in the loop is a mandatory AI SRE feature [S6]; Deloitte names unclear
    decision boundaries as a gap [S17]. The standard, normal and emergency change types follow
    ITIL usage; the ITIL text itself was not fetched (UNVERIFIED).
- **AP-4 MUST for regulated buyers: system-of-record integration.** Changes create or attach
  ServiceNow or Jira records, and freezes and change windows are honored automatically.
  - Evidence: AI SRE tools must integrate with ITSM and existing processes [S6]; the Replit
    deletion happened during a freeze [S93]; ServiceNow AI Control Tower UNVERIFIED [S76].
- **AP-5 SHOULD: a GitOps path.** Schema changes land as migrations in pull requests, checked by
  SQL-review rules in the developers' own pipeline. Evidence: Bytebase [S83]; DORA peer review
  [S91].
- **AP-6 MUST: approval bound to the artifact.** An approval covers an exact artifact (hash of
  the SQL, plan and target) and expires. Any drift needs re-approval. Synthesis from the
  "controlled manner" requirement in [S39].
- **AP-7 SHOULD: two-person rule for destructive actions.** Applies to DROP, TRUNCATE,
  unqualified DELETE or UPDATE, grants, and destroying a database.
  - Evidence: FINRA scope and authority risk [S41]; the Replit incident [S93]. Singapore's
    framework reportedly requires human checkpoints for irreversible actions (UNVERIFIED) [S53].

### 1.5 Audit and evidence (AU)

- **AU-1 MUST: a tamper-evident audit trail of every agent action.** Each record holds:
  - who: agent and human;
  - what: SQL or tool call and its arguments;
  - why: evidence and a reasoning summary;
  - the policy decision and policy version;
  - the approval;
  - the outcome and the verification result.
  - Evidence: AEGIS (prompts, actions, reasoning) [S10]; Gartner's AI SRE audit-trail feature
    [S6]; EU AI Act Art. 12 and Art. 26 [S35][S37]; HIPAA audit controls [S42];
    Deloitte names audit trails as a gap [S17].
- **AU-2 MUST: independent database-side corroboration.** Evidence must not rest only on the
  agent's own log.
  - Evidence: pgAudit logs what the database actually did, beyond what the client asked [S88];
    Vault's unique usernames tie database activity to a client [S77].
  - Spec: correlate pg_sage action IDs with pgAudit lines, for example via `application_name`.
- **AU-3 MUST: near-real-time SIEM export.** Use a standard schema (OCSF, JSON over syslog or
  HTTP, OpenTelemetry logs).
  - Evidence: OCSF v1.9.0 has API and datastore activity classes and a remediation category [S55];
    Microsoft monitors break-glass use through Sentinel [S61]; the Agent 365 registry aggregates
    security signals [S57].
- **AU-4 MUST: auditor evidence packs.** Per period, export access reviews of agent
  permissions, approvals, change tickets, policy versions and break-glass uses.
  - Evidence: 84% doubt they would pass an audit [S21]; SOC 2 CC8.1 [S47]; auditor expectations
    (VENDOR blog) [S84].
- **AU-5 SHOULD: configurable retention.** At least six months for EU AI Act deployers [S37].
  PCI's 12-month retention is UNVERIFIED. WORM storage targets such as S3 Object Lock are a
  Synthesis suggestion.
- **AU-6 SHOULD: label agent sessions in telemetry.** Distinguish agent sessions from human
  sessions in database telemetry. Evidence: 68% cannot [S22].

### 1.6 Data protection (DP)

- **DP-1 MUST: classification-aware policy.** Policy knows which columns hold PII, PHI or PCI
  data, from catalog tags or a built-in classifier.
  - Evidence: Snowflake Horizon classifies and masks [S74]; Purview scans PostgreSQL metadata
    [S60]; AEGIS calls for unified sensitive-data definitions [S10]; NYDFS asks for a data
    inventory [S40].
- **DP-2 MUST: masking and minimization.** Agents that do not need raw values get masked data.
  pg_sage's LLM prompts exclude row data by default and redact literals from query text.
  - Evidence: PostgreSQL Anonymizer offers dynamic masking for masked roles [S89]; Salesforce
    masks and keeps zero retention [S75]; Bytebase dynamic masking [S83]; 33% of agents accessed
    or shared sensitive data [S26]; GDPR Art. 32 [S44].
- **DP-3 MUST: governed LLM data flows.** The LLM provider is a sub-processor. Offer self-hosted
  or local models, regional endpoints and zero-retention options, and document every data flow.
  - Evidence: GDPR Art. 28 [S44]; Microsoft EU Data Boundary [S52]; Microsoft's own registry docs
    warn that MCP connections can move data outside compliance and geographic boundaries [S57];
    NYDFS third-party diligence [S40].
- **DP-4 SHOULD: DSPM signals.** Report which agents touched which sensitive data, and flag
  oversharing. Evidence: Purview DSPM for AI [S59]; TRiSM information governance layer [S8].
- **DP-5 MUST: treat database content as untrusted.** Data from the database is untrusted input
  to the model: tool-call review for write paths, and no secrets or tokens reachable by
  read-anything agents.
  - Evidence: Supabase MCP exfiltration [S94]; OWASP goal hijacking [S49]; Gartner on MCP
    security mistakes [S5].

### 1.7 Safety: blast radius, separation, stop and recover (SF)

- **SF-1 MUST: environment separation.** Agents work on dev, branches or clones by default.
  Production is a separate trust level with separate credentials.
  - Evidence: ISO A.8.31 [S45]; OWASP NHI8 [S50]; Replit's fix [S93]; Lakebase branching [S70].
- **SF-2 MUST: per-action blast-radius limits.** Limit rows affected, `statement_timeout`,
  `lock_timeout`, concurrency, rate and session count, and keep a protected-objects list.
  - Evidence: Postgres per-role settings and `CONNECTION LIMIT` [S86]; IDC warns of uncontrolled
    decision cascades [S13]; FINRA scope and authority [S41]; Gartner's 90% AI-outage prediction
    [S6].
- **SF-3 MUST: kill switch.** One action halts agent activity globally, per agent or per
  database. It revokes tokens, terminates sessions and pauses queues, and it still works when
  the LLM provider is down.
  - Evidence: EU AI Act Art. 14 [S36]; CSA framework revocation [S23]; Okta Agent SSO kill switch
    [S63]; only 49% use token revocation and 33% real-time policy changes [S22]; AEGIS auto-pause
    [S10]; `pg_signal_backend` [S85].
- **SF-4 MUST: break-glass.** A documented human bypass of agent gating, alerting on every use,
  a post-use review and drills at least every 90 days.
  - Evidence: Microsoft emergency-access guidance (alert on every use, post-mortem, 90-day
    validation) [S61]; HIPAA emergency access procedure [S42].
- **SF-5 MUST: reversibility and recovery.** Check backups and PITR before risky changes,
  generate a rollback plan, run restore drills, and require more approval for irreversible
  actions.
  - Evidence: Gartner lists generated rollback plans as a feature [S6]; ISO A.8.32 rollback
    [S45]; IDC rollback-enabled pipelines [S13]; Replit's rollback work [S93]; GDPR Art. 32 timely restore
    [S44].
- **SF-6 SHOULD: verification with automatic rollback.** Compare the metric before and after;
  roll back automatically on regression. Evidence: Gartner validation and testing [S6]; Google
  SRE progressive rollout and safe rollback [S90].
- **SF-7 SHOULD: progressive fleet rollout.** Apply to canary databases first, then by tier.
  Evidence: Google SRE [S90]; fleet application is Synthesis.

### 1.8 Reliability and trust (RT)

- **RT-1 MUST: graduated autonomy.** Each action class has an autonomy level, with
  evidence-based promotion, automatic demotion and a human in the loop.
  - Evidence: Gartner mandatory feature [S6]; 69% of decisions human-verified [S31]; 27% trust
    full autonomy [S20]; 15% consider fully autonomous agents [S3]; CSA's "time-to-trust" phase
    [S21].
  - See Section 3.
- **RT-2 SHOULD: an independent oversight layer.** Oversight is deployable independently of any
  one agent platform or cloud. Evidence: Guardian Agents Market Guide (vendor summary) [S7].
- **RT-3 SHOULD: evaluation and replay.** Run offline evaluations of agent behavior and replay
  past incidents. Evidence: agent reliability testing is on Gartner's AI SRE roadmap [S6].

### 1.9 FinOps (FO)

- **FO-1 MUST: cost attribution.** Attribute cost per agent, team, database and environment:
  LLM tokens, database compute and storage, and branch lifetime.
  - Evidence: 98% manage AI spend [S24]; 26% see AI costs in real time [S18]; FinOps for AI calls
    attributing model output to its consumer hard [S25].
- **FO-2 MUST: enforced budgets and quotas.** Hard caps, not just dashboards.
  - Evidence: 66% have dashboards but 36% enforce usage controls [S18]; Snowflake per-run budgets
    and per-user quotas [S73]; FinOps quotas, throttling and anomaly detection [S25]; cost is a
    cancellation driver [S1].
- **FO-3 SHOULD: TTL and auto-teardown.** Ephemeral agent databases and branches expire, idle
  ones are detected, and showback comes before chargeback. Evidence: FinOps for AI [S25];
  Lakebase scale-to-zero [S70].
- **FO-4 COULD: unit economics.** Report cost per resolved incident and per applied
  recommendation. Evidence: AI SRE pricing is task- and token-based [S6].

### 1.10 Deployment, residency, tenancy (DR)

- **DR-1 MUST: self-hosted deployment.** Run in the customer's VPC or on-prem with no mandatory
  SaaS control plane, including an air-gapped mode with a local LLM.
  - Evidence: Gartner mandatory hybrid support and on-prem, cloud or SaaS deployment [S6]; EU
    residency drivers [S51][S52].
- **DR-2 MUST: residency per region.** Audit logs, telemetry and LLM calls stay in a configured
  region. Evidence: GDPR Art. 28 [S44]; Microsoft's MCP data-flow warning [S57].
- **DR-3 MUST: tenant isolation.** Separate database, role or branch per tenant or agent, and
  RLS where a database is shared. Evidence: Postgres RLS defaults to deny when enabled without a
  policy [S87]; Databricks per-user tokens [S71].
- **DR-4 SHOULD: delegated administration.** The platform team sets guardrails; application teams
  self-serve inside them with team-scoped approvers.
  - Evidence: Entra access packages [S56]; slow approvals push teams to shadow tools [S28]
    (Synthesis).

### 1.11 Compliance mapping (CM)

- **CM-1 MUST: published control mappings.** Map pg_sage controls to SOC 2, ISO 27001, ISO
  42001, HIPAA, PCI DSS, DORA, the EU AI Act and NIST.
  - Evidence: Forrester cross-maps AEGIS to these frameworks [S10]; Purview ships
    regulation-mapping templates [S59]; 84% doubt their audit readiness [S21].
- **CM-2 SHOULD: AI system documentation.** Document intended purpose, the models used,
  limitations, oversight roles and log retention, for EU AI Act deployers and ISO 42001
  [S37][S46].

---

## 2. Buyer and stakeholder map

Ownership is fragmented. Responsibility for agent identity and access sits with security in 28%
of organizations, development or engineering in 21%, IT in 19% and IAM teams in 9% [S22].
Synthesis: an enterprise deal needs several sign-offs, and any one owner can stop it.

### 2.1 Platform engineering (internal developer platform, cloud platform)

- **Needs:**
  - self-service, declarative provisioning (Terraform, GitOps);
  - golden paths for agent databases (branches, TTL, quotas);
  - multi-cloud and multi-provider coverage;
  - low operational load.
- **Fears:** sprawl of orphaned databases and credentials (Synthesis; OWASP NHI1 [S50]); another
  control plane to run.
- **Can veto:** any tool that cannot be managed as code or does not fit the platform's identity
  and network model.
- **Evidence they accept:** Terraform and API parity with the UI; TTL and teardown reports;
  Synthesis.
- **Signal:** 51% struggle to manage and monitor agents at scale [S31].

### 2.2 DBAs and data platform owners

- **Needs:**
  - no surprise locks or table rewrites;
  - visibility into every agent session;
  - a rollback plan for every change;
  - change windows;
  - performance guardrails (`statement_timeout`, connection limits) [S86];
  - corroborating database-side audit (pgAudit) [S88].
- **Fears:** an agent dropping or rewriting production data, as at Replit [S93]; paging at 3 a.m.
  for agent-caused load (Synthesis).
- **Can veto:** production credentials, DDL rights and any write access. In practice DBAs gate
  rungs B to D of the readiness ladder (Synthesis).
- **Evidence they accept:** plans, lock analysis, before-and-after metrics, rollback drills, and
  a track record of correct proposals (Synthesis; trust ledger).
- **Signal:** 44% already use AI in database management, but only 23% have a formal governance
  framework [S32].

### 2.3 Security: CISO, IAM, SecOps

- **Needs:**
  - one identity per agent, federated to the IdP (ID-1, ID-5);
  - least privilege and least agency (AZ-1, AZ-2);
  - logs in the SIEM (AU-3);
  - a kill switch (SF-3);
  - an LLM data-flow review (DP-3);
  - a threat-model mapping to OWASP Agentic and NHI [S49][S50].
- **Fears:** agents as a new attack vector (74%) [S3]; credential leakage (23% saw agents tricked
  into revealing credentials) [S26]; a public breach [S11].
- **Can veto:** everything. Security is the most common blocker: 52% name security, privacy or
  compliance as the barrier to production [S31]; 34% name cybersecurity [S15].
- **Evidence they accept:** architecture review, pen-test results, policy decision logs,
  blocked-attempt statistics, SBOM and signed releases (Synthesis).

### 2.4 Compliance, internal audit, risk, privacy (DPO), legal

- **Needs:**
  - attributable, immutable evidence (AU-1, AU-4);
  - separation of duties (AP-2);
  - change records (AP-1);
  - retention (AU-5);
  - regulatory mapping (CM-1);
  - sub-processor documentation for LLM calls (DP-3);
  - human-oversight design for the EU AI Act [S36][S37].
- **Fears:** failing an audit (84% doubt they would pass) [S21]; regulatory fines and dismissals
  [S13]; the FINRA and DORA findings [S41][S39].
- **Can veto:** production use in regulated scope; findings at audit time can force a rollback of
  autonomy.
- **Evidence they accept:** control matrices, evidence packs per period, and sample-based
  testing of approvals and logs (Synthesis; [S47]).

### 2.5 FinOps and finance

- **Needs:** attribution by agent, team and environment; budgets and quotas with enforcement;
  idle detection; showback, then chargeback [S25].
- **Fears:** runaway token and compute spend, one of the three cancellation causes Gartner names
  [S1]; poor visibility (26% see costs in real time) [S18].
- **Can veto:** a soft veto through budget approval and renewal; they can end a pilot.
- **Evidence they accept:** cost per agent and per outcome; budget adherence reports (FO-1 to
  FO-4).

### 2.6 Application and AI teams (agent builders, coding-agent users)

- **Needs:** instant databases or branches, MCP tools that just work, realistic but masked data,
  fast approvals, no ticket queues (Synthesis).
- **Fears:** governance that slows them; 57% of workers using unapproved tools cite slow
  approval processes [S28].
- **Can veto:** nothing formally, but they route around controls. Only 14.4% of organizations
  have full approval for their agent fleets [S30]. They champion a tool; they do not buy it.
- **Evidence they accept:** time to first query and approval latency (Synthesis).

### 2.7 SRE and I&O leadership (buyer of AI SRE tooling)

- **Needs:**
  - incident integration (PagerDuty, incident.io, Opsgenie);
  - runbooks;
  - SLO linkage;
  - root-cause analysis;
  - human-in-the-loop remediation;
  - hybrid support [S6].
- **Fears:** an AI-caused outage [S6]; a vendor startup failing [S6].
- **Can veto:** production remediation rights.
- **Evidence they accept:** MTTR and incident-avoidance metrics, and verified remediation
  outcomes (Synthesis).

### 2.8 CIO, executive sponsor, procurement

- **Needs:** clear ROI, vendor viability, support terms, a license fit for an OSS sidecar
  (Synthesis).
- **Fears:** agent washing [S1]; lawsuits and dismissals [S13].
- **Can veto:** program funding.
- **Evidence they accept:** a business case tied to reliability and cost outcomes, and reference
  customers (Synthesis).

### 2.9 Typical sign-off order (Synthesis, not sourced)

1. App or AI team champions a pilot in dev on branches.
2. DBA agrees to read-only telemetry.
3. Security reviews identity, data flows and the LLM provider.
4. Privacy or DPO signs off on what reaches the LLM.
5. DBA and SRE accept proposals and approve-each in production.
6. The change manager or CAB pre-authorizes standard-change classes.
7. FinOps sets budgets.
8. Audit samples the evidence at the next cycle.

Each step corresponds to a rung or mode in Section 3.

---

## 3. Readiness ladder and earned autonomy

### 3.1 Two axes, not one

Enterprises grant trust along two independent axes (Synthesis, grounded in [S6][S31][S23]):

- **Scope**, what the agent may touch: rungs R0 to RD below.
- **Autonomy mode**, how the agent may act within that scope:
  - **M0 Observe.** Telemetry only; no proposals.
  - **M1 Propose.** Recommendation with evidence; a human executes.
  - **M2 Approve-each.** The agent executes after an explicit, artifact-bound approval (AP-6).
  - **M3 Standard change.** A pre-authorized class executes within an envelope; humans are
    notified and review afterwards. This borrows ITIL's standard-change idea; the ITIL
    definition was not checked against a fetched source (UNVERIFIED).
  - **M4 Autonomous within budget.** The agent decides within budgets and limits, with automatic
    verification and rollback and a periodic review.

Trust is granted per action class (for example index build, vacuum, config change, query
cancel, DML, DDL, provision, destroy), per environment (dev, branch, stage, prod) and per
database tier. A coding agent may be at RD/M4 on its own disposable branches and at R0/M0 in
production at the same time. Environment separation (SF-1) is what makes this safe.

This matches the data. Two thirds or more of agentic decisions are still human-verified [S31].
Dynatrace also reports that only 13% rely solely on fully autonomous agents and that
respondents expect a 50/50 human/agent split for IT (vendor blog summary, UNVERIFIED) [S31].
Trust in full autonomy is low (27%) [S20], and Gartner's AI SRE guidance keeps humans in the
loop [S6].

### 3.2 The rungs

#### R0: Observe metadata and telemetry (no row data)

- **Required before granting:**
  - a distinct agent identity (ID-1) with a monitoring-only role (`pg_monitor`,
    `pg_read_all_stats`) [S85];
  - query text with literals redacted before it reaches an LLM (DP-2);
  - an approved LLM data flow (DP-3);
  - logs to the SIEM (AU-3).
- **Approvers:** DBA, security architecture (Synthesis).
- **Typical mode:** M0 to M1.

#### RA: Read production data

- **Required before granting:**
  - ID-1 to ID-4: identity, sponsor, delegation chain, short-lived credentials;
  - AZ-1: least privilege by schema, table and column, read-only transactions;
  - AZ-6: no privilege amplification, no `BYPASSRLS`, RLS respected for on-behalf-of access;
  - DP-1 and DP-2: classification and masking, with raw PII only for an approved purpose;
  - DP-5: database content treated as untrusted model input;
  - SF-2: `statement_timeout`, row and result caps, connection limits;
  - AU-1 to AU-3: audit, pgAudit corroboration, SIEM;
  - a kill switch tested (SF-3).
- **Approvers:** security, data owner, privacy or DPO, DBA.
- **Why so strict:** 33% of agents accessed or shared sensitive data and 23% were tricked into
  revealing credentials [S26]. The Supabase leak came from an over-privileged read path [S94].
- **Evidence to move to RB:**
  - 30 or more days of RA with zero policy violations;
  - DLP and DSPM showing no unexpected sensitive-data access (DP-4);
  - every session attributable to an agent and a human (ID-3);
  - a successful kill-switch drill.
  - Thresholds are Synthesis proposals; tune per customer.

#### RB: Write data (DML)

- **Required before granting:** everything in RA, plus:
  - SF-2: row-count caps per statement and transaction; no unqualified UPDATE or DELETE; no
    TRUNCATE without the two-person rule (AP-7);
  - SF-5: verified PITR or backup, and a pre-image or compensating action for each change class;
  - AP-1 to AP-3 and AP-6: change records, separation of duties, risk tiers, artifact-bound
    approvals;
  - AZ-3: JIT write grants that expire;
  - the data owner's approval per table class.
- **Approvers:** data owner, DBA, change manager; security for scope.
- **Why so strict:** 80% saw unintended agent actions [S26]; FINRA flags agents acting beyond
  their authority [S41]; DORA requires controlled, verified changes [S39].
- **Evidence to move to RC:**
  - a run of approved writes at M2 with 100% post-change verification passes;
  - at least one rehearsed rollback per change class;
  - zero unexplained data diffs in reconciliation;
  - change records accepted by internal audit in a sample test.
  - Synthesis proposal.

#### RC: Change schema and configuration (DDL, indexes, parameters, grants)

This is where pg_sage's own AI DBA actions sit: index builds, vacuum and maintenance,
parameter changes, query cancellation.

- **Required before granting:** everything in RB, plus:
  - lock safety: `lock_timeout`, `CREATE INDEX CONCURRENTLY` and no table rewrites without
    approval (Synthesis from DBA practice; [S86]);
  - a generated and reviewed rollback plan [S6][S45];
  - a backup taken before the change and restore drills [S6];
  - change windows and freeze calendars from ITSM (AP-4);
  - progressive fleet rollout (SF-7);
  - post-change verification against baselines (SF-6);
  - a GitOps or SQL-review path for application schema migrations (AP-5);
  - grants and role changes kept at M2 or stricter (AZ-3).
- **Approvers:** DBA, change manager or CAB (who pre-authorizes classes for M3), SRE.
- **Why so strict:** roughly 70% of outages come from changes [S90]. Gartner expects 90% of
  organizations to suffer an AI-caused outage by 2029 [S6].
- **Evidence to promote a class to M3 (standard change):**
  - the trust ledger shows at least N executed changes in the class at M2 with a verified
    benefit and no regressions, where N is per class (for example 30 index builds across at
    least 5 databases; Synthesis proposal);
  - the human acceptance rate of M1 proposals in the class is above an agreed threshold;
  - a rollback drill passed for the class within the last 90 days;
  - the CAB approves the standard-change definition once (envelope: tables, size limits, hours,
    rate), not each change. DORA research supports moving CABs from gatekeeping individual
    changes to defining process [S91].

#### RD: Provision or destroy databases (including branches and clones)

- **Ephemeral databases and branches** (agent sandboxes, test clones) need:
  - quotas and budgets (FO-2) and TTL (FO-3);
  - tags for owner and agent (ID-2, ID-7);
  - masked or synthetic data by default (DP-2);
  - automatic teardown with credential revocation (ID-8).
  - Agents can reach M4 here early (Synthesis).
- **Persistent or production databases** need:
  - IaC parity (Terraform or GitOps) so that state is not forked (Synthesis);
  - region policy (DR-2) and a security baseline (TLS, pgAudit on, no public endpoint, secrets
    held in a vault [S77]);
  - registration in inventory and CMDB (ID-7);
  - FinOps approval above thresholds (FO-2);
  - for destroy: deletion protection, a final backup kept for a grace period, a soft-delete
    window, the two-person rule (AP-7) and the data owner's approval. Entra soft-delete for
    agent identities is an analogous pattern [S56].
- **Approvers:** platform engineering, FinOps, security; the data owner for destroy.
- **Evidence for autonomy:** a teardown and TTL record with zero orphaned resources over a period;
  cost within budget; a restore-from-final-backup drill (Synthesis).

### 3.3 Promotion and demotion rules (Synthesis proposals for the spec)

- **Promotion inputs, all from the trust ledger:**
  - number of executions per class and environment;
  - verified-outcome rate;
  - human acceptance rate of proposals;
  - rollbacks (planned and unplanned);
  - policy denials (attempted actions blocked);
  - incidents attributed to the agent;
  - time since the last failure;
  - drift between plan and actual.
- **Who approves a promotion:** the human role that owns the rung. DBA for RC classes, data owner
  for RB, CAB or change manager for M3 definitions, security for any scope expansion. No agent
  self-promotes (AP-2).
- **Automatic demotion triggers:**
  - a failed verification or unplanned rollback takes the class back to M2;
  - any security signal (credential anomaly, prompt-injection detection, policy-violation spike)
    sends the agent globally to M1 or M0 (SF-3);
  - any data-loss event freezes the class pending review;
  - an expired sponsor (ID-2) moves the agent to M0.
- **Every promotion and demotion is itself an audited change** (AU-1), so auditors can see why an
  agent had the autonomy it had on a given date.

### 3.4 What evidence convinces each stakeholder to move up

| Stakeholder | Convincing evidence (Synthesis unless cited) |
|---|---|
| DBA | Trust-ledger history per class: plans, lock analysis, before-and-after metrics, zero regressions, rollback drills |
| Security | Unique identities in the IdP, decision logs in the SIEM, statistics on blocked attempts, kill-switch drill records [S22][S61] |
| Audit and compliance | Evidence packs linking each change to its ticket, approver, policy version and verification [S47][S84] |
| Data owner and privacy | DSPM and DLP reports showing access only within purpose; a masking coverage report [S59] |
| FinOps | Cost per agent and per outcome; budget adherence [S25] |
| SRE and I&O | Fewer incidents and lower MTTR; verified remediations; no AI-caused outage in the window [S6] |

### 3.5 Mapping to external maturity models

- **CSA Agent Identity Governance tiers** [S23]:
  - Foundation (inventory and ownership): required for R0 and RA.
  - Intermediate (automation and monitoring): required for RB.
  - Advanced (JIT orchestration and constrained delegation): required for RC and RD in regulated
    firms.
  - The mapping is Synthesis.
- **Gartner guardian agent types** [S2]:
  - Reviewer: pg_sage reviews SQL and migrations that other agents propose.
  - Monitor: pg_sage watches agent sessions and database health.
  - Protector: `policy.Gate` blocks or modifies actions; the kill switch.
  - Synthesis: pg_sage can be a database-specific guardian for other agents as well as an
    autonomous DBA, and the same ledger serves both.

---

## 4. Integrations: mandatory versus nice to have

Legend: **M** = mandatory (deal-blocking), **S** = should have, **N** = nice to have.
Segments: **Reg** = regulated enterprise (finance, health, public sector); **Ent** = other
security-mature enterprise; **SMB** = startups and mid-market.

| Integration | Reg | Ent | SMB | Why (evidence) | Minimum viable for pg_sage (Synthesis) |
|---|---|---|---|---|---|
| Human SSO (OIDC/SAML) and group-to-role mapping | M | M | S | 85% call IAM vital [S27]; AEGIS standards [S10] | OIDC login; IdP groups map to pg_sage roles; local accounts only for break-glass (SF-4) |
| SCIM provisioning | M | S | N | Lifecycle and offboarding (ID-8) [S50] | SCIM 2.0 users and groups |
| Agent identity federation (OAuth token exchange, XAA, Entra Agent ID, workload identity, SPIFFE) | M (by 2027; timing is Synthesis) | S | N | XAA in MCP [S62]; Entra GA [S56]; Google [S68]; AgentCore [S65] | MCP server as an OAuth 2.1 resource server per spec [S54]; accept JWTs from a configured issuer; XAA next |
| SIEM export | M | M | S | AU-3; break-glass alerting [S61]; OCSF [S55] | OCSF-shaped JSON to syslog, HTTP or OTLP; per-action correlation ID |
| Secrets manager (Vault, AWS SM, GCP SM, Azure Key Vault) | M | M | S | OWASP NHI2 and NHI7 [S50]; 44% use or plan static keys [S21]; Vault dynamic DB credentials [S77] | Read DB credentials from a secrets manager; Vault dynamic roles for agent credentials |
| ITSM and ticketing (ServiceNow, Jira Service Management) | M | S | N | DORA Art. 9(4)(e) [S39]; SOC 2 CC8.1 [S47]; ITSM integration [S6] | Create or attach change records; read freeze windows; approval callbacks |
| Chat approvals (Slack, Teams) | S | S | S | Speed; avoids the slow-approval failure mode [S28] | Approve or deny with artifact hash; identity checked through the IdP |
| Paging and incidents (PagerDuty, incident.io, Opsgenie) | S | S | N | AI SRE peers in Gartner's list [S6] | Webhooks out; incident-linked actions in |
| Data catalog and classification (Purview, Unity Catalog, Collibra, Snowflake Horizon) | M where PII/PHI | S | N | DP-1 [S60][S74] | Import column tags; fall back to a built-in classifier; never send sample rows to the LLM by default |
| DSPM | S | N | N | DP-4 [S59] | Export agent data-access events |
| Policy engine (OPA, Cedar) | S | S | N | PC-2 [S82][S66] | Export policies; optional external decision point (OPA HTTP) |
| PAM and JIT broker (Idira, Delinea/StrongDM, Teleport) | S | N | N | Existing PAM investments [S79][S80][S81] | Let PAM broker pg_sage's privileged sessions; record broker session IDs |
| IaC and GitOps (Terraform, GitHub or GitLab checks) | S | M (platform teams) | S | Platform engineering needs (Section 2.1); Bytebase-style review [S83] | Terraform provider or modules; migration-PR checks |
| Agent registries and CMDB (Agent 365, Okta Universal Directory, ServiceNow CMDB) | S | N | N | ID-7 [S57][S63] | Publish an agent inventory API; push to the registry later |
| Backup and PITR systems (pgBackRest, cloud snapshots) | M for RB+ | M for RB+ | S | SF-5 [S6][S45] | Verify recoverability before RB, RC or RD actions |
| Observability (OpenTelemetry, Prometheus, Datadog, Grafana) | S | S | S | AI SRE context [S6] | OTel metrics and traces of agent actions |
| FinOps tools (cloud cost APIs, FOCUS exports) | N | S | N | FO-1 [S24][S25]; FOCUS fit is UNVERIFIED | Cost-per-agent export (CSV or JSON) |

Notes:

- **Mandatory in practice, everywhere:** human SSO, SIEM export, secrets-manager credentials and
  database-side audit. These are what security reviews ask first (Synthesis from Section 2.3 and
  [S22][S50]).
- **Agent identity federation** is moving from S to M during 2026–2027. All major IdPs and clouds
  now ship it [S56][S62][S65][S68], and MCP has standardized Enterprise-Managed Authorization
  [S62]. A tool that accepts only its own static tokens will look legacy within about 18 months
  (Synthesis).
- **ITSM** is mandatory where DORA [S39], SOX-style change control or a CAB exists (finance,
  health, public), Synthesis. Elsewhere a Git- or chat-based approval trail with exportable
  evidence is enough, which matches DORA research on peer review [S91].

### 4.1 Spec implications (Synthesis)

1. Make the readiness ladder (scope rung × autonomy mode × environment × database tier) the
   organizing model of AgentDB policy, and let the trust ledger drive promotion and demotion with
   human sign-off (Section 3).
2. Treat external IdP tokens (OIDC, XAA, SPIFFE) as the primary agent identity. Keep pg_sage
   scoped tokens as the fallback for SMB and air-gapped use, and give each its own Postgres role.
3. Put `policy.Gate` in front of every path an agent can use to reach a database: MCP tools,
   brokered sessions and pg_sage's own executor. Log the policy version on each decision.
4. Ship the kill switch, break-glass and evidence-pack export before more provider runners.
   Security and audit can veto without them; nobody vetoes for lack of another provider.
5. Default to read-only, masked and branch-first. Production write and DDL access is earned per
   class.
6. Integrate before building: SIEM (OCSF), secrets managers (Vault dynamic credentials), ITSM
   change records and catalog tags.
7. Make cost attribution and hard budgets first-class for agent databases (TTL, quotas,
   per-agent cost), because escalating cost is one of the cancellation causes Gartner names [S1].
8. Position pg_sage as a database-specific guardian agent and AI DBA (reviewer, monitor,
   protector) that is independent of any one cloud. That is the gap the analyst and vendor
   landscape leaves open [S6][S7].

---

## 5. Sources

Access date for all sources: 2026-10-05. Publication dates are as stated by the source;
"n.d." means none was found.

### Analysts

- [S1] Gartner press release, "Over 40% of Agentic AI Projects Will Be Canceled by End of 2027",
  2025-06-25. ANALYST. Direct fetch blocked; figures verified via TechResearchOnline (n.d.).
  https://www.gartner.com/en/newsroom/press-releases/2025-06-25-gartner-predicts-over-40-percent-of-agentic-ai-projects-will-be-canceled-by-end-of-2027
  ; https://techresearchonline.com/news/gartner-agentic-ai-projects-termination-forecast/
- [S2] Gartner press release, "Guardian Agents will Capture 10-15% of the Agentic AI Market by
  2030", 2025-06-11. ANALYST. Verified via IT Brief Asia, 2025-06-12.
  https://www.gartner.com/en/newsroom/press-releases/2025-06-11-gartner-predicts-that-guardian-agents-will-capture-10-15-percent-of-the-agentic-ai-market-by-2030
  ; https://itbrief.asia/story/guardian-agents-set-to-secure-15-of-ai-market-by-2030
- [S3] Gartner press release, "Just 15% of IT Application Leaders Are Considering, Piloting, or
  Deploying Fully Autonomous AI Agents", 2025-09-30. ANALYST. Verified via CIO Dive.
  https://www.gartner.com/en/newsroom/press-releases/2025-09-30-gartner-survey-finds-just-15-percent-of-it-application-leaders-are-considering-piloting-or-deploying-fully-autonomous-ai-agents
  ; https://www.ciodive.com/news/tech-leaders-AI-agents-autonomous-Gartner/761511/
- [S4] Gartner press release, "40% of Enterprise Apps Will Feature Task-Specific AI Agents by
  2026, Up from Less Than 5% in 2025", 2025-08-26. ANALYST. Headline only; page blocked.
  https://www.gartner.com/en/newsroom/press-releases/2025-08-26-gartner-predicts-40-percent-of-enterprise-apps-will-feature-task-specific-ai-agents-by-2026-up-from-less-than-5-percent-in-2025
- [S5] Gartner press release, "25% of All Enterprise GenAI Applications Will Experience At Least
  Five Minor Security Incidents Per Year By 2028", 2026-04-09. ANALYST. Figures from search
  summaries of the release and trade coverage.
  https://www.gartner.com/en/newsroom/press-releases/2026-04-09-gartner-predicts-25-percent-of-all-enterprise-gen-ai-applications-will-experience-at-least-five-minor-security-incidents-per-year-by-2028
- [S6] Gartner, "Market Guide for AI Site Reliability Engineering Tooling", G00836089, D. Betts,
  C. Saunderson, H. Ennaciri, 2026-01-26. ANALYST. Full text read from a reprint hosted by
  NeuBird, a listed vendor. https://neubird.ai/nr/gartner-ai-sre-market-guide-2026.pdf
- [S7] Gartner, "Market Guide for Guardian Agents", 2026-02-25. ANALYST, read only through VENDOR
  summaries: Orchid Security on The Hacker News (2026-03) and a PlainID release (2026).
  https://www.gartner.com/en/documents/7509053
  ; https://thehackernews.com/2026/03/5-learnings-from-first-ever-gartner.html
  ; https://www.prnewswire.com/news-releases/plainid-named-as-a-representative-vendor-in-the-2026-gartner-market-guide-for-guardian-agents-302702675.html
- [S8] Gartner, "Market Guide for AI Trust, Risk and Security Management", 2025. ANALYST, read via
  Mindgard (VENDOR) summary, updated 2025-08-20.
  https://mindgard.ai/blog/gartner-ai-trism-market-guide
- [S9] Gartner press release, "Top Predictions for IT Organizations and Users in 2025 and Beyond",
  2024-10-22. ANALYST. The 25% breach figure is corroborated by VentureBeat, 2025-12-30. The 40%
  CIO guardian-agent figure is UNVERIFIED.
  https://www.gartner.com/en/newsroom/press-releases/2024-10-22-gartner-unveils-top-predictions-for-it-organizations-and-users-in-2025-and-beyond
  ; https://venturebeat.com/security/machine-identities-outnumber-humans-82-to-1-legacy-iam-cant-keep-up
- [S10] Forrester, "The AEGIS Framework" (n.d.); IT Brief launch story, 2025-08-22; TechTarget,
  M. K. Pratt, 2026-08-05. ANALYST plus NEWS.
  https://www.forrester.com/technology/aegis-framework/
  ; https://itbrief.com.au/story/forrester-launches-aegis-to-help-cisos-secure-agentic-ai-systems
  ; https://www.techtarget.com/cybersecurity/tip/How-the-AEGIS-framework-mitigates-agentic-AI-risks
- [S11] Infosecurity Magazine, "Forrester: Agentic AI-Powered Breach Will Happen in 2026",
  2025-10-02. NEWS on ANALYST.
  https://www.infosecurity-magazine.com/news/forrester-agentic-ai-breach-2026/
- [S12] Forrester blog, "Oktane 2026 Recap", 2026-10-01. ANALYST.
  https://www.forrester.com/blogs/oktane-2026-recap-okta-announces-a-unified-iam-control-plane-with-agent-governance-and-identity-details-remaining-unclear/
- [S13] IDC blog, Z. Sun, "Agent Governance Has Now Become a Core AI Investment", 2026-07-29
  (cites FutureScape Oct 2025). ANALYST.
  https://www.idc.com/resource-center/blog/ai-agent-governance-enterprise-investment/
- [S14] Security Boulevard, "IDC Survey Finds Identity Control Gaps for AI Agents" (sponsored by
  Commvault), 2026-07-17. ANALYST, vendor-sponsored.
  https://securityboulevard.com/2026/07/idc-survey-finds-identity-control-gaps-for-ai-agents/

### Consultancy and nonprofit research

- [S15] PwC, "AI Agent Survey", fieldwork 2025-04-22 to 2025-04-28. CONSULTANCY. Direct fetch
  blocked; figures from Techstrong.ai coverage.
  https://www.pwc.com/us/en/tech-effect/ai-analytics/ai-agent-survey.html
  ; https://techstrong.ai/agentic-ai/ai-agents-are-gaining-traction-in-enterprises-but-with-some-hiccups-pwc-survey/
- [S16] Deloitte press release, "State of AI in the Enterprise 2026", 2026-01-21. CONSULTANCY.
  https://www.deloitte.com/us/en/about/press-room/state-of-ai-report-2026.html
- [S17] Deloitte Insights, "AI agents are scaling faster than their guardrails", 2026-04-24.
  CONSULTANCY.
  https://www.deloitte.com/us/en/insights/topics/emerging-technologies/ai-agents-scaling-faster.html
- [S18] KPMG, "Q2 2026 AI Pulse", 2026-06-24. CONSULTANCY.
  https://kpmg.com/us/en/media/news/q2-ai-pulse-2026.html
- [S19] McKinsey, "The State of AI in 2025", 2025. CONSULTANCY. UNVERIFIED: primary not fetched;
  figures from secondary summaries.
  https://medium.com/@david.hung.yang/deep-dive-into-mckinseys-the-state-of-ai-in-2025-from-everyone-using-ai-to-a-few-using-it-6095987cec14
- [S20] Capgemini Research Institute, "Rise of agentic AI: How trust is the key", July 2025.
  CONSULTANCY. Figures from search summaries of the report and release.
  https://www.capgemini.com/insights/research-library/ai-agents/
- [S21] Cloud Security Alliance and Strata Identity survey release, 2026-02-05. NONPROFIT,
  vendor-funded.
  https://cloudsecurityalliance.org/press-releases/2026/02/05/cloud-security-alliance-strata-survey-finds-that-enterprises-are-in-time-to-trust-phase-as-they-build-ai-autonomy-foundations
- [S22] Cloud Security Alliance and Aembit survey release, 2026-03-24. NONPROFIT, vendor-funded.
  https://cloudsecurityalliance.org/press-releases/2026/03/24/more-than-two-thirds-of-organizations-cannot-clearly-distinguish-ai-agent-from-human-actions
- [S23] CSA AI Labs, "Agent Identity Governance Framework v1" (draft), 2026-03-27. NONPROFIT.
  https://labs.cloudsecurityalliance.org/agentic/agentic-identity-governance-framework-v1/
- [S24] FinOps Foundation, "State of FinOps 2026", 2026 (exact date UNVERIFIED). NONPROFIT.
  https://data.finops.org/
- [S25] FinOps Foundation, "FinOps for AI Overview", 2026-02-17. NONPROFIT.
  https://www.finops.org/wg/finops-for-ai-overview/

### Vendor-sponsored surveys

- [S26] SailPoint (fieldwork by Dimensional Research), "AI agents: The new attack surface"
  release, 2025-05-28. VENDOR-SURVEY.
  https://www.sailpoint.com/press-releases/sailpoint-ai-agent-adoption-report
- [S27] Okta, "AI at Work 2025", 2025-08-12. VENDOR-SURVEY.
  https://www.okta.com/newsroom/articles/ai-at-work-2025--securing-the-ai-powered-workforce/
- [S28] Okta, "AI Agents at Work 2026", fieldwork March 2026. VENDOR-SURVEY.
  https://www.okta.com/newsroom/articles/ai-agents-at-work-2026-agentic-enterprise-security/
- [S29] CyberArk, "2025 Identity Security Landscape" release, 2025-04-23. VENDOR-SURVEY.
  https://www.gurufocus.com/news/2797088/machine-identities-outnumber-humans-by-more-than-80-to-1-new-report-exposes-the-exponential-threats-of-fragmented-identity-security-cybr-stock-news
- [S30] Gravitee, "State of AI Agent Security 2026", 2026-02-04. VENDOR-SURVEY.
  https://www.gravitee.io/blog/state-of-ai-agent-security-2026-report-when-adoption-outpaces-control
- [S31] Dynatrace, "Pulse of Agentic AI 2026" release, 2026-01-22. VENDOR-SURVEY. The 13% and
  50/50 figures come from the Dynatrace blog summary and are UNVERIFIED.
  https://www.dynatrace.com/news/press-release/pulse-of-agentic-ai-2026/
  ; https://www.dynatrace.com/news/blog/agentic-ai-report-reliable-autonomous-operations/
- [S32] Redgate, "2026 State of the Database Landscape", 2026. VENDOR-SURVEY.
  https://www.red-gate.com/solutions/state-of-database-landscape/

### Law, regulation, standards and specifications

- [S33] EU AI Act implementation timeline (Future of Life Institute site), updated 2026-08-31.
  https://artificialintelligenceact.eu/implementation-timeline/
- [S34] European Commission, "AI Act" policy page (AI Omnibus in force 2026-07-27), page dated
  2026-08-03. LAW. https://digital-strategy.ec.europa.eu/en/policies/regulatory-framework-ai
- [S35] EU AI Act Article 12, Record-keeping. LAW.
  https://artificialintelligenceact.eu/article/12/
- [S36] EU AI Act Article 14, Human oversight. LAW.
  https://artificialintelligenceact.eu/article/14/
- [S37] EU AI Act Article 26, Obligations of deployers. LAW.
  https://artificialintelligenceact.eu/article/26/
- [S38] EU AI Act Annex III, High-risk AI systems. LAW.
  https://artificialintelligenceact.eu/annex/3/
- [S39] DORA Article 9, Protection and prevention (unofficial consolidated text); EIOPA DORA page
  (applies from 2025-01-17). LAW.
  https://www.digital-operational-resilience-act.com/Article_9.html
  ; https://www.eiopa.europa.eu/digital-operational-resilience-act-dora_en
- [S40] NYDFS industry letter, "Cybersecurity Risks Arising from AI", 2024-10-16. REGULATOR.
  https://www.dfs.ny.gov/industry-guidance/industry-letters/il20241016-cyber-risks-ai-and-strategies-combat-related-risks
- [S41] FINRA, "2026 Annual Regulatory Oversight Report", GenAI section, 2025-12-09. REGULATOR.
  https://www.finra.org/rules-guidance/guidance/reports/2026-finra-annual-regulatory-oversight-report/gen-ai
- [S42] 45 CFR 164.312, HIPAA technical safeguards (Cornell LII). LAW.
  https://www.law.cornell.edu/cfr/text/45/164.312
- [S43] PCI Security Standards Council blog, "Now is the time ... future-dated requirements of
  PCI DSS v4.x" (requirements effective 2025-03-31). STANDARD.
  https://blog.pcisecuritystandards.org/now-is-the-time-for-organizations-to-adopt-the-future-dated-requirements-of-pci-dss-v4-x
- [S44] GDPR Articles 22, 28 and 32. LAW. https://gdpr-info.eu/art-22-gdpr/
  ; https://gdpr-info.eu/art-28-gdpr/ ; https://gdpr-info.eu/art-32-gdpr/
- [S45] ISMS.online, ISO 27001:2022 Annex A 8.32 Change Management (n.d.). STANDARD, secondary.
  https://www.isms.online/iso-27001/annex-a/8-32-change-management-2022/
- [S46] ISMS.online, ISO/IEC 42001 overview (standard published Dec 2023). STANDARD, secondary.
  https://www.isms.online/iso-42001/
- [S47] ISMS.online, SOC 2 CC8.1 change management explained (n.d.). STANDARD, secondary.
  https://www.isms.online/soc-2/controls/change-management-cc8-1-explained/
- [S48] NIST CAISI, "AI Agent Standards Initiative", launched 2026-02-17. NONPROFIT/GOVERNMENT.
  https://www.nist.gov/caisi/ai-agent-standards-initiative
- [S49] OWASP GenAI Security Project, "Top 10 for Agentic Applications 2026", 2025-12-09; release
  post 2025-12-10. NONPROFIT.
  https://genai.owasp.org/resource/owasp-top-10-for-agentic-applications-for-2026/
  ; https://genai.owasp.org/2025/12/09/owasp-genai-security-project-releases-top-10-risks-and-mitigations-for-agentic-ai-security/
- [S50] OWASP, "Non-Human Identities Top 10", 2025. NONPROFIT.
  https://owasp.github.io/www-project-non-human-identities-top-10/2025
- [S51] European Commission, "Data Act" (applicable 2025-09-12). LAW.
  https://digital-strategy.ec.europa.eu/en/policies/data-act
- [S52] Microsoft, "Microsoft completes landmark EU Data Boundary", 2025-02-26. VENDOR.
  https://blogs.microsoft.com/on-the-issues/2025/02/26/microsoft-completes-landmark-eu-data-boundary/
- [S53] IMDA Singapore, "Singapore Launches New Model AI Governance Framework for Agentic AI",
  2026-01-22. GOVERNMENT. Content UNVERIFIED: page body not retrievable.
  https://www.imda.gov.sg/resources/press-releases-factsheets-and-speeches/press-releases/2026/new-model-ai-governance-framework-for-agentic-ai
- [S54] Model Context Protocol specification, Authorization, version 2025-11-25. SPEC.
  https://modelcontextprotocol.io/specification/2025-11-25/basic/authorization
- [S55] Open Cybersecurity Schema Framework (OCSF) schema browser, v1.9.0. SPEC.
  https://schema.ocsf.io/

### Vendor documentation and announcements

- [S56] Microsoft Learn, "What's new in Microsoft Entra Agent ID", ms.date 2026-05-01, updated
  2026-08-13. VENDOR. https://learn.microsoft.com/en-us/entra/agent-id/whats-new-agent-id
- [S57] Microsoft Learn, "Agent Registry in Microsoft 365 admin center", 2026-09-21. VENDOR.
  https://learn.microsoft.com/en-us/microsoft-365/admin/manage/agent-registry
- [S58] Microsoft, "Ignite 2025 Book of News" (Agent 365 launch), Nov 2025. VENDOR. Figures from
  search summaries. https://news.microsoft.com/ignite-2025-book-of-news/
- [S59] Microsoft Learn, "DSPM for AI (classic)", ms.date 2025-12-15, updated 2026-06-25. VENDOR.
  https://learn.microsoft.com/en-us/purview/dspm-for-ai
- [S60] Microsoft Learn, "Connect to and manage PostgreSQL" (Purview), 2026-01-22. VENDOR.
  https://learn.microsoft.com/en-us/purview/register-scan-postgresql
- [S61] Microsoft Learn, "Manage emergency access admin accounts", 2026-06-04. VENDOR.
  https://learn.microsoft.com/en-us/entra/identity/role-based-access-control/security-emergency-access
- [S62] Okta, "Cross App Access extends MCP to bring enterprise-grade security to AI agent
  interactions", 2025-11-25. VENDOR.
  https://www.okta.com/newsroom/articles/cross-app-access-extends-mcp-to-bring-enterprise-grade-security-to-ai-agents/
- [S63] Okta press release, "Okta brings first-class identity to AI agents with Agent SSO",
  2026-08-24. VENDOR.
  https://www.okta.com/newsroom/press-releases/okta-brings-first-class-identity-to-ai-agents-with-agent-sso/
- [S64] SiliconANGLE, "Okta expands Cross App Access ecosystem", 2026-06-23. NEWS.
  https://siliconangle.com/2026/06/23/okta-expands-cross-app-access-ecosystem-secure-ai-agent-connections/
- [S65] AWS, "Amazon Bedrock AgentCore is now generally available", Oct 2025 (2025-10-13 per
  aws-news.com). VENDOR.
  https://aws.amazon.com/about-aws/whats-new/2025/10/amazon-bedrock-agentcore-available
- [S66] AWS, "Policy in Amazon Bedrock AgentCore is now generally available", 2026-03-03. VENDOR.
  https://aws.amazon.com/about-aws/whats-new/2026/03/policy-amazon-bedrock-agentcore-generally-available/
- [S67] AWS Security Blog, "Why Policy in Amazon Bedrock AgentCore chose Cedar", 2026-05-20.
  VENDOR.
  https://aws.amazon.com/blogs/security/why-policy-in-amazon-bedrock-agentcore-chose-cedar-for-securing-agentic-workflows/
- [S68] Google Cloud blog, "What's new in IAM: Security, governance, and runtime defense",
  2026-05-06. VENDOR.
  https://cloud.google.com/blog/products/identity-security/whats-new-in-iam-security-governance-and-runtime-defense
- [S69] Google Cloud docs, "Agent Identity overview" (n.d.). VENDOR.
  https://docs.cloud.google.com/iam/docs/agent-identity-overview
- [S70] Databricks docs, Lakebase Postgres (n.d.). VENDOR. https://docs.databricks.com/aws/en/oltp/
- [S71] Databricks docs, agent authentication (n.d.). VENDOR.
  https://docs.databricks.com/aws/en/generative-ai/agent-framework/agent-authentication
- [S72] Databricks docs, MCP on Databricks, updated 2026-09-11. VENDOR.
  https://docs.databricks.com/aws/en/generative-ai/mcp/
- [S73] Snowflake docs, Cortex Agents (n.d.). VENDOR.
  https://docs.snowflake.com/en/user-guide/snowflake-cortex/cortex-agents
- [S74] Snowflake, Horizon Catalog (n.d.). VENDOR.
  https://www.snowflake.com/en/product/features/horizon/
- [S75] Salesforce, Trusted AI and Einstein Trust Layer (n.d.). VENDOR.
  https://www.salesforce.com/artificial-intelligence/trusted-ai/
- [S76] ServiceNow, AI Control Tower product page. VENDOR. UNVERIFIED: page returned HTTP 403.
  https://www.servicenow.com/products/ai-control-tower.html
- [S77] HashiCorp Vault docs, database secrets engine (n.d.). VENDOR.
  https://developer.hashicorp.com/vault/docs/secrets/databases
- [S78] SPIFFE, overview (n.d.). NONPROFIT (CNCF project).
  https://spiffe.io/docs/latest/spiffe-about/overview/
- [S79] Teleport docs, MCP access (n.d.). VENDOR.
  https://goteleport.com/docs/enroll-resources/mcp-access/
- [S80] StrongDM, now part of Delinea, home page (n.d.). VENDOR. https://www.strongdm.com/
- [S81] Palo Alto Networks, Idira (CyberArk platform) (n.d.). VENDOR.
  https://www.paloaltonetworks.com/idira
- [S82] Open Policy Agent docs (n.d.). NONPROFIT (CNCF graduated).
  https://www.openpolicyagent.org/docs
- [S83] Bytebase home page (n.d.). VENDOR. https://www.bytebase.com/
- [S84] Kontext Security, "AI Agents and Compliance: What Security Teams Need to Know in 2026",
  2026-05-09, updated 2026-10-05. VENDOR blog.
  https://kontext.security/content/ai-agents-compliance-security-teams-2026

### PostgreSQL and engineering practice

- [S85] PostgreSQL docs, Predefined Roles (current). PROJECT DOCS.
  https://www.postgresql.org/docs/current/predefined-roles.html
- [S86] PostgreSQL docs, ALTER ROLE (current). PROJECT DOCS.
  https://www.postgresql.org/docs/current/sql-alterrole.html
- [S87] PostgreSQL docs, Row Security Policies (current). PROJECT DOCS.
  https://www.postgresql.org/docs/current/ddl-rowsecurity.html
- [S88] pgAudit repository README (n.d.). PROJECT DOCS. https://github.com/pgaudit/pgaudit
- [S89] PostgreSQL Anonymizer docs (stable). PROJECT DOCS.
  https://postgresql-anonymizer.readthedocs.io/en/stable/
- [S90] Google, "Site Reliability Engineering", Chapter 1 Introduction (2016 book, online).
  INDEPENDENT. https://sre.google/sre-book/introduction/
- [S91] DORA (DevOps Research and Assessment), "Capabilities: Streamlining change approval"
  (n.d.). INDEPENDENT research. https://dora.dev/capabilities/streamlining-change-approval/
- [S92] Wikipedia, "Change-advisory board" (n.d.). REFERENCE.
  https://en.wikipedia.org/wiki/Change-advisory_board

### Incidents

- [S93] Fortune, "AI-powered coding tool wiped out a software company's database", 2025-07-23.
  NEWS.
  https://fortune.com/2025/07/23/ai-coding-tool-replit-wiped-database-called-it-a-catastrophic-failure/
- [S94] General Analysis, "Supabase MCP can leak your entire SQL database", 2025-07-08.
  INDEPENDENT security research. https://www.generalanalysis.com/blog/supabase-mcp-blog

### Claims marked UNVERIFIED in this document (summary)

- Forrester AEGIS control count of 39 [S10].
- Gartner prediction that 40% of CIOs will demand guardian agents by 2028 [S9].
- McKinsey 2025 agent figures [S19].
- Dynatrace 13% fully-autonomous and 50/50 split figures [S31].
- PCI DSS requirement-level details (8.6.x, 7.2.5, 10.4.1.1, 12-month log retention) [S43].
- NYDFS Part 500 phase-in specifics [S40].
- HIPAA Security Rule NPRM status [S42].
- DORA RTS 2024/1774 detail [S39].
- Whether the AI Omnibus changed the AI literacy duty [S34].
- Singapore agentic framework content [S53].
- ServiceNow AI Control Tower capabilities [S76].
- FOCUS export fit [S24].
- OWASP Agentic Top 10 full list [S49].
- Exact Entra Agent ID GA month (April 2026 per secondary sources) [S56].
- ITIL definitions of standard, normal and emergency changes (ITIL text not fetched) [S92].
