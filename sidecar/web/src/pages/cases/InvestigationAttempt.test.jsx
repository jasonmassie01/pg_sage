import { cleanup, fireEvent, render, screen, within } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { InvestigationPanel } from './InvestigationPanel'
import { AttemptNotice } from './AttemptNotice'

// sre-01: an expired run of a still-open incident is followed by a new
// attempt. The expired run says so and links to the new attempt; a later
// attempt names the run it replaces; after the last attempt the panel says
// no further attempt starts by itself.

const OLD = '11111111-1111-4111-8111-111111111111'
const NEW = '22222222-2222-4222-8222-222222222222'

function inv(fields) {
  return { trigger_kind: 'lock_blocking', subject: 'incident 1', pinned: false,
    created_at: '2026-11-08T10:00:00Z', summary: { conclusive: false }, ...fields }
}

const expired = inv({ id: OLD, state: 'expired', failure_code: 'stale_evidence',
  attempt: 1, superseded_by: NEW,
  summary: { conclusive: false, reason: 'stale_evidence', evidence_age_s: 3600 } })
const second = inv({ id: NEW, state: 'queued', attempt: 2, supersedes: OLD })

const details = {
  [OLD]: { investigation: expired, hypotheses: [], evidence: [], evidence_available: true,
    tombstones: [], event_count: 3, chain_verified: true },
  [NEW]: { investigation: second, hypotheses: [], evidence: [], evidence_available: true,
    tombstones: [], event_count: 1, chain_verified: true },
}

vi.mock('../../hooks/useAPI', () => ({
  useAPI: url => {
    const id = Object.keys(details).find(k => url && url.endsWith(`/investigations/${k}`))
    return { data: id ? details[id] : null, loading: false, error: null, refetch: vi.fn() }
  },
}))
vi.mock('../../components/Toast', () => ({
  useToast: () => ({ success: vi.fn(), error: vi.fn() }),
}))

afterEach(() => cleanup())

describe('investigation attempts', () => {
  it('links an expired run to the new attempt and opens it', () => {
    render(<InvestigationPanel database="orders" investigation={expired}
      user={{ role: 'viewer' }} />)
    fireEvent.click(screen.getByTestId('investigation-toggle'))
    const notice = screen.getByTestId('investigation-new-attempt')
    expect(notice).toHaveTextContent('A new attempt was started')
    const link = within(notice).getByRole('link', { name: /open attempt 2/i })
    expect(link).toHaveAttribute('href', `#investigation-${NEW}`)
    fireEvent.click(link)
    const shown = screen.getByTestId('investigation-attempt')
    expect(shown).toHaveTextContent('Attempt 2')
    expect(shown).toHaveTextContent('replaces an earlier run that expired')
    expect(screen.queryByTestId('investigation-new-attempt')).toBeNull()
    fireEvent.click(within(shown).getByRole('link', { name: /open the earlier run/i }))
    expect(screen.getByTestId('investigation-new-attempt')).toBeInTheDocument()
  })

  it('shows nothing for a first attempt that was not replaced', () => {
    const { container } = render(<AttemptNotice
      investigation={inv({ id: OLD, state: 'concluded', attempt: 1 })} onOpen={vi.fn()} />)
    expect(container).toBeEmptyDOMElement()
    const none = render(<AttemptNotice investigation={null} onOpen={vi.fn()} />)
    expect(none.container).toBeEmptyDOMElement()
  })

  it('says the last attempt expired without starting another', () => {
    render(<AttemptNotice investigation={inv({ id: NEW, state: 'expired',
      failure_code: 'queue_expired', attempt: 5, supersedes: OLD })} onOpen={vi.fn()} />)
    expect(screen.getByTestId('investigation-attempts-exhausted'))
      .toHaveTextContent('No further attempt starts automatically')
  })

  it('does not call a non-retryable expiry exhausted', () => {
    render(<AttemptNotice investigation={inv({ id: NEW, state: 'expired',
      failure_code: 'budget_exhausted', attempt: 5, supersedes: OLD })} onOpen={vi.fn()} />)
    expect(screen.queryByTestId('investigation-attempts-exhausted')).toBeNull()
    expect(screen.getByTestId('investigation-attempt')).toHaveTextContent('Attempt 5')
  })
})

describe('the case moving to a newer attempt', () => {
  it('shows the newest attempt when the case list replaces the investigation', () => {
    const view = render(<InvestigationPanel database="orders" investigation={expired}
      user={{ role: 'viewer' }} />)
    fireEvent.click(screen.getByTestId('investigation-toggle'))
    expect(screen.getByTestId('investigation-new-attempt')).toBeInTheDocument()
    view.rerender(<InvestigationPanel database="orders" investigation={second}
      user={{ role: 'viewer' }} />)
    expect(screen.getByTestId('investigation-attempt')).toHaveTextContent('Attempt 2')
    expect(screen.queryByTestId('investigation-new-attempt')).toBeNull()
  })
})
