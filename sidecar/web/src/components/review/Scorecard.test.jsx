import { render, screen, within } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { Scorecard } from './Scorecard'
import { ReviewScorecard } from './ReviewScorecard'
import { useReviewScorecard } from '../../hooks/useReviewScorecard'
import golden from '../../../../internal/api/testdata/review_scorecard.golden.json'
  with { type: 'json' }

// review-05b: the scorecard by origin and the model gate card, on the
// Review page and, compact, on the Value page. The payload is review-05a's
// handler golden. No concurrent access tests: a pure render of one payload.

const api = vi.hoisted(() => ({ useAPI: vi.fn() }))
vi.mock('../../hooks/useAPI', () => ({ useAPI: (...args) => api.useAPI(...args) }))

const admin = { id: 1, role: 'admin' }
const viewer = { id: 2, role: 'viewer' }

function counts(extra = {}) {
  return { proposed: 0, accepted: 0, verified_improved: 0, no_change: 0, inconclusive: 0,
    regressed: 0, reverted: 0, unverifiable: 0, pending: 0, decided: 0, ...extra }
}

function payload(extra = {}) {
  const base = structuredClone(golden)
  base.gate = base.gate.map((row, i) => ({ ...row, id: 7 + i }))
  return { ...base, ...extra }
}

function gm1(extra = {}) {
  return { ...payload().gate.find(r => r.rule === 'G-M1'), ...extra }
}

function withGate(row) {
  return payload({ gate: [row] })
}

function rowOf(origin) {
  return screen.getByTestId(`scorecard-row-${origin}`)
}

