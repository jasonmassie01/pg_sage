import { fireEvent, render, screen } from '@testing-library/react'
import { describe, expect, it, vi, beforeEach } from 'vitest'
import { isMaskedSecret, SettingsPage } from './SettingsPage'

const refetch = vi.fn()

vi.mock('../hooks/useAPI', () => ({
  useAPI: vi.fn(url => {
    if (url?.includes('/shadow-report')) {
      return {
        data: {
          summary: {
            total_cases: 1,
            would_auto_resolve: 0,
            needs_approval: 1,
            avoided_toil_minutes: 30,
          },
          proof: [],
        },
        loading: false,
        error: null,
        refetch,
      }
    }
    return {
      data: {
        config: {
          'retention.snapshots_days': { value: '14', source: 'yaml' },
          'retention.findings_days': { value: '60', source: 'yaml' },
          'retention.actions_days': { value: '180', source: 'yaml' },
          'retention.explains_days': { value: '30', source: 'yaml' },
        },
        mode: 'fleet',
        databases: 3,
      },
      loading: false,
      error: null,
      refetch,
    }
  }),
}))

describe('isMaskedSecret', () => {
  it('detects config API secret masks', () => {
    expect(isMaskedSecret('****')).toBe(true)
    expect(isMaskedSecret('********1234')).toBe(true)
  })

  it('does not treat unmasked values as masked secrets', () => {
    expect(isMaskedSecret('sk-real-secret')).toBe(false)
    expect(isMaskedSecret('')).toBe(false)
  })
})

describe('SettingsPage', () => {
  beforeEach(() => {
    refetch.mockClear()
    localStorage.setItem('pg_sage_settings_mode', 'advanced')
  })

  it('does not render the shadow report over advanced setting tabs', () => {
    render(<SettingsPage database="all" />)

    fireEvent.click(screen.getByTestId('settings-tab-retention'))

    expect(screen.getByText('Snapshots (days)')).toBeInTheDocument()
    expect(screen.getByTestId('setting-retention.snapshots_days'))
      .toHaveValue(14)
    expect(screen.queryByTestId('shadow-mode-report')).not.toBeInTheDocument()
  })

  it('keeps the shadow report on the general settings tab', () => {
    render(<SettingsPage database="all" />)

    expect(screen.getByTestId('shadow-mode-report')).toBeInTheDocument()
  })
})
