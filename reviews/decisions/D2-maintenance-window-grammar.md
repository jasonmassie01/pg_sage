# D2 — Unify maintenance-window grammars (G4-B24)

**Current state:** two independent window parsers are both consulted for the same action. The same string can mean different things in each, be rejected by one, or silently mean "never". Neither has a timezone; both use the process clock, which is UTC in the shipped container.
**Recommendation:** make `policy.ParseWindow` the only engine and extend it to a strict **superset** of the legacy grammar. Adopt its midnight-wrap semantics, make cron windows one hour wide, and add an optional timezone. Validate `trust.maintenance_window` on write. Ship a migration that rewrites any stored policy cron entry to keep its exact current meaning.
**Door:** **one-way** for stored policy documents, kept behaviour-preserving by the migration. The change to the config grammar is **two-way**: config is hot-reloadable and editable in the UI.

Evidence was read at HEAD on 2026-09-27. Paths are relative to `sidecar/`. I verified the behaviour table in §3 by compiling both parsers unmodified into a scratch program. No repo code was changed.

## 1. The grammars and where each is used

| # | Setting | Parser | Validation | Consumers |
|---|---|---|---|---|
| G1 | policy doc `maintenance_windows []string` | `policy.ParseWindow` (`internal/policy/window.go:18-33`): `always/anytime/24x7/24/7`, bare `weekends`, `HH:MM-HH:MM`, `weekdays\|weekends HH:MM-HH:MM` (`:47-68,114-123`), full 5-field cron with lists, ranges and steps (`:156-243`) | Strict. `validateWindows` (`internal/policy/document.go:286-296`) runs on proposal (`internal/policy/store_queries.go:147-153`) and on **every** gate evaluation (`internal/policy/gate.go:98`, `gate_operator.go:23`). An empty list is rejected (`document.go:287-288`) | `inAnyWindow` (`gate.go:295-303`) in `windowDecision` (`gate.go:270-293`) and `operatorWindowDecision` (`gate_operator.go:37-54`) |
| G2 | config `trust.maintenance_window string` | `inMaintenanceWindowAt` (`internal/executor/trust.go:28-63`): presets (`:140-152`), `never/off/none/disabled` (`:41`), `always…` (`:43`), `HH:MM-HH:MM` (`:261-275`), `<days> [HH:MM-HH:MM]` with `daily`, lists and ranges (`:158-221`), cron with **single integers only** (`:91-97,122-136`) and a **1-hour** width (`:65-101`) | **None.** API apply assigns the raw string (`internal/api/config_apply.go:263-264`). Anything unparseable → `false`, i.e. never (`trust.go:58-61`). The only warning is at startup when the value is empty (`cmd/pg_sage_sidecar/main.go:542-544`) | `RuntimeState.InConfiguredWindow` (`internal/executor/policy_runtime.go:72-74`, `standing_policy.go:141`, `action_policy.go:31-46`) |
| G3 | `briefing.schedule` | its own cron (`internal/briefing/briefing.go:37-63`, dow 0-6) | logs WARN if invalid (`:137-139`) | a trigger schedule, not a window. **Out of scope**; could later share G1's field parser |

How they combine:
- **Moderate autonomous** actions need G2 **and** G1 (`gate.go:282-283`).
- **High-tier** actions need only G1 (`configured` is true for non-moderate, `gate.go:282`).
- **Operator-approved** moderate/high actions need G1, and G2 only when G2 is set (`gate_operator.go:44-45`).

## 2. Who is affected
- Built-in profiles (`internal/policy/document.go:313-327`):
  - Staffed: `["weekdays 01:00-05:00"]`, which is 01-05 **UTC** in the container. The image installs `tzdata` but sets no `TZ` (`Dockerfile:26-27`). That is evening peak in the Americas, a point the review also makes.
  - Unattended: `["always","weekends"]`. `weekends` is redundant because `always` matches first (`window.go:24`).
  - The default profile is `unattended` (`internal/config/defaults.go:187`, `main.go:763`). For most installs G1 therefore never restricts anything, and G2 alone gates autonomous moderate work.
