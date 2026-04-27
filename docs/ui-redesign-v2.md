# pg_sage UI Redesign v2 — Design Doc

**Status:** Proposal
**Author:** Claude (for jmass)
**Date:** 2026-04-16
**Scope:** `sidecar/web/` — React dashboard embedded into `internal/api/dist/`

---

## 1. Executive Summary

The current UI is a set of 15 snapshot views. It tells you *what is*, but not *what happened*, *what's trending*, or *what the agent did about it*. Competitors (pganalyze, Sentry, PlanetScale, CockroachDB Cloud) treat every finding as a first-class object with a narrative arc: first-seen → proposed action → approved → executed → resolved or regressed.

This redesign collapses the 15 pages into **5 primary surfaces** organized around that arc, introduces a global time axis, ranks by estimated impact rather than severity color, and makes the agent's actions visible as first-class timeline rows.

**Goal:** a user glancing at pg_sage for 10 seconds should see (1) is anything on fire, (2) what's trending worse, (3) what did the agent do while I was away. Today none of those questions have a one-glance answer.

---

## 2. Problem Statement

Concrete pain points from code review (file:line):

| # | Pain point | Evidence |
|---|---|---|
| 1 | Dashboard is 4 static cards. No time series. No trend. | `Dashboard.jsx` — 4 stat cards, no charts |
| 2 | No global time picker. Every page is point-in-time. | Entire codebase — no shared time context |
| 3 | Findings, Actions, Incidents are 3 separate pages for one workflow. | `Findings.jsx`, `Actions.jsx`, `IncidentsPage.jsx` |
| 4 | Severity taxonomy (critical/warning/info) not ranked by impact. User can't tell "fix this first." | `SeverityBadge.jsx` hardcoded 3-level |
| 5 | Trust mode buried in Settings — users forget whether agent is live. | `SettingsPage.jsx` Trust & Safety tab |
| 6 | No search, no command palette. 15 pages, sidebar-only nav. | `Layout.jsx` sidebar |
| 7 | DataTable expansion keyed by row index, breaks on re-fetch. | `DataTable.jsx:26` |
| 8 | Severity badge contrast ~3:1, fails WCAG AA. | `SeverityBadge.jsx` critical = #ef4444 on #3b1111 |
| 9 | Pagination silently truncates at 50. | `Findings.jsx:111` |
| 10 | No impact estimate on findings — just titles. | `sage.findings.impact_score` column and `analyzer.Finding.ImpactScore` field exist (`sidecar/internal/analyzer/finding.go:28`), but only the schema_lint subsystem populates them today. The API's `buildFindingsOrder()` (`sidecar/internal/api/handlers.go:678`) does not allow sorting by `impact_score`. Domain-specific fields (`impact`, `table_size`, `query_count`) sit unindexed inside `detail` JSONB. |
| 11 | Database snapshot page is a raw JSON dump. | `DatabasePage.jsx` |
| 12 | TimeAgo static, grows stale during long sessions. | `TimeAgo.jsx` |
| 13 | Modals have no focus trap / return. | Multiple inline `createPortal` calls |
| 14 | Polling not coalesced — every page re-fetches on navigation. | `useAPI.js` independent per caller |

---

## 3. Design Principles

Distilled from competitor research. Each is a rule we can hold a specific design decision against.

1. **Findings are objects with a narrative arc**, not alerts. Stable URL, status, actor, first-seen, last-seen, discussion of actions taken. Model: Sentry issue, pganalyze finding, Linear issue.
2. **Impact > severity.** Show "saves 2.3GB / +400ms p99" next to the finding. Sort by impact. Severity is a visual channel for scan, not the ranking.
3. **One time axis in view.** A global time picker in the header. Every chart, every "last seen," every sparkline shares it. Compare-to-previous-period as a first-class toggle.
4. **Every automated action has an actor and a row.** Human or agent, always named. Audit trail is the product.
5. **Trust mode is always visible per database.** Persistent header badge. You never wonder what the executor will do right now.
6. **Sparkline-in-row beats separate chart.** Trend at-a-glance is the triage signal. Dense lists, no dashboard wallpaper.
7. **SQL is the universal export.** Every recommendation produces copyable, executable SQL — even when the agent can run it. Trust comes from auditability.
8. **Fleet mode is spatial, single-DB is temporal.** Across databases: topology/grid. Within a database: timelines and plan history. Different mental models, different layouts.

---

## 4. Information Architecture

### 4.1 Page count: 15 → 5

| New page | Replaces | Purpose |
|---|---|---|
| **Overview** | Dashboard | Narrative home: hero + ranked feed + recent actor timeline |
| **Findings** | Findings, SchemaHealthPage, ForecastsPage, QueryHintsPage, IncidentsPage | One unified feed across all finding subsystems, filterable by subsystem (see §16 for the category→subsystem taxonomy). Note: no physical `source` column exists — the filter is a virtual mapping keyed off `category` prefixes. IncidentsPage merges only if `sage.incidents` is UNIONed or folded into `sage.findings` (§13). |
| **Actions** | Actions | Timeline of every executed/pending/rolled-back action, with the finding that triggered each |
| **Fleet** | DatabasePage, DatabasesPage | Spatial grid (fleet mode) or temporal drilldown (single DB) |
| **Settings** | SettingsPage, UsersPage, NotificationsPage, DatabaseSettingsPage, AlertLogPage | Keep tabbed settings; fold admin subpages + alert delivery log in as tabs (alert log is delivery telemetry, not findings) |

The other existing pages (`LoginPage`, database subpages) stay as-is.

