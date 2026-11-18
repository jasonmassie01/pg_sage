import { render, screen, within } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import { NeedsYou } from './NeedsYou'

// mcp-01b: an item an agent asked for names the agent and the handle the
// agent waits on (wait_for_decision), and links to the agent's page where
// its requests are listed; the item's own Open link is the approval row.
// No concurrent access tests: one static list.

vi.mock('../../hooks/useAPI', () => ({ useAPI: () => ({ data: null, loading: false }) }))
vi.mock('../../components/Toast', () => ({ useToast: () => ({ success: vi.fn(),
  error: vi.fn() }) }))

const base = { database: 'orders', why: '', risk: 'moderate', actionable: false,
  created_at: '2026-11-10T15:00:00Z', source: 'sage.action_queue' }

describe('NeedsYou agent requests', () => {
  it('shows the agent, the handle and both links on an agent item', () => {
    render(<NeedsYou onDecided={vi.fn()} items={[{ ...base, kind: 'change_approval', id: '7',
      title: 'Approve analyze', href: '#/actions?tab=pending&queue=7&database=orders',
      agent: 'agp_aaaaaaaaaaaaaaaaaaaa', handle: 'change_approval:7' }]} />)
    const row = screen.getByTestId('inbox-item-change_approval-7')
    const who = within(row).getByTestId('inbox-agent-change_approval-7')
    expect(who.textContent).toContain('agp_aaaaaaaaaaaaaaaaaaaa')
    expect(who.textContent).toContain('change_approval:7')
    expect(who.textContent).toContain('· waits on change_approval:7')
    expect(who.textContent).not.toContain('�')
    const agentLink = within(who).getByRole('link', { name: 'agp_aaaaaaaaaaaaaaaaaaaa' })
    expect(agentLink.getAttribute('href'))
      .toBe('#/agents?agent=agp_aaaaaaaaaaaaaaaaaaaa&database=orders')
    expect(within(row).getByRole('link', { name: 'Open' }).getAttribute('href'))
      .toBe('#/actions?tab=pending&queue=7&database=orders')
  })

  it('shows nothing extra on an item a person or pg_sage raised', () => {
    render(<NeedsYou onDecided={vi.fn()} items={[{ ...base, kind: 'change_approval', id: '8',
      title: 'Approve vacuum', href: '#/actions' }]} />)
    expect(screen.queryByTestId('inbox-agent-change_approval-8')).toBeNull()
  })

  it('never links an agent id that is not a principal id', () => {
    render(<NeedsYou onDecided={vi.fn()} items={[{ ...base, kind: 'agent_request', id: '9',
      title: 'Agent asks', href: '#/agents', agent: 'javascript:alert(1)',
      handle: 'agent_request:9' }]} />)
    const who = screen.getByTestId('inbox-agent-agent_request-9')
    expect(within(who).queryByRole('link')).toBeNull()
    expect(who.textContent).toContain('agent_request:9')
  })
})