- Stored documents: written once at bootstrap and never refreshed (`internal/policy/store.go:20-49`). Other writers are the API proposals (`internal/api/policy_handlers.go:46-71`) and MCP `propose_policy_change` (`internal/mcp/postgres_access.go:201`). No UI edits G1.
- UI:
  - Settings edits G2 as free text (`web/src/pages/SettingsPage.jsx:1141`). Its help text promises presets, day ranges and cron (`web/src/generated/config_meta.json:1127-1130`, from `internal/config/config.go:203`).
  - Readiness shows `requires_maintenance_window` (`web/src/components/ProviderReadinessMatrix.jsx:49`).
- Docs:
  - `docs/configuration.md:83,157` and `docs/deployment.md:93` document G2 as cron `"0 2 * * *"`.
  - No doc describes G1. The design source is `specs/agent-native-autonomy-build-spec.md:243-262`.
- Dead-config side finding: `tuner.analyze_maintenance_threshold_mb` claims to gate ANALYZE by the window (`config.go:436`). It has no consumer; the field only exists in `internal/tuner/types.go:77`. Spawn this as a separate cleanup.

## 3. Same string, different meaning (verified by running both parsers)

Dates are 2026-09-26 (Saturday) to 2026-09-28 (Monday), UTC.

| Expression | Instant | G1 policy | G2 legacy | Why |
|---|---|---|---|---|
| `0 2 * * *` (the documented example) | Mon 02:30 | false | **true** | G1 matches one minute (`window.go:252-254`). G2 opens a 1-hour window (`trust.go:84-88,99-101`) |
| `0 2 * * 0` | Sun 02:30 | false | true | same width difference |
| `30 * * * *` | Mon 05:45 / 05:10 | false / false | true / false | G2 is really :30-:59, not the "1h window" its comment claims (`trust.go:19,74-80`) |
| `0 2 * * 1-5` | Mon 02:00 | true | **false** | G2 `Atoi` rejects ranges → silently never (`trust.go:128-131`) |
| `*/15 2 * * *` | Mon 02:15 | true | false | same (`trust.go:95-96`) |
| `weekdays 22:00-06:00` (= preset `weeknights`, `trust.go:144`) | Sat 02:00 | **true** | false | G1 assigns post-midnight time to the previous day (`window.go:107-108`). G2 tests today's weekday (`trust.go:163,174`) |
| same | Mon 02:00 | false | **true** | Sunday night counts as a weeknight in G2 |
| `weeknights`, `nights`, `weekdays`, `never` | — | **REJECT** | accepted | G1 has no presets and no `never` |
| `daily 01:00-05:00`, `Mon-Fri 01:00-05:00`, `sat,sun 02:00-06:00` | Mon/Sat | **REJECT** | true | G1 `namedDays` knows only weekdays/weekends (`window.go:114-123`) |
| `00:00-00:00` | — | REJECT (zero duration, `window.go:83-84`) | false | G2 treats it as a silent never (`trust.go:249-250`) |
| `22:00-02:00`, `always`, `weekends`, `weekdays 01:00-05:00` | — | agree | agree | |

Neither grammar has a timezone field. Both call `time.Now()` in the process's local time (`gate.go:409-414`, `trust.go:25`, `policy_runtime.go:72`).

## 4. Options

