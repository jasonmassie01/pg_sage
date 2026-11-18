// reviewFixtures.js (review-01b): GET /api/v1/databases/{db}/review/grade
// payloads for the Review page's unit tests, in the shape of the Go
// golden (internal/api/testdata/review_grade_golden.json).

const TITLES = [
  ['recoverability', 'Recoverability', 'C'],
  ['change_safety', 'Change safety', 'A'],
  ['workload_stability', 'Workload and plan stability', 'B'],
  ['schema', 'Schema', 'B'],
  ['capacity_cost', 'Capacity and cost', 'A'],
  ['security_access', 'Security and access', 'B'],
  ['configuration', 'Configuration', 'A'],
  ['lifecycle', 'Lifecycle', 'A'],
]

export function dimension(id, title, letter, extra = {}) {
  return {
    dimension: id, title, letter, score: letter ? 85 : 30, coverage: letter ? 100 : 40,
    checks: [
      { check: `${id}.first`, status: 'pass', weight: 2, evidence: [`rubric:${id}.first`] },
      { check: `${id}.second`, status: letter ? 'warn' : 'unknown', weight: 1, evidence: [],
        reason: letter ? '' : 'the producer has not run yet' },
    ],
    ...extra,
  }
}

export function gradedRun(id = 12, extra = {}) {
  return {
    id, database: 'orders', kind: 'grade', triggers: ['daily'],
    rubric_version: 'pg_sage-rubric/1', status: 'done',
    started_at: '2026-11-16T09:00:00Z', finished_at: '2026-11-16T09:00:04Z',
    grade: {
      rubric_version: 'pg_sage-rubric/1', overall: 'B', overall_score: 86.4,
      coverage_pct: 95.2, known_dimensions: 8,
      dimensions: TITLES.map(([dim, title, letter]) => dimension(dim, title, letter)),
    },
    ...extra,
  }
}

export function delta(extra = {}) {
  return {
    first_run: false, rubric_changed: false, from_overall: 'C', to_overall: 'B',
    dimensions: [
      { dimension: 'schema', from_letter: 'C', to_letter: 'B', from_band: 'full',
        to_band: 'full', checks: [{ check: 'schema.duplicate_index', from: 'fail', to: 'pass' }] },
      { dimension: 'lifecycle', from_letter: 'B', to_letter: 'A', from_band: 'full',
        to_band: 'full',
        checks: [{ check: 'lifecycle.extension_drift', from: 'fail', to: 'warn' }] },
    ],
    ...extra,
  }
}

export function templatedReport(extra = {}) {
  return {
    run_id: 12,
    model_pass: { status: 'skipped', reason: 'grade-only run: the model pass runs only in a review' },
    findings: [
      { rank: 1, dimension: 'security_access', dimension_title: 'Security and access',
        check: 'security_access.rls_exposure', status: 'fail', critical: false, weight: 2,
        reason: '', evidence: ['first_look:1#rls_exposure'] },
      { rank: 2, dimension: 'recoverability', dimension_title: 'Recoverability',
        check: 'recoverability.restore_drill', status: 'warn', weight: 5,
        reason: 'last passed 40 days ago', evidence: ['drill:7'] },
    ],
    ...extra,
  }
}

export function gradePayload(extra = {}) {
  const previous = gradedRun(11, { started_at: '2026-11-15T09:00:00Z',
    finished_at: '2026-11-15T09:00:04Z' })
  return {
    latest: gradedRun(), previous, delta: delta(), running: null,
    report: templatedReport(), viewed_at: null, ...extra,
  }
}
