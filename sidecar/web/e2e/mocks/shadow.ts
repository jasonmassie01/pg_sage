// shadow.ts: shadow-ledger and trust payloads for shadow-page.spec.ts
// (earned-04: the Shadow Mode page rebuilt on /api/v1/shadow-decisions).
// Each payload names the Go code whose JSON it copies. Register these
// routes AFTER mockAllAPIs: Playwright matches the newest first.

import { type Page } from '@playwright/test'

// classSummary copies shadow.ClassSummary (internal/shadow): potential is
// potential_minutes, null when the class's toil model is retired with no
// successor (earned-04 decision), 0 when modelled but nothing counted.
export function classSummary(cls: string, family: string, counts: Partial<Record<
  'total' | 'pending' | 'correct' | 'incorrect' | 'neutral' | 'unscored' | 'counted',
  number>>, potential: number | null) {
  return {
    family, class: cls, total: 0, pending: 0, correct: 0, incorrect: 0, neutral: 0,
    unscored: 0, counted: 0, ...counts, potential_minutes: potential,
  }
}

// decision copies shadow.Decision as the class-filtered list serves it.
export function decision(id: number, sql: string) {
  return {
    id, sql, rollback_sql: 'DROP INDEX CONCURRENTLY public.idx_orders_created',
    prediction: { metric: 'mean_exec_time', expected_change_pct: -35, method: 'hypopg' },
    gate_verdict: 'shadow', gate_reason: 'below_earned_level', trusted_verdict: 'execute',
    status: 'scored', score: 'correct', score_source: 'verification', counted: true,
    score_reason: 'mean_exec_time fell 41%',
  }
}

export type LedgerAnswer = { status?: number, json: unknown }

// routeLedger answers GET /api/v1/shadow-decisions: the page's summary read
// from summary(), and the per-class list (?class=) from decisions(cls).
export async function routeLedger(page: Page, summary: () => LedgerAnswer,
  decisions: (cls: string) => unknown[] = () => []) {
  const classReads: string[] = []
  await page.route(url => url.pathname === '/api/v1/shadow-decisions', route => {
    const url = new URL(route.request().url())
    const cls = url.searchParams.get('class')
    if (!cls) return route.fulfill(summary())
    classReads.push(url.search)
    const database = url.searchParams.get('database')
    return route.fulfill({ json: { databases: [{ database, summary: [],
      decisions: decisions(cls) }] } })
  })
  return classReads
}

// trustRow copies one trust.Row of GET /api/v1/trust: the effective level
// label the gate holds a class to.
export function trustRow(cls: string, effective: string) {
  return { kind: 'self_initiated', class: cls, level: effective, effective }
}

export async function routeTrustLedger(page: Page, answer: () => LedgerAnswer) {
  await page.route(url => url.pathname === '/api/v1/trust', route => route.fulfill(answer()))
}

export const SHADOW_EMPTY = 'No shadow decisions yet.'
