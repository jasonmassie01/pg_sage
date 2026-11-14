import { render, screen, within } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import { ProviderReadinessMatrix } from './ProviderReadinessMatrix'

// advisors-08b: the readiness matrix has an "Instance settings" column
// naming which database of the fleet owns each instance's settings.
// No error propagation tests: the column reads a response that has already
// arrived; a failed fetch keeps the matrix's existing error handling.
// No concurrent access tests: a pure render with no shared state.
const longOwner = 'p'.repeat(65)

vi.mock('../hooks/useAPI', () => ({
  useAPI: () => ({
    loading: false,
    error: null,
    data: {
      summary: { ready_for_auto_safe: 1 },
      databases: [
        {
          name: 'primary',
          provider: 'self-managed',
          ready_for_auto_safe: true,
          capabilities: {
            instance_owner: {
              owner: 'primary', is_owner: true, key_source: 'system_identifier',
            },
            action_families: [
              { action_type: 'vacuum_table', supported: true, decision: 'execute' },
            ],
          },
        },
        {
          name: 'reporting',
          provider: 'self-managed',
          ready_for_auto_safe: true,
          capabilities: {
            instance_owner: {
              owner: 'primary', is_owner: false, key_source: 'system_identifier',
            },
          },
        },
        {
          name: 'solo',
          provider: 'rds',
          ready_for_auto_safe: false,
          capabilities: {},
        },
        {
          name: 'hostile',
          provider: 'self-managed',
          capabilities: {
            instance_owner: { owner: '<img src=x onerror=alert(1)>', is_owner: false },
          },
        },
        {
          name: 'long',
          provider: 'self-managed',
          capabilities: { instance_owner: { owner: longOwner, is_owner: false } },
        },
        {
          name: 'mystery',
          provider: 'self-managed',
          capabilities: {
            instance_owner: { is_owner: 'true', reason: 'instance identity unreadable' },
          },
        },
      ],
    },
  }),
}))

function cellFor(name) {
  const row = screen.getByText(name, { selector: 'td' }).closest('tr')
  return within(row).getByTestId('readiness-instance-owner')
}

describe('ProviderReadinessMatrix instance settings', () => {
  it('adds an Instance settings column', () => {
    render(<ProviderReadinessMatrix />)

    const headers = screen.getAllByRole('columnheader').map(h => h.textContent)
    expect(headers).toContain('Instance settings')
    const families = screen.getByLabelText('primary action families').closest('td')
    expect(families).toHaveAttribute('colspan', String(headers.length))
  })

  it('names the owner and the database that defers to it', () => {
    render(<ProviderReadinessMatrix />)

    expect(cellFor('primary')).toHaveTextContent(/^Owns the instance settings$/)
    expect(cellFor('reporting'))
      .toHaveTextContent(/^Instance settings handled by primary$/)
  })

  it('shows a dash for a database with no instance owner', () => {
    render(<ProviderReadinessMatrix />)

    expect(cellFor('solo')).toHaveTextContent(/^\u2014$/)
  })

  it('renders an HTML-looking owner as literal text', () => {
    const { container } = render(<ProviderReadinessMatrix />)

    expect(cellFor('hostile'))
      .toHaveTextContent('Instance settings handled by <img src=x onerror=alert(1)>')
    expect(container.querySelector('img')).toBeNull()
  })

  it('truncates a long owner and keeps the full name in the title', () => {
    render(<ProviderReadinessMatrix />)

    const cell = cellFor('long')
    expect(cell.textContent)
      .toBe(`Instance settings handled by ${'p'.repeat(63)}\u2026`)
    expect(cell).toHaveAttribute('title', longOwner)
  })

  it('shows an unknown owner with the reason in the title', () => {
    render(<ProviderReadinessMatrix />)

    const cell = cellFor('mystery')
    expect(cell).toHaveTextContent(/^Instance owner unknown$/)
    expect(cell).toHaveAttribute('title', 'instance identity unreadable')
  })
})
