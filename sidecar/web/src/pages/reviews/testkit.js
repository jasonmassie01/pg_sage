// testkit.js — shared fixtures for the Reviews page tests: change reviews
// in the shape of GET /api/v1/databases/{db}/change-reviews/{id}
// (changereview.RenderJSON) and the summary of
// GET /api/v1/change-reviews/summary. Only imported by *.test.jsx files.

export const viewer = { role: 'viewer', email: 'vera@example.com' }
export const operator = { role: 'operator', email: 'bob@example.com' }
export const admin = { role: 'admin', email: 'ada@example.com' }

export function response(status, body) {
  return {
    ok: status >= 200 && status < 300, status, statusText: `status ${status}`,
    json: async () => body,
  }
}

const hour = 3600 * 1000
export const NOW = Date.parse('2026-10-23T12:00:00Z')
const ago = ms => new Date(NOW - ms).toISOString()

const evidence = [
  { id: 'queryid:-4242', kind: 'queryid', label: 'SELECT email FROM orders WHERE id = $1',
    digest: 'a'.repeat(64), data: { calls: 412000, mean_ms: 0.8 } },
  { id: 'table:public.orders', kind: 'table', label: 'public.orders',
    digest: 'b'.repeat(64), data: { reltuples: 2000000, bytes: 734003200 } },
  { id: 'stmt:1', kind: 'stmt', label: 'statement 1', digest: 'c'.repeat(64),
    data: { sql: 'ALTER TABLE orders RENAME COLUMN email TO email_address' } },
  { id: 'inspect:0123456789ab', kind: 'inspect', label: 'model statement',
    digest: 'd'.repeat(64), data: { plan: 'Index Scan using orders_pkey' } },
]

const findings = [
  { rule_id: 'CR-15', kind: 'new_object_antipattern', severity: 'info', origin: 'rule',
    title: 'int4 primary key', text: 'id is int4', evidence_ids: ['table:public.orders'] },
  { rule_id: 'CR-03', kind: 'column_still_referenced', severity: 'critical',
    origin: 'rule', title: 'email is still read by 1 statement',
    text: '412000 calls this week', evidence_ids: ['queryid:-4242', 'stmt:1'] },
  { rule_id: 'CR-01', kind: 'lock_and_rewrite', severity: 'warning', origin: 'rule',
    title: 'ACCESS EXCLUSIVE on orders', text: 'brief lock',
    evidence_ids: ['table:public.orders'] },
  { kind: 'expand_contract', severity: 'warning', origin: 'model',
    title: 'Use expand/contract', text: 'add the column first',
    evidence_ids: ['queryid:-4242'],
    suggestion: { language: 'ruby', code: 'add_column :orders, :email_address, :text' } },
]

const statements = [
  { index: 1, origin: 'sql_input', file: 'db/migrate/1_rename.sql', line: 3,
    sql: 'ALTER TABLE orders RENAME COLUMN email TO email_address', kind: 'rename_column',
    evidence_id: 'stmt:1' },
  { index: 2, origin: 'model_extracted', file: 'app/models/order.rb', line: 12,
    sql: 'SELECT * FROM orders WHERE status = $1 ORDER BY created_at LIMIT 50',
    kind: 'select', matched_queryid: -4242, confidence: 0.8, orm: 'activerecord',
    evidence_id: 'inspect:0123456789ab' },
  { index: 3, origin: 'model_translated', file: 'db/migrate/2_idx.rb', line: 4,
    sql: 'CREATE INDEX CONCURRENTLY orders_status_idx ON orders (status)',
    kind: 'create_index', confidence: 0.9, evidence_id: 'inspect:0123456789ab' },
  { index: 4, origin: 'model_extracted', file: 'app/controllers/orders.rb', line: 20,
    sql: 'SELECT id FROM orders WHERE customer_id = $1', kind: 'select',
    confidence: 0.6, evidence_id: 'inspect:0123456789ab' },
]

