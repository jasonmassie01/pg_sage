// Review page payload (review-01b): the Go route's golden response, GET
// /api/v1/databases/{db}/review/grade as
// TestReviewRoutes_GoldenPayloadOfTheReviewPage builds it. It is read from
// internal/api/testdata/review_grade_golden.json itself, so the mock cannot
// drift from the route (regenerate the golden with
// SAGE_UPDATE_RESTORE_DRILL_GOLDEN=1).

import golden from '../../../internal/api/testdata/review_grade_golden.json' with { type: 'json' }

// reviewGrade is the golden with no run in progress and not yet opened by
// this person, so the page sends its read receipt.
export function reviewGrade() {
  return { ...structuredClone(golden), running: null, viewed_at: null }
}

// reviewWithModelPass is the golden as review-03a will extend it: a done
// model pass with a link, a hypothesis, model text and a plan.
export function reviewWithModelPass() {
  const base = reviewGrade()
  return {
    ...base,
    report: {
      ...base.report,
      model_pass: { status: 'done', model: 'local-llama' },
      links: [{ a: 'schema.unindexed_fk', b: 'workload_stability.query_regression',
        relation: 'the slow join reads the unindexed key' }],
      hypotheses: [{ title: 'Partition orders by month', kind: 'partition',
        target: 'public.orders', rationale: '<img src=x onerror=alert(1)>' }],
      sections: [{ dimension: 'schema', text: 'Two foreign keys have no index.' }],
      plan_id: 'p-77',
    },
  }
}