### 4.2 Primary nav (sidebar)

```
OVERVIEW           — home
FINDINGS     ● 12  — ranked feed (badge = new since last visit)
ACTIONS      ○ 3   — timeline (badge = pending approvals)
FLEET              — topology / database drilldown
─────────────
SETTINGS           — config, users, notifications, trust
```

Trust-mode badge and database picker move to the **top bar**, not the sidebar. They are page-global context.

---

## 5. Wireframes

### 5.1 Top bar — present on every page

```
┌──────────────────────────────────────────────────────────────────────────┐
│  pg_sage    [Database: prod-us-east-1 ▾]    [⚡ Advisory]    [⏱ 24h ▾]  │
│                                                                          │
│                                            [🟢 Healthy]  [⌘K]  [👤 jm]  │
└──────────────────────────────────────────────────────────────────────────┘
```

- **Database picker** — existing, kept in place
- **Trust-mode badge** — color-coded, click to change (confirmation required to escalate)
  - 🟢 Monitor · 🟡 Advisory · 🔴 Autonomous · ⛔ Emergency Stop
- **Time picker** — presets (1h / 6h / 24h / 7d / 30d / custom) + "compare to previous period" toggle. Writes to React context; every useAPI call reads from it.
- **Health dot** — overall cluster health; clicking opens a small popover with per-DB health
- **⌘K** — command palette trigger (see §5.6)
- **User menu** — profile, theme, keyboard help, logout

### 5.2 Overview page

```
┌──────────────────────────────────────────────────────────────────────────┐
│                                                                          │
│  3 databases · 2 findings need attention · agent resolved 7 in 24h      │
│  ▁▁▁▂▃▅▇▇██▇▅▃▂▁▁  (cluster health trend, 24h window)                   │
│                                                                          │
├──────────────────────────────────────────────────────────────────────────┤
│  TOP FINDINGS — by estimated impact                                      │
├──────────────────────────────────────────────────────────────────────────┤
│  🔴  Index missing on orders.customer_id                                 │
│      Saves est. 340ms p99 · 12k queries/hr scan full table              │
│      prod-us-east-1 · first seen 3d ago · ▁▁▂▃▅▇██                      │
│      [Review & approve] [Dismiss]                                        │
│  ────────────────────────────────────────────────────                    │
│  🟡  Unused index idx_orders_status_v2 — 1.2GB wasted                   │
│      No scans in 14d · safe to drop                                      │
│      prod-us-east-1 · first seen 14d ago · ▇▆▅▃▂▁▁                      │
│      [Review] [Dismiss]                                                  │
│  ────────────────────────────────────────────────────                    │
│  🟡  Slow query regression — SELECT * FROM shipments WHERE...           │
│      p99 jumped 120ms→340ms at 14:22 · plan changed                     │
│      prod-eu-west-1 · 2h ago · ▁▁▁▂██▇▇                                 │
│      [Open plan diff] [Dismiss]                                          │
├──────────────────────────────────────────────────────────────────────────┤
│  RECENT AGENT ACTIVITY — last 24h                                        │
├──────────────────────────────────────────────────────────────────────────┤
│  14:32  🤖 Agent   VACUUM on orders.line_items        ✓ reclaimed 800MB │
│  13:05  🤖 Agent   CREATE INDEX ON shipments(...)    ✓ 340ms saved      │
│  10:18  👤 jmass   Approved: DROP INDEX idx_orders_v1 ✓ 1.2GB freed    │
│  02:00  🤖 Agent   Nightly integration run            ✓ see email       │
│                                                               [See all →]│
└──────────────────────────────────────────────────────────────────────────┘
```

**Design notes:**
- One narrative sentence at top: X DBs, Y need attention, Z resolved. Human-readable, not card-grid.
- Cluster health trend = 1-row sparkline across the top. Scrubs with the global time picker.
- **Top findings** = the Sentry-style ranked feed. Three rows visible, "See all" to get to Findings page. Each row: severity dot, title, impact line, database/when, sparkline, inline actions.
- **Agent activity** = three most recent timeline rows. Actor icon first (human vs agent), clear result. Click any row to jump to Actions page filtered.
- The stat cards (databases/healthy/degraded) disappear. That information is in the sentence at top and on the Fleet page.

### 5.3 Findings page

```
┌──────────────────────────────────────────────────────────────────────────┐
│  FINDINGS                                                                │
│  Source: [All ▾]  Status: (Open) (Snoozed) (Resolved)  Sort: [Impact ▾] │
│  ┌──┬─────────────────────────────────────────────────────────┬────────┐│
│  │🔴│ Index missing on orders.customer_id                      │3d ago  ││
│  │  │ Saves 340ms p99 · 12k scans/hr  · ▁▁▂▃▅▇██                │▃▅▇██  ││
│  ├──┼─────────────────────────────────────────────────────────┼────────┤│
│  │🟡│ Unused index idx_orders_status_v2 — 1.2GB wasted         │14d ago ││
│  │  │ No scans in 14d · safe to drop  · ▇▆▅▃▂▁▁                 │▆▅▃▂▁  ││
│  ├──┼─────────────────────────────────────────────────────────┼────────┤│
│  │🟡│ Slow query regression — SELECT * FROM shipments...       │2h ago  ││
│  │  │ p99 jumped 120ms→340ms · plan changed  · ▁▁▂██▇          │▂██▇   ││
│  └──┴─────────────────────────────────────────────────────────┴────────┘│
│                                                                          │
│  Showing 24 of 47 · [Load more]                                         │
└──────────────────────────────────────────────────────────────────────────┘
```

