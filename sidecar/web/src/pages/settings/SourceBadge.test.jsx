import { render, screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import { MaskedBadge, MaskedOverridesContext, SourceBadge } from './SourceBadge'

// config-03: the source badge says override or file/default (never yaml for
// a default), and a key whose file value a UI override replaces is masked.

const trustMask = {
  key: 'trust.level',
  file_value: 'observation',
  override_value: 'autonomous',
  effective_value: 'observation',
  updated_by: 7,
  updated_at: '2026-11-10T08:30:00Z',
}

describe('SourceBadge', () => {
  it('shows override and file/default', () => {
    const { rerender } = render(<SourceBadge source="override" />)
    expect(screen.getByText('override')).toBeTruthy()
    rerender(<SourceBadge source="file/default" />)
    expect(screen.getByText('file/default')).toBeTruthy()
    expect(screen.queryByText('override')).toBeNull()
  })

  it('gives file/default its own color, unlike an unknown source', () => {
    const { rerender } = render(<SourceBadge source="file/default" />)
    const known = screen.getByText('file/default').style.color
    rerender(<SourceBadge source="unknown-source" />)
    expect(known).not.toBe(screen.getByText('unknown-source').style.color)
  })
})

describe('MaskedBadge', () => {
  it('shows masked with both values for a masked key', () => {
    render(
      <MaskedOverridesContext.Provider value={{ 'trust.level': trustMask }}>
        <MaskedBadge configKey="trust.level" />
      </MaskedOverridesContext.Provider>,
    )
    const badge = screen.getByText('masked')
    expect(badge.getAttribute('title')).toContain('observation')
    expect(badge.getAttribute('title')).toContain('autonomous')
    expect(badge.getAttribute('title')).toContain('user 7')
  })

  it('renders nothing for a key that is not masked', () => {
    const { container } = render(
      <MaskedOverridesContext.Provider value={{ 'trust.level': trustMask }}>
        <MaskedBadge configKey="collector.batch_size" />
      </MaskedOverridesContext.Provider>,
    )
    expect(container.textContent).toBe('')
  })

  it('renders nothing without a provider', () => {
    const { container } = render(<MaskedBadge configKey="trust.level" />)
    expect(container.textContent).toBe('')
  })
})
