# Session history: "pg_sage review and AI SRE feature spec" (2026-09-26 to 2026-10-06)

Research input for the AgentDB spec (`reviews/2026-10-05-agentdb/PROMPT.md`). Mined from the
Claude Code session transcript `72f07164-…jsonl` (about 61 MB, 29,150 lines when read; the
session was still live, so the snapshot ends at about 2026-10-06 01:31 UTC).

**Method.** Python scripts over the JSONL (no jq on this machine). Sources used, in order of
weight:

- **[U]** human-typed messages: string `user` entries plus mid-turn `queued_command` prompts,
  minus task notifications, CI-monitor events, compaction summaries and system reminders.
  77 typed messages; 75 distinct (two were re-pasted).
- **[A]** the assistant's user-facing text, especially its reply before each [U] message, which is
  needed to decode numbered answers such as "1. Migrate 2. Set it up for me".
- **[C]** the 7 automatic compaction summaries (09-26 23:18, 09-27 02:18, 09-28 13:03,
  10-02 17:43, 10-03 05:34, 10-03 21:43, 10-05 00:55).
- **[N]** sub-agent completion notices (review, fix, memo and build agents).
- **[W]** files the session wrote, read back from tool inputs: `MASTER-SPEC.md`,
  `AI-SRE-SPEC.md`, `ROADMAP.md`, `reviews/decisions/LEDGER.md`.
- **[R]** research files this session produced on 2026-10-05 for the LinkedIn post, still in the
  old session scratchpad (`…/72f07164-…/scratchpad/li/`). Their quotes from *other* threads are
  marked "(other thread)".

**Redactions.** On 2026-10-02 the user pasted an OpenAI API key in chat; it is not reproduced
here, and the session told the user to rotate it. Database passwords, cloud account and project
identifiers, the user's email and private table names from the dogfood database are left out.
The Codex reports path is shortened.

All times are UTC. The user works in US Central time (UTC-5).

---

## 0. Key findings for the AgentDB spec (one screen)

1. **The user never mentioned AgentDB in this session.** No [U] message contains agent DB,
   tenant, provisioning, MCP, Lakebase, Neon or Supabase. "Agents" in [U] always means Claude
   sub-agents. All AgentDB work was assistant-initiated, under the review mandate and the
   product principle.
2. **What AgentDB work happened was hardening, not product direction.**
   - 2026-09-26: a full review found 30 bugs (6 P0).
   - 2026-09-26: all P0s and P1s were fixed, partly. This added tenant-bound agent tokens,
     `create_uncertain`, receipt-checked destroy and the local-provisioning opt-in.
   - 2026-09-28: **D4** was decided and built. Humans stay global (one operator team per
     install). Approvals are attributed. Cloud registration needs an approved request that can be
     used once, including from blueprints and templates.
   - 2026-09-28: the runtime refactor gave AgentDB databases the analyzer, executor and policy
     gate they had lacked.
3. **After 2026-09-30 AgentDB disappears from the conversation.**
   - The 2026-10-02 AI-first ROADMAP has **no AgentDB track**.
   - Its "reach" phase went to agents as *callers* instead: MCP v2 for coding agents, the
     `pg_sage.specialist.v1` API for incident agents, and Ask Sage.
   - Sub-agents still logged AgentDB gaps:
     - RDS databases created by AgentDB never join the fleet.
     - 18 `agent_db_*` tables had no retention. Fixed in v1.8.3.
     - The ping handler's Bearer-prefix check is inconsistent with the agent API.
   - The live AgentDB cloud tests (RDS, Cloud SQL, Lakebase, 3 gauntlets) were skipped all
     session and never run.
4. **Deferred and never rescheduled:**
   - "AgentDB hardening before advertising multi-tenant use: tenant identities, plus a state
     machine for tearing tenants down" ([A] 09-28 01:01).
   - "Roadmap AgentDB principals", meaning human tenant grants (LEDGER D4).
   - The D4 memo's open question was never asked of the user: is one install ever shared by
     several customer teams?
5. **The user's own statements about AgentDB value come from other threads** (via [R]):
   - 2026-05-07: pg_sage is "an agentic Postgres DBA sidecar, not a full agent framework".
   - 2026-06-10: agent databases may be ephemeral, "in the 100s or 1000s per hour", poorly
     tuned, and need governance, security, chargeback and archival, with sporadic usage.
   - [R] also flags an unbuilt "control plane" that a stale, uncommitted launch blog still
     promises.
6. **Decided product frame, not to be reopened:**
   - pg_sage "gains trust and then becomes autonomous" (2026-09-28 correction).
   - LLM features are on by default; "we are becoming an ai dba" (2026-10-02 correction).
   - Publicly it is called an **agentic DBA**; trust is mentioned once, not made the theme
     (2026-10-05).
   - Versions are v2.x: v2.1.0 came after v1.10.0, and there is no v2.0.0.

---

## 1. Chronology of human-typed directives

Format: time, then a paraphrase with short quotes, then → what it led to. "(queued)" means the
message was typed while the assistant was working.

### 2026-09-26 (Sat): the review and AI SRE brief

- **20:00** Initial brief, pasted twice; the first send was interrupted at 20:00:35:
  - Review every feature group: first make sure there are no bugs, "No dead code paths that go
    nowhere", nothing half-wired.
  - Then improve every existing feature.
  - Then add new functionality: "an AI driven SRE feature". Do deep research, spec it out and
    write everything to files.
  - "Enrich this prompt"; meta-prompt for questions the user isn't asking.
  - Integrate Codex's findings ("codex astra" got the same prompt) "if they are good".
  - Then fix the bugs. "Don't stop until done. ultrathink".
  - → 10 feature-group review agents and 3 AI-SRE research agents, on worktree branch
    `claude/full-review-ai-sre-2026-09-26`.
- **20:13** Path to Codex's reports folder (`~/.codex/.chatgpt-projects/…/reports`).
- **20:19** "Codex is doing a testing pass. Check back in with codex for updates before you
  finish. Keep going!!!"
- **20:40** "Keep going". A session rate limit had killed the fix agents; they were resumed with
  SendMessage.
- **22:43 (queued)** "Codex update report after testing".
- **23:15** Answers to the 5 decisions in MASTER-SPEC §10.4:
  - "1. Should be autonomous after trust is earned": unused/invalid index drops and per-table
    autovacuum tuning run unattended once trust is earned.
  - "2 and 3 Do what is best": keep the Codex fixture changes; keep the meta-db trust ceiling and
    YAML-only `allow_private_targets`.
  - "4. Delete": the frozen C extension.
  - "5. Yes": move the review folder out of `docs/` so mkdocs doesn't publish it.
  - "Start fixing and building if that is what is next."

### 2026-09-27 (Sun): Azure, gate authority, AST validation

- **01:36** "1. Build Azure. We will setup an account and test tomorrow. 2. Yes. 3. Yes", plus
  "about to sleep so proceed as far as you can and around things that need input".
  - (1) Add an Azure provider adapter.
  - (2) Standing-policy change classes and windows also restrict operator-approved actions.
  - (3) Adopt `pg_query_go` (cgo) for parse-tree SQL validation and fix the release build.
- **17:34** "1. Migrate 2. Set it up for me 3. Fo what is best":
  - (1) Migrate older stored policy documents to the split change classes.
  - (2) Set up the Azure live test infrastructure.
  - (3) On a build without cgo, autonomous changes queue for human approval.
- **18:02** "Install the cli and use my browser and saved credit card to sign up to Azure. Make
  it happen." → The CLI was installed after the user approved the admin (UAC) prompt. The
  assistant **refused** to create the account or enter payment details; the user signed up.
- **23:18** "Azure cli installed. Test all the flavors of postgres at Azure if possible."
  → The live matrix passed on Flexible Server PG 14–18, General Purpose and elastic clusters
  (Citus). PG 11–13 are refused by design. Cosmos DB for PostgreSQL could not be created
  (being retired). Two Azure unit bugs were found and fixed (`kB`/`8kB`).
- **23:22 (queued)** "Added subscription".

### 2026-09-28 (Mon): ship v1.6.0, product principle, overnight D1–D8

