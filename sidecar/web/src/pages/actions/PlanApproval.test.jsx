import { act, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { PlanApproval } from './PlanApproval'
import { PLAN_HASH, coverageFor, jsonResponse, planStep, proposedPlan } from './planTestData'

// ux-03: the plan's approval controls call plan-03a's
// POST /api/v1/plans/{id}/approve {plan_hash, mode, up_to} with the hash
// the person was shown.

const approved = { ok: true, database: 'orders', plan_id: 7, status: 'approved' }

function mount(detail = proposedPlan(), extra = {}) {
  const onDecided = vi.fn()
  const onChanged = vi.fn()
  render(<PlanApproval detail={detail} database="orders" onDecided={onDecided}
    onChanged={onChanged} canReview {...extra} />)
  return { onDecided, onChanged, box: screen.getByTestId('plan-approval') }
}

function stubFetch(...answers) {
  const fetch = vi.fn()
  for (const a of answers) fetch.mockImplementationOnce(() => Promise.resolve(a))
  vi.stubGlobal('fetch', fetch)
  return fetch
}

afterEach(() => { vi.unstubAllGlobals() })

describe('PlanApproval', () => {
  it('Approve all sends the plan hash with mode all', async () => {
    const fetch = stubFetch(jsonResponse(approved))
    const { box, onDecided } = mount()
    await act(async () => {
      fireEvent.click(within(box).getByRole('button', { name: 'Approve all 3 stages' }))
    })
    await waitFor(() => expect(onDecided).toHaveBeenCalledTimes(1))
    const [url, init] = fetch.mock.calls[0]
    expect(url).toBe('/api/v1/plans/7/approve?database=orders')
    expect(init.method).toBe('POST')
    expect(JSON.parse(init.body)).toEqual({ plan_hash: PLAN_HASH, mode: 'all' })
  })

  it('Approve up to sends the chosen stage', async () => {
    const fetch = stubFetch(jsonResponse(approved))
    const { box, onDecided } = mount()
    const select = within(box).getByLabelText('Approve up to stage')
    expect([...select.options].map(o => o.value)).toEqual(['1', '2'])
    fireEvent.change(select, { target: { value: '2' } })
    await act(async () => {
      fireEvent.click(within(box).getByRole('button', { name: 'Approve up to stage 2' }))
    })
    await waitFor(() => expect(onDecided).toHaveBeenCalled())
    expect(JSON.parse(fetch.mock.calls[0][1].body))
      .toEqual({ plan_hash: PLAN_HASH, mode: 'up_to', up_to: 2 })
  })

  it('Step by step sends mode step', async () => {
    const fetch = stubFetch(jsonResponse(approved))
    const { box, onDecided } = mount()
    await act(async () => {
      fireEvent.click(within(box).getByRole('button', { name: 'Approve step by step' }))
    })
    await waitFor(() => expect(onDecided).toHaveBeenCalled())
    expect(JSON.parse(fetch.mock.calls[0][1].body)).toEqual({ plan_hash: PLAN_HASH,
      mode: 'step' })
  })

  it('does not offer up to a stage past a never-automatic step', () => {
    const steps = [1, 2, 3, 4].map(n => planStep(n * 10, n, `i${n}`))
    const detail = proposedPlan({ steps,
      coverage: coverageFor(steps, { 2: { needs_own_approval: true,
        reason: 'drops an object' } }) })
    const { box } = mount(detail)
    const select = within(box).getByLabelText('Approve up to stage')
    expect([...select.options].map(o => o.value)).toEqual(['1'])
    expect(box).toHaveTextContent('Stage 2 always needs its own approval: drops an object')
  })

  it('offers no up-to choice when stage 1 is never-automatic', () => {
    const steps = [1, 2].map(n => planStep(n * 10, n, `i${n}`))
    const detail = proposedPlan({ steps,
      coverage: coverageFor(steps, { 1: { needs_own_approval: true, reason: 'x' } }) })
    const { box } = mount(detail)
    expect(within(box).queryByLabelText('Approve up to stage')).toBeNull()
    expect(within(box).getByRole('button', { name: 'Approve all 2 stages' })).toBeEnabled()
  })

  it('shows no approve buttons for a plan with zero stages', () => {
    mount(proposedPlan({ steps: [], coverage: [] }))
    const box = screen.getByTestId('plan-approval')
    expect(within(box).queryAllByRole('button')).toHaveLength(0)
    expect(box).toHaveTextContent('This plan has no steps')
  })

  it('shows no controls for a plan that is not awaiting approval', () => {
    const detail = proposedPlan({ plan: { status: 'running' } })
    render(<PlanApproval detail={detail} database="orders" onDecided={() => {}}
      onChanged={() => {}} />)
    expect(screen.queryByTestId('plan-approval')).toBeNull()
  })

  it('a 409 content_changed says the approval was voided and reloads the plan', async () => {
    stubFetch(jsonResponse({ error: 'the plan changed after it was shown; review it again',
      code: 'content_changed' }, 409))
    const { box, onChanged, onDecided } = mount()
    await act(async () => {
      fireEvent.click(within(box).getByRole('button', { name: 'Approve all 3 stages' }))
    })
    expect(await within(box).findByRole('alert'))
      .toHaveTextContent('This plan changed after approval; approval voided')
    expect(onChanged).toHaveBeenCalledTimes(1)
    expect(onDecided).not.toHaveBeenCalled()
  })

  it('a 500 keeps the controls enabled and shows the error', async () => {
    stubFetch(jsonResponse({ error: 'could not approve the plan' }, 500))
    const { box, onChanged, onDecided } = mount()
    const all = within(box).getByRole('button', { name: 'Approve all 3 stages' })
    await act(async () => { fireEvent.click(all) })
    expect(await within(box).findByRole('alert'))
      .toHaveTextContent('could not approve the plan (500)')
    expect(all).toBeEnabled()
    expect(within(box).getByRole('button', { name: 'Approve step by step' })).toBeEnabled()
    expect(onChanged).not.toHaveBeenCalled()
    expect(onDecided).not.toHaveBeenCalled()
  })

  it('a network failure keeps the controls enabled with the error', async () => {
    vi.stubGlobal('fetch', vi.fn().mockRejectedValue(new TypeError('Failed to fetch')))
    const { box } = mount()
    const all = within(box).getByRole('button', { name: 'Approve all 3 stages' })
    await act(async () => { fireEvent.click(all) })
    expect(await within(box).findByRole('alert')).toHaveTextContent('Failed to fetch')
    expect(all).toBeEnabled()
  })

  it('approve clicked twice sends one request', async () => {
    let release
    const fetch = vi.fn(() => new Promise(r => { release = r }))
    vi.stubGlobal('fetch', fetch)
    const { box, onDecided } = mount()
    const all = within(box).getByRole('button', { name: 'Approve all 3 stages' })
    fireEvent.click(all)
    fireEvent.click(all)
    fireEvent.click(within(box).getByRole('button', { name: 'Approve step by step' }))
    expect(fetch).toHaveBeenCalledTimes(1)
    expect(all).toBeDisabled()
    expect(all).toHaveAttribute('aria-busy', 'true')
    await act(async () => { release(jsonResponse(approved)) })
    await waitFor(() => expect(onDecided).toHaveBeenCalledTimes(1))
    expect(fetch).toHaveBeenCalledTimes(1)
  })

  it('a viewer sees why it cannot approve, and no buttons', () => {
    const { box } = mount(proposedPlan(), { canReview: false })
    expect(within(box).queryAllByRole('button')).toHaveLength(0)
    expect(box).toHaveTextContent('Operators and admins approve plans')
  })
})
