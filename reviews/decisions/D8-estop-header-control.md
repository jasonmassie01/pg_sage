# D8 — Operator e-stop control in the dashboard header (G9-B13 / G9-I05)

**Current state:** the stop/resume API is operator+ and per-DB or fleet-wide, but the only button is in the
admin-only Settings page. The header shows a fleet-wide "any stopped" badge. That badge can be wrong after
a restart, and the stop records no actor.
**Recommendation:** fix the state plumbing first:
1. Restore the in-memory latch from the persisted flag at startup.
2. Expose per-DB `emergency_stopped`, `stopped_by` and `stopped_at`.
3. Record the actor.

Then add an operator+ header control scoped to the header database picker, with arm/confirm, and a Resume
that is shown only when stopped.
**Door type:** two-way door. The UI change and additive fields reuse the existing API. The startup latch
restore changes behaviour toward fail-safe only.

Evidence was verified on master `f99a302`. All paths are under `sidecar/`.

## 1. What the code does today

**API.**
- `POST /api/v1/emergency-stop` and `POST /api/v1/resume` are both `operatorUp`
  (`internal/api/router.go:295-301`).
- The handlers read `?database=` and call `mgr.EmergencyStopStrict` / `mgr.ResumeStrict`. They return 404
  for an unknown DB and 500 when persistence fails (`internal/api/handlers.go:972-1020`).
- Neither handler reads the session user.

**Scope.**
- `""` means every instance; a name means one instance (`internal/fleet/emergency_stop.go:84-102`).

**Latch semantics.**
- Stop latches memory first and then persists. A persistence failure never un-stops
  (`internal/fleet/emergency_stop.go:104-121`).
- Resume persists first and releases only the databases it wrote (`:123-142`).
- Instances without an executor are skipped (`:148-170`).
- The persisted flag is `sage.config.emergency_stop`, written with `updated_by = 'executor'`
  (`internal/executor/trust.go:320-339`).
- Executors re-read the persisted flag and fail closed on read errors (`internal/executor/trust.go:300-317`;
  `internal/executor/executor.go:404-411`).

**Latch restore gap.**
- `inst.Stopped` is set only at `emergency_stop.go:112`, `:134` and `:198` (inheritance on replacement).
- Nothing loads it from `sage.config` at startup (`grep '\.Stopped =' internal cmd`).
- The code acknowledges this: "after a restart the latch resets while sage.config may still say 'stopped'"
  (`emergency_stop.go:144-147`).

**State exposure.**
- `FleetOverview.Summary.EmergencyStopped` is a fleet-wide OR over `inst.Stopped`
  (`internal/fleet/manager.go:105-125,145`; `internal/fleet/overview_types.go:18`).
- `DatabaseStatus` has no per-DB stop field (`manager.go:114-120`).
- Per-DB state leaks only indirectly, as `blocked_reason: "emergency stop is active"` in capability
  families (`internal/fleet/capabilities.go:281-297`).

**UI.**
- The only stop/resume buttons are in the Settings → General tab (`web/src/pages/SettingsPage.jsx:1019-1065`):
  - arm/confirm countdown (`:931-955`);
  - a scope label (`:1023-1028`);
  - Resume always enabled (`:1055-1064`);
  - the database name is interpolated into the query string without encoding (`:902-903`, `:964-965`).
- `/settings` renders only for admins (`web/src/App.jsx:147,197-198`), so operators cannot reach it even
  though the API allows them.
- The header shows `EmergencyBadge` only when `summary.emergency_stopped` is true
  (`web/src/components/Layout.jsx:98-112,134-135,336`).
- The header already has a `DatabasePicker` next to it (`Layout.jsx:336-340`).
- After a stop, Settings calls its own `refetch()` (config). Fleet data is polled every 30 s
  (`SettingsPage.jsx:925-927`; `App.jsx:86-88`), so the badge lags by up to 30 s.

## 2. Concrete risk today

1. **Operators cannot pull the brake.** Operators approve and execute actions but have no UI path to stop
   them (`App.jsx:197-198` vs `router.go:295-297`). In an incident, the person watching the queue has to
   find an admin or use curl.
2. **The state can be wrong after a restart.** If the sidecar restarts while stopped:
   - executors stay blocked (good, fail-closed via `trust.go:300-317`);
   - but `summary.emergency_stopped=false`, so the header shows no badge;
   - and capabilities report families as ready, because `inst.Stopped=false` (`capabilities.go:285`).
   An operator sees "ready, not stopped" while nothing executes. Or they resume a stop they never knew
   about.
3. **No attribution.** The persisted row says `updated_by='executor'` (`trust.go:330-332`), and the only
   other trace is a log line (`emergency_stop.go:114`). Nobody can answer who stopped it, when, or who
   resumed it.