describe('Scorecard', () => {
  it('renders the counts by origin with their denominators and the gate card', () => {
    render(<Scorecard data={payload()} user={viewer} />)
    const model = rowOf('model')
    expect(within(model).getByText('Model')).toBeInTheDocument()
    expect(within(model).getByTestId('cell-proposed')).toHaveTextContent('50')
    expect(within(model).getByTestId('cell-accepted')).toHaveTextContent('50 of 50')
    expect(within(model).getByTestId('cell-improved')).toHaveTextContent('45 of 50')
    expect(within(model).getByTestId('cell-improved')).toHaveTextContent('90%')
    expect(within(model).getByTestId('cell-no_change')).toHaveTextContent('5')
    expect(within(model).getByTestId('cell-reverted')).toHaveTextContent('0 of 50')
    expect(within(model).getByTestId('cell-reverted')).toHaveTextContent('0%')
    expect(within(rowOf('generator')).getByTestId('cell-improved'))
      .toHaveTextContent('50 of 50100%')
    expect(screen.getByTestId('scorecard-reviews')).toHaveTextContent('50 reviews')
    expect(screen.getByTestId('gate-line')).toHaveTextContent(
      'Model gate: 50 of 50 reviews; model 90% improved vs generators 100%; reverts 0%')
    expect(screen.getByTestId('gate-verdict'))
      .toHaveTextContent("Waiting for an admin's approval")
    expect(screen.getByTestId('gate-m2')).toHaveTextContent('the model pass stays on')
  })

  it('shows the gate line of the plan entry, rounded to whole percent', () => {
    render(<Scorecard data={withGate(gm1({ evidence: {
      reviews: { k: 32, n: 50 }, model_improved: { k: 39, n: 50 },
      generator_improved: { k: 42, n: 50 }, model_reverts: { k: 0, n: 50 },
    } }))} user={viewer} />)
    expect(screen.getByTestId('gate-line')).toHaveTextContent(
      'Model gate: 32 of 50 reviews; model 78% improved vs generators 84%; reverts 0%')
  })

  it('a rate above 100% or a negative count renders invalid, never the number', () => {
    const data = payload()
    data.scorecard.by_origin.model = counts({ proposed: 10, accepted: -1,
      verified_improved: 12, decided: 10, reverted: 0, no_change: 2.5 })
    data.gate = [gm1({ evidence: { reviews: { k: 50, n: 50 },
      model_improved: { k: 60, n: 50 }, generator_improved: { k: 40, n: 50 },
      model_reverts: { k: -1, n: 50 } } })]
    render(<Scorecard data={data} user={viewer} />)
    const model = rowOf('model')
    expect(within(model).getByTestId('cell-accepted')).toHaveTextContent('invalid')
    expect(within(model).getByTestId('cell-improved')).toHaveTextContent('invalid')
    expect(within(model).getByTestId('cell-improved')).not.toHaveTextContent('120')
    expect(within(model).getByTestId('cell-improved')).not.toHaveTextContent('12 of')
    expect(within(model).getByTestId('cell-no_change')).toHaveTextContent('invalid')
    expect(within(model).getByTestId('cell-reverted')).toHaveTextContent('0 of 10')
    const line = screen.getByTestId('gate-line')
    expect(line).toHaveTextContent('model improved: invalid')
    expect(line).toHaveTextContent('reverts: invalid')
    expect(line).not.toHaveTextContent('120%')
    expect(line).toHaveTextContent('generators 80%')
  })

  it('an invalid review count reads invalid, not "No reviews yet"', () => {
    const data = payload()
    data.scorecard.reviews = -3
    render(<Scorecard data={data} user={viewer} />)
    expect(screen.getByTestId('scorecard-reviews')).toHaveTextContent('invalid')
    expect(screen.queryByText('No reviews yet')).toBeNull()
  })

  it('no reviews shows "No reviews yet" and the gate 0 of 50 reviews', () => {
    render(<Scorecard data={{ scorecard: { database: 'orders', reviews: 0, by_origin: {},
      by_generator: {}, per_review: [] }, gate: [],
    thresholds: { min_reviews: 50, max_gap_points: 10, max_revert_pct: 2 } }}
    user={admin} />)
    expect(screen.getByText('No reviews yet')).toBeInTheDocument()
    expect(screen.queryByRole('table')).toBeNull()
    expect(screen.getByTestId('gate-line'))
      .toHaveTextContent('Model gate: 0 of 50 reviews; not evaluated yet')
    expect(screen.queryByRole('link', { name: /approve/i })).toBeNull()
  })

  it('a null or empty payload still says no reviews and 0 of the minimum', () => {
    const { rerender } = render(<Scorecard data={null} user={viewer} />)
    expect(screen.getByText('No reviews yet')).toBeInTheDocument()
    expect(screen.getByTestId('gate-line'))
      .toHaveTextContent('Model gate: 0 of invalid reviews; not evaluated yet')
    rerender(<Scorecard data={{}} user={viewer} />)
    expect(screen.getByText('No reviews yet')).toBeInTheDocument()
  })

  it('a gate row without its counts shows its reason', () => {
    render(<Scorecard data={withGate(gm1({ evidence: {},
      reason: 'G-M1: the scorecard could not be read' }))} user={viewer} />)
    expect(screen.getByTestId('gate-line'))
      .toHaveTextContent('Model gate: G-M1: the scorecard could not be read')
  })

  it('a denominator of 9 is too few to judge and 10 shows the rate', () => {
    const data = payload()
    data.scorecard.by_origin = {
      model: counts({ proposed: 9, accepted: 9, verified_improved: 8, decided: 9 }),
      generator: counts({ proposed: 10, accepted: 10, verified_improved: 8, decided: 10 }),
    }
    data.gate = [gm1({ evidence: { reviews: { k: 9, n: 50 },
      model_improved: { k: 8, n: 9 }, generator_improved: { k: 8, n: 10 },
      model_reverts: { k: 0, n: 9 } } })]
    render(<Scorecard data={data} user={viewer} />)
    const model = within(rowOf('model')).getByTestId('cell-improved')
    expect(model).toHaveTextContent('8 of 9')
    expect(model).toHaveTextContent('too few to judge')
    expect(model).not.toHaveTextContent('%')
    const gen = within(rowOf('generator')).getByTestId('cell-improved')
    expect(gen).toHaveTextContent('8 of 10')
    expect(gen).toHaveTextContent('80%')
    expect(screen.getByTestId('gate-line')).toHaveTextContent(
      'Model gate: 9 of 50 reviews; model improved: too few to judge vs generators 80%; '
      + 'reverts: too few to judge')
  })

  it('the gate card follows not_eligible, pending_approval, granted with no stale text', () => {
    const rows = {
      not_eligible: gm1({ verdict: 'not_eligible', reason: 'G-M1: 12 of 50 reviews' }),
      pending_approval: gm1({ verdict: 'pending_approval' }),
      granted: gm1({ verdict: 'granted', approved_by: 'dba@example.com',
        approved_at: '2026-11-16T10:00:00Z' }),
    }
    const { rerender } = render(<Scorecard data={withGate(rows.not_eligible)} user={admin} />)
    expect(screen.getByTestId('gate-verdict')).toHaveTextContent('Not eligible')
    expect(screen.getByTestId('gate-reason')).toHaveTextContent('G-M1: 12 of 50 reviews')
    expect(screen.queryByRole('link', { name: /approve/i })).toBeNull()

    rerender(<Scorecard data={withGate(rows.pending_approval)} user={admin} />)
    expect(screen.getByTestId('gate-verdict'))
      .toHaveTextContent("Waiting for an admin's approval")
    expect(screen.queryByText(/Not eligible/)).toBeNull()
    expect(screen.queryByText('G-M1: 12 of 50 reviews')).toBeNull()
    const link = screen.getByRole('link', { name: 'Approve in the Inbox' })
    expect(link).toHaveAttribute('href', '#/')

    rerender(<Scorecard data={withGate(rows.granted)} user={admin} />)
    expect(screen.getByTestId('gate-verdict')).toHaveTextContent('Granted by an admin')
    expect(screen.getByTestId('gate-verdict')).toHaveTextContent('dba@example.com')
    expect(screen.queryByText(/Waiting for an admin/)).toBeNull()
    expect(screen.queryByRole('link', { name: /approve/i })).toBeNull()
  })

  it('only an admin sees the approval link', () => {
    const data = withGate(gm1({ verdict: 'pending_approval' }))
    for (const user of [viewer, { id: 3, role: 'operator' }, null]) {
      const { unmount } = render(<Scorecard data={data} user={user} />)
      expect(screen.queryByRole('link', { name: /approve/i })).toBeNull()
      expect(screen.getByTestId('gate-verdict'))
        .toHaveTextContent("Waiting for an admin's approval")
      unmount()
    }
  })

  it('an eligible row is named and offers no approval until it is filed', () => {
    const { rerender } = render(<Scorecard data={withGate(gm1({ verdict: 'eligible' }))}
      user={admin} />)
    expect(screen.getByTestId('gate-verdict'))
      .toHaveTextContent('Eligible, not yet filed for approval')
    expect(screen.queryByText(/Unknown verdict/)).toBeNull()
    expect(screen.queryByRole('link', { name: /approve/i })).toBeNull()
    rerender(<Scorecard data={withGate(gm1({ verdict: 'pending_approval' }))} user={admin} />)
    expect(screen.queryByText(/Eligible, not yet filed/)).toBeNull()
    expect(screen.getByRole('link', { name: 'Approve in the Inbox' })).toBeInTheDocument()
  })

  it('an unknown verdict is named, not shown as a known state', () => {
    render(<Scorecard data={withGate(gm1({ verdict: 'promoted' }))} user={admin} />)
    expect(screen.getByTestId('gate-verdict')).toHaveTextContent('Unknown verdict (promoted)')
    expect(screen.queryByRole('link', { name: /approve/i })).toBeNull()
  })

  it('compact shows the gate card and improved and reverted only', () => {
    render(<Scorecard data={payload()} user={viewer} compact database="orders db" />)
    const model = rowOf('model')
    expect(within(model).getByTestId('cell-improved')).toHaveTextContent('45 of 50')
    expect(within(model).queryByTestId('cell-accepted')).toBeNull()
    expect(within(model).queryByTestId('cell-no_change')).toBeNull()
    expect(screen.getByTestId('gate-line')).toHaveTextContent('Model gate: 50 of 50 reviews')
    expect(screen.getByRole('link', { name: 'Open the review' }))
      .toHaveAttribute('href', '#/review?db=orders%20db')
  })

  it('origin text from the server is React text, never HTML', () => {
    const data = payload()
    data.scorecard.by_origin = { '<img src=x onerror=alert(1)>': counts({ proposed: 1 }) }
    const { container } = render(<Scorecard data={data} user={viewer} />)
    expect(container.querySelector('img')).toBeNull()
    expect(screen.getByText('<img src=x onerror=alert(1)>')).toBeInTheDocument()
  })
})