| Option | What | Door | Verdict |
|---|---|---|---|
| A. Keep both; validate G2 | Reject unparseable G2 on config apply/load and add a doc page for G1 | two-way | Necessary but not sufficient. Divergent meanings remain, and the operator path intersects both |
| **B. One engine, superset grammar** | `ParseWindow` accepts every G2 form. Wrap = previous-day (G1). Cron = the matching minute opens a **1-hour** window, with an optional explicit `@<duration>` suffix. Add `timezone` (policy field + `trust.maintenance_timezone`, default = process local, so today's behaviour holds). G2 keeps `never` via a config-only wrapper | stored docs one-way → migration below. Config two-way | **Recommend** |
| C. One engine, literal cron | Same as B but cron keeps G1's 1-minute meaning | changes documented G2 behaviour (`docs/configuration.md:83`) from 1 h to 1 min and silently shrinks existing configs to near-never | Reject |

B is the recommendation because the documented, user-facing grammar is G2's, and every operator-facing surface (UI help, docs, presets) describes it. G1's 1-minute cron window is a trap: no one writes a 60-second maintenance window on purpose. The review also asks for `ParseWindow("0 2 * * *").Contains(02:30) == true` (`reviews/2026-09-26/group-04-executor-safety.md:380-383`). Previous-day wrap is the correct reading of "weeknights". The two instants that flip under B (Mon 00-06 out, Sat 00-06 in) are called out in the release notes and in a startup log line.

**Migration to keep stored policy meaning.** On bootstrap, scan `sage.policy` rows with status `active` or `proposed`. For each cron entry, write a new version ratified by `system-migration` that rewrites `<cron>` → `<cron> @1m`. That preserves the exact current meaning, and the policy preview explains the change. Shipped profiles contain no cron entries, so most installs get no-op scans.

**Downgrade hazard:** an old binary rejects `@1m`. The gate then fails closed with `policy_unavailable` (`gate.go:97-99`), which is safe but blocks all mutations until the policy is rolled back to the prior version. Note this in the release notes.

## 5. Implementation sketch
1. `internal/policy/window.go`:
   - add the presets, `daily/everyday`, day lists and ranges, and case-insensitive day names (port `trust.go:140-241`)
   - make the cron window width a parameter with default 1 h, plus the `@<dur>` suffix
   - add `ParseWindowIn(expr, *time.Location)`
   - split the file if it passes 500 lines (`window_friendly.go`, `window_cron.go`)
2. `internal/policy/document.go`: optional `timezone` wire field. `DisallowUnknownFields` stays; old docs omit the field and get the default.
3. `internal/executor/trust.go`: delete the parser (lines 15-290). `inMaintenanceWindowAt` becomes a thin wrapper: empty/`never` → false, else `policy.ParseWindowIn`. Replace, don't deprecate.
4. `internal/config/config.go` validation + `internal/api/config_apply.go:263`: reject unparseable `trust.maintenance_window` with a 400 that names the grammar. Add `trust.maintenance_timezone`.
5. Migration: `internal/policy/window_migration.go` + a call in `Store.Bootstrap` (`store.go:20`).
6. Docs: update `docs/configuration.md:83,157` and `docs/deployment.md:93`, add a G1 section, regenerate `config_meta.json`.

## 6. Test plan (write first; each must fail on HEAD)
- T1 `ParseWindow("0 2 * * *").Contains(Mon 02:30)` is true, and `Contains(03:00)` is false.
- T2 One table test feeds every row of §3 through **both** `policy.ParseWindow` and `executor.inMaintenanceWindowAt` and asserts identical results. It fails today on every row where the two columns differ. Exclude `never` and `00:00-00:00`, which stay config-only or rejected by design.
- T3 `ParseWindow` accepts `weeknights`, `nights`, `weekdays`, `daily 01:00-05:00`, `Mon-Fri 01:00-05:00`, `sat,sun 02:00-06:00`.
- T4 Wrap: `weeknights` contains Sat 02:00 and does **not** contain Mon 02:00.
- T5 Timezone: with `America/New_York`, `weekdays 01:00-05:00` contains 2026-09-28 06:00 UTC. With no timezone field, the result equals today's process-local result.
- T6 Migration: a stored active doc with `["0 2 * * *"]` → a new version `["0 2 * * * @1m"]`. It has the same `Contains` result as before at 02:00:30 and 02:30. Shipped-profile rows are untouched and the version doesn't change.
- T7 Config apply of `trust.maintenance_window="weeknigths"` (typo) → 400. Today the typo is accepted and means never.
- T8 Gate integration: autonomous moderate with G2 `nights` and a policy `always` at 23:00 → execute. Today this already passes; keep it as a regression guard.
