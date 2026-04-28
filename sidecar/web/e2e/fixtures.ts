// fixtures.ts — Shared mock API responses and route interceptors
// for Playwright smoke tests that run without a live backend.
//
// Every e2e spec should call mockAllAPIs(page) in a beforeEach
// to intercept /api/* routes and return deterministic fixture data.

import { type Page } from '@playwright/test'

/* ---------- Mock response payloads ---------- */

export const mockAuthMe = {
  user: 'test',
  role: 'admin',
  email: 'test@test.com',
}

export const mockDatabases = {
  summary: {
    total_databases: 2,
    healthy: 2,
    degraded: 0,
    total_critical: 0,
    emergency_stopped: false,
  },
  databases: [
    {
      name: 'primary',
      trust_level: 'advisory',
      status: {
        connected: true,
        error: null,
        health_score: 95,
        findings_critical: 0,
        findings_warning: 1,
      },
    },
    {
      name: 'replica',
      trust_level: 'observation',
      status: {
        connected: true,
        error: null,
        health_score: 88,
        findings_critical: 0,
        findings_warning: 0,
      },
    },
  ],
  total: 2,
}

export const mockFindings = {
  findings: [
    {
      id: 1,
      severity: 'warning',
      title: 'Unused index: idx_users_email',
      database_name: 'primary',
    },
    {
      id: 2,
      severity: 'info',
      title: 'Table bloat above 20%: orders',
      database_name: 'primary',
    },
  ],
  total: 2,
}

export const mockCases = {
  cases: [
    {
      id: 'finding:primary:stale_statistics:table:public.orders',
      case_id: 'case-1',
      title: 'Stats are stale',
      severity: 'warning',
      state: 'open',
      why_now: 'table changed since last analyze',
      impact_score: 60,
      urgency_score: 75,
      action_candidates: [
        {
          action_type: 'analyze_table',
          risk_tier: 'safe',
          status: 'pending',
          lifecycle_state: 'blocked',
          blocked_reason: 'action is in cooldown',
          verification_status: 'not_started',
          attempt_count: 2,
          expires_at: '2026-04-28T12:00:00Z',
          cooldown_until: '2026-04-27T12:30:00Z',
          guardrails: ['dedicated connection', 'statement_timeout'],
          policy_decision: {
            decision: 'execute',
            risk_tier: 'safe',
            requires_approval: false,
            requires_maintenance_window: false,
          },
        },
      ],
      actions: [
        {
          id: 'queue:7',
          type: 'analyze_table',
          risk_tier: 'safe',
          status: 'pending',
          lifecycle_state: 'blocked',
          blocked_reason: 'action is in cooldown',
          verification_status: 'not_started',
          attempt_count: 2,
          expires_at: '2026-04-28T12:00:00Z',
          cooldown_until: '2026-04-27T12:30:00Z',
        },
        {
          id: 'log:88',
          type: 'analyze',
          risk_tier: 'safe',
          status: 'success',
          lifecycle_state: 'executed',
          verification_status: 'verified',
          attempt_count: 0,
        },
      ],
    },
  ],
  total: 1,
}

export const mockShadowReport = {
  total_cases: 14,
  would_auto_resolve: 12,
  requires_approval: 2,
  blocked: 1,
  estimated_toil_minutes: 360,
  blocked_reasons: ['requires approval'],
  proof: [
    {
      case_id: 'case-1',
      title: 'Stats are stale',
      action_type: 'analyze',
      policy_decision: 'execute',
      status: 'success',
      verification_status: 'verified',
      estimated_toil_minutes: 15,
    },
  ],
}

export const mockActions = {
  actions: [
    {
      id: 1,
      type: 'drop_index',
      status: 'pending',
      title: 'Drop unused index idx_users_email',
      database_name: 'primary',
      created_at: '2026-04-10T12:00:00Z',
    },
  ],
  total: 1,
}

export const mockPendingActions = {
  pending: [
    {
      id: 7,
      action_type: 'analyze_table',
      action_risk: 'safe',
      status: 'pending',
      finding_id: 42,
      database_name: 'primary',
      proposed_sql: 'ANALYZE public.orders',
      proposed_at: '2026-04-27T12:00:00Z',
      expires_at: '2026-04-28T12:00:00Z',
      policy_decision: 'execute',
      lifecycle_state: 'blocked',
      blocked_reason: 'action is in cooldown',
      verification_status: 'not_started',
      attempt_count: 2,
      cooldown_until: '2026-04-27T12:30:00Z',
      guardrails: ['dedicated connection', 'statement_timeout'],
      shadow_toil_minutes: 15,
    },
  ],
  total: 1,
}

