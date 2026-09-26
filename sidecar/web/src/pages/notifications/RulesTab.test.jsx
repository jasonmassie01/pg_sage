import { fireEvent, render, screen } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { RulesTab } from './RulesTab'
import { EVENT_TYPES, defaultMinSeverity } from './shared'

// The rule form must offer every backend event type and default
// min_severity to the event's own severity (notify.DefaultMinSeverity),
// so a new rule can always fire (G9-B28 + G7-B06 contract).

beforeEach(() => {
  vi.stubGlobal('fetch', vi.fn().mockResolvedValue({
    ok: true, json: async () => ({ rules: [], channels: [] }),
  }))
})
afterEach(() => vi.unstubAllGlobals())

describe('notification rule defaults', () => {
  it('lists every backend event type', () => {
    for (const ev of ['action_executed', 'action_failed', 'approval_needed',
      'finding_critical', 'query_rewrite_suggested', 'incident_detected',
      'incident_escalated', 'incident_resolved']) {
      expect(EVENT_TYPES).toContain(ev)
    }
  })

  it('mirrors the backend default min severity', () => {
    expect(defaultMinSeverity('action_executed')).toBe('info')
    expect(defaultMinSeverity('finding_critical')).toBe('critical')
    expect(defaultMinSeverity('incident_detected')).toBe('warning')
    expect(defaultMinSeverity('incident_resolved')).toBe('warning')
    expect(defaultMinSeverity('unknown_event')).toBe('info')
  })

  it('updates the severity default when the event changes', () => {
    render(<RulesTab />)
    const severity = screen.getByTestId('add-rule-severity')
    expect(severity).toHaveValue('info')
    fireEvent.change(screen.getByTestId('add-rule-event'),
      { target: { value: 'incident_escalated' } })
    expect(severity).toHaveValue('warning')
    fireEvent.change(screen.getByTestId('add-rule-event'),
      { target: { value: 'finding_critical' } })
    expect(severity).toHaveValue('critical')
  })
})
