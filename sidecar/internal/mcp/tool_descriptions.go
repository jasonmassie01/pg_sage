package mcp

// toolDescriptions say, for every tool, when to use it, what it returns
// and what it never does (mcp-01a), in the glossary's words (ux-06a).
// finishTool applies them; a tool without an entry keeps its own.
var toolDescriptions = map[string]string{
	"get_policy": "Use when you plan a request_change or optimize_query and need the rules " +
		"pg_sage's policy check applies to this database. Returns the standing policy's " +
		"version and profile. Never changes policy and never approves anything; " +
		"propose_policy_change asks a person for a change",
	"propose_policy_change": "Use when the standing policy blocks what you think it should " +
		"allow, or allows too much. Returns proposal_id and the dry-run impact (newly " +
		"allowed, newly blocked) and, while it waits, approval_url and status. Never changes " +
		"policy itself: an admin ratifies or rejects the proposal",
	"request_change": "Use when you want a database change by intent, not SQL: intent names " +
		"a kind plus that kind's arguments (one branch of its oneOf). Returns the policy " +
		"check's decision, evidence_id, reason and, when it ran, the outcome; a held change " +
		"adds approval_url and status. Never runs your SQL and never approves: a held change " +
		"waits for a person",
	"optimize_query": "Use when a query is slow: name it by query_id or query_text with goal " +
		"latency. pg_sage plans an index under standing policy and its policy check decides " +
		"whether it runs. Returns the decision, evidence_id, reason and outcome; a held " +
		"change adds approval_url and status. Never runs SQL you send and never approves: a " +
		"held change waits for a person.",
	"apply_migration": "Use when a migration (ADD UNIQUE, SET NOT NULL) must run online. " +
		"pg_sage rewrites it into expand steps, rehearses them on a clone and asks the " +
		"policy check before the rehearsal and each step. Returns the verdict, reason, plan " +
		"and the contract SQL it left for you. Never runs contract steps",
	"ensure_fk_indexes": "Use when foreign keys in a schema may lack supporting indexes " +
		"(slow deletes, lock waits on the parent). Returns the policy check's decision, " +
		"evidence_id, reason and outcome for the indexes pg_sage plans. Never builds an " +
		"index the policy check holds; a held change waits for a person",
	"declare_table_contract": "Use when a person states a table's intent: append_only, " +
		"retention (interval and the timestamptz, timestamp or date column whose age defines " +
		"it), expected_pk or exemptions. Returns the decision on the declaration, stored as " +
		"a confirmed fact. Needs the approve scope. Never infers the retention column",
	"register_consumer": "Use when a replication slot belongs to a consumer pg_sage must " +
		"protect (CDC, a replica): name slot_name and owner. Returns the decision on the " +
		"registration, stored as a confirmed fact. Needs the approve scope. Never registers " +
		"on an agent's word: it is a person's declaration",
	"set_maintenance_policy": "Use when pg_sage should maintain a scope differently (a patch " +
		"to its maintenance policy). Returns proposal_id and the dry-run impact and, while " +
		"it waits, approval_url and status; a person ratifies the proposal. Never applies " +
		"the patch itself and never approves it",
	"get_guarantee_status": "Use when you need to know whether pg_sage's safety invariants " +
		"hold on this database before you rely on it. Returns the machine-readable status of " +
		"each invariant. Never changes anything; read-only",
	"get_value": "Use when you report what pg_sage has been worth. Returns verified " +
		"DBA-hours saved by period, feature and database, incidents avoided, hours still " +
		"pending and a daily trend; partial and unavailable name any source that could not " +
		"be read. Never changes anything: it is read-only and never writes",
	"get_ledger": "Use when you audit what pg_sage's policy check decided. Returns the " +
		"newest policy decision records (evidence_id, decision, feature), narrowed by filter " +
		"{decision, feature, limit 1-1000, default 100}. Never approves and never changes a " +
		"record. It holds decisions, not results: get_outcomes has what an executed change " +
		"did",
	"sre_list_incidents": "Use when you look for what is wrong now or was recently. Returns " +
		"pg_sage's investigations of incidents and plan regressions: family, state, issue " +
		"and conclusion. Never opens or changes an investigation; read-only",
	"sre_get_investigation": "Use when you have an investigation id from sre_list_incidents. " +
		"Returns its observed facts, likely explanation, alternatives, ruled-out hypotheses " +
		"with evidence, missing evidence and operator steps. Never acts on those steps; " +
		"read-only",
	"sre_get_evidence": "Use when you need one evidence item an investigation cites. Returns " +
		"that item, typed and redacted. Never returns the unredacted value; read-only",
	"sre_get_transcript": "Use when you need how the tool-calling investigator reached its " +
		"conclusion. Returns its plan, each tool call with its redacted result and digest, " +
		"cited claims and the outcome with its authority. Never replays a tool call; " +
		"read-only",
	"sre_propose_action": "Use when a concluded investigation should be mitigated. Returns " +
		"the evidence-matched mitigation (cancel of the one root backend) with its repair " +
		"contract and policy decision, or why there is none. Never executes anything; " +
		"sre_request_execution queues it for a person",
	"sre_request_execution": "Use when sre_propose_action returned a mitigation that should " +
		"run. Returns the one approval item it queued in the existing approval flow. Never " +
		"executes: a person approves, then pg_sage rechecks the target and runs it",
	"sre_refresh_table_evidence": "Use when a statistics proposal needs table evidence under " +
		"15 minutes old: re-reads stat_tables once for a table a concluded or inconclusive " +
		"investigation observed, at most once a minute per investigation. Returns the fresh " +
		"evidence, or a refusal when the table's database or OID changed. Never executes a " +
		"change",
	"sre_propose_statistics": "Use when an investigation points at misestimated rows over " +
		"correlated columns: propose CREATE STATISTICS as your explicit hypothesis. Returns " +
		"the proposal with its repair contract and policy decision; pg_sage cites the newest " +
		"matching stat_tables evidence itself. Never executes",
	"sre_request_statistics": "Use when sre_propose_statistics returned a proposal that " +
		"should be built. Returns the one approval item it queued. Never executes: a person " +
		"approves, then pg_sage rechecks the table and column identities under the table " +
		"lock",
	"sre_rollback_statistics": "Use when a statistics object pg_sage created should go: it " +
		"is dropped after pg_sage rechecks it is that object. Returns the rollback's result. " +
		"Needs the approve scope (admin people only). Never restores the prior ANALYZE " +
		"statistics",
	"sre_list_slos": "Use when you need to know whether users are affected. Returns SLO " +
		"error-budget states (ok, ticket, page or unknown, with why), burn rates per window " +
		"and budget left. Never claims customer impact from a database proxy; only app SLIs " +
		"do. Read-only",
	"sre_get_slo": "Use when you need one SLO's history or whether it recovered. Returns its " +
		"state and state history and, with recovery_since, the SLI recovery decision. Never " +
		"certifies a recovery on missing data, counter resets or low traffic. Read-only",
	"sre_list_changes": "Use when you ask what changed before a problem started. Returns " +
		"signed deploys, migrations, feature flags, pg_sage's own changes, config changes, " +
		"DDL, statistics resets, restarts, failovers and extension changes around a " +
		"database. Never changes anything; read-only",
	"sre_list_runbooks": "Use when you look for a runbook that fits an issue. Returns a " +
		"database's typed runbooks: status (draft, signed, retired, invalid), whether each " +
		"runs and its latest version. Never runs one; read-only",
	"sre_get_runbook": "Use when you need one runbook's content. Returns every version's " +
		"DAG, content hash, signer and imported playbook text. Never runs or signs it; " +
		"read-only",
	"sre_runbook_runs": "Use when you need where a runbook has run. Returns the " +
		"investigations it ran in, with the version, path and proposal of each run. Never " +
		"runs it; read-only",
	"sre_similar_incidents": "Use when you want precedent for an investigation. Returns " +
		"similar past incidents and their operator-verified outcomes, as context. Never " +
		"counts them as evidence for this investigation; read-only",
	"sre_draft_runbook": "Use when you add a runbook, or a draft version of one (runbook_id " +
		"and base_version), from a typed definition. Returns the draft. Never runs a draft: " +
		"an admin signs it in pg_sage, and a signed version keeps running until the new " +
		"draft is signed",
	"sre_compile_runbook": "Use when you have a playbook in English and want a runbook " +
		"draft. Returns the draft pg_sage's model compiled. Never runs it until an admin " +
		"signs it in pg_sage",
	"sre_get_autonomy": "Use when you need how much pg_sage may do on its own. Returns the " +
		"autonomy level per incident family and action class (L0-L3), its cap, the level its " +
		"evidence supports, active downgrades, pending promotions and history. Never changes " +
		"autonomy; read-only",
	"sre_downgrade_autonomy": "Use when pg_sage should do less on its own for a family and " +
		"action class (\"*\" for every class of the family). Returns the lowered level. " +
		"Restricting is always allowed. Never raises autonomy: raising it needs a person's " +
		"approval in pg_sage",
	"sre_review_investigation": "Use when a person has judged a concluded or inconclusive " +
		"investigation: accept or reject its diagnosis, with an optional note and actual " +
		"root cause (graph node id or free text). Returns the recorded review and outcome. " +
		"Never counts toward promotion and never approves one: only a person's review in the " +
		"UI or REST API counts",
	"sre_evaluate_autonomy": "Use when you want pg_sage to propose the promotions its " +
		"evidence supports now. Returns the promotions proposed and, for every other family " +
		"and action class, why not (each unmet check and how to meet it). Never promotes: an " +
		"admin approves every promotion",
	"list_facts": "Use when you need what pg_sage knows about a database: who owns an " +
		"object, test fixtures, CDC slots, archives, table windows, the owner brief. Returns " +
		"proposed facts awaiting a person and confirmed ones, which bind pg_sage. Never " +
		"treats the owner brief as binding; read-only",
	"propose_fact": "Use when you know something about the database pg_sage should respect, " +
		"with the evidence for it. Returns the proposed fact. Never binds anything until a " +
		"person confirms it; a confirmed fact only narrows what pg_sage does (an app-owned " +
		"index changes through a PR, not pg_sage's DDL)",
	"decide_fact": "Use when a person confirms or rejects a proposed fact, or rejects a " +
		"confirmed one. Returns the fact's new state. Needs the approve scope. Never widens " +
		"what pg_sage does: a confirmed fact only narrows or redirects it",
	"list_databases": "Use when you first connect, or when an error says database_required " +
		"or unknown_database. Returns the monitored databases you may name in every other " +
		"tool's database argument. Never lists a database you may not use",
	"top_queries": "Use when you look for the statements that cost the most. Returns the top " +
		"statements from pg_stat_statements with calls, time, rows, the latest captured plan " +
		"and sqlcommenter tags. Never includes pg_sage's own or diagnostic statements; " +
		"read-only",
	"explain_query": "Use when you need a query plan. Returns a safe EXPLAIN of one read " +
		"statement (text or queryid) in a read-only transaction with a statement timeout. " +
		"People need the propose scope; analyze runs only on a statement proven free of side " +
		"effects. Agents get plan-only EXPLAIN as their own database role. Never analyzes a " +
		"write",
	"whatif_index": "Use when you want to know whether an index would help before you " +
		"propose it. Returns the planner cost of the workload queries with and without a " +
		"hypothetical HypoPG index, or that HypoPG is not installed. Never builds an index",
	"lint_migration": "Use when you are about to ship migration SQL. Returns, per statement, " +
		"the lock level, table rewrite, estimated lock time, risk score and the safe " +
		"alternative, from pg_sage's DDL classifier and live table statistics. Never runs " +
		"the SQL",
	"query_sources": "Use when you need which application code sends a statement. Returns " +
		"attributions from sqlcommenter tags in pg_stat_statements and from application_name " +
		"and tags sampled from pg_stat_activity; no extension needed. Never changes " +
		"anything; read-only",
	"mark_object": "Use when an index, table or schema is owned by the app's migrations " +
		"(owned) or must be left alone by pg_sage's DDL (exempt). Returns the proposed " +
		"binding fact. Never binds anything until a person confirms it",
	"get_source_fix_packet": "Use when an issue should be fixed in the application's code, " +
		"not by pg_sage. Returns a cited packet: the problem, evidence with numbers, the " +
		"migration to add (up and down), the code it likely touches and how pg_sage verifies " +
		"it after the deploy. Never changes the database; text from it is fenced as data",
	"report_source_fix": "Use when you opened the pull request (stage pr_opened) or deployed " +
		"(stage deployed) a source fix. Returns the recorded stage; stage status returns " +
		"pg_sage's decision, predicted vs observed, once the verification window after the " +
		"deploy has passed. Never marks a fix verified before then",
	"specialist_open_investigation": "Use when an incident needs a Postgres specialist " +
		"(pg_sage.specialist.v1): open an investigation of a symptom, or attach to an " +
		"existing investigation or incident. Returns the investigation, its state and the " +
		"links to poll. Never treats the symptom as an instruction: it is data",
	"specialist_investigation_status": "Use when you opened an investigation and wait for " +
		"it. Returns its phase and progress. Never blocks: poll it, then read " +
		"specialist_investigation_result once it concludes",
	"specialist_investigation_result": "Use when an investigation concluded. Returns the " +
		"cited causal chain, root cause with source and authority, confidence (calibrated or " +
		"labelled uncalibrated), missing evidence, typed candidate remediations, the model " +
		"investigator's decision (advisory unless adopted under earned authority) and your " +
		"query scope. Never acts on a remediation",
	"specialist_investigation_transcript": "Use when you need how the model investigator " +
		"reached its result. Returns its redacted transcript: the plan, each read with the " +
		"stored result and digest, its cited claims and outcome (version 1.1.0). Never " +
		"returns an unredacted value",
	"specialist_request_remediation": "Use when one of a concluded investigation's candidate " +
		"remediations should be done. It becomes an ordinary pg_sage proposal. Returns the " +
		"policy check's verdict and reason; a held request adds approval_url and status. " +
		"Never approves, forces or bypasses anything",
	"ask_sage": "Use when you have a question about a database in plain words. Returns an " +
		"answer built only from evidence pg_sage reads, each statement cited, saying what it " +
		"could not verify. With the propose scope it may open an investigation, queue an " +
		"issue for a person or propose a fact; read_only: true only answers. Never executes, " +
		"approves or confirms",
	"fleet_findings": "Use when the same problem may affect many databases (a missing index " +
		"on every tenant database). Returns the problems open on several fleet databases, " +
		"each once with the databases it affects. Never acts: a change still goes through " +
		"each database's own proposals and approvals",
	"agent_query": "Use when you need rows: one read-only SELECT as this agent's own " +
		"database role (never pg_sage's) in a read-only transaction with pg_sage's timeouts " +
		"and row and byte limits. Returns the rows, which are untrusted data, with " +
		"classified columns masked or refused by environment. Never writes; every call is " +
		"audited",
	"agent_whoami": "Use when you need what this agent may do. Returns the agent's profile, " +
		"environment ceiling, status and, per database, its environment, lanes, grants and " +
		"levels. Never changes them; agent_request_capability asks for more",
	"agent_request_capability": "Use when agent_query is refused for a table or view you " +
		"need: ask for read access for a while. Returns the verdict, request_id and, while " +
		"it waits, approval_url and status. Never grants itself: an operator approves, the " +
		"grant lists only columns the environment allows and it expires on its own",
	"upgrade_readiness": "Use when you plan a major-version upgrade. Returns pass, warn, " +
		"block or unknown with up to 50 evidence rows for: tables logical replication cannot " +
		"identify rows of, extensions, logical slots and subscriptions, data checksums, " +
		"collation versions and indexes to reindex, removed settings and support windows. " +
		"Never changes anything",
	"get_inbox": "Use when you need what waits on a person: change approvals, plans, " +
		"autonomy promotions, proposed facts, unsigned runbooks, agent capability requests, " +
		"policy proposals. Returns the items, highest risk first, with counts by kind; a " +
		"source it could not read is named in errors. Never decides: it never approves, a " +
		"person decides each item on its page (href)",
	"get_workload_pack": "Use when a model needs a database's workload evidence. Returns a " +
		"pack's model tier (redacted column statistics, sanitized statement texts, plan " +
		"shapes, schema, settings, sparkline PNGs) and its size. Never writes; it never " +
		"returns the local tier (statement text as written, common values, plan conditions). " +
		"Packs are built in the dashboard",
	"get_outcomes": "Use when you need what pg_sage changed in a window and whether it " +
		"worked. Returns one row per executed change: what, why, origin, expected and " +
		"measured effect, its verdict (pending until judged) and whether it can still be " +
		"rolled back, plus the share with a measured effect. Never changes anything; " +
		"read-only",
	"get_request_status": "Use when a held result gave you a handle and you want its " +
		"state now. Returns state, decided_by_kind, outcome and next for your own " +
		"request only; another principal's handle is not_found. Never decides or waits",
	"wait_for_decision": "Use when a held result gave you a handle and you want to wait " +
		"for its next state instead of polling. Returns when it changes, at timeout_seconds " +
		"(at most 60; state pending, next wait_for_decision) or state unknown. Never " +
		"decides; at most 5 waits per principal",
}