export function makeReview(over = {}) {
  return {
    id: '11111111-1111-4111-8111-111111111111', database: 'orders', source: 'mcp',
    actor: 'mcp:token:3', repo: 'acme/billing',
    pr_url: 'https://github.com/acme/billing/pull/42', pr_number: 42,
    pr_title: 'Rename orders.email', head_sha: 'abcdef1',
    input_hash: 'e'.repeat(64), state: 'complete', stage: 'done',
    verdict: 'risky', deterministic_verdict: 'risky', parse_mode: 'ast',
    model_status: 'ok', statements, findings, evidence,
    dropped: [
      { kind: 'finding', text: 'uncited claim', reason: 'uncited' },
      { kind: 'claim', text: 'made-up number', reason: 'ungrounded' },
    ],
    not_determined: [{ subject: 'app/jobs/sync.rb', reason: 'cut from the offer' }],
    summary: { critical: 1, warning: 2, info: 1, rules: ['CR-01', 'CR-03', 'CR-15'],
      model_findings: 1, dropped_count: 2, app_code_analyzed: true },
    expectations: { post_conditions: [{ statement_index: 1, kind: 'column_renamed',
      relation: 'public.orders', column: 'email', new_name: 'email_address' }],
    watch_queryids: [-4242], relations: ['public.orders'], predictions: [] },
    markdown: '<!-- pg_sage-review -->', tokens: 18234,
    landing_state: 'awaiting', receipt_verdict: 'pending',
    shadow_score: 'unscored', credit_state: 'none', credited_minutes: 0,
    created_at: ago(5 * hour), completed_at: ago(5 * hour - 60000),
    updated_at: ago(5 * hour - 60000), marker: '<!-- pg_sage-review -->',
    ...over,
  }
}

// Three reviews for the list: risky awaiting a DBA (db orders), review
// concurred and landed (db billing), safe with a clean receipt.
export function listFixture() {
  return {
    reviews: [
      makeReview(),
      makeReview({ id: '22222222-2222-4222-8222-222222222222', database: 'billing',
        pr_title: 'Add index on invoices', pr_number: 7, repo: 'acme/invoices',
        pr_url: 'https://github.com/acme/invoices/pull/7', verdict: 'review',
        dba_verdict: 'concur', dba_actor: 'user:1', landing_state: 'landed',
        landing_source: 'catalog', receipt_verdict: 'pending',
        created_at: ago(30 * hour) }),
      makeReview({ id: '33333333-3333-4333-8333-333333333333', pr_title: '',
        repo: '', pr_number: 0, pr_url: 'https://github.com/acme/web/pull/9',
        verdict: 'safe', landing_state: 'landed', receipt_verdict: 'clean',
        created_at: ago(3 * 24 * hour) }),
    ],
    databases: ['billing', 'orders'], unavailable: [], partial: false,
  }
}

export const decidedReceipt = {
  verdict: 'regressed', reason: 'latency', window_hours: 24,
  before_from: '2026-10-21T10:00:00Z', before_to: '2026-10-22T10:00:00Z',
  after_from: '2026-10-22T10:05:00Z', after_to: '2026-10-23T10:05:00Z',
  targets: [
    { queryid: -4242, verdict: 'regressed', before_mean_ms: 1.25, after_mean_ms: 2.5,
      before_calls: 51000, after_calls: 49000, change_pct: 100, p_value: 0.0001 },
    { queryid: 77, verdict: 'neutral', before_mean_ms: 4, after_mean_ms: 4.1,
      before_calls: 900, after_calls: 950, change_pct: 2.5 },
  ],
  tolerance: 'missed',
  incidents: ['9d1c0b6e-aaaa-4bbb-8ccc-123456789abc'], lock_findings: [42],
  born_queryids: [{ queryid: 555, first_seen: '2026-10-22T11:00:00Z', mean_ms: 3.2,
    calls: 120, relations: ['public.orders'] }],
  confounders: [], contradicted_by: ['queryid:-4242', 'incident:9d1c0b6e'],
}

export function summaryFixture(over = {}) {
  return {
    databases: ['orders'], unavailable: [], partial: false,
    since: '2026-09-23T12:00:00Z', reviews: 40, reviews_per_week: 9.3,
    verdict_mix: { safe: 25, review: 10, risky: 5 },
    dba_verdict_mix: { concur: 9, override: 3 },
    dba_wait_p50_seconds: 5400, dba_wait_p90_seconds: 86400 * 2,
    first_comment_p50_seconds: 95,
    concurrence: {
      safe: { n: 2, of: 2, rate: 1 },
      review: { n: 5, of: 7, rate: 5 / 7 },
      risky: { n: 2, of: 3, rate: 2 / 3 },
    },
    receipts: { clean: 12, clean_low_traffic: 3, regressed: 1, unverifiable: 2 },
    false_safe: 1,
    override_by_rule: { 'CR-17': { reviews: 6, overrides: 3, rate: 0.5,
      reasons: ['pg_stat_statements was reset'] } },
    caught_before_merge: 2, hours_credited: 2.5, hours_pending: 0.75,
    tokens_per_review: 12000,
    auto_clear: { eligible: 14, scored: 12, correct: 12, correct_rate: 1,
      regressions_90d: 0, min_scored: 30, min_correct_rate: 0.97, max_regressions: 0,
      ready: false, label: 'shadow only — not enabled' },
    ...over,
  }
}
