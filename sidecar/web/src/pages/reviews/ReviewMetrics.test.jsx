import { render, screen, within } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import { ReviewMetrics } from './ReviewMetrics'
import { summaryFixture } from './testkit'

// CHECK-UI-03 (metrics): concurrence rate, median time to DBA verdict,
// false-safe count, hours credited and pending, and the shadow auto-clear
// readiness bar labeled "shadow only — not enabled". All measured values
// from GET /api/v1/change-reviews/summary.

describe('ReviewMetrics', () => {
  it('shows concurrence, DBA wait, false-safe and credited hours', () => {
    render(<ReviewMetrics summary={summaryFixture()} />)
    // 9 of 12 decided reviews concurred across the verdict classes.
    expect(screen.getByTestId('metric-concurrence')).toHaveTextContent('75%')
    expect(screen.getByTestId('metric-concurrence')).toHaveTextContent('9 of 12')
    expect(screen.getByTestId('metric-dba-wait')).toHaveTextContent('1.5h')
    expect(screen.getByTestId('metric-dba-wait')).toHaveTextContent('p90 2.0d')
    expect(screen.getByTestId('metric-false-safe')).toHaveTextContent('1')
    expect(screen.getByTestId('metric-hours')).toHaveTextContent('2.5 h credited')
    expect(screen.getByTestId('metric-hours')).toHaveTextContent('0.8 h pending')
  })

  it('breaks concurrence down by verdict class', () => {
    render(<ReviewMetrics summary={summaryFixture()} />)
    const byClass = screen.getByTestId('metric-concurrence-by-verdict')
    expect(byClass).toHaveTextContent('risky 2 of 3')
    expect(byClass).toHaveTextContent('review 5 of 7')
    expect(byClass).toHaveTextContent('safe 2 of 2')
  })

  it('shows the receipts, override rate per rule and the other measures', () => {
    render(<ReviewMetrics summary={summaryFixture()} />)
    const receipts = screen.getByTestId('metric-receipts')
    expect(receipts).toHaveTextContent('12 clean')
    expect(receipts).toHaveTextContent('1 regressed')
    expect(receipts).toHaveTextContent('2 unverifiable')
    const overrides = screen.getByTestId('metric-overrides')
    expect(overrides).toHaveTextContent('CR-17')
    expect(overrides).toHaveTextContent('3 of 6')
    expect(overrides).toHaveTextContent('pg_stat_statements was reset')
    expect(screen.getByTestId('metric-reviews')).toHaveTextContent('40')
    expect(screen.getByTestId('metric-reviews')).toHaveTextContent('9.3 / week')
    expect(screen.getByTestId('metric-caught')).toHaveTextContent('2')
    expect(screen.getByTestId('metric-tokens')).toHaveTextContent('12,000')
    expect(screen.getByTestId('metric-first-comment')).toHaveTextContent('2m')
  })

  it('shows the auto-clear readiness bar as shadow only', () => {
    render(<ReviewMetrics summary={summaryFixture()} />)
    const bar = screen.getByTestId('autoclear-bar')
    expect(bar).toHaveTextContent('shadow only — not enabled')
    expect(bar).toHaveTextContent('12 of 30 scored')
    expect(bar).toHaveTextContent('100% correct (needs 97%)')
    expect(bar).toHaveTextContent('0 regressions in 90 days')
    expect(bar).toHaveTextContent(/not ready/i)
    const progress = within(bar).getByRole('progressbar')
    expect(progress).toHaveAttribute('aria-valuenow', '12')
    expect(progress).toHaveAttribute('aria-valuemax', '30')
  })

  it('never claims auto-clear is on, even when the bar is met', () => {
    render(<ReviewMetrics summary={summaryFixture({ auto_clear: {
      ...summaryFixture().auto_clear, scored: 31, correct: 31, ready: true } })} />)
    const bar = screen.getByTestId('autoclear-bar')
    expect(bar).toHaveTextContent('shadow only — not enabled')
    expect(bar).toHaveTextContent(/bar met/i)
    expect(within(bar).getByRole('progressbar')).toHaveAttribute('aria-valuenow', '30')
  })

  it('shows unknown values as a dash, never as zero', () => {
    render(<ReviewMetrics summary={summaryFixture({ concurrence: {},
      dba_wait_p50_seconds: null, dba_wait_p90_seconds: null,
      first_comment_p50_seconds: null,
      auto_clear: { ...summaryFixture().auto_clear, correct_rate: null, scored: 0 } })} />)
    expect(screen.getByTestId('metric-concurrence')).toHaveTextContent('—')
    expect(screen.getByTestId('metric-concurrence')).not.toHaveTextContent('0%')
    expect(screen.getByTestId('metric-dba-wait')).toHaveTextContent('—')
    expect(screen.getByTestId('autoclear-bar')).toHaveTextContent('— correct')
  })

  it('marks a partial fleet summary', () => {
    render(<ReviewMetrics summary={summaryFixture({ partial: true,
      unavailable: ['billing'] })} />)
    expect(screen.getByTestId('metrics-partial')).toHaveTextContent('billing')
  })

  it('renders nothing without a summary', () => {
    const { container } = render(<ReviewMetrics summary={null} />)
    expect(container).toBeEmptyDOMElement()
  })
})
