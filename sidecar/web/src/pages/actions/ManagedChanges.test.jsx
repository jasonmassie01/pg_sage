import { fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { ManagedChanges } from './ManagedChanges'

// Managed clouds (roadmap phase 3): a parameter-group or database-flag
// change pg_sage cannot run as SQL is shown as an approval card with the
// exact CLI command, the reboot requirement and the rollback. Approving
// records the decision; pg_sage never applies it.

vi.mock('../../components/Toast', () => ({
  useToast: () => ({ success: vi.fn(), error: vi.fn() }),
}))

const proposal = {
  provider: 'rds', mechanism: 'parameter_group', target: 'orders-pg16',
  parameter: 'shared_buffers', value: '524288', pg_value: '4GB', unit: '8kB',
  current_value: '{DBInstanceClassMemory/32768}', running_value: '131072',
  apply_method: 'pending-reboot', reboot_required: true,
  rollback: { reset: true, cli: 'aws rds reset-db-parameter-group --region us-east-1' },
  cli: 'aws rds modify-db-parameter-group --region us-east-1 ' +
    '--db-parameter-group-name orders-pg16\naws rds reboot-db-instance --region us-east-1',
  console_url: 'https://us-east-1.console.aws.amazon.com/rds/home?region=us-east-1',
  notes: ['Host memory 16 GiB from performance_insights'],
  blockers: [], requires_approval: true, auto_apply: false,
  auto_apply_withheld: 'pg_sage never applies provider parameter changes in this release',
}

const listBody = { meaning: 'Managed provider changes', databases: [
  { database: 'orders', proposals: [
    { id: 5, status: 'pending', proposal, created_at: '2026-10-04T10:00:00Z' },
    { id: 6, status: 'approved', decision_note: 'tonight',
      proposal: { ...proposal, parameter: 'max_connections', value: '500',
        pg_value: '500', unit: '', blockers: ['default group cannot be modified'] } },
  ] },
] }

function response(status, body) {
  return { ok: status >= 200 && status < 300, status, statusText: `status ${status}`,
    json: async () => body }
}

function stubFetch(postStatus = 200) {
  const mock = vi.fn(async (url, init = {}) => {
    if ((init.method || 'GET') === 'POST') {
      return response(postStatus, postStatus === 200
        ? { id: 5, status: 'approved', applied_by_pg_sage: false }
        : { error: 'proposal is no longer pending' })
    }
    return response(200, listBody)
  })
  vi.stubGlobal('fetch', mock)
  return mock
}

afterEach(() => vi.unstubAllGlobals())

describe('ManagedChanges', () => {
  it('shows the exact command, reboot and rollback of each proposal', async () => {
    const mock = stubFetch()
    render(<ManagedChanges database="orders" canDecide />)
    const cards = await screen.findAllByTestId('managed-change-card')
    expect(cards).toHaveLength(2)
    expect(mock.mock.calls[0][0]).toBe('/api/v1/managed-changes?database=orders')
    const first = cards[0]
    expect(first).toHaveTextContent('shared_buffers')
    expect(first).toHaveTextContent('4GB')
    expect(within(first).getByTestId('managed-change-reboot')).toHaveTextContent(/reboot/i)
    expect(within(first).getByTestId('managed-change-cli')).toHaveTextContent(
      'aws rds modify-db-parameter-group')
    expect(within(first).getByTestId('managed-change-rollback')).toHaveTextContent(
      'reset-db-parameter-group')
    expect(first).toHaveTextContent(/never applies/i)
    expect(within(cards[1]).getByTestId('managed-change-blockers')).toHaveTextContent(
      'default group cannot be modified')
    expect(within(cards[1]).queryByTestId('managed-change-approve')).toBeNull()
  })

  it('approves without applying and refreshes', async () => {
    const mock = stubFetch()
    render(<ManagedChanges database="orders" canDecide />)
    fireEvent.click(await screen.findByTestId('managed-change-approve'))
    await waitFor(() => {
      const post = mock.mock.calls.find(([, init]) => init?.method === 'POST')
      expect(post?.[0]).toBe('/api/v1/managed-changes/5/approve?database=orders')
    })
    await waitFor(() => expect(mock.mock.calls.filter(([, init]) =>
      (init?.method || 'GET') === 'GET').length).toBeGreaterThan(1))
  })

  it('shows a refused decision', async () => {
    stubFetch(409)
    render(<ManagedChanges database="orders" canDecide />)
    fireEvent.click(await screen.findByTestId('managed-change-reject'))
    expect(await screen.findByTestId('managed-change-error')).toHaveTextContent(
      'no longer pending')
  })

  it('hides decisions from viewers and renders nothing when empty', async () => {
    stubFetch()
    const { unmount } = render(<ManagedChanges database="orders" canDecide={false} />)
    await screen.findAllByTestId('managed-change-card')
    expect(screen.queryByTestId('managed-change-approve')).toBeNull()
    unmount()
    vi.stubGlobal('fetch', vi.fn(async () => response(200, { databases: [] })))
    const { container } = render(<ManagedChanges database="orders" canDecide />)
    await waitFor(() => expect(container).toBeEmptyDOMElement())
  })
})
