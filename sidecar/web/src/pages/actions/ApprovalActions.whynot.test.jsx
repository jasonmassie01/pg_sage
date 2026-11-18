import { fireEvent, render, screen } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { ApprovalActions } from './ApprovalActions'

// ux-08b: a card's blocker line opens the Why not panel for the card's
// finding. A card the gate does not block has no blocker line to open.
// No concurrency test: one read per open.

const apiGet = vi.fn()
vi.mock('../../lib/api', async importOriginal => ({ ...(await importOriginal()),
  apiGet: (...args) => apiGet(...args) }))
vi.mock('../../components/Toast', () => ({ useToast: () => ({ success: vi.fn(),
  error: vi.fn() }) }))

const card = { queue_id: 62, database: 'orders', title: 'Index on public.t',
  card_hash: 'hash-62', sql: 'CREATE INDEX CONCURRENTLY i ON public.t (c)',
  risk: { tier: 'moderate' }, finding: { id: 17, object: 'public.t' } }

beforeEach(() => {
  apiGet.mockReset()
  apiGet.mockResolvedValue({ database: 'orders', failing: [], links: [] })
})

describe('ApprovalActions blocker line', () => {
  it('opens the panel for the card finding', async () => {
    render(<ApprovalActions card={card} eligible={false}
      blockedReason="change class not allowed" />)
    expect(screen.getByTestId('approval-blocker')).toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', { name: 'Why not?' }))
    expect(await screen.findByTestId('why-not-panel')).toBeInTheDocument()
    expect(apiGet.mock.calls[0][0]).toBe('/api/v1/whynot?database=orders&finding_id=17')
  })

  it('asks by object when the card has no finding id', async () => {
    render(<ApprovalActions card={{ ...card, finding: undefined, targets: ['public.t'] }}
      eligible={false} blockedReason="change class not allowed" />)
    fireEvent.click(screen.getByRole('button', { name: 'Why not?' }))
    await screen.findByTestId('why-not-panel')
    expect(apiGet.mock.calls[0][0]).toBe('/api/v1/whynot?database=orders&object=public.t')
  })

  it('offers nothing on an eligible card', () => {
    render(<ApprovalActions card={card} eligible />)
    expect(screen.queryByRole('button', { name: 'Why not?' })).toBeNull()
  })
})
