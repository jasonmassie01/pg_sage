import { render, screen, waitFor, within } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { ModelLift } from './ModelLift'

// Roadmap 2.4 (measure the model): "model lift over deterministic, per
// family" from the newest held-out bench measurement: the model's Safe
// Pass against the causal graph's, its override precision (with the
// Wilson lower bound the rule reads), the inconclusive-case lift, and
// whether the model may override the graph's root for the family or its
// roots stay advisory (L1), with the reason.

const lockLift = {
  family: 'lock_blocking', granted: true, status: 'adopt',
  reason: 'override precision 16/16, lower bound 0.81 >= 0.80 (held-out, live model)',
  rule: { eligible: true, lower_bound: 0.8064, threshold: 0.8, min_overrides: 10 },
  lift: {
    arm: 'causal-graph+llm', baseline: 'causal-graph', family: 'lock_blocking',
    split: 'held_out', runs: 40, safe_pass: { k: 38, n: 40 },
    baseline_safe_pass: { k: 30, n: 40 }, top1: { k: 20, n: 22 },
    baseline_top1: { k: 18, n: 22 }, override_precision: { k: 16, n: 16 },
    override_safe_pass: { k: 36, n: 40 }, inconclusive_runs: 6,
    inconclusive_resolved_right: 3, inconclusive_resolved_wrong: 1, forbidden_actions: 0,
  },
  report: { provenance: 'signed release report (pg_sage 1.9.1)', signed: true,
    generated_at: '2026-10-04T06:40:00Z' },
}

const walAdvisory = {
  family: 'wal_retention', granted: false, status: 'advisory',
  reason: 'no held-out live-model measurement of this family in a bench report for this build',
  rule: { eligible: false, lower_bound: null, threshold: 0.8, min_overrides: 10 },
}

function mockFetch(body, ok = true, status = 200) {
  globalThis.fetch = vi.fn(() => Promise.resolve({
    ok, status, json: () => Promise.resolve(body),
  }))
}

afterEach(() => {
  delete globalThis.fetch
})

describe('ModelLift', () => {
  it('shows the lift and the authority of a measured family', async () => {
    mockFetch({ database: 'orders', threshold: 0.8, min_overrides: 10,
      meaning: 'held-out, live model', families: [lockLift, walAdvisory] })
    render(<ModelLift database="all" />)
    const row = await screen.findByTestId('model-lift-lock_blocking')
    expect(globalThis.fetch).toHaveBeenCalledWith('/api/v1/model-lift',
      expect.objectContaining({ credentials: 'include' }))
    expect(row).toHaveTextContent('38/40')
    expect(row).toHaveTextContent('30/40')
    expect(row).toHaveTextContent('+20 pts')
    expect(row).toHaveTextContent('16/16')
    expect(row).toHaveTextContent('lower bound 0.81')
    expect(row).toHaveTextContent('+2')
    expect(row).toHaveTextContent('3 right')
    expect(row).toHaveTextContent('1 wrong')
    expect(within(row).getByTestId('model-lift-authority'))
      .toHaveTextContent(/may override/)
    expect(screen.getByTestId('model-lift')).toHaveTextContent('signed release report')
  })

  it('keeps an unmeasured family advisory with the reason', async () => {
    mockFetch({ database: 'orders', threshold: 0.8, min_overrides: 10, meaning: 'x',
      families: [lockLift, walAdvisory] })
    render(<ModelLift database="all" />)
    const row = await screen.findByTestId('model-lift-wal_retention')
    expect(within(row).getByTestId('model-lift-authority')).toHaveTextContent(/advisory/)
    expect(row).toHaveTextContent('no held-out live-model measurement')
    expect(row).toHaveTextContent('not measured')
  })

  it('says when nothing was measured yet', async () => {
    mockFetch({ database: 'orders', threshold: 0.8, min_overrides: 10, meaning: 'x',
      families: [walAdvisory] })
    render(<ModelLift database="all" />)
    expect(await screen.findByTestId('model-lift-empty')).toHaveTextContent(/advisory/)
  })

  it('asks for the selected database', async () => {
    mockFetch({ database: 'billing', families: [] })
    render(<ModelLift database="billing" />)
    await waitFor(() => expect(globalThis.fetch).toHaveBeenCalledWith(
      '/api/v1/model-lift?database=billing', expect.anything()))
  })

  it('shows an error', async () => {
    mockFetch({ error: 'database not found' }, false, 404)
    render(<ModelLift database="nope" />)
    expect(await screen.findByRole('alert')).toHaveTextContent('database not found')
  })

  it('renders a negative lift with its sign', async () => {
    const worse = { ...lockLift, granted: false, status: 'advisory', reason: 'lower bound',
      lift: { ...lockLift.lift, safe_pass: { k: 25, n: 40 },
        inconclusive_resolved_right: 0, inconclusive_resolved_wrong: 4 } }
    mockFetch({ database: 'orders', families: [worse] })
    render(<ModelLift database="all" />)
    const row = await screen.findByTestId('model-lift-lock_blocking')
    expect(row).toHaveTextContent('-12.5 pts')
    expect(row).toHaveTextContent('-4')
  })
})
