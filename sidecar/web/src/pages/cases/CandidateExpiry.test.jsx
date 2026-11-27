import { render, screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import { CandidateArtifacts } from './CandidateArtifacts'

// cases-10: a candidate shows how long its evidence is good for, says when
// it expired, and when a failed attempt is retried.

const base = { action_type: 'analyze', risk_tier: 'safe', rollback_class: 'none' }

describe('CandidateArtifacts evidence expiry', () => {
  it('shows when the evidence stops being valid', () => {
    render(<CandidateArtifacts candidate={{ ...base, expires_at: '2026-11-12T09:00:00Z' }} />)
    expect(screen.getByTestId('candidate-expiry'))
      .toHaveTextContent('Evidence valid until 2026-11-12 09:00 UTC')
  })

  it('says the evidence expired for an expired candidate', () => {
    render(<CandidateArtifacts candidate={{
      ...base, expires_at: '2026-11-10T09:00:00Z', blocked_reason: 'evidence_expired',
    }} />)
    expect(screen.getByTestId('candidate-expiry'))
      .toHaveTextContent('Evidence expired at 2026-11-10 09:00 UTC')
    expect(screen.queryByText(/valid until/)).toBeNull()
  })

  it('shows when a failed attempt is retried', () => {
    render(<CandidateArtifacts candidate={{ ...base, retrying_after: '2026-11-11T14:05:00Z' }} />)
    expect(screen.getByTestId('candidate-retry'))
      .toHaveTextContent('Retrying after 2026-11-11 14:05 UTC')
  })

  it('shows nothing for a missing, zero or invalid time', () => {
    for (const value of [undefined, null, '', '0001-01-01T00:00:00Z', 'not a date']) {
      const { unmount } = render(<CandidateArtifacts candidate={{
        ...base, expires_at: value, retrying_after: value,
      }} />)
      expect(screen.queryByTestId('candidate-expiry')).toBeNull()
      expect(screen.queryByTestId('candidate-retry')).toBeNull()
      unmount()
    }
  })
})
