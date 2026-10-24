import { render, screen, within } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import { ReceiptPanel } from './ReceiptPanel'
import { decidedReceipt, makeReview } from './testkit'

// CHECK-UI-03 (receipt): the awaiting, landed and decided states, with
// before/after numbers per watched queryid and links to the incidents and
// findings that contradicted the review.

describe('ReceiptPanel', () => {
  it('shows an awaiting review with what the receipt will watch', () => {
    render(<ReceiptPanel review={makeReview()} />)
    const panel = screen.getByTestId('receipt-panel')
    expect(within(panel).getByTestId('receipt-state')).toHaveTextContent('awaiting landing')
    expect(panel).toHaveTextContent('1 catalog post-condition')
    expect(panel).toHaveTextContent('1 watched statement')
    expect(screen.queryAllByTestId('receipt-target')).toHaveLength(0)
  })

  it('shows a landed review with its bracket and pending window', () => {
    render(<ReceiptPanel review={makeReview({ landing_state: 'landed',
      landing_source: 'change_event', landed_lower: '2026-10-22T09:50:00Z',
      landed_at: '2026-10-22T10:05:00Z', receipt_verdict: 'pending' })} />)
    expect(screen.getByTestId('receipt-state')).toHaveTextContent('landed, receipt pending')
    const landing = screen.getByTestId('receipt-landing')
    expect(landing).toHaveTextContent('change event')
    expect(landing).toHaveTextContent('2026-10-22 09:50')
    expect(landing).toHaveTextContent('2026-10-22 10:05')
  })

  it('shows a decided receipt with before/after numbers and contradicting links', () => {
    render(<ReceiptPanel review={makeReview({ landing_state: 'landed',
      landing_source: 'catalog', landed_lower: '2026-10-22T10:00:00Z',
      landed_at: '2026-10-22T10:05:00Z', receipt_verdict: 'regressed',
      receipt: decidedReceipt, receipt_decided_at: '2026-10-23T10:10:00Z',
      receipt_hash: 'f'.repeat(64) })} />)
    expect(screen.getByTestId('receipt-state')).toHaveTextContent('regressed')
    expect(screen.getByTestId('receipt-reason')).toHaveTextContent('latency')
    const rows = screen.getAllByTestId('receipt-target')
    expect(rows).toHaveLength(2)
    expect(rows[0]).toHaveTextContent('-4242')
    expect(rows[0]).toHaveTextContent('1.25 ms')
    expect(rows[0]).toHaveTextContent('2.50 ms')
    expect(rows[0]).toHaveTextContent('+100%')
    expect(rows[0]).toHaveTextContent('51,000')
    expect(rows[0]).toHaveTextContent('49,000')
    expect(rows[0]).toHaveTextContent('regressed')
    expect(rows[1]).toHaveTextContent('+2.5%')

    const incident = screen.getByTestId('receipt-incident')
    expect(incident).toHaveTextContent('9d1c0b6e-aaaa-4bbb-8ccc-123456789abc')
    expect(incident.closest('a') || incident).toHaveAttribute('href', '#/incidents')
    const finding = screen.getByTestId('receipt-lock-finding')
    expect(finding).toHaveTextContent('finding 42')
    expect(finding.closest('a') || finding).toHaveAttribute('href', '#/cases')
    expect(screen.getByTestId('receipt-born')).toHaveTextContent('555')
    expect(screen.getByTestId('receipt-born')).toHaveTextContent('3.20 ms')
    expect(screen.getByTestId('receipt-contradicted')).toHaveTextContent('queryid:-4242')
    expect(screen.getByTestId('receipt-tolerance')).toHaveTextContent('missed')
    expect(screen.getByTestId('receipt-window')).toHaveTextContent('24h')
    expect(screen.getByTestId('receipt-hash')).toHaveTextContent('ffffffffffff')
  })

  it('shows a clean receipt without contradictions', () => {
    render(<ReceiptPanel review={makeReview({ landing_state: 'landed',
      landing_source: 'reported', landed_lower: '2026-10-22T10:00:00Z',
      landed_at: '2026-10-22T10:00:00Z', receipt_verdict: 'clean',
      receipt: { ...decidedReceipt, verdict: 'clean', reason: 'no_change',
        targets: [{ ...decidedReceipt.targets[1] }], incidents: [], lock_findings: [],
        contradicted_by: [], born_queryids: [], tolerance: 'met' } })} />)
    expect(screen.getByTestId('receipt-state')).toHaveTextContent('clean')
    expect(screen.queryByTestId('receipt-incident')).toBeNull()
    expect(screen.queryByTestId('receipt-contradicted')).toBeNull()
    expect(screen.getByTestId('receipt-tolerance')).toHaveTextContent('met')
  })

  it('names the confounders of an unverifiable receipt', () => {
    render(<ReceiptPanel review={makeReview({ landing_state: 'landed',
      landing_source: 'catalog', landed_lower: '2026-10-22T10:00:00Z',
      landed_at: '2026-10-22T10:05:00Z', receipt_verdict: 'unverifiable',
      receipt: { ...decidedReceipt, verdict: 'unverifiable', reason: 'confounded',
        targets: [], confounders: ['stats_reset at 2026-10-22T12:00:00Z'],
        incidents: [], lock_findings: [], contradicted_by: [] } })} />)
    expect(screen.getByTestId('receipt-state')).toHaveTextContent('unverifiable')
    expect(screen.getByTestId('receipt-confounders')).toHaveTextContent('stats_reset')
  })

  it.each([
    ['expired', 'landing expired'],
    ['not_applicable', 'already in place'],
  ])('shows landing state %s', (state, text) => {
    render(<ReceiptPanel review={makeReview({ landing_state: state,
      receipt_verdict: state === 'expired' ? 'unverifiable' : 'pending' })} />)
    expect(screen.getByTestId('receipt-state')).toHaveTextContent(text)
  })

  it('waits for a running review to complete', () => {
    render(<ReceiptPanel review={makeReview({ state: 'running', landing_state: '',
      receipt_verdict: '' })} />)
    expect(screen.getByTestId('receipt-state')).toHaveTextContent(/after the review completes/i)
  })

  it('tolerates a decided receipt with missing lists', () => {
    render(<ReceiptPanel review={makeReview({ landing_state: 'landed',
      landing_source: 'catalog', landed_lower: '2026-10-22T10:00:00Z',
      landed_at: '2026-10-22T10:05:00Z', receipt_verdict: 'clean_low_traffic',
      receipt: { verdict: 'clean_low_traffic', reason: 'low_traffic', window_hours: 24 },
    })} />)
    expect(screen.getByTestId('receipt-state')).toHaveTextContent('clean, low traffic')
    expect(screen.queryAllByTestId('receipt-target')).toHaveLength(0)
  })
})
