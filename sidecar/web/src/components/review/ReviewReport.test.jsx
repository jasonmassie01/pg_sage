import { render, screen, within } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import { ReviewReport } from './ReviewReport'
import { templatedReport } from '../../test/reviewFixtures'

// review-01b: the ranked findings, the cross-dimension links, the
// hypotheses as advice, the model-pass status and the plan link. Model
// text is React text only. No concurrent access tests: a pure render.

const XSS = '<img src=x onerror=alert(1)>'

function modelReport(extra = {}) {
  return templatedReport({
    model_pass: { status: 'done', model: 'local-llama' },
    links: [{ a: 'schema.unindexed_fk', b: 'workload_stability.query_regression',
      relation: 'the slow join reads the unindexed key' }],
    hypotheses: [{ title: 'Partition orders by month', kind: 'partition',
      target: 'public.orders', rationale: 'the table grows 4 GB a month' }],
    sections: [{ dimension: 'schema', text: 'Two foreign keys have no index.' }],
    plan_id: 'p-77',
    ...extra,
  })
}

describe('ReviewReport', () => {
  it('ranks the findings with their dimension and evidence', () => {
    render(<ReviewReport report={templatedReport()} database="orders" />)
    const items = screen.getAllByTestId('report-finding')
    expect(items).toHaveLength(2)
    expect(items[0]).toHaveTextContent('1')
    expect(items[0]).toHaveTextContent('Security and access')
    expect(items[0]).toHaveTextContent('security_access.rls_exposure')
    expect(items[0]).toHaveTextContent('Fail')
    expect(items[1]).toHaveTextContent('last passed 40 days ago')
    expect(items[1]).toHaveTextContent('drill:7')
  })

  it('a skipped model pass shows the templated report and no model section', () => {
    render(<ReviewReport report={templatedReport({ sections: [{ dimension: 'schema',
      text: 'stale model text' }] })} database="orders" />)
    expect(screen.getByTestId('model-pass'))
      .toHaveTextContent('Model pass: skipped: grade-only run')
    expect(screen.getAllByTestId('report-finding')).toHaveLength(2)
    expect(screen.queryByTestId('model-section')).toBeNull()
    expect(screen.queryByText('stale model text')).toBeNull()
  })

  it('shows the model pass, links, hypotheses as advice and the plan link', () => {
    render(<ReviewReport report={modelReport()} database="orders db" />)
    expect(screen.getByTestId('model-pass')).toHaveTextContent('Model pass: done by local-llama')
    const link = screen.getByTestId('report-link')
    expect(link).toHaveTextContent('schema.unindexed_fk')
    expect(link).toHaveTextContent('workload_stability.query_regression')
    const hyp = screen.getByTestId('report-hypothesis')
    expect(hyp).toHaveTextContent('Advice')
    expect(hyp).toHaveTextContent('Partition orders by month')
    expect(hyp).toHaveTextContent('pg_sage does not act on advice')
    expect(within(screen.getByTestId('model-section')).getByText(
      'Two foreign keys have no index.')).toBeInTheDocument()
    expect(screen.getByRole('link', { name: 'Review the plan' }))
      .toHaveAttribute('href', '#/actions?tab=plans&plan=p-77&database=orders%20db')
  })

  it('shows a failed model pass with its reason', () => {
    render(<ReviewReport report={modelReport({ model_pass: { status: 'failed',
      reason: 'all 3 runs failed' } })} database="orders" />)
    expect(screen.getByTestId('model-pass'))
      .toHaveTextContent('Model pass: failed: all 3 runs failed')
    expect(screen.queryByTestId('model-section')).toBeNull()
  })

  it('renders model text as literal text, never HTML', () => {
    const { container } = render(<ReviewReport database="orders" report={modelReport({
      hypotheses: [{ title: XSS, kind: 'k', target: 't', rationale: XSS }],
      sections: [{ dimension: 'schema', text: XSS }],
      links: [{ a: XSS, b: 'b', relation: XSS }],
      findings: [{ rank: 1, dimension: 'schema', dimension_title: XSS, check: XSS,
        status: 'fail', weight: 1, reason: XSS, evidence: [XSS] }],
    })} />)
    expect(container.querySelector('img')).toBeNull()
    expect(screen.getByTestId('model-section')).toHaveTextContent(XSS)
    expect(screen.getByTestId('report-hypothesis')).toHaveTextContent(XSS)
  })

  it('says nothing failed when the findings are empty', () => {
    render(<ReviewReport report={templatedReport({ findings: [] })} database="orders" />)
    expect(screen.getByText('No check failed or warned in this grade.')).toBeInTheDocument()
  })

  it('has no plan link without a plan', () => {
    render(<ReviewReport report={templatedReport()} database="orders" />)
    expect(screen.queryByRole('link', { name: 'Review the plan' })).toBeNull()
  })

  it('renders nothing with no report', () => {
    const { container } = render(<ReviewReport report={null} database="orders" />)
    expect(container).toBeEmptyDOMElement()
  })
})