export const mockPendingCount = { count: 1 }

export const mockConfig = {
  mode: 'fleet',
  databases: 2,
  config: {
    'collector.interval_seconds': { value: 60, source: 'yaml' },
    'collector.batch_size': { value: 100, source: 'default' },
    'collector.max_queries': { value: 500, source: 'default' },
    'analyzer.interval_seconds': { value: 120, source: 'yaml' },
    'analyzer.slow_query_threshold_ms': { value: 1000, source: 'default' },
    'analyzer.seq_scan_min_rows': { value: 10000, source: 'default' },
    'analyzer.unused_index_window_days': { value: 7, source: 'default' },
    'analyzer.index_bloat_threshold_pct': { value: 30, source: 'default' },
    'analyzer.table_bloat_dead_tuple_pct': { value: 10, source: 'default' },
    'analyzer.regression_threshold_pct': { value: 20, source: 'default' },
    'analyzer.cache_hit_ratio_warning': { value: 0.95, source: 'default' },
    'trust.level': { value: 'advisory', source: 'yaml' },
    'execution_mode': { value: 'approval', source: 'yaml' },
    'trust.tier3_safe': { value: 'true', source: 'default' },
    'trust.tier3_moderate': { value: 'false', source: 'default' },
    'trust.tier3_high_risk': { value: 'false', source: 'default' },
    'trust.maintenance_window': { value: '0 2 * * 0', source: 'default' },
    'trust.rollback_threshold_pct': { value: 10, source: 'default' },
    'trust.rollback_window_minutes': { value: 30, source: 'default' },
    'trust.rollback_cooldown_days': { value: 7, source: 'default' },
    'trust.cascade_cooldown_cycles': { value: 3, source: 'default' },
    'safety.cpu_ceiling_pct': { value: 80, source: 'default' },
    'safety.query_timeout_ms': { value: 30000, source: 'default' },
    'safety.ddl_timeout_seconds': { value: 300, source: 'default' },
    'safety.lock_timeout_ms': { value: 5000, source: 'default' },
    'llm.enabled': { value: 'true', source: 'yaml' },
    'llm.endpoint': {
      value: 'https://api.openai.com/v1', source: 'yaml',
    },
    'llm.api_key': { value: 'sk-***', source: 'yaml' },
    'llm.model': { value: 'gpt-4o', source: 'yaml' },
    'llm.timeout_seconds': { value: 30, source: 'default' },
    'llm.token_budget_daily': { value: 500000, source: 'default' },
    'llm.context_budget_tokens': { value: 8000, source: 'default' },
    'llm.optimizer.enabled': { value: 'true', source: 'default' },
    'llm.optimizer.min_query_calls': { value: 100, source: 'default' },
    'llm.optimizer.max_new_per_table': { value: 3, source: 'default' },
    'advisor.enabled': { value: 'true', source: 'default' },
    'advisor.interval_seconds': { value: 300, source: 'default' },
    'alerting.enabled': { value: 'true', source: 'yaml' },
    'alerting.slack_webhook_url': {
      value: 'https://hooks.slack.com/test', source: 'yaml',
    },
    'alerting.pagerduty_routing_key': { value: '', source: 'default' },
    'alerting.check_interval_seconds': { value: 60, source: 'default' },
    'alerting.cooldown_minutes': { value: 15, source: 'default' },
    'alerting.quiet_hours_start': { value: '22:00', source: 'default' },
    'alerting.quiet_hours_end': { value: '06:00', source: 'default' },
    'retention.snapshots_days': { value: 30, source: 'default' },
    'retention.findings_days': { value: 90, source: 'default' },
    'retention.actions_days': { value: 180, source: 'default' },
    'retention.explains_days': { value: 30, source: 'default' },
  },
}

export const mockManagedDatabases = {
  databases: [
    {
      id: 1,
      name: 'primary',
      host: 'db.example.com',
      port: 5432,
      database_name: 'app_production',
      trust_level: 'advisory',
      execution_mode: 'approval',
      enabled: true,
    },
    {
      id: 2,
      name: 'replica',
      host: 'replica.example.com',
      port: 5432,
      database_name: 'app_production',
      trust_level: 'observation',
      execution_mode: 'manual',
      enabled: true,
    },
  ],
}

export const mockForecasts = {
  forecasts: [],
  total: 0,
}

export const mockAlerts = {
  alerts: [],
  total: 0,
}

export const mockOAuthConfig = {
  enabled: false,
}

