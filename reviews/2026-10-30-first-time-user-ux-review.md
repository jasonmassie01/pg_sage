# First-time-user review: Cases and Actions

Implementation: [dev.9](2026-10-30-v2.5-dev9-acceptance.md) and [dev.10](2026-10-30-v2.5-dev10-acceptance.md). The original assessment below is retained as review evidence.

Date: 2026-10-30. Scope: a read-only walkthrough of the deployed pg_sage App,
supported by screenshots and rendered-component/source inspection. Actions was
reviewed live in its Executed, Pending Approval and Recommendations views.
Two independent reviews examined novice comprehension and workflow consistency.
This is a heuristic review, not a usability study with actual new users. No
product code, settings, approvals, findings or database objects were changed.

## Assessment

A first-time visitor can recognize a database monitoring tool and a list of
warnings. They cannot reliably answer which problem deserves attention, what
would happen if they ignored it, whether pg_sage has changed anything, or how
to get from a Case to a reviewed change.

The visual shell is coherent. The information hierarchy exposes specialist and
implementation vocabulary before establishing the user's task. Adding a longer
introductory paragraph alone will not solve this.

The intended model should be explicit:

**Cases: what needs attention and what we know.**
**Actions: proposed changes, decisions and results.**
**Ask Sage: help understanding the evidence.**

The workflow is: review a case → investigate when evidence is insufficient →
review a proposed change → apply through an available, authorized path → verify
the outcome. Not every case has a supported change, and approval is not proof
that a change executed or helped.

## What a novice encounters

| Current screen | Likely question | Recommended change |
| --- | --- | --- |
| Cases mixes a nearly exhausted sequence, duplicate indexes, slow queries and tuning advice | “Is something broken? Which one matters first?” | Summarize current issues by meaningful attention state and explain ordering. Separate measured condition, potential consequence and urgency rationale. |
| Technical object names lead titles | “What is an index or sequence? Which part of my system is involved?” | Lead with the condition in plain language; show exact database/object identity as supporting context. Do not invent application ownership or business impact. |
| CRITICAL plus “critical severity requires immediate review” | “Why? How much time do I have?” | Explain the actual evidence and consequence. State when growth rate, time to limit or user impact is unknown. Repeating the severity is not a rationale. |
| Warnings default to “not urgent” | “Was urgency assessed, or is this a default?” | Do not convert missing assessment into reassurance. Use “Urgency not assessed” where appropriate; keep measured severity distinct. |
| `Next: drop_unused_index: provider_unsupported`, `Policy: blocked` | “Am I meant to drop it? Is the database unable to do that?” | Translate the exact blocker: observation mode, pg_sage execution limitation, missing permission, safety evidence or approval requirement. Name the available next step. |
| A case says “Needs investigation” | “Who is investigating? Should I wait or do something?” | State the actor, evidence gap and available action. Link an existing investigation or show a supported investigation/manual-review path. |
| Suppress is often the only visible button | “Is this how I fix the issue?” | Make review/explanation the primary action. Move suppression to a secondary menu and explain that it hides a finding without repairing the condition. |
| Evidence, SQL, rollback, locks, guardrails and PR output are expanded in the list | “Must I understand all of this before deciding?” | Use a compact triage list and a focused detail view. Reveal specialist evidence and procedures on demand. |
| Actions opens on an empty Executed tab despite proposed recommendations | “Why are there no actions if Cases has problems?” | Default to the work relevant to the current user/mode. Show proposed changes and their availability; reserve History for recorded outcomes. |
| Autonomous index builds appears above the Actions tabs | “Will it start changing my database after seven days?” | Show constraints beside affected work. In Observation mode explicitly say that the mode prevents execution; completing a baseline does not itself enable it. |
| Actions promises safe work can run; only a small Observation badge explains the current mode | “Can this tool change my database right now?” | Put the effective behavior in a visible, mode-aware status strip. |
| Recommendations starts with ID, State, Revision and Attempts | “What am I being asked to decide?” | Lead with purpose, target, reason and availability. Keep revision, attempt budget and machine verdicts in details/history. |
| Pending Approval says “Approve runs it now,” but the button vocabulary is generic | “Am I approving a plan or immediately running SQL?” | Label the consequence precisely: “Approve and run” when applicable; “I will apply it” for supported external changes. Explain any remaining gates inline. |

