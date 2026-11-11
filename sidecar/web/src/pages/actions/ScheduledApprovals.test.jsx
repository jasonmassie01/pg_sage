import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { ScheduledApprovals } from './ScheduledApprovals'
import { scheduledLabel } from './scheduledLabel'
import { approvalOutcomeMessage } from './ApprovalCard'

// policy-04: an approval outside the maintenance window is scheduled for
// the next opening; the Actions page shows "Scheduled for <time>" with a
// Cancel that returns the item to pending.

const row = {
  id: 41, database_name: 'orders', status: 'scheduled', action_risk: 'moderate',
  proposed_sql: 'CREATE INDEX CONCURRENTLY i ON public.t (c)',
  run_after: '2026-11-17T07:00:00Z', window_closes_at: '2026-11-17T11:00:00Z',
  scheduled_by: 2,
}

afterEach(() => { vi.restoreAllMocks() })

describe('ScheduledApprovals', () => {
  it('shows when each approval will run, with Cancel', () => {
    render(<ScheduledApprovals rows={[row]} onChanged={() => {}} />)
    const el = screen.getByTestId('scheduled-approvals')
    expect(el).toHaveTextContent(/Scheduled for/)
    expect(el).toHaveTextContent('CREATE INDEX CONCURRENTLY i ON public.t (c)')
    expect(el).toHaveTextContent('orders')
    expect(screen.getByRole('button', { name: /Cancel schedule of action 41/ })).toBeEnabled()
  })

  it('cancels through the unschedule route and refreshes', async () => {
    const fetchMock = vi.spyOn(globalThis, 'fetch').mockResolvedValue(
      new Response(JSON.stringify({ ok: true, status: 'pending' }), { status: 200 }))
    const onChanged = vi.fn()
    render(<ScheduledApprovals rows={[row]} onChanged={onChanged} />)
    fireEvent.click(screen.getByRole('button', { name: /Cancel schedule of action 41/ }))
    await waitFor(() => expect(onChanged).toHaveBeenCalled())
    expect(fetchMock).toHaveBeenCalledWith('/api/v1/actions/41/unschedule?database=orders',
      expect.objectContaining({ method: 'POST', credentials: 'include' }))
  })

  it('reports a failed cancel and keeps the row', async () => {
    vi.spyOn(globalThis, 'fetch').mockResolvedValue(
      new Response(JSON.stringify({ error: 'action is not scheduled' }), { status: 409 }))
    const onChanged = vi.fn()
    render(<ScheduledApprovals rows={[row]} onChanged={onChanged} />)
    fireEvent.click(screen.getByRole('button', { name: /Cancel schedule of action 41/ }))
    expect(await screen.findByRole('alert')).toHaveTextContent('action is not scheduled')
    expect(onChanged).not.toHaveBeenCalled()
    expect(screen.getByTestId('scheduled-approvals')).toHaveTextContent(/Scheduled for/)
  })

  it('renders nothing without scheduled approvals', () => {
    const { container } = render(<ScheduledApprovals rows={[]} onChanged={() => {}} />)
    expect(container).toBeEmptyDOMElement()
    const missing = render(<ScheduledApprovals onChanged={() => {}} />)
    expect(missing.container).toBeEmptyDOMElement()
  })

  it('labels an unknown run time honestly', () => {
    expect(scheduledLabel({})).toBe('the next maintenance window')
    expect(scheduledLabel({ run_after: 'not a date' })).toBe('the next maintenance window')
    expect(scheduledLabel(row)).not.toBe('the next maintenance window')
  })
})

describe('approvalOutcomeMessage for a scheduled approval', () => {
  it('says the approval waits for its window', () => {
    const got = approvalOutcomeMessage(200, { ok: true, status: 'scheduled', executed: false,
      run_after_label: 'Tue 2026-11-17 01:00 America/Chicago' })
    expect(got.ok).toBe(true)
    expect(got.text).toMatch(/Scheduled for the window opening Tue 2026-11-17 01:00 America\/Chicago/)
  })
})