export const mockLLMStatus = {
  general: {
    model: 'gpt-4o',
    enabled: true,
    tokens_used: 250000,
    token_budget: 500000,
    budget_exhausted: false,
    circuit_open: false,
    resets_at: '2026-04-13T00:00:00Z',
  },
  optimizer: {
    model: 'gpt-4o',
    enabled: true,
    tokens_used: 50000,
    token_budget: 200000,
    budget_exhausted: false,
    circuit_open: false,
    resets_at: '2026-04-13T00:00:00Z',
  },
}

export const mockLLMStatusExhausted = {
  general: {
    model: 'gpt-4o',
    enabled: true,
    tokens_used: 500000,
    token_budget: 500000,
    budget_exhausted: true,
    circuit_open: false,
    resets_at: '2026-04-13T00:00:00Z',
  },
  optimizer: {
    model: 'gpt-4o',
    enabled: true,
    tokens_used: 200000,
    token_budget: 200000,
    budget_exhausted: true,
    circuit_open: false,
    resets_at: '2026-04-13T00:00:00Z',
  },
}

/**
 * Intercept all /api/* routes with fixture data so tests
 * can run without a live backend.
 */
export async function mockAllAPIs(page: Page) {
  // Catch-all for any unhandled API route — return 200 empty JSON
  // so the app doesn't show error banners for minor endpoints.
  // Registered FIRST so it has LOWEST priority (Playwright uses
  // LIFO ordering: last-registered handler matches first).
  await page.route('**/api/**', route =>
    route.fulfill({ status: 200, json: {} }),
  )

  // Auth
  await page.route('**/api/v1/auth/me', route =>
    route.fulfill({ json: mockAuthMe }),
  )
  await page.route('**/api/v1/auth/oauth/config', route =>
    route.fulfill({ json: mockOAuthConfig }),
  )

  // Managed databases — register BEFORE general /databases so the
  // more specific route matches first.
  await page.route('**/api/v1/databases/managed**', route =>
    route.fulfill({ json: mockManagedDatabases }),
  )

  // Databases (fleet overview used by Layout + Dashboard).
  // Trailing ** catches optional query params (?database=...).
  await page.route('**/api/v1/databases**', route => {
    const url = route.request().url()
    // If /managed slipped through, fulfill correctly.
    if (url.includes('/managed')) {
      return route.fulfill({ json: mockManagedDatabases })
    }
    return route.fulfill({ json: mockDatabases })
  })

  // Findings
  await page.route('**/api/v1/findings**', route =>
    route.fulfill({ json: mockFindings }),
  )

  // Cases and Shadow Mode
  await page.route('**/api/v1/cases**', route =>
    route.fulfill({ json: mockCases }),
  )
  await page.route('**/api/v1/shadow-report**', route =>
    route.fulfill({ json: mockShadowReport }),
  )

  // Actions
  await page.route('**/api/v1/actions**', route => {
    const url = route.request().url()
    if (url.includes('/pending/count')) {
      return route.fulfill({ json: mockPendingCount })
    }
    if (url.includes('/pending')) {
      return route.fulfill({ json: mockPendingActions })
    }
    return route.fulfill({ json: mockActions })
  })

  // Config
  await page.route('**/api/v1/config/global', route => {
    if (route.request().method() === 'PUT') {
      return route.fulfill({
        status: 200,
        json: { ok: true },
      })
    }
    return route.fulfill({ json: mockConfig })
  })

  // Emergency stop / resume
  await page.route('**/api/v1/emergency-stop**', route =>
    route.fulfill({ status: 200, json: { ok: true } }),
  )
  await page.route('**/api/v1/resume**', route =>
    route.fulfill({ status: 200, json: { ok: true } }),
  )

  // Forecasts
  await page.route('**/api/v1/forecasts**', route =>
    route.fulfill({ json: mockForecasts }),
  )

  // Alerts
  await page.route('**/api/v1/alerts**', route =>
    route.fulfill({ json: mockAlerts }),
  )

  // LLM status (token budget) — not exhausted by default
  await page.route('**/api/v1/llm/status', route =>
    route.fulfill({ json: mockLLMStatus }),
  )

  // LLM budget reset
  await page.route('**/api/v1/llm/budget/reset', route =>
    route.fulfill({ status: 200, json: { ok: true } }),
  )

  // LLM models (discover)
  await page.route('**/api/v1/llm/models', route =>
    route.fulfill({
      json: { models: [{ id: 'gpt-4o', name: 'GPT-4o' }] },
    }),
  )
}