## Cases: recommended hierarchy

1. **Scope and safety:** selected database, effective mode, last successful check
   and whether evidence is stale or incomplete.
2. **Attention summary:** what requires review, what needs more evidence, what
   is being verified. Counts must not imply health when collection failed.
3. **Compact case:** plain-language condition, affected object, observed facts,
   qualified impact, reason for attention, current action availability and one
   clear next step.
4. **Case detail:** observations, likely explanation, alternative explanations,
   missing evidence, timeline and proposed change.
5. **Technical details:** exact identifiers, SQL, counters, plans, locks,
   provider/policy decisions, approval and recovery details.

Keep the database and case identity intact across all links. “Explain this” can
open Ask Sage with the specific case/evidence already scoped; a novice should
not need to compose a query using a finding ID. This is a proposed interaction,
not a capability that the current cards already provide.

### Example: sequence case

> **An ID counter is nearly full**
>
> Observed: this sequence has used about 95% of its configured range.
> At its limit, requests for another sequence value can fail. We have not
> established when that limit will be reached or which application calls it.
>
> **Next step: Review capacity options**
> Database changes are disabled in the current Observation mode.
>
> Supporting detail: exact sequence name, current value, configured maximum,
> increment, observation time and available recovery options.

### Example: spill case

> **A query used temporary disk space**
>
> Observed: 2,834 temporary blocks written during an approximately one-minute
> collection interval. This may add disk work; the effect on users has not
> been established.
>
> No memory setting has been validated. Review the query's execution plan and
> a controlled memory experiment before choosing a change.
>
> **Review investigation steps** · Explain this · Technical evidence

The example's rounded interval is explanatory design copy; a grounded Ask answer
must still obey its exact-number verification contract. Human-readable units
should be converted deterministically using known units/block size, and retain
the exact measurement/window in details. Do not guess bytes from a block count.

## Actions: recommended hierarchy

Use task-oriented views with counts and a defined relationship to Cases:

- **Proposed changes:** what could be done, which case it addresses, expected
  effect and its evidence, risk, current availability and next decision.
- **Needs your review:** items the current person can actually decide. Clearly
  distinguish approval-and-execution from an external/manual commitment.
- **In progress:** queued, executing or awaiting verification, with the current
  step and relevant blocker. An absent capability must not look like a queued job.
- **History:** successful, failed, rejected, expired, rolled back and inconclusive
  work. “Executed” is inaccurate for a ledger that includes non-executed entries.

Each proposed change should answer: **Why this change? What will change? What is
the risk? Can pg_sage run it here? Who must act? How would we recover? How will
we know whether it helped?** Unsupported or missing answers should remain visible
as unknowns, rather than becoming reassuring defaults.

Show the effective restriction in plain language:

| Actual restriction | User-facing meaning |
| --- | --- |
| Observation mode | “Database changes are disabled. Review Cases and proposed changes; nothing will run in this mode.” |
| pg_sage provider/action limitation | “pg_sage cannot run this action for this Lakebase connection. Review the supported manual procedure.” Only offer a procedure that is actually valid. |
| Missing permissions | “This connection lacks permission for the proposed change.” Identify the required authority without suggesting broad grants. |
| Incomplete safety evidence | “Waiting for sufficient disk-load history.” State what is missing and whether pg_sage will recheck; do not promise an unsupported ETA. |
| Human decision required | “Ready for your review.” Explain precisely what the primary button will do. |

Avoid saying “Lakebase does not support this” when the actual limitation is
pg_sage's executor, the connection's permissions or the current execution mode.
Waiting seven days must never be presented as sufficient to bypass those gates.

Useful mode-aware empty states:

- “No changes have been recorded in the selected time range.” Link to all history.
- “Nothing needs your approval for this database.” Link to proposed/blocked work.
- “Changes are disabled in Observation mode. Review detected opportunities in Cases.”
- “No cases match this filter.” Provide Show all cases.
- “No open cases; last successful check [time].” Only say this when collection
  is known to be healthy; otherwise explain the evidence gap.