4. **Scope ambiguity in the fleet view.** The badge is "any DB stopped". With per-DB stops, the user
   cannot tell which databases are stopped.
5. Minor: an unencoded `database` query value (`SettingsPage.jsx:903`). A name containing `&` or `#`
   would mis-scope the request.

Under the ledger's lens (trust → autonomy), a visible, attributable, always-reachable brake is a
precondition for autonomous execution. It is not optional polish.

## 3. Options

| Option | What | Pros | Cons | Door |
|---|---|---|---|---|
| A. Open Settings → General to operators | Route-gate change only. | Tiny. | Brake stays two clicks deep. State bugs (risks 2–4) remain. Exposes other admin settings. | Two-way |
| B. Header control, fleet-wide only | "Stop all" in the header for operator+. Per-DB stop stays in Settings. | Unambiguous scope. | Over-stops in multi-DB fleets. Operators still can't do a per-DB stop. | Two-way |
| **C. Header control scoped to the picker, with state plumbing (recommended)** | Operator+ header button. The label says the scope ("Stop orders_db" / "Stop all 7 databases"). Arm/confirm. Per-DB state and attribution come from the API. Resume shows only when the target is stopped and names who stopped it and when. | Brake is where the eyes are. Honest state. Audit. | Needs backend plumbing (below). | Two-way |

**Who may resume?** Keep operator+ (matches the API, `router.go:299-301`). The confirm dialog must show
`stopped_by` and `stopped_at`. Restricting resume to admins would be a posture change, so treat it as a
separate decision if wanted.

## 4. Implementation sketch

**Backend (do this first; it is useful even without the UI).**
- **Restore the latch at startup.** When an instance with an executor is registered, read
  `CheckEmergencyStop` and set `inst.Stopped` and `applyExecutorStopGate`. Fail closed: a read error
  means stopped (same policy as `trust.go:313-314`). Candidates: fleet registration in
  `internal/fleet/types.go:~244` or the manager's register path.
- **Record the actor.**
  - Change the signature to `SetEmergencyStop(ctx, pool, stopped bool, actor string)` and write
    `updated_by=actor` (`trust.go:320-339`).
  - Thread `actor` through `EmergencyStopStrict(name, actor)` and `ResumeStrict`.
  - The handlers pass the session email (`UserFromContext`, as `authenticatedActor` does at
    `agent_db_agent_api.go:160-169`).
  - Emit an audit/action-log row.
  - No schema change: `sage.config` already has `updated_by` and `updated_at` (`trust.go:329-332`).
- **Expose per-DB state.** Add `EmergencyStopped bool`, `EmergencyStoppedBy string` and
  `EmergencyStoppedAt *time.Time` to `DatabaseStatus` (`manager.go:114-120`). Populate the latter two from
  the persisted row. Keep the summary boolean and add `emergency_stopped_count`.

**Frontend.**
- Add a new `components/EmergencyStopControl.jsx` (keeps `Layout.jsx` under 500 lines).
- Render it in the header for `admin|operator` (reuse `canReviewActions`, `Layout.jsx:119-120`).
- Scope follows `selectedDB`. `encodeURIComponent` the name.
- On success, call the App fleet refetch (pass it down with `fleetData`).
- Resume shows only when the target's `emergency_stopped` is true.
- Move the Settings buttons onto the same component. Replace, don't duplicate.

## 5. Test plan (write first; each must fail today)

**Go**
- `TestStartupRestoresPersistedEmergencyStop` (`internal/fleet`, integration): set
  `sage.config.emergency_stop='true'` and register the instance. Expect `inst.Stopped==true`,
  `Summary.EmergencyStopped==true`, and capability families blocked. Today all three are false/ready.
- `TestStartupStopFlagReadErrorFailsClosed`: an injected read error means stopped.
- `TestFleetOverviewExposesPerDatabaseEmergencyStop`: stop `db1` of two. `db1.emergency_stopped==true`
  and `db2==false`.
- `TestEmergencyStopRecordsSessionActor` (`internal/api`): POST as `op@x`. `sage.config.updated_by=='op@x'`
  and the per-DB `stopped_by=='op@x'`.
- `TestResumeRecordsSessionActor`.
- `TestEmergencyStopViewerForbidden`: guards the role (a viewer gets 403).

**Vitest**
- `header shows emergency-stop control for operator` (`data-testid="header-emergency-stop"`), and not for
  viewers. This is the G9-B13 acceptance test.
- `header stop label reflects picker scope` ("all N databases" vs the DB name).
- `resume is hidden unless the selected database is stopped`, and shows `stopped_by`.
- `stop success triggers fleet refetch`: the fleet refetch mock is called once.
- `database name is URL-encoded in stop request`: a name like `a&b` becomes `?database=a%26b`.

**Manual:** `MANUAL: restart the sidecar while stopped; the header badge must show STOPPED on first
paint`.
