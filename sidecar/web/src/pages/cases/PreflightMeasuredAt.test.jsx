import { render, screen } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { CandidateArtifacts } from './CandidateArtifacts'
import { measuredAgeText } from './preflightAge'

// migration-01: the DDL preflight's live values are measured when the
// migration was detected; the panel says so and how long ago that was.

const NOW = Date.parse('2026-11-09T12:30:00Z')

function candidate(preflight) {
  return {
    action_type: 'ddl_preflight', risk_tier: 'high',
    output_modes: ['generate_pr_or_script'], rollback_class: 'forward_fix_only',
    ddl_preflight: {
      summary: 'ACCESS EXCLUSIVE rewrite risk', lock_level: 'ACCESS EXCLUSIVE',
      checks: [{ name: 'activity', status: 'warn', detail: '2 active queries' }],
      ...preflight,
    },
  }
}

describe('DDL preflight measured_at', () => {
  beforeEach(() => { vi.useFakeTimers(); vi.setSystemTime(NOW) })
  afterEach(() => { vi.useRealTimers() })

  it('shows the detection time and its age', () => {
    render(<CandidateArtifacts candidate={candidate({
      measured_at: '2026-11-09T12:25:00Z',
    })} />)
    const note = screen.getByTestId('preflight-measured-at')
    expect(note).toHaveTextContent('Measured at detection, 5 min ago')
    expect(note).toHaveAttribute('title', '2026-11-09T12:25:00Z')
  })

  it('says nothing was measured for a legacy preflight', () => {
    render(<CandidateArtifacts candidate={candidate({ checks: [
      { name: 'activity', status: 'unknown', detail: 'Not measured' },
      { name: 'lock_level', status: 'pass', detail: 'ACCESS EXCLUSIVE' },
    ] })} />)
    expect(screen.queryByTestId('preflight-measured-at')).toBeNull()
    expect(screen.getByTestId('preflight-not-measured'))
      .toHaveTextContent('Live conditions were not measured')
  })

  it('claims nothing about a preflight measured by another source', () => {
    render(<CandidateArtifacts candidate={candidate({})} />)
    expect(screen.queryByTestId('preflight-measured-at')).toBeNull()
    expect(screen.queryByTestId('preflight-not-measured')).toBeNull()
  })

  it('formats boundaries and rejects bad times', () => {
    expect(measuredAgeText('2026-11-09T12:29:30Z', NOW)).toBe('just now')
    expect(measuredAgeText('2026-11-09T12:29:00Z', NOW)).toBe('1 min ago')
    expect(measuredAgeText('2026-11-09T11:30:00Z', NOW)).toBe('1 h ago')
    expect(measuredAgeText('2026-11-07T12:30:00Z', NOW)).toBe('2 d ago')
    expect(measuredAgeText('2026-11-09T12:35:00Z', NOW)).toBe('just now')
    expect(measuredAgeText('not a time', NOW)).toBe('')
    expect(measuredAgeText('', NOW)).toBe('')
    expect(measuredAgeText(undefined, NOW)).toBe('')
  })
})
