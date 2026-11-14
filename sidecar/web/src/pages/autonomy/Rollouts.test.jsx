import { render, screen } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { Rollouts } from './Rollouts'

// executor-04 / dashboard-14: a running fleet canary shows each database's
// stored state; a missing state reads "unknown". Finished runs read no
// detail. The browser check is e2e/autonomy-canary.spec.ts.
// No concurrent access tests: a read-only list with one owner.

const run = (state, id = 'rollout-7') => ({
  id, source_database: 'orders', state, applied_instances: 1,
  created_at: '2026-11-13T08:00:00Z',
})

let details
const asked = []
vi.mock('../../hooks/useAPI', () => ({
  useAPI: (url, interval) => {
    asked.push({ url, interval })
    return details[url] || { data: null, loading: true, error: null }
  },
}))

const detailURL = '/api/v1/sre/autonomy/rollouts/rollout-7'

beforeEach(() => {
  details = {}
  asked.length = 0
})

describe('Rollouts', () => {
  it('lists each database state of a running canary', () => {
    details[detailURL] = { data: { instances: [
      { database: 'reporting', ordinal: 1, status: 'awaiting_verdict' },
      { database: 'billing', ordinal: 2, status: '' },
    ] }, error: null }
    render(<Rollouts data={{ items: [run('canary')] }} />)
    expect(screen.getAllByTestId('rollout-instance').map(li => li.textContent))
      .toEqual(['reporting: awaiting_verdict', 'billing: unknown'])
    expect(asked).toContainEqual({ url: detailURL, interval: 15000 })
  })

  it('reads the detail of an expanding run too', () => {
    render(<Rollouts data={{ items: [run('expanding')] }} />)
    expect(asked.map(a => a.url)).toContain(detailURL)
  })

  it('reads no detail for finished runs', () => {
    render(<Rollouts data={{ items: [run('complete'), run('halted', 'r2'),
      run('interrupted', 'r3')] }} />)
    expect(asked).toEqual([])
    expect(screen.getAllByTestId('rollout-run')).toHaveLength(3)
  })

  it('says unknown when no database has reported yet', () => {
    details[detailURL] = { data: { instances: [] }, error: null }
    render(<Rollouts data={{ items: [run('canary')] }} />)
    expect(screen.getByTestId('rollout-instances-unknown'))
      .toHaveTextContent('Databases: unknown')
  })

  it('treats a missing instances field as none reported', () => {
    details[detailURL] = { data: { run: run('canary') }, error: null }
    render(<Rollouts data={{ items: [run('canary')] }} />)
    expect(screen.getByTestId('rollout-instances-unknown')).toBeInTheDocument()
  })

  it('names the error when the detail cannot be read', () => {
    details[detailURL] = { data: null, error: '500: rollout store unavailable' }
    render(<Rollouts data={{ items: [run('canary')] }} />)
    expect(screen.getByTestId('rollout-instances-error'))
      .toHaveTextContent('Per-database state could not be read: 500: rollout store unavailable')
    expect(screen.getByTestId('rollout-run')).toHaveTextContent('orders: canary, 1 applied')
  })

  it('reads a run with no state as unknown', () => {
    render(<Rollouts data={{ items: [run('')] }} />)
    expect(screen.getByTestId('rollout-run')).toHaveTextContent('orders: unknown, 1 applied')
    expect(asked).toEqual([])
  })

  it('shows the empty state for no runs and for no data', () => {
    const { rerender } = render(<Rollouts data={{ items: [] }} />)
    expect(screen.getByText('No canary rollouts.')).toBeInTheDocument()
    rerender(<Rollouts data={null} />)
    expect(screen.getByText('No canary rollouts.')).toBeInTheDocument()
  })
})
