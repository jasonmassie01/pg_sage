import { render, screen, waitFor, within } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { PlansTab } from './PlansTab'
import { ACTION_TABS, actionTabParam } from './actionFocus'
import { jsonResponse, proposedPlan } from './planTestData'

// ux-03: the Plans tab lists plans and opens one from ?plan=<id>.

function stub(body, status = 200) {
  const fetch = vi.fn(async () => jsonResponse(body, status))
  vi.stubGlobal('fetch', fetch)
  return fetch
}

afterEach(() => {
  vi.unstubAllGlobals()
  window.location.hash = ''
})

describe('PlansTab', () => {
  it('is a known Actions tab, so tab=plans opens it', () => {
    expect(ACTION_TABS[0]).toBe('plans')
    expect(actionTabParam('#/actions?tab=plans&plan=7')).toBe('plans')
  })

  it('lists plans with their status and a link to each', async () => {
    const fetch = stub({ plans: [
      { database: 'orders', id: 7, goal: 'cut blocks per call', status: 'proposed',
        created_at: '2026-11-16T09:30:00Z', steps: null },
      { database: 'billing', id: 3, goal: 'drop the redundant index', status: 'halted',
        created_at: '2026-11-15T09:30:00Z', steps: null }], total: 2, errors: [] })
    render(<PlansTab database="all" canReview />)
    const list = await screen.findByTestId('plans-list')
    const links = within(list).getAllByRole('link')
    expect(links.map(a => a.getAttribute('href'))).toEqual([
      '#/actions?tab=plans&plan=7&database=orders',
      '#/actions?tab=plans&plan=3&database=billing'])
    expect(list).toHaveTextContent('Awaiting approval')
    expect(list).toHaveTextContent('Halted')
    expect(fetch.mock.calls[0][0]).toBe('/api/v1/plans')
  })

  it('scopes the list to the selected database', async () => {
    const fetch = stub({ plans: [], total: 0, errors: [] })
    render(<PlansTab database="orders" canReview />)
    expect(await screen.findByText('No plans yet.')).toBeInTheDocument()
    expect(fetch.mock.calls[0][0]).toBe('/api/v1/plans?database=orders')
  })

  it('names a database whose plans could not be read', async () => {
    stub({ plans: [], total: 0, errors: [{ database: 'billing', error: 'timeout' }] })
    render(<PlansTab database="all" canReview />)
    expect(await screen.findByTestId('plans-errors'))
      .toHaveTextContent('Plans could not be read from billing')
  })

  it('opens the plan named by ?plan= with a way back to the list', async () => {
    window.location.hash = '#/actions?tab=plans&plan=7&database=orders'
    const fetch = stub(proposedPlan())
    render(<PlansTab database="all" canReview />)
    expect(await screen.findByTestId('plan-header'))
      .toHaveTextContent('cut blocks per call on the order lookups')
    expect(screen.getByRole('link', { name: 'All plans' }))
      .toHaveAttribute('href', '#/actions?tab=plans')
    await waitFor(() => expect(fetch.mock.calls[0][0]).toBe('/api/v1/plans/7?database=orders'))
  })
})