## Trust and correctness issues to fix before polishing copy

The live missing-FK-index case shows an unnamed `CREATE INDEX CONCURRENTLY ON …`
proposal, followed by generated rollback `DROP INDEX CONCURRENTLY ON;` and
verification using `'ON'::regclass`. Those artifacts are invalid: the generator
has treated the SQL keyword ON as the index name. The case still presents the
change as reversible. This is a product correctness defect discovered during
the walkthrough, not a novice-comprehension issue. No script was executed.

Fix or withhold artifacts that cannot identify their actual created object.
Likewise, a comment saying “recreate from version control” is not a captured,
validated rollback plan. Recovery claims should distinguish a complete restore
procedure, a partial/manual procedure and unavailable recovery.

Other semantic issues found in source/rendered content:

- High-risk ledger entries can render “advisory” in the Risk column. Risk and
  execution permission must be separate concepts.
- Approval success messaging must distinguish decision accepted, execution
  started, SQL completed and outcome verified. A successful API decision does
  not itself prove execution or improvement.
- The current global time selector applies to the action ledger, while pending
  and recommendation fetches have different scope. Label the scope locally.
- A preflight check with an unknown table size should not appear as an
  unqualified positive safety signal.
- Provider-managed advice, including the displayed WAL/system-setting advice,
  needs an applicability review. Generic PostgreSQL guidance should not become
  a user instruction merely because it was generated and stored.

## Visual and vocabulary refinements

Keep the nerdy theme and the page-specific memes. Put operational context and
urgent information first, and make the meme a compact secondary region or
footer so it does not compete with critical findings or the primary task.
Retain monospace for SQL and identifiers; use plain language for decisions.

Consider descriptive companion labels: Cases → Issues & investigations,
Actions → Changes & history, Fleet → Databases, Value → Overview. Avoid replacing
all existing navigation terms at once; first make the existing pages teach what
they mean. Explain specialized concepts inline where first encountered.

The demo fixtures should be explicitly identified as controlled test data. A new
visitor should not mistake a synthetic 95%-full sequence for an unannounced
production emergency. This needs explicit fixture/environment metadata, not an
assumption that any unfamiliar schema is disposable.

## Recommended implementation order and acceptance

1. Correct misleading/incomplete recovery artifacts, risk labels, unknown-to-safe
   defaults, mode contradictions and approval-result wording.
2. Add visible scope/mode/collection context and meaningful empty/blocked states.
3. Introduce compact Cases and a focused detail view with a real linked next step.
4. Reorganize Actions around decisions, progress and outcomes; contextualize
   safety constraints beside affected work.
5. Add case-scoped Ask explanations, glossary help and compact meme placement.

Validate with a real first-time user. After a brief walkthrough they should be
able to identify the selected database and mode, explain one case in ordinary
language, separate observed facts from unknown impact, choose a supported next
step, predict the consequence of an approval button and locate the recorded
result. Test empty, stale, provider-limited, permission-limited, observation,
pending-review and verification-inconclusive states—not only populated success
screens. Preserve an expert path to exact SQL/evidence with minimal navigation.

## Primary evidence

- Live Actions: Executed empty state, Pending Approval empty state and populated
  Recommendations table, after refreshing the deployed frontend.
- Current Cases/Fleet/Ask screenshots under private `.local-test-results/nerdlab-dev8-final-*`.
- [Cases page](../sidecar/web/src/pages/CasesPage.jsx),
  [case controls](../sidecar/web/src/pages/cases/CaseControls.jsx),
  [Actions](../sidecar/web/src/pages/Actions.jsx),
  [recommendation view](../sidecar/web/src/pages/actions/RecommendationsTab.jsx),
  [trust badge](../sidecar/web/src/components/TrustBadge.jsx),
  [case urgency projection](../sidecar/internal/cases/projector.go), and
  [DDL artifact generation](../sidecar/internal/cases/ddl_safety.go).