describe('ReviewScorecard', () => {
  let refetch
  beforeEach(() => {
    refetch = vi.fn()
    api.useAPI.mockReset()
  })

  function Probe({ database }) {
    return <ReviewScorecard query={useReviewScorecard(database)} database={database}
      user={viewer} />
  }

  it('reads the database scorecard each minute and renders it', () => {
    api.useAPI.mockReturnValue({ data: payload(), loading: false, error: null, refetch,
      updatedAt: 1 })
    render(<Probe database="orders db" />)
    expect(api.useAPI).toHaveBeenCalledWith(
      '/api/v1/databases/orders%20db/review/scorecard', 60000)
    expect(screen.getByTestId('gate-line')).toHaveTextContent('Model gate: 50 of 50')
  })

  it('a 500 shows the QueryState error with Retry and no table', () => {
    api.useAPI.mockReturnValue({ data: null, loading: false, refetch, errorStatus: 500,
      error: 'the review scorecard of orders could not be read' })
    render(<Probe database="orders" />)
    const section = screen.getByTestId('review-scorecard')
    expect(within(section).getByRole('alert'))
      .toHaveTextContent('the review scorecard of orders could not be read')
    within(section).getByRole('button', { name: 'Retry' }).click()
    expect(refetch).toHaveBeenCalledTimes(1)
    expect(screen.queryByTestId('gate-line')).toBeNull()
  })

  it('without a database it asks for one and reads nothing', () => {
    api.useAPI.mockReturnValue({ data: payload(), loading: false, error: null, refetch })
    for (const database of ['all', null, '']) {
      const { unmount } = render(<Probe database={database} />)
      expect(api.useAPI).toHaveBeenLastCalledWith(null, 60000)
      expect(screen.getByText('Select a database to see its review scorecard.'))
        .toBeInTheDocument()
      expect(screen.queryByTestId('gate-line')).toBeNull()
      unmount()
    }
  })
})