**Filter row:**
- **Subsystem** — dropdown that maps to the `?source=` query param. Today the backend honors only `source=schema_lint` (matches `category LIKE 'schema_lint:%'`, see `fleet/types.go:145-160` and `api/handlers.go:661`). To cover the other 30+ categories (`slow_query`, `unused_index`, `forecast_*`, `lock_chain`, `runaway_query`, `query_tuning`, etc.), extend `FindingFilters.Source` to accept values like `rules`, `optimizer`, `advisor`, `forecaster`, `migration_advisor`, `query_tuning` and map each to a `category IN (...)` WHERE clause (§16 appendix). Alternative: add a physical `source_kind` column and backfill — cleaner, but a migration.
- **Status** — pill group, multi-select
- **Sort** — Impact (default) / First seen / Last seen / Severity. Impact sort requires two backend changes: (a) `buildFindingsOrder()` must add `impact_score` to its allowlist; (b) non-schema_lint subsystems must populate `Finding.ImpactScore` before the UI can meaningfully rank them. Until (b) lands, "Impact" sort falls back to severity with impact as a secondary key so empty-impact rows don't all sink to the bottom.

**Row anatomy** (48px tall):
- Severity dot (12px, WCAG AA contrast fixed)
- Title line (title, truncate to 1 line)
- Impact line (secondary, e.g., "Saves 340ms p99 · 12k scans/hr")
- Inline sparkline (30-day occurrence trend, keyed to global time picker)
- Right column: relative time

Click row → detail drawer slides in from right (not a modal — persists selection when filters change). Escape or click outside to close. Keyboard: `j`/`k` to move selection, `Enter` to open, `e` to snooze, `r` to resolve, `a` to approve proposed action (if any).

**URL / history semantics:** `j`/`k` updates only local component state — it must NOT call `history.pushState` or `navigate()`. If every keypress pushed to the history stack, the browser back button would need to be hammered dozens of times to escape a triage session. The dedicated page route `/findings/:id` is only used for explicit sharing/deep-link entry; `Enter` on a row opens the drawer in-place without changing the URL. "Copy link to this finding" in the drawer header is how a user promotes a selection into a shareable URL.

### 5.4 Finding detail (side drawer)

```
┌────────────────────────────────────────────────────────────┐
│  🔴 Index missing on orders.customer_id                [✕] │
│  prod-us-east-1  ·  schema_lint:missing_index              │
├────────────────────────────────────────────────────────────┤
│  IMPACT                                                    │
│  Estimated p99 reduction: 340ms                            │
│  Queries affected: 12k/hr full-scan orders.customer_id     │
│  Table size: 48GB · Rows: 340M                             │
├────────────────────────────────────────────────────────────┤
│  WHY THIS MATTERS                                          │
│  Queries filtering on customer_id are doing sequential     │
│  scans. The table is large enough that the analyzer...     │
├────────────────────────────────────────────────────────────┤
│  RECOMMENDED SQL                                       [⎘] │
│  ┌──────────────────────────────────────────────────────┐ │
│  │ CREATE INDEX CONCURRENTLY idx_orders_customer_id     │ │
│  │   ON public.orders (customer_id);                    │ │
│  └──────────────────────────────────────────────────────┘ │
│                                                            │
│  Trust mode: Advisory · [Approve & run] [Propose] [Copy]  │
├────────────────────────────────────────────────────────────┤
│  TIMELINE                                                  │
│  ● 3d ago   First seen by analyzer                        │
│  ● 2d ago   🤖 Proposed CREATE INDEX CONCURRENTLY ...     │
│  ● now      ⏸ Awaiting approval                           │
├────────────────────────────────────────────────────────────┤
│  OCCURRENCE TREND (24h)                                    │
│  ▁▁▁▂▃▅▇██▇▅▃▂▁▁                                           │
└────────────────────────────────────────────────────────────┘
```

**Key sections** in order:
1. Header with severity, title, database, category
2. **Impact** — the numbers that motivate action
3. **Why this matters** — the narrative (already in backend as `recommendation` or analyzer prose)
4. **Recommended SQL** — copy button, [Approve & run] if trust allows, [Propose] otherwise
5. **Timeline** — first-seen, every action, current status. Same style as Sentry's activity feed.
6. **Occurrence trend** — bigger version of the row sparkline, scrubbable

Destructive action buttons require the trust-mode gate (same as today).

### 5.5 Actions page

```
┌──────────────────────────────────────────────────────────────────────────┐
│  ACTIONS — unified timeline                                              │
│  Status: (Pending) (Running) (Done) (Rolled-back) (Failed)              │
│  Actor: [All ▾]                                                          │
├──────────────────────────────────────────────────────────────────────────┤
│  14:32  🤖 Agent    VACUUM orders.line_items                             │
│         ✓ reclaimed 800MB · triggered by finding #1293                   │
│         [Expand SQL]  [Rollback info]                                   │
│  ──────────────────────────────────────────────────────                  │
│  13:05  🤖 Agent    CREATE INDEX CONCURRENTLY shipments_customer_idx    │
│         ✓ 3.2s · 340ms p99 savings confirmed · finding #1287            │
│  ──────────────────────────────────────────────────────                  │
│  10:18  👤 jmass    Approved: DROP INDEX idx_orders_v1                  │
│         ✓ 1.2GB freed · finding #1281                                   │
│  ──────────────────────────────────────────────────────                  │
│  09:02  ⏸ Agent     Propose: ALTER TABLE orders SET (autovacuum...)     │
│         ⏸ Awaiting approval · risk: MODERATE · finding #1279            │
│         [Approve] [Reject]                                              │
└──────────────────────────────────────────────────────────────────────────┘
```

