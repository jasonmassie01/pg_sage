import { act, render, screen } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { GitopsBanner } from './GitopsBanner'
import { stubFetch } from '../../test/fetchStub'

// cli-02a: Settings shows the GitOps directory's last reconcile from
// GET /api/v1/gitops: its mode and diff, held and waiting widenings, or its
// error with every problem's file and line. Hidden when the directory is
// off or the user is not an admin. No concurrent access tests: one render,
// single-threaded timers.

const report = (extra = {}) => ({
  configured: true,
  report: {
    directory: '/etc/sage/policy', mode: 'dry_run', hash: 'abc123', applied: false,
    at: '2026-11-18T09:00:00Z',
    changes: [
      { kind: 'principal', name: 'ci-bot', op: 'create', file: 'principals/ci-bot.yaml' },
      { kind: 'policy_set', name: 'prod', op: 'hold', detail: 'retire held: orders' },
    ],
    ...extra,
  },
})

beforeEach(() => { vi.useFakeTimers() })
afterEach(() => {
  vi.useRealTimers()
  vi.unstubAllGlobals()
})

const settle = () => act(async () => { await vi.advanceTimersByTimeAsync(0) })

describe('GitopsBanner', () => {
  it('shows the mode, the directory and every change with its file', async () => {
    stubFetch({ '/api/v1/gitops': { json: report() } })
    render(<GitopsBanner />)
    await settle()
    const banner = screen.getByTestId('gitops-banner')
    expect(banner).toHaveTextContent('/etc/sage/policy')
    expect(banner).toHaveTextContent('dry run: nothing is written')
    expect(screen.getByTestId('gitops-change-0'))
      .toHaveTextContent('create principal ci-bot (principals/ci-bot.yaml)')
    expect(screen.getByTestId('gitops-change-1'))
      .toHaveTextContent('hold policy_set prod: retire held: orders')
  })

  it('shows the error and each problem with its file and line', async () => {
    stubFetch({ '/api/v1/gitops': { json: report({ changes: [], error: 'invalid',
      problems: [{ file: 'grants/g.yaml', line: 4, message: 'database ledger' }] }) } })
    render(<GitopsBanner />)
    await settle()
    expect(screen.getByTestId('gitops-error')).toHaveTextContent('invalid')
    expect(screen.getByTestId('gitops-problem-0'))
      .toHaveTextContent('grants/g.yaml:4: database ledger')
  })

  it('says the rows match when there is nothing to do', async () => {
    stubFetch({ '/api/v1/gitops': { json: report({ mode: 'apply', changes: [] }) } })
    render(<GitopsBanner />)
    await settle()
    expect(screen.getByTestId('gitops-banner')).toHaveTextContent('the rows match the files')
  })

  it('renders nothing when the directory is off or the user is not an admin', async () => {
    stubFetch({ '/api/v1/gitops': [{ json: { configured: false } },
      { status: 403, json: { error: 'admin role required' } }] })
    const { unmount } = render(<GitopsBanner />)
    await settle()
    expect(screen.queryByTestId('gitops-banner')).toBeNull()
    unmount()
    render(<GitopsBanner />)
    await settle()
    expect(screen.queryByTestId('gitops-banner')).toBeNull()
  })
})
