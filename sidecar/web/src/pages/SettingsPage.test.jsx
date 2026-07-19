import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { describe, expect, it, vi, beforeEach } from 'vitest'
import { isMaskedSecret, SettingsPage } from './SettingsPage'

const refetch = vi.fn()
let configReadOnly = false

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
		  'retention.snapshots_days': { value: '14', source: 'override' },
          'retention.findings_days': { value: '60', source: 'yaml' },
          'retention.actions_days': { value: '180', source: 'yaml' },
          'retention.explains_days': { value: '30', source: 'yaml' },
		  'trust.level': { value: 'observation', source: 'yaml' },
		  execution_mode: { value: 'approval', source: 'db_override' },
        },
        mode: 'fleet',
        databases: 3,
        desired_generation: 7,
        read_only: configReadOnly,
        write_guidance: configReadOnly ? 'edit the YAML file' : undefined,
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
    configReadOnly = false
    globalThis.fetch = vi.fn().mockResolvedValue({
      ok: true,
      json: async () => ({ status: 'updated' }),
    })
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

  it('renders YAML fleet configuration as readable but not editable', () => {
    configReadOnly = true
    render(<SettingsPage database="all" />)

    fireEvent.click(screen.getByTestId('settings-tab-retention'))

    expect(screen.getByTestId('settings-read-only')).toHaveTextContent(
      /edit the YAML file/i
    )
    expect(screen.getByTestId('setting-retention.snapshots_days'))
      .toBeDisabled()
    expect(screen.queryByTestId('settings-save')).not.toBeInTheDocument()
  })

  it('sends the desired generation with global config writes', async () => {
    render(<SettingsPage database="all" />)
    fireEvent.click(screen.getByTestId('settings-tab-retention'))
    fireEvent.change(screen.getByTestId('setting-retention.snapshots_days'), {
      target: { value: '21' },
    })
    fireEvent.click(screen.getByTestId('settings-save'))
    fireEvent.click(screen.getByTestId('config-diff-confirm'))

    await waitFor(() => expect(globalThis.fetch).toHaveBeenCalled())
    const [, options] = globalThis.fetch.mock.calls[0]
    expect(JSON.parse(options.body)).toMatchObject({
      'retention.snapshots_days': '21',
      expected_generation: 7,
    })
  })

  it('sends the desired generation with database config writes', async () => {
	  render(<SettingsPage database="orders" databaseId={12} />)
	  fireEvent.click(screen.getByTestId('settings-tab-trust-safety'))
	  fireEvent.change(screen.getByTestId('setting-execution_mode'), {
	    target: { value: 'manual' },
	  })
	  fireEvent.click(screen.getByTestId('settings-save'))
	  fireEvent.click(screen.getByTestId('config-diff-confirm'))

	  await waitFor(() => expect(globalThis.fetch).toHaveBeenCalled())
	  const [, options] = globalThis.fetch.mock.calls[0]
	  expect(JSON.parse(options.body)).toMatchObject({
	    execution_mode: 'manual',
	    expected_generation: 7,
	  })
	})

	it('refetches after a generation conflict', async () => {
	  globalThis.fetch.mockResolvedValueOnce({
	    ok: false,
	    status: 409,
	    json: async () => ({ error: 'config generation conflict' }),
	  })
	  render(<SettingsPage database="all" />)
	  fireEvent.click(screen.getByTestId('settings-tab-retention'))
	  fireEvent.change(screen.getByTestId('setting-retention.snapshots_days'), {
	    target: { value: '21' },
	  })
	  fireEvent.click(screen.getByTestId('settings-save'))
	  fireEvent.click(screen.getByTestId('config-diff-confirm'))

	  await waitFor(() => expect(refetch).toHaveBeenCalled())
	})

	it('shows restart-pending fields returned by the server', async () => {
	  globalThis.fetch.mockResolvedValueOnce({
	    ok: true,
	    status: 200,
	    json: async () => ({
	      status: 'updated',
	      pending_restart: ['retention.snapshots_days'],
	    }),
	  })
	  render(<SettingsPage database="all" />)
	  fireEvent.click(screen.getByTestId('settings-tab-retention'))
	  fireEvent.change(screen.getByTestId('setting-retention.snapshots_days'), {
	    target: { value: '21' },
	  })
	  fireEvent.click(screen.getByTestId('settings-save'))
	  fireEvent.click(screen.getByTestId('config-diff-confirm'))

	  await waitFor(() => expect(screen.getByText(
	    /pending restart.*retention\.snapshots_days/i
	  )).toBeInTheDocument())
	})

	it('shows restart-pending fields returned by reset', async () => {
	  globalThis.fetch.mockResolvedValueOnce({
	    ok: true,
	    status: 200,
	    json: async () => ({
	      status: 'deleted',
	      pending_restart: ['retention.snapshots_days'],
	    }),
	  })
	  render(<SettingsPage database="all" />)
	  fireEvent.click(screen.getByTestId('settings-tab-retention'))
	  fireEvent.click(screen.getByTestId('reset-retention.snapshots_days'))

	  await waitFor(() => expect(screen.getByText(
	    /pending restart.*retention\.snapshots_days/i
	  )).toBeInTheDocument())
	})
})