Single chronological feed. Actor icon (human/agent), timestamp (global time picker scopes it), action summary, result line, finding link. Pending items surface at top when filter=Pending. Today's Actions page already has most of this — the change is to merge the "Executed" and "Pending Approval" tabs into one feed with status filters.

### 5.6 Fleet page

**Fleet mode (database="all"):**

```
┌──────────────────────────────────────────────────────────────────────────┐
│  FLEET  ·  3 databases                                                   │
│                                                                          │
│  ┌─────────────────┐  ┌─────────────────┐  ┌─────────────────┐          │
│  │ 🟢 prod-us-east │  │ 🟡 prod-eu-west │  │ 🟢 staging-shared│          │
│  │ Health: 94      │  │ Health: 67      │  │ Health: 99       │          │
│  │ ▁▂▃▅▇██▇▅▃▂     │  │ ▅▃▂▁▁▂▃▅▇█      │  │ ▇▇▇██████▇       │          │
│  │ ⚡ Advisory     │  │ ⚡ Advisory     │  │ 🟢 Monitor       │          │
│  │ 2 open · 0 ⏸   │  │ 8 open · 1 ⏸   │  │ 0 open · 0 ⏸    │          │
│  └─────────────────┘  └─────────────────┘  └─────────────────┘          │
└──────────────────────────────────────────────────────────────────────────┘
```

Grid of tiles. Each: health dot, name, health score, sparkline (QPS or latency, picker-aware), trust badge, open-findings / pending-actions count. Click a tile to drill into that database (sets the global DB picker, routes to Overview).

**Single-DB mode (database=specific):**

Shows temporal drilldowns:
- Query fingerprint list (from existing QueryHints + pg_stat_statements data) with sparklines, sort by total time
- Connection/lock live view (refresh button)
- Extension/config/role audit (existing snapshot data, but structured, not JSON dump)
- Tab bar: Queries · Indexes · Locks · Config · Raw snapshot

Replaces today's `DatabasePage.jsx` raw JSON dump.

### 5.7 Command palette (⌘K)

```
┌────────────────────────────────────────────┐
│  > orders                                  │
├────────────────────────────────────────────┤
│  🔍 Finding: Index missing on orders...   │
│  🔍 Finding: Unused idx on orders.status  │
│  📄 Jump to Findings filtered by orders   │
│  ─────────────────────────────────────     │
│  🗄  Switch to prod-us-east-1             │
│  ⚡  Change trust mode →                   │
│  🏃 Run nightly integration now           │
│  🆘 Emergency stop                         │
└────────────────────────────────────────────┘
```

Fuzzy search across findings, pages, databases, and actions. Also exposes commands: switch DB, change trust mode, emergency stop, trigger a scheduled run. Keyboard: up/down, enter to select, escape to close.