- **00:59** "Is flexible azure's managed pg?" → Yes, it is Azure's main managed Postgres.
- **01:01** "So what is the list of next steps?" → A five-part list. It included the deferred
  product decisions (with the AgentDB tenancy and registration item) and "AgentDB hardening".
- **01:02** "Ship" → Pushed the 301-commit branch as PR #49 after a secret scan.
- **01:03 (queued)** "Rev and post a change log/what's new." → v1.6.0 changelog with a "What's
  new" section.
- **01:10** "Yes, make it so" → Auto-fix turned on for PR #49.
- **01:12** "Go" → The assistant held the merge: CI wasn't green yet, and the release would have
  shipped untested code.
- **01:14** "For #2. I want you to understand the project, gain trust and then become
  autonomous, and then do what is best." (#2 = the deferred product decisions.) → The assistant
  misread this as a trust ramp for *itself*: observe, then advisory, then autonomous, with memos
  for the user to ratify.
- **01:16** **Correction:** "No, the idea of the project is for pg_sage to gain trust and then
  become autonomous." → The assistant decided D1–D8 itself through that lens. It wrote the
  `feedback_product_principle` memory and a lesson.
- **01:18 (queued)** "Let's spin up separate agents to do 3 and 4 in parallel." → Agents for the
  structural refactors (#3) and Sage SRE M0 (#4).
- **01:21 (queued)** "Check the ci. One failure" → A flaky command-palette web test, fixed.
- **01:38** "I want you to keep going overnight. If something needs input, we circle back in the
  morning. Try to handle it on your own." → Eight agents overnight: D1+D2, D3, D4+D7, D5, D6,
  D8, the refactors and SRE M0, then M1 and M2.
- **11:19** "Where are we at?"
- **11:28** "1. Move forward. 2. The index should ship with the product if they are needed.
  3. Decide what is best."
  - (1) Merge PR #52.
  - (2) D6 earned IO admission stays **on by default**, so pg_sage builds the indexes it needs
    once it has load evidence.
  - (3) Keep every behaviour change, including the D4 approved-request registration and
    re-authorization of operator actions. The assistant added a startup notice for maintenance
    windows whose meaning changed (PR #53).
- **12:58** "Go for it" → Merge #54 (SRE M2) and cut v1.7.0.
- **13:01** "Do whatever you need to do to wrap it up. What is next?"

### 2026-09-30 (Wed)

- **16:12** "Keep going". **16:15** "Check it now and keep going...." → v1.7.0 tagged and
  published; PR #56 (avoided-incident credit) opened.

### 2026-10-02 (Fri): LLM on by default, M3–M7, v1.8.0/1.8.1, lifeos dogfood, AI-first review

- **00:44** Asked how to reach the session from the Claude Android app. Then "Yo", "Merge it from
  mobile...." (#56 merged) and "Go" (start Sage SRE M3).
- **00:59** **Correction:** "Llm features should be on by default and the whole purpose of pg
  sage. We are becoming an ai dba." → The M3 model turn, which had been off "until the bench
  gates pass", now defaults on, as does every other LLM feature (PR #57). Without a configured
  LLM, features fall back to deterministic behaviour.
- **02:04** "What is left after this?"
- **02:06** "I want m4-m7 to also be banged out overnight. Figure out how to get it done. Dont
  stop until done. Add agents. Make it happen." → PRs #58–#65 overnight.
- **15:28** "Give me a status summary".
- **15:56** "Yo, merge and push to github 1.8" → v1.8.0.
- **15:58** "What is next?" → The assistant proposed proving things live, running on a real DB,
  SRE follow-ups (item 3) and engineering debt (item 4). It also asked three product questions:
  should the bench get its own repo, is MCP or the UI the main channel, is hosted in scope.
- **16:04** Pasted an OpenAI key *(redacted)* and asked for the cheapest model ("luna or tera?").
  Also: run the live LLM tests; "Start 1.8 on the lifeos database"; "Set every trust level so it
  elevates in hours not days/weeks"; "Plan and execute 3 & 4"; monitor lifeos. → gpt-6-luna;
  dogfooding on the user's real `lifeos` database starts at autonomous trust with fast-trust
  settings. The key went into a mode-600 env file; rotation was advised repeatedly.
- **16:07 (queued)** "added oai credits".
- **17:13 (system)** The user started two suggested background tasks (timing-fragile tests) in
  separate sessions.
- **19:03** "That's a lot. Summarize since my last turn".
- **19:06** "Knock out next 1-5 and 72-74":
  - merge #73, then #74, and cut v1.8.1;
  - move lifeos onto it with the LLM on;
  - split main.go, executor.go and router.go;
  - fix the snapshot bloat;
  - turn on auto-fix for #72–#74.
- **21:45** "Promoted." The user believed they had promoted an autonomy level. Nothing was
  proposable, because shadow reviews were API-only. This exposed that trust could not be earned
  inside the product.
- **21:53** "What is next? One request when we started that i do not think we did. I wanted you
  to review all existing features … iterate on them expand on them … Think automated ai first. AI
  driven." → Six parallel reviewers produced `reviews/2026-10-02-ai-next/ROADMAP.md` and six
  area reports (PR #77). The Phase 0 safety fixes became v1.8.2.
- **23:47** "Latest status?"
- **23:56** "Install hypopg. What about existing feature update review?" → HypoPG installed in
  the running lifeos container, plus a per-feature table of what was fixed and what is next.
- **23:57** "What is cpu looking like?" → Found and mitigated a schema-guard CPU burn on lifeos
  (about 85% DB CPU).

### 2026-10-03 (Sat): performance, v1.8.2–v1.8.5, build the roadmap

- **00:49** "Let's add a performance review all around on pg_sage. It must ship high performing.
  We shouldn't be finding missing indexes or having to add hints." → Static SQL and measured
  audits, a blocking CI perf gate and partitioned history (v1.8.3).
- **10:59** "Merge it" (#87, the v1.8.3 cut).
- **16:09** "Wwhat is next?". **19:30** "What is the status?"
- **19:34** "Drop test schema. What about the existing feature enhancements and improvements?
  Have we built those? If not, plan and build"
  - The 160 leaked `test_*` schemas on lifeos were dumped (56 MB) and then dropped.
  - The plan `~/.claude/tasks/todo-2026-10-03-roadmap-build.md` split the work into waves 1–4.

### 2026-10-04 (Sun): Phase 1 and 2 done, Phase 3 started, v1.9.0/v1.10.0

- **11:38 (queued)** "What is status?"
- **11:41** "So is there a phase 2?" → Phase 2, "the model drives", has four items: 2.1
  investigator, 2.2 tuning agent, 2.3 ownership facts, 2.4 measure the model. Phase 3 is
  "reach".
- **11:43** "Let's knock out all of that." → All of Phase 2 plus the Phase 3 tracks, with
  parallel agents.
- **15:32** "Status?"
- **19:28** "We should go to 2.1 instead of 1.11 btw." → The next release became v2.1.0 instead
  of v1.11.0; the assistant took it literally and skipped 2.0.0.

### 2026-10-05 (Mon): v2.1.0, live clouds, v2.2.0, LinkedIn

- **00:35** "Are we at a stopping point?" **00:36** "I am almost at usage limit. Try to get it
  done." **00:38** "Include a change log." → Finish v2.1.0 and let the running agents end in PRs.
  A v1.8.5→v2.1.0 changelog was given in chat.
- **02:52** "Yo".
- **12:02** "What do we need to do to Cloud test?" → What a live test of the managed-cloud PR
  (#125) needs: an instance plus read-only credentials.
- **13:15** "Finish the pr's. Clis for aws and gcp are authd"
  - Release PR #126 (v2.2.0) combines #121–#125.
  - The AWS RDS live telemetry test passed. It found and fixed a false `shared_preload_libraries`
    drift finding.
  - GCP was blocked by billing in one project.
- **14:44** The user created a Cloud SQL instance themselves in another GCP project: "I didn't do
  anything to fix anything though. Try again." → The GCP test passed there. It found that Cloud
  SQL telemetry had never been collected, and the fix sends one metric per request. Both cloud
  test instances were deleted afterwards.
- **15:06 (queued)** "I haven't post a pg_sage update on LinkedIn in 4 months. Focus in on new
  features rather than bug fixes. Needs hype and viralbility"
- **15:11** "How long until done.? Does that clear our to do list?" → About 3 hours. Four
  engineering items remain: fleet learning, the index "replace" action, specialist API polish,
  history outside the monitored DB.
- **17:13** "how we going?"
- **18:19** "Do this --->" about the queued suggested task "Fix flaky disk-slot-keeping-up bench
  decoy". → Done in-session as PR #127.
- **23:20** "yo". **23:25** "give me the linkedin post again".
- **23:35** (sent twice) Rejected the draft:
  - "I mainly want to give a summary of what pg_sage is and then list the main features of the
    last few releases in the past 4 months."
  - "we are leaning too hard into earning trust. Its important but overdone."
  - "Let's call it an agentic DBA not an AI DBA."
  - Review all previous specs, threads and the GitHub repo.
  - "content is more important" than hype.
  - Audience: "DBAs, Dev, OSS maxis, accidental dba's, tech managers. Basically, geeks."
  - A funny meme, maybe the midwit one.
  - → Three research agents, six meme JPGs and a 2,930-character post. Saved to memory as
    `feedback_messaging.md`.

### 2026-10-06 (Tue): quickstart, v2.2.1 queue, v2.3 started

- **00:08** "I heard you are supposed to post a link in the first comment. Is that true? Why?"
  → Partly true. Links moved to a first comment. The docs site was found to be 404 because
  GitHub Pages was off.
- **00:16** "Test and verify the quickstart is valid line by line." → 24 of 28 checks pass, 2 fail
  (a sequence-runway miss and a stale MCP row) and 2 were untested. PR #131 fixes them.
- **00:46 (system)** The user started five suggested tasks in their own sessions:
  - finish moving startup code out of main.go;
  - skip replay reports in bench ingest;
  - clamp the SRE active-time charge at zero;
  - PG18 self-exclusion;
  - the first-look timeout.
- **01:11** "Ok, what's next?"
- **01:12** "1. yes. 2. yes. 4. Do it for me. 6. Do it."
  - (1) Merge #128 and #129.
  - (2) Ship v2.2.1 once #130 and #131 are green, then upgrade lifeos.
  - (4) Turn on GitHub Pages for the public docs site.
  - (6) Start v2.3: fleet learning, index replace, specialist mapping plus `query_id`, history
    outside the monitored DB.
  - Item 3 (post on LinkedIn) and item 5 (user to-dos) were left with the user.

**Shape of the user's input:** 77 typed messages over 10 days, with 20 on 10-02 alone. Most are
terse go/status/merge prompts ("Yo", "Go", "Ship", "Merge it", "Status?"). The few long messages
carry the direction: the initial brief, the AI-first feature review, the performance mandate,
the lifeos dogfood brief and the LinkedIn rejection.

---

## 2. Product decisions, rationale and user corrections

### 2.1 The product frame, as the user stated it

| Date | User statement | What it settled |
|---|---|---|
| 09-26 23:15 | "Should be autonomous after trust is earned" | Earned autonomy for index drops and autovacuum tuning (autonomous trust + `tier3_moderate` + 31-day ramp + open window) |
| 09-28 01:16 | "the idea of the project is for pg_sage to gain trust and then become autonomous" | **The product principle.** The assistant decides open product calls itself through this lens (memory `feedback_product_principle.md`) |
| 10-02 00:59 | "Llm features should be on by default and the whole purpose of pg sage. We are becoming an ai dba." | LLM features default on (PR #57). Trust comes from validation, citations, deterministic fallback and budgets, not from leaving the AI off |
| 10-03 00:49 | "It must ship high performing. We shouldn't be finding missing indexes or having to add hints." | pg_sage's own footprint is a release gate (blocking perf gate, v1.8.3) |
| 10-04 19:28 | "We should go to 2.1 instead of 1.11" | Versioning moves to v2.x (v2.1.0, v2.2.0, v2.3.0 next) |
| 10-05 23:35 | "call it an agentic DBA not an AI DBA"; trust "important but overdone" | Public copy leads with what pg_sage is, then features; trust gets one line (memory `feedback_messaging.md`) |

The memory files record the same frame: "pg_sage gains trust, then becomes autonomous… Human
approval is the route to autonomy, not a permanent crutch". Also: "pg_sage is an AI DBA… LLM
features… default to ON whenever an LLM is configured". The past-threads research [R] shows the
naming arc: "autonomous postgres dba agent" → "agentic postgres dba" (2026-04-08) → "AI DBA"
(10-02) → back to "agentic DBA" (10-05).

### 2.2 Decisions made in the session, mostly by the assistant under the principle

- **Gate and executor:**
  - `policy.Gate` is the only authority. Operator-approved actions are re-authorized right
    before they run, and change classes and windows bind operator approvals too (user "2. Yes",
    09-27).
  - With no gate attached, nothing executes.
  - One `buildDatabaseRuntime` serves all four modes: standalone, YAML fleet, meta-db and AgentDB.
  - One `Executor.Apply` pipeline: authorize → lease → slot → re-authorize → execute → verify.
- **SQL safety:** libpg_query parse-tree validation is a second layer. Release builds use cgo
  (`sql-ast: libpg_query`). A build without it queues autonomous changes for approval.
- **C extension deleted** from master (user "4. Delete"). The last source is at tag
  `c-extension-final`; mode `extension` fails at startup with guidance.
- **D1–D8** (ledger `reviews/decisions/LEDGER.md`, decided 09-28):
  - **D1:** enforce the refusal set, lock ceiling and serialize mode precisely. Refused
    self-initiated actions go to approval; unused-index drops stay earned-autonomous.
  - **D2:** one maintenance-window grammar with an optional timezone; a migration keeps the
    stored meaning.
  - **D3:** the value ledger is aggregated across the fleet, and credited rows are never purged.
  - **D4:** see §3.5.
  - **D5:** retention deletes use only an owner-declared column.
  - **D6:** earned IO admission. After a 7-day learned baseline (or declared capacity),
    `CREATE INDEX CONCURRENTLY` runs unattended in quiet periods. This is default-on: the memo
    had recommended opt-in, and the user confirmed default-on with "The index should ship with
    the product if they are needed".
  - **D7:** explicit SSO linking; never link accounts on email alone.
  - **D8:** the emergency stop survives restarts, is attributed, and has a header control.
- **Recommendation state machine:** a failed apply backs off and then becomes `abandoned`. An
  approval pins one revision's content hash. An operator rejection stays sticky.
- **Dogfood speed versus the human gate:** fast trust (ramp hours, promotion thresholds) is
  configurable, but promotion above L1 still needs admin approval.
- **Index verification:** unverified optimizer `CREATE INDEX` needs approval even at full
  autonomy (Phase 0), including installs without HypoPG. From v1.8.5, only HypoPG-verified index
  advice runs unattended.
- **Self-visibility:** pg_sage stops hiding from `pg_stat_statements`. It tags its own SQL,
  excludes it from analysis and reports its self-cost.
- **One trust system (v1.9.0):** each action class earns its level from verified outcomes, and
  `trust.level` is the cap. Shadow mode records what pg_sage would have done below its level.
  Hygiene and performance changes have separate blast-radius budgets.
- **Model authority (v1.10.0):** the model may override the causal graph per incident family
  only when the 95% Wilson lower bound of its held-out precision is at least 0.80 (16 of 16).
  Self-reported confidence is ignored.
- **Managed clouds (v2.2.0):** pg_sage never applies parameter-group or flag changes; approving
  one means "I will run it". Telemetry is on by default and never raises trust.
- **Self-configuration (v2.2.0):**
  - On by default.
  - Every key is classified as safety-critical, operator preference or derivable.
  - Derived values soak in shadow for 24 h, and an operator-set value always wins.
- **History outside the monitored DB:** deferred in v2.2.0 (design written), then started for
  v2.3 as an opt-in.
- **Publishing:** turning on GitHub Pages needed the user's OK; given 10-06 ("Do it for me").

### 2.3 User corrections and pushback, with what changed

1. **09-28 01:16, the product principle.** The assistant had turned "gain trust, then become
   autonomous" into a ramp for its own decision-making. Corrected: it is pg_sage's product
   principle, and the assistant should simply decide. → Deleted the wrong memory and recorded a
   lesson.
2. **10-02 00:59, LLM on by default.** The assistant had shipped the M3 model turn off by default
   pending bench gates. Corrected to on by default for all LLM features.
3. **10-02 21:53, a missed original requirement.** The user noticed the 09-26 "improve every
   existing feature" ask had not been done. → AI-first review and ROADMAP.
4. **10-03 19:34, checking the follow-through.** "Have we built those? If not, plan and build". →
   The waves 1–4 build plan.
5. **10-04 19:28, versioning.** v2.1 instead of v1.11.
6. **10-05 14:44, cloud test pushback.** The assistant reported GCP blocked by billing; the user
   showed another project worked. "Try again" led to the test passing there.
7. **10-05 23:35, messaging.** The trust-themed "AI DBA" draft was rejected (see §1). The
   assistant had also offered a "Built solo with Claude Code" closer and later withdrew it,
   because Codex/GPT built parts and Gemini reviewed code.
8. **10-02 21:45 "Promoted.", not a correction but a product signal.** The user could not earn
   trust through the UI. This drove ROADMAP Phase 1: review buttons, a promotion coach and one
   trust system.

**Assistant mistakes it disclosed, for context:**

- A repo-wide `git reflog expire`/`gc` wiped the stash list; the stash was restored.
- Codex evidence (a 29 MB `.exe`) was committed, which forced a history rewrite.
- One SendMessage was misrouted.
- A suggested `/explain` fix was wrong.
- The assistant wrongly claimed the executor skipped a finding; it was actually parked by the
  blast-radius limit.
- The assistant refused to create an Azure account or enter card data. That was a safety limit,
  not a mistake.

### 2.4 Standing constraints kept across compactions [C]

- **CLAUDE.md rules:** two-phase testing; `-count=1`; report skips; coverage floors; size limits
  of 50-line functions, 500-line files and 100-character lines; parameterized SQL.
- **Commits:** typed commit messages with Co-Authored-By.
- **Secrets:** never commit them; secret-scan before pushing.
- **CI:** never poll PR CI; auto-fix drives the fixes; merging green PRs is allowed.
- **lifeos:** read-only for agents; reconfigured only as part of an upgrade.
- **Cloud and LLM:** live tests are opt-in (`PG_SAGE_LIVE_*`); never create accounts or enter
  payment details.
- **Shared Docker VM:** the rules in `~/.claude/tasks/overnight-agent-rules.md`.

---

## 3. AgentDB, agent databases, tenancy and agents as users

### 3.1 Background from before this session (other threads, via [R] research files)

- **AgentDB origin:**
  - Specs `docs/superpowers/specs/2026-05-07-agent-database-deployment-module.md` and
    `2026-05-09-agentdb-live-provisioning-ga.md` (other thread).
  - "v1.1 AgentDB: provisioning and tuning for agent-created databases on RDS, Cloud SQL and
    Lakebase" (2026-05-10).
  - v1.5.0 (2026-09-07): "AgentDB, pg_sage's control plane for databases created for AI-agent
    workloads, now manages Neon branches and Supabase projects, with ownership, expiry and
    validated Terraform templates" (PR #48).
- **The user's 2026-05-07 framing of the module** (other thread):
  - pg_sage is "an agentic Postgres DBA sidecar, not a full agent framework".
  - The module lets agents request DB or schema deployments by API or UI.
  - pg_sage provisions, tunes, tracks cost, requires pings, archives and cleans abandoned DBs,
    backs up, and publishes machine-readable tuning recommendations.
- **The user's 2026-06-10 statement of the need** (other thread): "consider how we can serve
  databases deployed by agents. This is the agentDB portion."
  - They "may be ephemeral dbs", in the "100s or 1000s per hour".
  - They "may be poorly tuned".
  - They "may need governance, security, chargeback, archival".
  - They "may have sporadic usage patterns".
- 2026-05-09/10: the user had Opus 4.7 review AgentDB live-provisioning GA artifacts and
  end-user workflows for enterprise readiness (other thread).

### 3.2 2026-09-26: the group 8 (AgentDB) review [N]

The report is `reviews/2026-09-26/group-08-agentdb.md` (written under `docs/`, moved out later).
It found **30 bugs: 6 P0, 14 P1, 9 P2 and one P3 group.** Its verdict: "The approval layer for
live cloud create/destroy is solid. Almost everything around it is broken". pg_sage could leave
cloud databases running and billing forever, or delete one it doesn't own.

- **P0s:**
  - B01: the unauthenticated agent ping could set status `deleted` and hide a live database.
  - B02: archived or expired deployments were never revisited.
  - B03: re-registering orphaned a live instance and could move its tenant.
  - B04: destroy fell back to a guessed name with no ownership or tag check.
  - B05: no tenant isolation. Tenant, regions and approver came from the request body, and
    agents used operator cookies.
  - B06: failed creates leaked resources. A Neon or Supabase retry could create a second billed
    project.
- **Notable P1s:**
  - B07: the region allowlist was checked but ignored on AWS create.
  - B08: the cost cap could be bypassed (flat $100 for unknown sizes; extend-lease uncapped).
  - B09: GCP used a static token, deletion protection was on, and no credentials reached the
    agent.
  - B10: an operator "approve" overrode a policy deny.
  - B11–B12: the restore-verified gate was a UI checkbox.
  - B13–B15: UI live execute and destroy always returned 400; `secret_ref` was dropped.
  - B16: every AgentDB call, including the ping, re-ran about 100 schema statements, 35 of them
    taking exclusive locks, on a monitored production DB.
  - B17–B18: local provisioning ran `CREATE SCHEMA`/`CREATE DATABASE` on a monitored DB, and the
    emergency stop and trust level were never checked, even by the 5-minute auto-destroy.
- **Dead code and unwired paths:**
  - Monitoring claims and `ScheduleMonitoring` had no worker. The reviewer said to wire them,
    because the 60-second collector keeps Neon and Supabase scale-to-zero compute awake.
  - `DestroyProvisionLive` should be deleted.
  - `BudgetGate` was not enforced.
  - `HeuristicBlueprintGenerator` was test-only.
  - The 54 API tests used test-only routers with no authorization layer.
  - AgentDBs had only a collector, whose snapshot writes failed every cycle (no `sage` schema).
- **Could not verify:** Supabase/Neon name uniqueness, GCP's deletion-protection error, the
  Lakebase create-branch response shape, and the lock level of `ADD COLUMN IF NOT EXISTS`.

### 3.3 2026-09-26: fixes (branch `fix/2026-09-26-agentdb`, report `fixes-agentdb.md`) [N]

- **Fixed:**
  - All six P0s.
  - Every P1 at least partly; leftovers are marked DEFERRED.
  - 1,409 tests passed; agentdb coverage 74%.
- **B01:** a ping records only last-ping time and a checked health value.
- **B02:** the reconciler revisits archived live deployments and records a block reason. One bad
  row no longer stops the batch.
- **B03:** a re-register returns the existing row; another tenant reusing the id gets 409.
- **B04:** destroy needs a recorded resource id plus a creation receipt, and checks AWS tags or
  GCP labels. Deployments that only ever did a dry run can't be destroyed live.
- **B05:** **tenant-bound agent tokens**, minted by an admin and valid only on
  `/api/v1/agent-api/`. Tenant and agent come from the token, and allowed regions from server
  config. "Human operators and admins stay global, as documented."
- **B06:** the create id is saved before the provider call. An unclear result becomes
  `create_uncertain`, which blocks retry until a lookup resolves it.
- **Smaller fixes and dead code:**
  - A GCP token that refreshes.
  - `DestroyProvisionLive` deleted; `BudgetGate` wired in.
  - Dry-run backups can't record "verified".
  - Approval and audit actors come from the signed-in user.
  - Template approval is tied to the template's content hash.
- **New settings:**
  - `PG_SAGE_AGENTDB_LOCAL_PROVISIONING` (local DDL is opt-in).
  - `PG_SAGE_GCP_TOKEN_SOURCE`, `…_METADATA_URL` and `…_ACCESS_TOKEN_TTL_SECONDS`.
  - `secret_ref` must be `env:PG_SAGE_AGENTDB_*`.
  - The Supabase password env var became a master key with a derived password per project.
- **Codex tests:** Codex's `preflight_surface_agentdb` tests were adopted. One was changed
  because it encoded bugs B11 and B17.
- **Left as product decisions:**
  - Tenant scoping for human operators.
  - Whether cloud registration requires an approved request.
  - API routes check only the saved e-stop flag, not the fleet's in-memory stop.

### 3.4 How MASTER-SPEC treated AgentDB [W]

- **The P0 register:** P0-15 (ping), P0-16 (TTL never revisits), P0-17 (re-register orphans),
  P0-18 (destroy by derived name) and P0-19 (no tenant isolation; ambiguous create never
  reconciled). AgentDB's 14 P1s are listed too.
- **Executive summary:** autonomous installs are unsafe until the Wave-0 fixes land. These P0s
  fire once an operator "uses AgentDB live mode" — "That's exactly the path the product is
  selling".
- **"Questions you aren't asking":**
  - #1: every mode, AgentDB included, builds its own runtime.
  - #6: "Who may delegate authority? Human roles, standing policy, agent tokens and tenant scope
    are separate checks that are currently conflated (MCP, AgentDB)."
- **§10.5 deferred decisions:** "AgentDB: scoping human operators to tenants, and requiring an
  approved request before cloud registration". These became D4.

### 3.5 D4: AgentDB tenancy and registration (memo → decision → build)

- **Memo**, 09-28 01:21, `reviews/decisions/D4-agentdb-tenancy-and-registration.md`, on master
  `f99a302`:
  - **D4a, human tenant scoping:** keep human admins and operators global and state it as a
    product decision ("one operator team per install"). Record approvers. Later, if needed,
    add an opt-in per-user tenant grant table. Two-way door.
  - **D4b, approval before cloud registration:** require an approved, unused request and switch
    the UI to `/requests/{id}/provision`. Two-way door.
  - **Findings:** approve and deny recorded no approver, contradicting
    `docs/agent-db-deployments.md`. The UI got a request approved but then registered directly
    via `POST /agent-dbs`, so the approval was never linked or used up. The risk was bounded,
    because real cloud creates still needed admin live authorization plus the cost, region and
    TTL policy.
  - **Open question for the user:** is a single install ever shared by several customer teams?
    If so, tenant grants should come before any multi-team pilot. **Never asked.**
- **Decision in LEDGER (the assistant, 09-28):** "Humans stay global (one operator team per
  install) until multi-team installs exist (roadmap AgentDB principals). Record the approver on
  approve/deny. Cloud registration requires an approved, unused request".
  - Rationale: "A single operator team per install is the reality today. Tenant-scoping humans
    now adds ceremony without a user. Approval is how pg_sage earns the right to spend cloud
    money, so an approval that is never linked, consumed or attributed is not evidence."
- **Build** (`claude/auth-d4-d7`; commits `ffd4904`, then `cb235fb`):
  - A direct cloud `POST /agent-dbs` without `request_id` returns 409 "approved request
    required".
  - Approve and deny need a signed-in user and record `decided_by` and the time. Policy
    auto-approvals show `decided_by: "policy"`.
  - An approval is claimed atomically and only once. A reuse or a lost race gets 409, and a
    failed provision releases the claim.
  - A tenant, agent, provider or level that doesn't match the request gets 409.
  - `local_postgres` registers are unchanged (still gated by the env opt-in).
  - **Follow-up, sent back by the assistant:** blueprint and Terraform-template cloud plans must
    also consume an approved request. They reuse the same claim mechanism rather than making
    design approvals single-use. The audit event records the request approver, the consumer and
    the design approver.
  - Docs say "one operator team per install". **No tenant grants were built.** The first D4
    commit added 27 Go tests (14 store, 13 API); the blueprint/template follow-up added 8 more
    (5 store, 3 API), including a race test with exactly one winner.
- **Shipped** in PR #52 (merged 2026-09-28), so in **v1.7.0**. The PR description lists "Cloud
  database registration now requires an approved request" as a one-way behaviour change, and
  the user's "3. Decide what is best" kept it.

### 3.6 Runtime parity

The 09-28 structural refactor's parity tests showed "**AgentDB databases had no analyzer, no
executor and no policy gate at all**". AgentDB "only had a collector", and its status showed the
Postgres database name instead of the instance name. After `buildDatabaseRuntime`, AgentDB
databases get the full runtime in every mode: D6 IO sampler, SRE fast path and probes, value
source, e-stop restore.

### 3.7 Raised once and never scheduled again

- 09-28 01:01 [A], step 5 of the next-steps list: "**AgentDB hardening** before advertising
  multi-tenant use: tenant identities, plus a state machine for tearing tenants down."
- LEDGER D4: "roadmap AgentDB principals" (human tenant grants). These appear in no later plan.
- G8 leftovers marked PARTIAL or DEFERRED: B09 (GCP), B10/B14/B17/B18/B20, B21/B22/B24/B29/B30,
  and wiring the monitoring worker (SURF-08). The detail is in `fixes-agentdb.md`.
- Live provider verification: every AgentDB live test stayed skipped all session (AWS RDS,
  Cloud SQL and Lakebase provisioning plus three "gauntlets", `PG_SAGE_LIVE_*`). The session's
  live cloud work tested *monitoring* only: Azure parameters (09-27), RDS and Cloud SQL
  telemetry (10-05).

### 3.8 AgentDB after 09-30: absent from the plan, still noticed by sub-agents

- **The 2026-10-02 ROADMAP** (`reviews/2026-10-02-ai-next/ROADMAP.md`, PR #77) does not mention
  AgentDB. Its Phase 3 "Reach" tracks are:
  - MCP v2 for coding agents;
  - the Postgres specialist other agents call;
  - Ask Sage;
  - self-configuration;
  - managed clouds;
  - five-minute time to value;
  - a cheap, self-aware observer;
  - fleet learning.
- **Platform review [N]:** "RDS databases created through Agent DB never join the fleet."
- **API audit [N]:**
  - About 69 agent-db endpoints (agent-ping, 4 `agent-api` routes, 3 mux routes and about 61 in
    a hand-written subrouter).
  - Bearer tokens existed only for `/api/v1/agent-api/`: tenant-bound, admin-minted, checked by
    `requireAgentPrincipal`.
  - `agentDBTokenPingHandler` doesn't reject a header missing the `Bearer ` prefix, unlike
    `requireAgentPrincipal`.
- **UI audit [N]:**
  - The `/agent-dbs` page uses about 30 endpoints: requests, blueprints, Terraform, backups and
    deploy requests.
  - Hard-coded form defaults: `tenant_agent`, `agent_runner`, `local_schema_xs`, budget 10,
    `pgvector`.
  - mkdocs nav leaves out `agent-db-deployments`.
  - LLM use: AgentDB blueprint generation uses an LLM.
- **Perf audit, 10-03 [N]:**
  - The 18 lazily created `agent_db_*` tables had **no retention**. Fixed in v1.8.3; the cost
    samples are exempt as the budget ledger.
  - The shared test servers had leftover `agentdb_local_database_test_*` databases.
- **Product summary, 10-05 [R]:**
  - "AgentDB is a module, not a mode."
  - Databricks Lakebase is an "**AgentDB provisioning target only** (gated live branch
    creation). No monitoring adapter."
  - The README lists Lakebase but not Azure, which is stale.
- **LinkedIn research, 10-05 [R]:**
  - AgentDB was left out of the post.
  - "Unbuilt items: control plane, agent push mode, fleet learning".
  - The uncommitted launch blog "still promises a control plane".

### 3.9 Agents as users: what was built instead (v1.10.0 to v2.2.0)

- **MCP v2 for coding agents** (PR #113, v1.10.0):
  - **Protocol:** handshakes 2025-03-26 to 2025-11-25 plus stateless 2026-07-28, over stdio and
    HTTP.
  - **Tokens** (`internal/mcptoken`):
    - Scopes are read, propose and approve.
    - **Approve only ever goes on a person's own operator token; an agent token can never hold
      it.** A database check enforces this.
    - Tokens can be limited to chosen databases, expire after 1–90 days, are stored hashed, and
      have an admin UI page.
  - **Transport and auth:**
    - HTTP is now the default transport; stdio is "now an agent" (read and propose only).
    - HTTP MCP is **token-only**: a session cookie gets 401 `mcp_token_required`.
  - **Tools:**
    - Every tool takes a `database` argument.
    - `apply_migration` was fixed to take `{table, sql, cycle}`.
    - New tools: top queries with plans, safe EXPLAIN, HypoPG what-if, migration lint,
      mark-owned/exempt (stays proposed until a person confirms), query-to-source attribution,
      source-fix packets and report/verify.
  - **Gate path:** every MCP path into the gate clears the approval flags, so agents never
    approve and never confirm facts.
  - **Open questions it raised:** should source-fix verdicts feed the trust ledger, and where
    should the PG18 tag go?
- **Postgres specialist API** (PR #118, v2.1.0):
  - **Contract:** `pg_sage.specialist.v1` under `/api/v1/specialist` with OpenAPI 3.1; a golden
    test plus a breaking-change checker.
  - **Identity:** callers use MCP tokens. Read opens or attaches to an investigation; propose
    requests a remediation.
  - **Remediation:** callers can request only pg_sage's own candidates, by id, on concluded
    investigations. Custodian actions go through `Executor.Apply`, and "there is no field to
    approve, force or bypass anything".
  - **Safety and adapters:** per-token rate limits and caps; caller text is fenced. PagerDuty
    webhooks need a token plus a signature. A generic signed webhook exists. AWS DevOps Agent
    and Datadog are documented only.
  - **Product calls:** opening an investigation needs only read; a requested remediation is
    judged like pg_sage's own initiative; the contract is on by default (token-only).
  - **Left open:** mapping the investigator's result into `Snapshot`/`MapResult`, and an
    optional `query_id`. Both were started 10-06 in v2.3.
- **Ask Sage** (PR #121, v2.2.0):
  - **Engine and tools:** runs on `internal/agentloop`. At v2.2.0 it has 15 tools (12 read, 3
    propose-only, per [R]). An operator's full tool set briefly reached 17, over the LLM
    client's limit of 16, and was consolidated.
  - **Grounding:** a claim is kept only if it cites evidence containing its numbers.
  - **Writes:** it can open an investigation, queue one of pg_sage's own findings, or propose a
    fact, at most once per question and only with the propose scope.
  - **Limits:** it never executes, approves or confirms. An import-boundary test enforces this.
  - **Budget:** its own durable per-DB, per-user budget; on by default.
  - **Provenance:** proposed items are recorded as `proposed_via='ask_sage'`.
- **Counts at v2.2.0 [R]:** 48 MCP tools (28 read, 15 propose, 5 approve; agent tokens can never
  approve) and 415 config keys.

### 3.10 Related signals for an AgentDB spec

- **The AI-SRE spec [W] excluded "cloud provisioning"** (and automatic PR publication) from SRE
  actions through R2. It is allowed only as a reviewable manual script.
- **Market research, 09-26 [N]:**
  - The best distribution may be "the Postgres specialist that other AI SREs call".
  - Neon and PlanetScale test changes on branches before applying.
  - Two AI agents deleted production databases (Replit 2025, PocketOS 2026).
  - Practitioners' takeaway: prompt instructions are not a control. Typed actions behind a
    policy engine are the selling point.
- **Landscape, 10-02 [N]:** Neon tests DDL on a temporary branch. MCP servers are "almost all
  read-only or raw SQL". pg_sage's gated change MCP is unique, and it should "become the
  Postgres specialist that AWS, PagerDuty and Datadog agents call".
- **The ROADMAP's view of the platform [W]:** on managed clouds pg_sage was "almost read-only"
  before v2.2.0.

### 3.11 Open questions never answered by the user

- **D4a:** will one install ever serve several customer teams?
- **AI-SRE spec §14** (repeated 10-02):
  - Is MCP or the UI the main channel?
  - Should PGIncidentBench move to its own permissively licensed repo?
  - Which LLMs must be supported on day one?
  - Should the pre-incident runways come first? (They were built in M6.)
  - Is a hosted version in scope?
- **MCP v2:** should source-fix verdicts feed the trust ledger?
- **Specialist:** should opening an investigation need propose instead of read?

---

## 4. The AI SRE spec ("Sage SRE")

**When and why.** Written 2026-09-26 (20:29 UTC), from the user's initial brief: "It should be an
AI driven SRE feature. I want you to do deep research and spec it out." It is
`reviews/2026-09-26/AI-SRE-SPEC.md` (first under `docs/`). The MASTER-SPEC §6 summarizes it.

**Inputs:**

- **Three research agents:**
  - market (`research-ai-sre-market.md`, about 35 products);
  - prior art (`research-ai-sre-prior-art.md`; DBA-Bench at 17.9% Safe Pass vs 93.4% for a
    human DBA, D-Bot, OpenRCA failure modes, Google's L0–L4 autonomy);
  - code substrate (`research-ai-sre-substrate.md`; detection ran on the 600 s tick, Tier-2 LLM
    RCA could never fire, incidents never notified anyone, the LLM client couldn't call tools).
- **Codex's "Sage Incident Investigator" spec**, adopted nearly verbatim as the R1 core: three
  families, read-only, 35 acceptance checks.

**What it decided:**

- **Positioning:** "The open-source Postgres responder your AI SRE calls: self-hosted,
  deterministic detection, measured accuracy, and a reversible executor the LLM can't bypass."
- **Non-goals:** cross-service RCA, Kubernetes, on-call scheduling.
- **Ten principles:**
  - deterministic first;
  - catalog-only probe tools (the model never writes SQL);
  - every claim cites evidence, with typed probe results;
  - competing hypotheses with falsification;
  - abstention is a feature;
  - authority lives outside the model (gate plus repair contracts);
  - recovery is verified independently;
  - autonomy is earned per incident family, never by time;
  - untrusted text is data;
  - pg_sage's own actions are change events.
- **Releases:**
  - R0: substrate repairs.
  - R1: read-only investigator for locks, connections and WAL.
  - R1.1: approved cancel, ChatOps, SLO burn rates, change events.
  - R2: eight more families, pre-incident runways, typed runbooks, incident memory.
  - R3: earned autonomy L0–L4 per family × action class, game days, fleet canary.
  - Excluded through R2: arbitrary SQL, failover, restarts, global GUCs, slot drops, VACUUM
    FULL, **cloud provisioning**, automatic PRs.
- **Promotion bars:**
  - L2: ≥ 90% factual precision, ≥ 80% top-1, 0 safety violations, and a 30-day shadow with
    ≥ 95% accepted.
  - L3: adds ≥ 95% Safe Pass and ≥ 50 verified L2 recoveries.
  - L4: reserved.
  - Automatic downgrade on burn, failover, stale evidence or a concurrent action.
- **Surfaces:**
  - An MCP "distribution surface" of 7 `sre.*` tools. "The calling agent never holds database
    credentials. All authority stays in pg_sage."
  - The external-AI-SRE persona: Datadog Bits, Azure SRE Agent, AWS DevOps Agent, PagerDuty,
    Claude Code.
- **Evaluation:**
  - **PGIncidentBench**, open: replay cases, Docker fault programs with decoys, composite faults.
  - Per-family metrics, baselines always shown, pre-registered gates.
  - CHECK-36..42 added to Codex's CHECK-01..35.
- **Milestones M0–M7.**

**How it evolved:**

| When | Milestone or change | PR / release |
|---|---|---|
| 09-28 | M0: `plan_hash`, 60 s lock-chain fast path, `ChatWithTools`, evidence-bound narration. M1: probe registry, causal graph, leases and budgets | #52 → v1.7.0 |
| 09-28 | M2: investigation loop in every mode, API/MCP/export, Cases panel, first bench scenarios | #54 → v1.7.0 |
| 10-02 | M3: LLM review turn (reorder, one probe, cited claims), switched to **default on** after the user's correction; PGIncidentBench v1 with a hostile fake model | #57, #58 → v1.8.0 |
| 10-02 | M4: replay corpus, auto-start by default, start/stop/resume. M5: approved cancel of the blocking backend, ChatOps, SLO burn, signed change events. M6: 7 more families, runways, signed runbooks, incident memory. M7: earned autonomy L0–L4 per family, admin-approved promotion | #60, #62, #63, #64 → v1.8.0 |
| 10-02 | ROADMAP verdict: "the AI is an annotator, not the driver". Phase 2: "the model drives" | #77 |
| 10-04 | 2.4: model measurement (override authority per family by held-out Wilson bound). 2.1: tool-calling investigator, which became the default `sre.llm.mode` | #111 → v1.10.0; #116 → v2.1.0 |
| 10-04 | The "specialist" distribution idea became `pg_sage.specialist.v1` | #118 → v2.1.0 |
| 10-06 | Specialist ↔ investigator mapping plus `query_id` | v2.3 agent running |

**Still unmeasured or unproven at session end [R]:**

- The nightly live-model bench never ran, because the repo secret `PG_SAGE_BENCH_OPENAI_API_KEY`
  isn't set. "Don't claim the LLM improves diagnosis."
- The live Gemini replay hit a 429.
- Slack, Telegram and Prometheus were tested against fakes only.
- Factual precision has no human-graded result.

---

## 5. Release history (v1.2 to v2.2.0)

From the session's release research [R] (CHANGELOG plus `gh release`) and the `gh release edit`
titles seen in the transcript. There are 17 versions between 2026-06-05 and 2026-10-05; the 12
from v1.6.0 on were cut in this session.

| Version | Date | Title and contents (one line) |
|---|---|---|
| v1.2 | 2026-06-11 | "Autonomous DBA Core + LLM-Native Tier": query store, per-query verify-and-revert, maintenance autopilot, doc-grounded config advice, managed-cloud-aware config (tag only; GitHub release was only drafts) |
| v1.3 / v1.3.1 | 07-19 / 07-22 | Correctness and safety remediation: manual mode really manual, LLM endpoint SSRF guard, hard per-DB token budgets, 70% coverage floor |
| v1.4.0 | 07-23 | "Agent-Native Autonomy": standing policy gate, evidence ledger (`sage.decision`), verified index lifecycle (HypoPG + lease + revert), wraparound and WAL custodians, migrations rehearsed on a clone, gated MCP "for agents", value report |
| v1.5.0 | 09-07 | "Neon and Supabase": provider detection and portable actions, Supabase logs and metrics, Vector Evidence Lab, **AgentDB manages Neon branches and Supabase projects** (PR #48) |
| v1.6.0 | 09-27 (GH 09-28) | "Safety gate, SQL parse-tree validation, Azure": one gate authority incl. operator approvals, libpg_query AST check, Azure Flexible Server live-verified PG14–18, PG14–18 CI matrix and skip budget, C extension removed (PRs #49–#51) |
| v1.7.0 | 09-30 | "Sage SRE investigations, earned index autonomy": M0–M2, 60 s lock chains, `plan_hash`, D6 quiet-period index builds, durable recommendations, header e-stop, D1–D8 incl. **D4** (PRs #52–#55) |
| v1.8.0 | 10-02 | "Sage SRE: eleven incident families, approved actions, earned autonomy": M3–M7, runways, PGIncidentBench, approved cancel, SLO burn, signed runbooks, **LLM on by default** |
| v1.8.1 | 10-02 | "Fast trust, big-catalog fixes from dogfooding, current OpenAI models": fast-trust knobs, PgBouncer diagnosis, fleet hot reload, gpt-5/6 compatibility, lifeos fixes |
| v1.8.2 | 10-03 | "Safety first: reversible config, safe EXPLAIN, per-database trust, promote from the UI" (Phase 0) |
| v1.8.3 | 10-03 | "Ships high performing: pg_sage keeps its own footprint small": blocking perf gate, partitioned history, self-cost, stops hiding from pg_stat_statements, `agent_db_*` retention |
| v1.8.4 | 10-03 | "Dogfood fixes: idle sidecar CPU, verified indexes build themselves, snapshot cap works" |
| v1.8.5 | 10-03 (published 10-04) | "Safety: only verified index advice runs unattended" |
| v1.9.0 | 10-04 | "Earned trust: verified actions, shadow mode, approval cards": one trust ledger per action class, shadow mode, predicted vs observed verification, approval cards in UI/Slack/Telegram, rejection memory, Sigstore-signed bench |
| v1.10.0 | 10-04 | "The model earns authority: binding facts, model measurement, MCP v2": ownership facts, model-lift gate, MCP v2 with scoped tokens, `CREATE STATISTICS` |
| v2.1.0 | 10-04 (GH 10-05) | "The model drives: tool-calling investigator, tuning agent, specialist API": investigator, one tuning agent per DB, `pg_sage.specialist.v1`, one-second first look, pg_stat_statements capacity advice |
| v2.2.0 | 10-05 | "Ask Sage, self-configuration, managed clouds": Ask Sage, self-config, self-budget and pprof, RDS/Aurora/Cloud SQL telemetry and parameter-group proposals, one change per object (PRs #121–#126) |
| v2.2.1 | pending (10-06) | First-look own timeout and retry (#130), quickstart fixes (#131), plus #128/#129. Release watcher waiting on CI |
| v2.3.0 | planned | Fleet learning, index replace, specialist mapping plus `query_id`, history outside the monitored DB (4 agents started 10-06 01:14) |

**Scale (from [R] at 10-05):**

- 102 merged PRs since 06-05; 66 from `claude/` branches.
- 1,749 commits since 06-05; 1,253 of them in October 1–5.
- About 243k lines of Go plus about 359k lines of tests, and 11,297 test functions.
- 13 GitHub stars.

---

## 6. Most recent work (2026-10-03 to 2026-10-06)

**10-03:**

- **Performance mandate:**
  - Static and measured audits.
  - `sage.decision` growth: about 41k rows an hour from the gate's random evidence ids.
  - The live-update poller full-scanned 4 tables every 2 s.
  - Collector catalog cost and `clock_timestamp()` predicates.
  - Missing indexes on the ledger and 0% HOT updates on findings.
  - **Fixes:** a perf gate that blocks CI; daily partitions for query_store and snapshots;
    paced retention; a bounded `catalogread` helper.
  - **Result on lifeos (v1.8.4):** sidecar CPU about 110% → about 0%, the guard query
    14.2 s → 1.4 ms, decision writes about 38k/h → about 3/h.
- **v1.8.2, v1.8.3 and v1.8.4 released.**
- **"Drop test schema…":** the 160 leaked `test_*` schemas were dumped and dropped on lifeos. The
  build plan has four waves:
  - W1: verification, approval cards, signed bench.
  - W2: one trust system, budgets, shadow mode → v1.9.0.
  - W3: Phase 2 → v1.10.0/v2.1.0.
  - W4: Phase 3.

**10-04:**

- **v1.8.5, v1.9.0 and v1.10.0 released.**
- **Phase 2 PRs:**
  - #109 facts;
  - #110 dogfood round 2;
  - #111 model measurement;
  - #115 tuning agent (−12.2k/+6.6k lines; replaced the optimizer, advisor and tuner prompts);
  - #116 investigator.
- **Phase 3 PRs:**
  - #113 MCP v2;
  - #117 first look (time to first finding about 10 min → about 1.1 s);
  - #118 specialist;
  - #121 Ask Sage;
  - #123 self-configuration;
  - #124 observer;
  - #125 managed clouds.
- **Versioning switched to v2.1.0.**

**10-05:**

- **v2.1.0 released.** lifeos runs it: first look found 54 findings across 1,791 relations in
  about 1 s. The tuning agent and investigator are live there.
- **Integration:** #121–#125 were combined into release PR #126.
- **Live cloud tests:**
  - AWS RDS PG16: passed. Fixed a false `shared_preload_libraries` drift.
  - Cloud SQL: passed in a second project. Fixed telemetry that had never been collected.
  - Test instances deleted.
- **v2.2.0 released** after a bench flake and a GitHub Actions outage. lifeos runs 2.2.0.
- **Fixes:** #127 (bench decoy timeout, plus a clock-dependent Ask Sage test).
- **New bug found:** the first look is clamped by the 500 ms default query timeout (task chip,
  later PR #130).
- **LinkedIn post:** rewritten per the user's direction; six meme options; the docs site was 404
  because GitHub Pages was off.

**10-06:**

- **Quickstart check** against the 2.2.0 image: 24/28 checks pass. PR #131 fixes:
  - the sequence check (a `DEFAULT nextval()` without `OWNED BY` was missed);
  - read-only startup grant warnings;
  - the wrong trust log line;
  - self-size noise below 256 MB;
  - the quickstart's MCP row and bloat wording.
  - Left open (task chip): "Grant more" misses the `CREATE`-on-schema grant needed at higher
    trust.
- **On the user's go-ahead:**
  - #128 and #129 merged.
  - A v2.2.1 watcher waits for #130 and #131.
  - GitHub Pages enabled.
  - #131's conflict with #129 resolved.

**Open at the end of the transcript (about 01:31 UTC 10-06):**

1. **v2.2.1:** waiting for #130 (one failing check, handled by its own session) and #131 (CI
   rerun). Then cut the release, publish and upgrade lifeos.
2. **Four v2.3 agents running**, each tests-first, each opening a PR without merging:
   - **Fleet learning:** privacy-preserving schema fingerprints; look-alike priors as
     evidence only, "never raising authority or trust levels on their own… never crossing
     tenant/fleet boundaries"; aggregated fleet findings ("same missing index shape on 30
     tenant DBs"); need-based LLM budgets; leader election via an advisory lock or lease in the
     meta DB.
   - **Index replace:** a two-step durable action. Create the wider index concurrently, verify,
     then soft-drop the subsumed one with drop-window watching. Refuse constraint-backed
     indexes and respect binding facts. Hold the table lease for both steps; cap the class at
     L2.
   - **Specialist mapping:** additive v1 fields for investigator claims, verdict
     (agree/conclude/contest/unmodeled), adopted vs advisory root, transcript link; an
     optional `query_id` scope.
   - **History outside the monitored DB:** opt-in `history.store: meta|monitored`. History
     tables get a database identity column in the meta DB, one store interface, catalog joins
     split and joined in Go, a resumable migration, and refusal to start on an unmigrated mode
     change. The perf gate must pass in both modes.
3. **Task chips:** the replication-lag bench decoy flake; the `CREATE`-on-schema grant in
   "Grant more".
4. **The user's own to-dos:**
   - rotate the OpenAI key and the weak lifeos DB password;
   - confirm or reject the proposed ownership fact on one lifeos index;
   - post on LinkedIn after v2.2.1;
   - optionally make HypoPG permanent in the lifeos image and set the bench secret.

**Done earlier in the session, for reference:**

- Retention deletes through Apply.
- Operator typed-target lease.
- Fleet/meta hot reload.
- `serialize_mode` queue.
- The file splits.
- Detector episodes → incidents.
- PgBouncer diagnosis.
- Bench report retention.
- Avoided-incident credit.

---

## 7. What the user cares about (themes), and AgentDB signals

**Themes, ranked by how often and how strongly they come up:**

1. **Autonomy is the product, and so is speed of delivery.** "Don't stop until done", "keep going
   overnight", "Add agents. Make it happen", "Knock out…", "Let's knock out all of that". The
   user hands over decisions ("Do what is best", "Decide what is best") and expects
   end-to-end completion: fix, test, merge, release, deploy to lifeos.
2. **Earned trust, then autonomy, with the AI on.** Two explicit corrections set the frame:
   gain trust → autonomous, and LLM on by default. The user wants it to *act*: "The index
   should ship with the product if they are needed" and "Set every trust level so it elevates
   in hours not days/weeks" (for dogfooding). They also don't want trust to dominate the
   messaging.
3. **Following through on the original ask.** The user twice pointed out that "improve every
   existing feature" from 09-26 hadn't been done. They track their own requests.
4. **Real-world proof.** Dogfooding on their own database (lifeos), live Azure, AWS and GCP
   tests, "Test all the flavors", "Test and verify the quickstart is valid line by line".
5. **Performance and footprint.** "It must ship high performing"; "What is cpu looking like?"
6. **Releases and visibility.** A changelog and "what's new" for every release ("Rev and post a
   change log", "Include a change log"), v2.x versioning, a LinkedIn post after four quiet
   months, content over hype, a geek audience, and the label "agentic DBA".
7. **Low ceremony.** Terse prompts and remote control from a phone ("Merge it from mobile…").
   The user starts suggested task chips in parallel sessions.

**What the user said about AgentDB's value, direction or priority:**

- **In this session: nothing.** AgentDB never appears in the user's 77 messages. No decision
  about it was put to the user. D4 was decided by the assistant under the product principle,
  and the user's blanket "3. Decide what is best" (09-28 11:28) ratified the
  approved-request behaviour change.
- **Implied priority:** after AgentDB's bugs were fixed on 09-26/28, the user's direction (Sage
  SRE, the AI-first roadmap, performance, trust UX, agents-as-callers, managed clouds,
  messaging) moved the work away from it. Nothing the user said pulled it back. The 10-02
  roadmap and v2.3 plan contain no AgentDB item. This is an inference from silence, not a
  stated decision.
- **In other threads** (via [R]), the user's stated view is that agent-created databases are a
  real workload. They are ephemeral, high-churn (hundreds to thousands an hour), poorly tuned
  and sporadic, and they need governance, security, chargeback and archival. AgentDB is meant
  to be "the agentDB portion" of an agentic DBA, not a general agent framework.
- **Public positioning** (10-05 post, approved direction): AgentDB was left out. The
  "Agent-native" bullet features the MCP tools for Claude Code and Cursor and the specialist API
  for incident agents.

---

## Appendix: pointers

- **Review and spec files** (repo, after the move out of `docs/`):
  - `reviews/2026-09-26/{MASTER-SPEC,AI-SRE-SPEC,group-08-agentdb,fixes-agentdb}.md`
  - `reviews/2026-09-26/research-ai-sre-*.md` and `reviews/2026-09-26/codex/`
  - `reviews/decisions/{LEDGER,D4-agentdb-tenancy-and-registration,MORNING-2026-09-28}.md`
  - `reviews/2026-10-02-ai-next/{ROADMAP,platform,interfaces,landscape,…}.md`
  - Per-track reports: `reviews/2026-10-0x-*-report.md` (e.g. `2026-10-04-p3-mcp-v2-report.md`,
    `2026-10-04-p3-specialist-report.md`, `2026-10-04-p3-ask-sage-report.md`,
    `2026-10-04-p3-observer-report.md`, `2026-10-05-p3-managed-clouds-report.md`).
- **Plans and memory** (user home):
  - `~/.claude/tasks/todo-2026-10-03-roadmap-build.md` (final open list)
  - `~/.claude/tasks/todo-2026-10-05-v220.md`
  - `~/.claude/tasks/overnight-agent-rules.md`
  - `~/.claude/tasks/lessons.md`
  - memory `feedback_product_principle.md`, `feedback_messaging.md`, `project_pg_sage.md`
- **Session research** (old scratchpad `…/72f07164-…/scratchpad/li/`):
  - `product_summary.md` (verified feature inventory; "AgentDB is a module")
  - `release_features.md` (per-release list)
  - `transcripts_findings.md` (framing arc and past-thread quotes)
- **Key PRs:** see the release table above. AgentDB-relevant: #48 (pre-session, v1.5.0),
  #52 (D4 + runtime parity), #113 (MCP tokens), #118 (specialist), #121 (Ask Sage).
