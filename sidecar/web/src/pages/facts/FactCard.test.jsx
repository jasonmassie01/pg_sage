import { render, screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import { FactCard } from './FactCard'

// facts-08: a confirmed fact whose subject is absent keeps binding; the
// card says since when, so an operator sees it before it expires.

const confirmed = {
  id: 7, database: 'orders', type: 'slot_consumer', subject_kind: 'slot',
  subject: 'cdc_orders', value: { consumer: 'debezium' }, source: 'operator',
  proposed_by: 'alice@example.com', evidence: [], rationale: '', status: 'confirmed',
  decided_by: 'alice@example.com', decided_at: '2026-10-01T10:00:00Z',
  decision_note: '', expires_at: null, expired_reason: '', proposals: 1,
  created_at: '2026-10-01T10:00:00Z', updated_at: '2026-10-01T10:00:00Z',
  summary: 'slot cdc_orders belongs to debezium',
  provenance: 'fact #7, confirmed by alice@example.com on 2026-10-01',
}

describe('FactCard absence', () => {
  it('shows since when the subject of a binding fact is absent', () => {
    render(<FactCard fact={{ ...confirmed, missing_since: '2026-11-01T09:00:00Z' }}
      canDecide={false} onDecided={() => {}} />)
    const absent = screen.getByTestId('fact-absent-7')
    expect(absent).toHaveTextContent('Absent since')
    expect(absent).toHaveTextContent('still binds')
  })

  it('shows nothing about absence while the subject exists', () => {
    render(<FactCard fact={confirmed} canDecide={false} onDecided={() => {}} />)
    expect(screen.queryByTestId('fact-absent-7')).toBeNull()
  })

  it('ignores an unparsable absence date', () => {
    render(<FactCard fact={{ ...confirmed, missing_since: 'not a date' }}
      canDecide={false} onDecided={() => {}} />)
    expect(screen.queryByTestId('fact-absent-7')).toBeNull()
  })
})
