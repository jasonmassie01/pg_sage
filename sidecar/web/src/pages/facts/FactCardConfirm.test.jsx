import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'

vi.mock('../../lib/facts', async (importOriginal) => {
  const actual = await importOriginal()
  return { ...actual, decideFact: vi.fn() }
})

import { decideFact } from '../../lib/facts'
import { FactCard } from './FactCard'

// facts-07: revoking or expiring a confirmed fact lifts a guard, so it
// asks first, naming the fact and what may then act on its subject.
// Cancel sends nothing; confirming a proposal stays one click.

const confirmed = {
  id: 9, database: 'orders', type: 'slot_consumer', subject_kind: 'slot',
  subject: 'cdc_orders', value: { consumer: 'debezium' }, source: 'operator',
  proposed_by: 'alice@example.com', evidence: [], rationale: '', status: 'confirmed',
  decided_by: 'alice@example.com', decided_at: '2026-10-01T10:00:00Z',
  decision_note: '', expires_at: null, expired_reason: '', proposals: 1,
  summary: 'slot cdc_orders belongs to debezium',
  provenance: 'fact #9, confirmed by alice@example.com on 2026-10-01',
}

function renderCard(fact = confirmed, onDecided = vi.fn()) {
  render(<FactCard fact={fact} canDecide onDecided={onDecided} />)
  return onDecided
}

describe('FactCard widening decisions', () => {
  beforeEach(() => {
    decideFact.mockReset()
    decideFact.mockResolvedValue({})
  })

  it('asks before revoking, naming the fact and its subject', () => {
    renderCard()
    fireEvent.click(screen.getByTestId('fact-revoke-9'))
    const dialog = screen.getByRole('alertdialog')
    expect(dialog).toHaveTextContent('slot cdc_orders belongs to debezium')
    expect(dialog).toHaveTextContent(
      'pg_sage and operator approvals may then act on cdc_orders again')
    expect(decideFact).not.toHaveBeenCalled()
  })

  it('cancel sends nothing and closes the confirmation', () => {
    const onDecided = renderCard()
    fireEvent.click(screen.getByTestId('fact-expire-9'))
    fireEvent.click(screen.getByTestId('fact-widen-cancel-9'))
    expect(screen.queryByRole('alertdialog')).toBeNull()
    expect(decideFact).not.toHaveBeenCalled()
    expect(onDecided).not.toHaveBeenCalled()
  })

  it('sends the decision with the note once confirmed', async () => {
    const onDecided = renderCard()
    fireEvent.change(screen.getByTestId('fact-note-9'), { target: { value: ' moved ' } })
    fireEvent.click(screen.getByTestId('fact-expire-9'))
    fireEvent.click(screen.getByTestId('fact-widen-confirm-9'))
    await waitFor(() => expect(onDecided).toHaveBeenCalledTimes(1))
    expect(decideFact).toHaveBeenCalledTimes(1)
    expect(decideFact).toHaveBeenCalledWith('orders', 9, 'expire', 'moved')
    expect(screen.queryByRole('alertdialog')).toBeNull()
  })

  it('shows the error and keeps the card when the decision fails', async () => {
    decideFact.mockRejectedValue(new Error('conflict'))
    const onDecided = renderCard()
    fireEvent.click(screen.getByTestId('fact-revoke-9'))
    fireEvent.click(screen.getByTestId('fact-widen-confirm-9'))
    expect(await screen.findByTestId('fact-error-9'))
      .toHaveTextContent('Could not reject fact #9: conflict')
    expect(onDecided).not.toHaveBeenCalled()
  })

  it('confirms a proposal in one click, with no confirmation', async () => {
    const onDecided = renderCard({ ...confirmed, status: 'proposed' })
    fireEvent.click(screen.getByTestId('fact-confirm-9'))
    expect(screen.queryByRole('alertdialog')).toBeNull()
    await waitFor(() => expect(onDecided).toHaveBeenCalledTimes(1))
    expect(decideFact).toHaveBeenCalledWith('orders', 9, 'confirm', '')
  })
})
