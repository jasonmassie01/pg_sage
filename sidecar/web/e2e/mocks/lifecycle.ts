// lifecycle.ts: case payloads for cases-lifecycle.spec.ts (cases-10: the
// next-step, expiry and retry lines; sre-09: a live investigation that
// refreshes) and migration-preflight.spec.ts (migration-01: the preflight
// measured_at panel). Each payload names the Go code whose JSON it copies.
// Register these routes AFTER mockAllAPIs: Playwright matches the newest first.

import { type Page } from '@playwright/test'
import { findingCase, investigationRecord } from './cases'

// NOW is the spec's fixed browser clock and the mocked server's "now".
export const NOW = '2026-11-12T12:00:00Z'

// plusMinutes is NOW moved by minutes, as RFC 3339.
export const plusMinutes = (minutes: number) =>
  new Date(Date.parse(NOW) + minutes * 60000).toISOString()

// recommendation copies cases.CaseRecommendation (recommendation_status.go):
// next_step is what recommendationNextStep wrote for the head.
export function recommendation(state: string, nextStep: string,
  extra: Record<string, unknown> = {}) {
  return {
    id: 'rec-11', state, revision: 2, attempt_count: 1,
    updated_at: '2026-11-12T11:00:00Z', current_content: true, next_step: nextStep,
    url: '/api/v1/recommendations/rec-11', ...extra,
  }
}

// candidate copies cases.ActionCandidate for a finding's analyze_table.
export function candidate(extra: Record<string, unknown> = {}) {
  return {
    action_type: 'analyze_table', risk_tier: 'safe', proposed_sql: 'ANALYZE public.orders',
    rollback_class: 'reversible', expires_at: plusMinutes(60), ...extra,
  }
}

// markExpired copies cases.MarkExpiredCandidates: a candidate that
// IsExecutable(now) rejects (now is not before expires_at) gets
// blocked_reason evidence_expired, unless a recommendation refusal holds it.
export function markExpired(c: Record<string, unknown>, now = NOW) {
  const expires = c.expires_at ? Date.parse(String(c.expires_at)) : NaN
  if (Number.isNaN(expires) || Date.parse(now) < expires) return c
  if (String(c.blocked_reason || '').startsWith('recommendation_')) return c
  return { ...c, blocked_reason: 'evidence_expired' }
}

// lifecycleCase is a finding case with an optional recommendation head and
// candidate, as cases_recommendation.go attaches them.
export function lifecycleCase(id: string, extra: Record<string, unknown> = {}) {
  return findingCase(id, 'orders', 'stale_statistics', extra)
}

/* ---------- DDL preflight (migration-01) ---------- */

// preflightCandidate copies the ddl_preflight candidate of
// TestFindingDetail_VacuumFullMeasuredOnRealServer: measured_at is the RFC
// 3339 time measuredAtFromDetail passes through (internal/cases/ddl_preflight.go),
// absent when nothing was measured.
export function preflightCandidate(preflight: Record<string, unknown>) {
  return {
    action_type: 'ddl_preflight', risk_tier: 'high', rollback_class: 'forward_fix_only',
    ddl_preflight: {
      summary: 'ACCESS EXCLUSIVE rewrite risk', lock_level: 'ACCESS EXCLUSIVE',
      requires_rewrite: true,
      checks: [{ name: 'activity', status: 'warn', detail: '2 active queries' },
        { name: 'rewrite', status: 'warn', detail: 'full table rewrite' }],
      ...preflight,
    },
  }
}

// UNMEASURED_CHECKS are a legacy preflight's live checks: every one unknown.
export const UNMEASURED_CHECKS = [
  { name: 'activity', status: 'unknown', detail: 'Not measured' },
  { name: 'pending_locks', status: 'unknown', detail: 'Not measured' },
  { name: 'lock_level', status: 'pass', detail: 'ACCESS EXCLUSIVE' },
]

export function migrationCase(preflight: Record<string, unknown>) {
  return findingCase('52', 'orders', 'migration_risk', {
    title: 'ALTER TABLE orders rewrites the table',
    action_candidates: [preflightCandidate(preflight)],
  })
}

/* ---------- A live investigation (sre-09) ---------- */

// ROOT_CAUSE copies one root_cause hypothesis of sre.Detail (internal/sre
// record.go) for a disk_runway run: what the panel lists as likely.
export const ROOT_CAUSE = {
  node: 'wal_retention', subject: 'slot cdc_orders', status: 'root_cause', confidence: 0.86,
  label: 'WAL retained by an inactive slot',
  support: [{ evidence_id: 'ev-1', text: 'slot cdc_orders holds 41 GB of WAL' }],
}

export type LiveAnswer = { status?: number, json: unknown }

// routeLiveInvestigation answers the case cards' investigations list with
// listed() and the open panel's detail with detail(); the detail requests
// are counted, so a spec can see polling stop at a terminal state. The
// detail's sub-resources (events, proposals, similar) answer empty.
export async function routeLiveInvestigation(page: Page,
  listed: () => ReturnType<typeof investigationRecord>, detail: () => LiveAnswer) {
  const counts = { list: 0, detail: 0 }
  await page.route(url => url.pathname === '/api/v1/investigations', route => {
    counts.list += 1
    return route.fulfill({ json: { items: [listed()] } })
  })
  await page.route('**/api/v1/databases/*/investigations/**', route => {
    const path = new URL(route.request().url()).pathname
    if (!/\/investigations\/[^/]+$/.test(path)) return route.fulfill({ json: {} })
    counts.detail += 1
    return route.fulfill(detail())
  })
  return counts
}

// liveDetail copies sre.Detail for record: a concluded run carries its root
// cause, a running one has no hypotheses yet.
export function liveDetail(record: ReturnType<typeof investigationRecord>): LiveAnswer {
  const done = record.state === 'concluded'
  return { json: {
    investigation: record, hypotheses: done ? [ROOT_CAUSE] : [], revisions: done ? 2 : 1,
    evidence: [], evidence_available: true, tombstones: [], event_count: done ? 6 : 3,
    chain_verified: true,
  } }
}