This is the single highest-productivity addition for a power user (you). Implementation: [cmdk](https://cmdk.paco.me/) or hand-rolled — probably hand-rolled since you already avoid heavy deps.

---

## 6. Component Spec

### 6.1 New components

| Name | Purpose |
|---|---|
| `TimeRangeProvider` | React context supplying `{from, to, preset, compareMode}`. Wraps the whole app. |
| `TimeRangePicker` | Header component. Presets + custom range + compare-toggle. Writes to context. |
| `TrustBadge` | Per-database trust mode, header-mounted. Opens confirm modal on change. |
| `FindingRow` | One row in the unified feed. Severity dot + title + impact line + sparkline + relative time + expand chevron. |
| `FindingDetailDrawer` | Side drawer replacing the current modal-or-inline-expansion pattern. Receives a finding id; fetches detail; focus-trapped. |
| `ActivityTimelineItem` | One row in the Actions timeline. Actor icon, timestamp, summary, result, finding link. |
| `DatabaseTile` | Fleet page tile. Health dot, name, score, sparkline, trust, counts. |
| `ImpactLine` | Renders "Saves 340ms p99 · 12k scans/hr" from a finding's `detail.impact_*` fields. Formatter. |
| `CommandPalette` | ⌘K overlay. Fuzzy matcher, result renderer, command registry. |
| `PlanDiffView` | Side-by-side EXPLAIN tree diff. Tier 2 — skip in v1. |
| `LiveTimeAgo` | `TimeAgo` but subscribes to a 30s ticker so "2m ago" becomes "3m ago" without navigation. |

### 6.2 Components to retire or retire-after-replacement

- `Dashboard.jsx` stat cards — replaced by narrative sentence + feed
- `DatabasePage.jsx` raw JSON dump — replaced by Fleet single-DB drilldown
- The inline modal pattern in Findings/Actions — replaced by `FindingDetailDrawer`

### 6.3 Components to keep and lightly patch

| Keep | Patch |
|---|---|
| `DataTable` | Change expansion key from row index to row id (fix for re-fetch bug) |
| `SeverityBadge` | Swap colors for WCAG AA contrast (critical: #fee2e2 text on #7f1d1d bg ~ 9:1) |
| `SQLBlock` | Keep as-is; excellent |
| `SparklineChart` | Expose `data` via `TimeRangeProvider` so it scrubs with global picker |
| `EmptyState` | Keep |
| `useAPI` | Add `TimeRangeProvider` awareness so `from`/`to` join the query params |

---

## 7. Interaction Patterns

### 7.1 Time picker as context

```jsx
// <App>
<TimeRangeProvider default="24h">
  <Layout> ... </Layout>
</TimeRangeProvider>

// inside any component
const { from, to } = useTimeRange()
const { data } = useAPI(`/api/v1/findings?from=${from}&to=${to}`)
```

Every chart, every useAPI caller picks it up. Changing the picker re-fetches everything in view.

**Required change to `useAPI`:** today's hook (`sidecar/web/src/hooks/useAPI.js`) has no `AbortController`. The dep array is `[fetchData, interval, url]`, so a new URL triggers a fresh `fetch` but the prior in-flight request is not cancelled — whichever resolves last wins the `setData`. With a global time picker, users will scrub rapidly and trigger dozens of overlapping fetches. Changes required:

1. `AbortController` per call. Abort on cleanup.
2. Before `setData(json)` in the success branch, check `if (!controller.signal.aborted)` — an aborted request can still resolve network-side and overwrite newer state. Catching `AbortError` alone is not sufficient.
3. Ignore `AbortError` in the catch branch (don't surface as a user-visible error).

**Debounce at the source, not just the transport.** `AbortController` protects the UI from stale writes but does nothing for the backend — rapid scrubbing still issues one request per picker change, and each one spawns a full `/api/v1/findings` query. Debounce the `TimeRangeContext` publisher by ~250ms so downstream fetches coalesce. Implement as a single `useDeferredValue`-or-equivalent inside the provider so every subscriber gets the same debounced value — do not debounce per caller.

**`from`/`to` plumbing.** Thread the picker's ISO timestamps into the query string on the caller side so the backend can honor the window once the handler params are added (today none of the `/api/v1/*` handlers accept `from`/`to` — Phase 1 backend prerequisite). Window semantics: see §12 resolved answer on `from`/`to` (overlapping-window semantics, not `last_seen`-only).

### 7.2 Keyboard map

Global:
- `⌘K` / `Ctrl+K` — command palette
- `g o` — go Overview
- `g f` — go Findings
- `g a` — go Actions
- `g t` — go Fleet
- `g s` — go Settings
- `?` — keyboard help overlay

Finding feed / drawer:
- `j` / `k` — next/prev
- `Enter` — open detail
- `Esc` — close detail
- `e` — snooze
- `r` — resolve
- `a` — approve proposed action (if present)
- `/` — focus filter

This is Linear-tier keyboard UX, which matters a lot for a single-user power tool (your use case).

### 7.3 Destructive action gating

Keep the existing pattern — it's good:
- Emergency stop: 5-second armed countdown
- Trust escalation: confirm modal with explicit text
- Suppress finding: confirm modal
- Approve proposed action: single click (already gated by trust mode)

New additions:
- Rollback agent action: confirm modal explaining what will be undone
- Change database from palette: no confirm (reversible)
- Trigger scheduled run from palette: no confirm (just runs)

### 7.4 Drawer vs modal

Drawers (slide from right) for **selected object** interaction. Lets user keep the list visible, scroll through entries, keep filter state. Use for:
- Finding detail
- Action detail
- Database drilldown from fleet tile (optional — could also full-page route)

Modals for **confirmation** / one-shot forms only:
- Trust escalation confirm
- Emergency stop
- Suppress confirm
- Approve SQL review (before execution)

### 7.5 Loading / error states

- Skeleton rows in lists (not spinners) — maintain layout, look calm
- ErrorBanner for top-level fetch failures (keep existing)
- Per-row error for action failures (e.g., "Approve failed — retry")
- Never show a spinner for < 200ms (use a debounce)

---

## 8. Accessibility & Visual Polish

- **Contrast**: repaint SeverityBadge backgrounds. Text-on-bg must be ≥ 4.5:1 (AA) on normal text, 3:1 on large. Current critical/warning are borderline.
  - Critical: bg `#450a0a`, text `#fecaca` → 10:1
  - Warning: bg `#422006`, text `#fde68a` → 9:1
  - Info: bg `#0c2742`, text `#bfdbfe` → 10:1
- **Focus rings**: add `focus-visible:ring-2 ring-accent` to all interactive elements. Sidebar nav currently has no visible focus.
- **Modal focus trap**: use a small trap util or the `inert` attribute on background. Restore focus to trigger on close.
- **Keyboard help overlay**: `?` opens a list of shortcuts, grouped by context.
- **Reduced motion**: respect `prefers-reduced-motion` — disable sparkline animations and drawer slide, use fade.
- **Light mode (stretch)**: introduce CSS variables for a light theme; user preference stored in localStorage. Not required for v1 — but the variable system already there makes this a 1-day job when we want it.

---

## 9. Migration Plan — phased rollout

Each phase is independently shippable. Ship as incremental PRs; users see improvement continuously, not after a big-bang swap.

### Phase 1 — foundation (Week 1)

Frontend:
- Add `TimeRangeProvider`, `TimeRangePicker`, top-bar refactor
- `TrustBadge` in header
- Fix DataTable row-id keying
- Fix SeverityBadge contrast
- Add `LiveTimeAgo`
- `useAPI.js` — add `AbortController`, ignore `AbortError`, thread `from`/`to` from `TimeRangeContext` into the query string

Backend (prerequisites for Phase 2 and later):
- Extend `buildFindingsOrder()` allowlist to include `impact_score` (`sidecar/internal/api/handlers.go:700`) — needs `NULLS LAST` so subsystems that don't emit impact yet don't dominate the tail.
- Populate `Finding.ImpactScore` from the rest of the subsystems (analyzer rules, optimizer, forecaster, tuner, advisor). **A normalized 0..1 score is NOT a cross-subsystem comparator** — a 0.9 on `schema_lint:varchar_255` (a cosmetic rule) cannot outrank a 0.9 on `forecast_disk_growth` (disk full in 2h). Two-part approach:
  (a) each subsystem emits an intra-subsystem impact score (0..1) for within-subsystem ranking;
  (b) a separate `severity` ordering (the existing 3-tier `critical/warning/info`) remains the cross-subsystem key.
  The UI default sort becomes **severity DESC, then impact_score DESC, then last_seen DESC**. The `Sort: Impact` dropdown option is only meaningful when a single `subsystem` filter is active — greyed out otherwise and documented in the dropdown tooltip.
- Accept `?from=&to=` on `/api/v1/findings`, `/api/v1/actions`, `/api/v1/snapshots/history`, `/api/v1/forecasts`, `/api/v1/alert-log`. Default = last 24h if unset. Semantics per resource: see §12 resolved answer (overlapping-window for findings, BETWEEN for time-series).

Ship as one PR per layer. No destructive changes — additive.

### Phase 2 — unified findings (Week 2)

Backend:
- Extend `FindingFilters.Source` and the WHERE construction in `api/handlers.go` so `source=rules|optimizer|advisor|forecaster|query_tuning|schema_lint|migration_advisor` each map to the right `category IN (...)` or `category LIKE` clause (see §16).
- Add dedicated URL `GET /api/v1/findings/:id` if it doesn't already exist (it does — verify response shape is rich enough for the drawer).

Frontend:
- Build `FindingRow` + `FindingDetailDrawer`.
- Add `ImpactLine` — reads `impact_score` when present, else falls back to human-readable summary from `detail.impact` / `detail.table_size` / `detail.query_count`.
- Replace `pages/Findings.jsx` list with `FindingRow`; route `/findings/:id` to the same drawer content mounted as a page (enables deep-linking without duplicating logic — the drawer wraps a `<FindingDetailPanel>` that the page also renders).
- Collapse SchemaHealthPage, ForecastsPage, QueryHintsPage, IncidentsPage, AlertLogPage into Findings with a subsystem filter.
- Redirect old URLs to `/findings?source=X`.

Major visible change. Screenshot before/after for the changelog.

### Phase 3 — Overview rebuild (Week 2-3)

Backend:
- New endpoint `GET /api/v1/fleet/health?from=&to=&bucket=` returning a time series of `{ts, health_score}` per database plus a cluster-wide aggregate. No equivalent endpoint exists today — the current `/api/v1/databases` returns a single point-in-time `health_score` per DB (computed in `fleet/health.go`), not a series. Easiest implementation: persist a per-tick row to `sage.health_history` when `fleet.Manager` recomputes, then query with `date_bin`.

Frontend:
- Replace `Dashboard.jsx` with new Overview layout.
- Cluster health sparkline sourced from the new endpoint.
- Recent-activity feed (reuses `ActivityTimelineItem`, hits `/api/v1/actions?limit=10`).

### Phase 4 — Actions unification (Week 3)
- Merge Executed + Pending Approval into one timeline
- Actor-first formatting
- Finding link on every row

### Phase 5 — Fleet page (Week 3-4)
- Build `DatabaseTile`
- Replace raw JSON `DatabasePage.jsx` with structured drilldown tabs (Queries / Indexes / Locks / Config)

### Phase 6 — Command palette + keyboard (Week 4)
- ⌘K overlay
- Global shortcuts
- Help overlay (`?`)

### Phase 7 — Polish (ongoing)
- Drawer focus trap
- Pagination UX ("showing 24 of 47")
- Empty/loading state review per page
- Prefers-reduced-motion
- Optional: light theme

---

## 10. Out of Scope (v1)

Explicitly NOT doing now:
- Full plan-diff viewer (requires EXPLAIN tree parser — defer to Tier 2)
- Light theme (variable system ready; toggle deferred)
- Mobile-optimized layout (dashboard is desktop-first, same as competitors)
- In-app chat/LLM assistant (anti-pattern per research)
- Real-time streaming (polling is sufficient for current data cadence)

---

## 11. Success Criteria

After shipping, a user should be able to:

1. Answer "is anything on fire?" in 5 seconds from Overview without clicking anything.
2. See what the agent did in the last 24h without navigating to Actions.
3. Sort findings by impact, not by severity taxonomy. **Backend dependency:** `buildFindingsOrder()` allowlist extension + subsystem-wide `ImpactScore` population. Without both, "sort by impact" degrades to "sort by severity" for the unscored subsystems.
4. Scrub a time range once and have every chart on screen re-scope. **Backend dependency:** handlers listed in §9 Phase 1 must accept `from`/`to`.
5. Jump from any page to any other page, any database, or any finding with ⌘K and 3-5 keystrokes.
6. Tell at a glance, on every page, which trust mode this database is in.
7. Approve or reject a proposed action without leaving the finding detail.

None of (1)-(7) is possible in the current UI.

---

## 12. Resolved Questions (previously open — answered by code investigation)

1. **Impact score source.** Partially in place. The `sage.findings.impact_score` column exists (`internal/schema/bootstrap.go`), and `analyzer.Finding.ImpactScore float64` is the Go-side carrier (`internal/analyzer/finding.go:28`). `UpsertFindings` writes it when non-zero and treats zero as SQL NULL (`finding.go:88-101`) or preserves the prior value on updates (`finding.go:66-68`). **However, only the schema_lint subsystem assigns a non-zero value today** — all other subsystems (analyzer rules, forecaster, tuner, advisor, optimizer) leave it at zero. Decision: ship Phase 1 backend work to populate `ImpactScore` across subsystems (even a 0..1 normalized score), then ship the UI "sort by impact" in Phase 2. Until then, the UI sorts by severity with impact as a secondary key.

2. **Cluster health aggregate endpoint.** Does not exist. `/api/v1/databases` emits a current `health_score` per DB; there is no time-series endpoint and no historical table. Decision: add `GET /api/v1/fleet/health?from=&to=&bucket=` in Phase 3 backed by a new `sage.health_history` write on every `fleet.Manager` health recompute. Client-side aggregation is rejected — it re-fetches every DB's history and bloats the dashboard.

3. **Drawer vs page for finding detail.** Both. Drawer on list views for fast triage; `/findings/:id` renders the same `FindingDetailPanel` as a page for deep-linking and copy-paste URL sharing. Single component, two mount sites.

4. **Source-filter taxonomy.** No physical `source` column — `sage.findings.category` holds 40+ distinct string values. The current backend filter `FindingFilters.Source` accepts only `""` or `"schema_lint"` (the latter matches `category LIKE 'schema_lint:%'`). Decision: keep the virtual-filter approach (category-prefix / allowlist mapping, §16 appendix) to avoid a migration. Extend `FindingFilters.Source` to accept the taxonomy values and build a `category IN (...)` clause per value. Revisit a physical `source_kind` column only if the category set explodes or the allowlist becomes unmaintainable.

5. **Command palette command registry.** Hardcoded in a single `commands.js` module for v1. Each command is `{id, label, run, keywords, requiresTrustAtLeast?}`. Extract a registry pattern only when a plugin or user-defined command need emerges — which doesn't exist today.

### Also resolved

- **`from`/`to` param semantics for Findings.** Use **overlapping-window** semantics: `first_seen <= $to AND (resolved_at IS NULL OR resolved_at >= $from)` — equivalent to "the finding was live at some point in [from, to]". Using `last_seen` alone is wrong: a finding with `first_seen=3d ago` and `last_seen=now` would drop out of a "yesterday" window even though it was unambiguously active then. For Actions, use `executed_at BETWEEN $from AND $to`. For snapshots/forecasts, use `ts BETWEEN $from AND $to`.
- **Compare-to-previous-period.** Client requests twice and overlays. Simpler endpoints, no schema bloat. Acceptable because the time-range bucket count is bounded (max ~200 points per series).
- **Retention of `sage.health_history`.** 30d rolling window mirroring other retention defaults (`internal/retention/`). Confirm against existing retention knobs before Phase 3.
- **Pagination total count.** Exact `COUNT(*)` over the filtered `sage.findings` query will degrade as the table grows. Use a PostgreSQL plan-based estimate (`EXPLAIN (FORMAT JSON)` → `"Plan Rows"`) when the filter is broad, and exact only when the filter is narrow (e.g., single `source`). The UI copy changes from "Showing 24 of 47" to "Showing 24 of ~50" in the broad case — acceptable.

---

## 13. File-level Change Inventory

For implementation tracking. Each path is under `sidecar/web/src/` unless noted.

### New files
- `contexts/TimeRangeContext.jsx`
- `components/TimeRangePicker.jsx`
- `components/TrustBadge.jsx`
- `components/FindingRow.jsx`
- `components/FindingDetailDrawer.jsx`
- `components/ActivityTimelineItem.jsx`
- `components/DatabaseTile.jsx`
- `components/ImpactLine.jsx`
- `components/LiveTimeAgo.jsx`
- `components/CommandPalette.jsx`
- `components/KeyboardHelp.jsx`
- `hooks/useKeyboardShortcuts.js`
- `hooks/useTimeRange.js`
- `pages/OverviewPage.jsx` (replaces Dashboard.jsx)
- `pages/FleetPage.jsx` (replaces DatabasePage.jsx)

### Modified files
- `App.jsx` — wrap in `TimeRangeProvider`, new route table, command palette mount
- `Layout.jsx` — new top bar with trust badge + time picker, reduced sidebar
- `hooks/useAPI.js` — pick up `from`/`to` from `TimeRangeContext`, add `AbortController`
- `components/DataTable.jsx` — expansion state keyed by row id, not index
- `components/SeverityBadge.jsx` — new color tokens, AA contrast
- `components/TimeAgo.jsx` — replace with `LiveTimeAgo` or add ticker
- `pages/Findings.jsx` — rewrite to use `FindingRow` + `FindingDetailDrawer`; accept `?source=` filter
- `pages/Actions.jsx` — merge tabs into one timeline
- `pages/SettingsPage.jsx` — fold in Users, Notifications, DatabaseSettings as tabs

### Retired files
- `pages/Dashboard.jsx`
- `pages/DatabasePage.jsx`
- `pages/SchemaHealthPage.jsx` (merged into Findings with `source=schema_lint`)
- `pages/ForecastsPage.jsx` (merged into Findings with `source=forecaster`)
- `pages/QueryHintsPage.jsx` (merged into Findings with `source=query_tuning`; some content also lives in Fleet single-DB drilldown)
- `pages/IncidentsPage.jsx` (currently reads from a separate `sage.incidents` table. **Do not UNION `sage.incidents` with `sage.findings` at query time** — paginating and sorting across a dynamic UNION is slow at scale and fights every index. Instead, **physically migrate** incidents into `sage.findings` with `subsystem='incident'` and a set of incident categories (`incident_open`, `incident_resolved`, etc.) before Phase 2 lands. Write a migration + backfill; retire `sage.incidents` once readers move over. If that's out of scope for Phase 2, keep IncidentsPage standalone and defer the merge until Phase 5 with the migration as a hard prereq.)
- `pages/AlertLogPage.jsx` (alert log is orthogonal to findings — keep it available under Settings → Alerts tab rather than folding into Findings. Adjust info architecture in §4.1 accordingly.)
- `pages/UsersPage.jsx` (becomes a Settings tab)
- `pages/NotificationsPage.jsx` (becomes a Settings tab)
- `pages/DatabaseSettingsPage.jsx` (becomes a Settings tab)
- `pages/DatabasesPage.jsx` (replaced by Fleet)

Page count: 15 → 5 primary (Overview, Findings, Actions, Fleet, Settings) + Login.

---

## 14. What This Does *Not* Solve

Honest list:
- It doesn't make the backend findings emit impact estimates where they currently don't. That's a separate analyzer-quality workstream.
- It doesn't address fleet-scale perf of the SPA (15 DBs × 30s polling = 30 requests/min; fine, but plan for it when fleet grows).
- It doesn't change the polling model to push/SSE. Polling is adequate for current data rates. Switch when we feel the latency.
- It doesn't change the auth model or introduce RBAC UI beyond what already exists.

---

## 15. Appendix — competitor influence map

| Pattern | Source | Where applied in this doc |
|---|---|---|
| Ranked finding feed | Sentry, pganalyze | §5.2, §5.3 |
| Global time picker with compare | Datadog, Grafana | §5.1, §7.1 |
| Sparkline-in-row | PlanetScale Insights | §5.2, §5.3 |
| Spatial fleet view | CockroachDB Cloud | §5.6 |
| Actor-named timeline | Vercel, Linear | §5.5 |
| Narrative finding detail | pganalyze | §5.4 |
| Command palette | Linear | §5.7 |
| Keyboard-first list navigation | Linear | §7.2 |
| Side drawer for detail | Linear, Supabase | §5.4, §7.4 |
| SQL as universal export | Supabase Studio, PgHero | §3, §5.4 |
| Impact > severity ranking | pganalyze Index Advisor | §3, §5.3 |

---

## 16. Appendix — category → subsystem taxonomy

The `source` dropdown in §5.3 maps to this table. The mapping is implemented as an allowlist-to-SQL rewrite in `api/handlers.go`:

| Subsystem (`?source=`) | Matches `category` values | Emitted by |
|---|---|---|
| `rules` | `index_health`, `unused_index`, `invalid_index`, `duplicate_index`, `missing_fk_index`, `slow_query`, `high_plan_time`, `query_regression`, `seq_scan_heavy`, `high_total_time`, `lock_chain`, `connection_leak`, `cache_hit_ratio`, `checkpoint_pressure`, `stat_statements_pressure`, `replication_lag`, `inactive_slot`, `slow_replication_slot`, `sequence_exhaustion`, `sort_without_index`, `work_mem_promotion`, `table_bloat`, `xid_wraparound`, `extension_drift`, `plan_regression` | `internal/analyzer/rules_*.go` |
| `forecaster` | `storage_forecast`, `forecast_disk_growth`, `forecast_connection_saturation`, `forecast_cache_pressure`, `forecast_sequence_exhaustion`, `forecast_query_volume`, `forecast_checkpoint_pressure` | `internal/forecaster/*` |
| `query_tuning` | `query_tuning`, `stale_statistics`, `runaway_query` | `internal/tuner/*`, `internal/executor/runaway.go` |
| `advisor` | whatever `advisor/prompt.go` emits via the `category` param — dynamic, LLM-chosen | `internal/advisor/*` |
| `optimizer` | index recommendations — typically posted with `category="unused_index"` or `"missing_fk_index"`; unify under `optimizer` via a provenance hint in `detail` when needed | `internal/optimizer/*` |
| `schema_lint` | any `category` with prefix `schema_lint:` (28+ rule IDs: `missing_pk`, `duplicate_index`, `invalid_index`, `fk_type_mismatch`, `jsonb_in_joins`, `low_cardinality_index`, `missing_fk_index`, `mxid_age`, `nullable_unique`, `overlapping_index`, `sequence_overflow`, `serial_usage`, `timestamp_no_tz`, `toast_heavy`, `txid_age`, `unused_index`, `varchar_255`, `char_usage`, `bloated_table`, `int_pk`, `wide_table`, …) | `internal/schema/lint/rule_*.go` |
| `migration_advisor` | (not yet emitted; reserved for the planned migration advisor workstream) | — |

Notes:
- `optimizer` overlaps with `rules` on some categories today (both can surface `unused_index`). **Do not disambiguate via a JSONB `provenance` field** — list queries filter on subsystem and a JSONB condition on a large table will miss indexes and slow the Findings page as the table grows. Instead, add a physical `sage.findings.subsystem text` column indexed on `(status, subsystem, last_seen desc)`. Backfill from the current category prefix / emitter. Callers set `subsystem` explicitly at emit time. The virtual category-mapping (this table) is the bootstrap strategy for Phase 2; the physical column is the durable answer and should land in Phase 3 or earlier.
- `advisor`'s category is dynamic because the LLM picks it. For the filter to work, `advisor/prompt.go` must constrain output to an allowlist or every advisor finding must set `subsystem='advisor'` explicitly regardless of category.
- The allowlist in `buildFindingsOrder` must remain column-name-only; never accept arbitrary strings from the client.
