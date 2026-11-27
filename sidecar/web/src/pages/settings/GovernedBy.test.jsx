import { render, screen } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { setTimeZoneMode } from '../../lib/time'
import { GovernedBy, GovernedByLine } from './GovernedBy'
import { governedByText } from './policySetsFormat'

// fleetpol-01c: a database's page and its Trust & Safety tab say which
// policy set governs it, and when its own document is in force, the
// exception: what differs, who set it and until when.
// No concurrent access tests: a render of two reads.

const responses = {}
const errors = {}
const requested = []
vi.mock('../../hooks/useAPI', () => ({
  useAPI: url => {
    requested.push(url)
    const key = url ? url.split('?')[0] : ''
    return { data: url ? responses[key] : undefined, loading: false,
      error: url ? errors[key] || null : null, refetch: vi.fn() }
  },
}))

function exception(over = {}) {
  return { database_id: 9, set_name: 'prod', set_by: 'alice@example.com',
    reason: 'month-end batch', set_at: '2026-11-10T09:00:00Z',
    expires_at: '2026-12-01T00:00:00Z',
    diff: [{ field: 'lock_duration_ceiling_ms', set: 30000, exception: 120000 }], ...over }
}

beforeEach(() => {
  for (const k of Object.keys(responses)) delete responses[k]
  for (const k of Object.keys(errors)) delete errors[k]
  requested.length = 0
  setTimeZoneMode('utc')
})

afterEach(() => setTimeZoneMode('local'))

describe('governedByText', () => {
  it('names the set a database follows', () => {
    expect(governedByText({ set: 'prod', version: 2 }, [], 4)).toBe('Governed by: prod')
  })

  it('describes an exception with its fields, owner and expiry', () => {
    expect(governedByText({ set: '' }, [exception()], 9)).toBe(
      'Governed by: prod (exception: lock_duration_ceiling_ms changed by '
      + 'alice@example.com, expires 2026-12-01)')
  })

  it('says when an exception never expires', () => {
    expect(governedByText({ set: '' }, [exception({ expires_at: null })], 9))
      .toBe('Governed by: prod (exception: lock_duration_ceiling_ms changed by '
        + 'alice@example.com, no expiry)')
  })

  it('shortens a long diff and says when nothing differs', () => {
    const diff = ['a', 'b', 'c', 'd'].map(field => ({ field, set: 1, exception: 2 }))
    expect(governedByText({ set: '' }, [exception({ diff })], 9))
      .toContain('exception: a, b and 2 more changed by')
    expect(governedByText({ set: '' }, [exception({ diff: [] })], 9))
      .toContain('exception: same as the set, by alice@example.com')
  })

  it('ignores another database\'s exception', () => {
    expect(governedByText({ set: 'prod' }, [exception({ database_id: 3 })], 9))
      .toBe('Governed by: prod')
  })

  it('names its own document when no set governs it', () => {
    expect(governedByText({ set: '', version: 1 }, [], 9))
      .toBe('Governed by: its own policy document (no policy set matches it)')
  })

  it('returns nothing without a policy', () => {
    expect(governedByText(null, [], 9)).toBe('')
    expect(governedByText(undefined, undefined, 9)).toBe('')
  })
})

describe('GovernedBy', () => {
  it('reads the database\'s policy and the exceptions', () => {
    responses['/api/v1/policy'] = { set: '', version: 3 }
    responses['/api/v1/policy-exceptions'] = { exceptions: [exception()] }
    render(<GovernedBy databaseId={9} />)
    expect(requested).toContain('/api/v1/policy?database_id=9')
    expect(requested).toContain('/api/v1/policy-exceptions')
    expect(screen.getByTestId('governed-by')).toHaveTextContent(
      'Governed by: prod (exception: lock_duration_ceiling_ms changed by alice@example.com')
  })

  it('uses a policy it is given instead of reading it again', () => {
    responses['/api/v1/policy-exceptions'] = { exceptions: [] }
    render(<GovernedByLine databaseId={4} policy={{ set: 'prod', version: 2 }} />)
    expect(requested.filter(u => u && u.startsWith('/api/v1/policy?'))).toHaveLength(0)
    expect(screen.getByTestId('governed-by')).toHaveTextContent('Governed by: prod')
  })

  it('renders nothing without a database id', () => {
    const { container } = render(<GovernedBy />)
    expect(container).toBeEmptyDOMElement()
    expect(requested.every(u => u === null)).toBe(true)
  })

  it('shows the server message when the exceptions read fails', () => {
    responses['/api/v1/policy'] = { set: '', version: 3 }
    errors['/api/v1/policy-exceptions'] = 'policy store unavailable (500)'
    render(<GovernedBy databaseId={9} />)
    expect(screen.getByRole('alert')).toHaveTextContent('policy store unavailable (500)')
  })

  it('renders the owner as text, never HTML', () => {
    responses['/api/v1/policy'] = { set: '' }
    responses['/api/v1/policy-exceptions'] = { exceptions: [
      exception({ set_by: '<img src=x onerror=alert(1)>' })] }
    const { container } = render(<GovernedBy databaseId={9} />)
    expect(container.querySelector('img')).toBeNull()
    expect(screen.getByTestId('governed-by')).toHaveTextContent('<img src=x onerror=alert(1)>')
  })
})
