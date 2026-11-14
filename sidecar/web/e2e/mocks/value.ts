// value.ts: Value page cost and Overview briefing payloads for the
// dashboard-15 specs (market-04 cost card, llm-06b briefing card). Every
// payload names the Go test (or struct) whose JSON it copies. Register
// these routes AFTER mockAllAPIs: Playwright matches the newest first.

import { type Page } from '@playwright/test'

/* ---------- Value cost (GET /api/v1/value, cost section) ---------- */

// pricedCost copies cost.Section as TestValueCost_ConfiguredRatesPriceTheQueries
// serves it: operator rates from config, as of their oldest date, one query
// carrying the instance's compute cost.
export const pricedCost = {
  available: true, rate_source: 'config', as_of: '2026-11-01', currency: 'USD',
  window_start: '2026-11-12T09:00:00Z', window_end: '2026-11-13T09:00:00Z',
  window_hours: 24, compute: 48, storage: 6, iops: null, total: 54, idle_share: 0.25,
  top_queries: [{ database: 'orders', queryid: '4242', exec_seconds: 3600,
    share: 0.75, cost: 36, currency: 'USD' }],
  resources: [], dropped_pairs: 0, partial: false,
}

// noRates copies cost.Section without operator rates: unavailable with
// reason "rates not configured" and no figure (pg_sage ships no prices).
export const noRates = {
  available: false, reason: 'rates not configured',
  window_start: '2026-11-12T09:00:00Z', window_end: '2026-11-13T09:00:00Z',
  window_hours: 24, compute: null, storage: null, iops: null, total: null,
  idle_share: null, top_queries: null, resources: null, dropped_pairs: 0, partial: false,
}

export async function routeValue(page: Page, cost: unknown) {
  await page.route(url => url.pathname === '/api/v1/value', route =>
    route.fulfill({ json: { cost } }))
}

/* ---------- Briefings (GET /api/v1/briefings/latest) ---------- */

// briefing copies briefing.Record as the latest route serves it
// (TestBriefingsLatestFromDatabaseRoundTripsContentJSON): content_json is
// the stored Payload, delivery_status the per-channel Delivery outcomes.
export const briefing = {
  id: 7, database: 'orders', generated_at: '2026-11-13T08:00:00Z',
  period_start: '2026-11-12T08:00:00Z', period_end: '2026-11-13T08:00:00Z',
  mode: 'daily', llm_used: true, token_count: 812,
  content_text: 'orders: 3 open findings. Vacuum lag on public.events is the one to watch.',
  content_json: { total_open: 3, open_by_severity: { critical: 1, warning: 2 } },
  delivery_status: {
    slack: { status: 'sent', http_status: 200, at: '2026-11-13T08:00:02Z' },
    pagerduty: { status: 'error', http_status: 503, error: 'receiver unavailable',
      at: '2026-11-13T08:00:03Z' },
  },
}

// NO_BRIEFING copies noBriefingMessage("") with briefing.NoBriefingReason:
// the 404 the route answers when no database has a briefing.
export const NO_BRIEFING = 'no briefing yet: pg_sage writes one at each briefing.schedule '
  + 'time and none is stored for this database yet (an empty briefing.schedule turns '
  + 'briefings off)'

export async function routeBriefing(page: Page,
  answer: () => { status?: number, json: unknown }) {
  await page.route(url => url.pathname === '/api/v1/briefings/latest', route =>
    route.fulfill(answer()))
}
