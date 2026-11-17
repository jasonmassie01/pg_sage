import { act, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { PlanView } from './PlanView'
import { PLAN_HASH, jsonResponse, planStep, proposedPlan, withStates } from './planTestData'

// ux-03: the plan detail view at #/actions?tab=plans&plan=<id>. GETs are
// served from a list of answers per route (the last repeats), so each
// poll can move the plan on; POSTs are recorded. The plan stays on its
// first answer until a decision was posted (unless answers.always), so a
// poll cannot move it before the click.

function route(answers) {
  const calls = { posts: [], gets: [] }
  const next = { plan: 0, outcomes: 0 }
  const pick = (key, list) => {
    if (key === 'plan' && !answers.always && calls.posts.length === 0) return list[0]
    if (key === 'plan' && !answers.always && next.plan === 0) next.plan = 1
    return list[Math.min(next[key]++, list.length - 1)]
  }
  vi.stubGlobal('fetch', vi.fn(async (url, init = {}) => {
    if (init.method === 'POST') {
      calls.posts.push({ url, body: JSON.parse(init.body) })
      return jsonResponse(answers.post || { ok: true, status: 'approved' })
    }
    calls.gets.push(url)
    if (url.startsWith('/api/v1/outcomes')) {
      const got = pick('outcomes', answers.outcomes || [{ rows: [] }])
      return got instanceof Response ? got.clone() : jsonResponse(got)
    }
    const got = pick('plan', answers.plan)
    return got instanceof Response ? got : jsonResponse(got)
  }))
  return calls
}

function mount(props = {}) {
  render(<PlanView id="7" database="orders" canReview pollMs={20} {...props} />)
}

afterEach(() => { vi.unstubAllGlobals() })

describe('PlanView', () => {
  it('renders the header and the ordered stages', async () => {
    const steps = [planStep(72, 3, 'idx_c'), planStep(70, 1, 'idx_a', { expected_pct: 30 }),
      planStep(71, 2, 'idx_b', { expected_pct: 10 })]
    route({ plan: [proposedPlan({ steps, plan: { origin: 'model', model: 'gpt-x',
      origin_ref: 'gen-run-5', pack_hash: 'sha256:abc123' } })] })
    mount()
    const head = await screen.findByTestId('plan-header')
    expect(head).toHaveTextContent('cut blocks per call on the order lookups')
    expect(head).toHaveTextContent('Scope: public.orders · query 101')
    expect(head).toHaveTextContent('Expected: stage 1 blocks per call down 30%; '
      + 'stage 2 blocks per call down 10%; stage 3 blocks per call down (size not estimated)')
    expect(head).toHaveTextContent('Risk: every stage can run under one approval')
    expect(head).toHaveTextContent('Origin: a model (gpt-x), gen-run-5, by alice')
    expect(head).toHaveTextContent('Evidence pack sha256:abc123')
    expect(head).toHaveTextContent('Awaiting approval')
    const stages = within(screen.getByTestId('plan-stages')).getAllByTestId(/^plan-stage-\d+$/)
    expect(stages.map(s => s.dataset.testid)).toEqual(
      ['plan-stage-1', 'plan-stage-2', 'plan-stage-3'])
  })

  it('Approve all sends the hash, then the stages move to running and verified', async () => {
    const base = proposedPlan()
    const approved = withStates(base, 'approved', {})
    const running = withStates(base, 'running', { 1: { state: 'running' } })
    const verified = withStates(base, 'completed', {
      1: { state: 'kept', verdict: 'improved' }, 2: { state: 'kept', verdict: 'improved' },
      3: { state: 'kept', verdict: 'improved' } })
    const calls = route({ plan: [base, approved, running, verified] })
    mount()
    const all = await screen.findByRole('button', { name: 'Approve all 3 stages' })
    await act(async () => { fireEvent.click(all) })
    expect(calls.posts).toEqual([{ url: '/api/v1/plans/7/approve?database=orders',
      body: { plan_hash: PLAN_HASH, mode: 'all' } }])
    // The plan moves on each 20 ms poll; a loaded machine needs more than
    // waitFor's default second to render the later answers.
    await waitFor(() => expect(screen.getByTestId('plan-stage-status-1'))
      .toHaveTextContent('Running'), { timeout: 3000 })
    await waitFor(() => expect(screen.getByTestId('plan-stage-status-3'))
      .toHaveTextContent('Verified: kept'), { timeout: 3000 })
    expect(screen.getByTestId('plan-header')).toHaveTextContent('Completed')
    expect(screen.queryByTestId('plan-approval')).toBeNull()
    expect(calls.gets[0]).toBe('/api/v1/plans/7?database=orders')
  })

  it('shows This plan has no steps for a plan with zero stages', async () => {
    route({ plan: [proposedPlan({ steps: [], coverage: [] })] })
    mount()
    expect(await screen.findByText('This plan has no steps')).toBeInTheDocument()
    expect(screen.queryAllByRole('button', { name: /Approve/ })).toHaveLength(0)
  })

  it('shows the measured effect from the outcome ledger row of each stage', async () => {
    const done = withStates(proposedPlan(), 'running', {
      1: { state: 'kept', verdict: 'improved', action_log_id: 501 },
      2: { state: 'executed', action_log_id: 502 } })
    const calls = route({ plan: [done], outcomes: [{ rows: [
      { action_id: 501, verdict: 'improved', tier: 'canary',
        measured: { metric: 'blocks_per_call', change_pct: -42 } },
      { action_id: 999, verdict: 'regressed', measured: { metric: 'x', change_pct: 5 } },
    ] }] })
    mount()
    const one = await screen.findByTestId('plan-stage-1')
    await waitFor(() => expect(one).toHaveTextContent('Measured: blocks per call -42%'))
    expect(one).toHaveTextContent('Tested in production')
    expect(screen.getByTestId('plan-stage-2')).toHaveTextContent('Measured: not measured yet')
    expect(calls.gets.find(u => u.startsWith('/api/v1/outcomes')))
      .toMatch(/^\/api\/v1\/outcomes\?database=orders&since=\d+h&limit=500$/)
  })

  it('says the effect of a stage that ran is unreadable when the ledger fails', async () => {
    const done = withStates(proposedPlan(), 'running', {
      1: { state: 'kept', verdict: 'improved', action_log_id: 501 } })
    route({ plan: [done], outcomes: [jsonResponse({ error: 'ledger down' }, 500)] })
    mount()
    const one = await screen.findByTestId('plan-stage-1')
    await waitFor(() => expect(one).toHaveTextContent('Measured: could not be read'),
      { timeout: 3000 })
    expect(one).not.toHaveTextContent('not measured yet')
    expect(screen.getByTestId('plan-stage-2')).toHaveTextContent('Measured: not measured yet')
  })

  it('reads no ledger while no stage has run', async () => {
    const calls = route({ plan: [proposedPlan()] })
    mount()
    await screen.findByTestId('plan-stage-1')
    expect(calls.gets.some(u => u.startsWith('/api/v1/outcomes'))).toBe(false)
  })

  it('says the approval was voided when the plan changed after approval', async () => {
    route({ plan: [proposedPlan({ approvals: [{ id: 41, plan_id: 7, mode: 'all',
      plan_hash: 'old-hash', decided_at: '2026-11-16T09:00:00Z',
      voided_at: '2026-11-16T09:20:00Z', void_reason: 'plan edited' }] })] })
    mount()
    expect(await screen.findByTestId('plan-voided'))
      .toHaveTextContent('This plan changed after approval; approval voided')
  })

  it('a 30-stage plan scrolls inside the view with stable stage numbers', async () => {
    const steps = Array.from({ length: 30 }, (_, i) => planStep(100 + i, i + 1, `i${i + 1}`))
    route({ always: true,
      plan: [proposedPlan({ steps }), proposedPlan({ steps: [...steps].reverse() })] })
    mount()
    const list = await screen.findByTestId('plan-stages')
    expect(list.style.overflowY).toBe('auto')
    expect(list.style.maxHeight).not.toBe('')
    const numbers = () => within(list).getAllByTestId(/^plan-stage-\d+$/)
      .map(s => s.dataset.testid)
    const want = steps.map(s => `plan-stage-${s.ordinal}`)
    expect(numbers()).toEqual(want)
    await new Promise(r => setTimeout(r, 80))
    expect(numbers()).toEqual(want)
  })

  it('goes proposed, approved, running, then halted on stage 3 of 5', async () => {
    const steps = [1, 2, 3, 4, 5].map(n => planStep(n * 10, n, `i${n}`))
    const base = proposedPlan({ steps })
    const halted = withStates(base, 'halted', {
      1: { state: 'kept', verdict: 'improved' }, 2: { state: 'kept', verdict: 'no_change' },
      3: { state: 'reverted', verdict: 'regressed',
        state_reason: 'blocks per call rose 40%' } })
    route({ plan: [base, withStates(base, 'approved', {}),
      withStates(base, 'running', { 1: { state: 'running' } }), halted] })
    mount()
    const button = await screen.findByRole('button', { name: 'Approve all 5 stages' })
    await act(async () => { fireEvent.click(button) })
    const halt = await screen.findByTestId('plan-halted', {}, { timeout: 2000 })
    expect(halt).toHaveTextContent('Halted at stage 3: blocks per call rose 40%')
    expect(halt).toHaveTextContent('Reverted: stage 3')
    expect(halt).toHaveTextContent('Kept: stages 1, 2')
    expect(halt).toHaveTextContent('Never started: stages 4, 5')
    expect(screen.getByTestId('plan-stage-status-4')).toHaveTextContent('Not started')
  })

  it('shows the read error with a retry, and no controls', async () => {
    route({ plan: [jsonResponse({ error: 'plan not found' }, 404)] })
    mount()
    expect(await screen.findByText(/plan not found/)).toBeInTheDocument()
    expect(screen.queryByTestId('plan-approval')).toBeNull()
  })

  it('a step-by-step plan offers each uncovered stage its own approval', async () => {
    const base = proposedPlan()
    const stepMode = { ...withStates(base, 'approved', {}),
      approvals: [{ id: 42, plan_id: 7, mode: 'step', plan_hash: PLAN_HASH }],
      coverage: base.coverage.map(c => ({ ...c,
        reason: 'the plan is approved step by step: approve this step to run it' })) }
    const calls = route({ plan: [stepMode] })
    mount()
    const button = await screen.findByRole('button', { name: 'Approve stage 2' })
    await act(async () => { fireEvent.click(button) })
    expect(calls.posts).toEqual([{ url: '/api/v1/plans/7/steps/71/approve?database=orders',
      body: { plan_hash: PLAN_HASH } }])
  })
})
