import { render, screen } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { DatabricksStatusCard } from './DatabricksStatusCard'

let apiState
let requested

vi.mock('../../hooks/useAPI', () => ({
  useAPI: (url) => {
    requested = url
    return { ...apiState, refetch: vi.fn() }
  },
}))

const NOW = new Date('2026-11-11T12:00:00Z')

function status(databricks) {
  return {
    runtime_liveness: 'partial',
    databricks: {
      active: true,
      endpoints: [],
      gateway: {
        authentication_observed: true,
        last_auth_success_at: '2026-11-11T11:59:00Z',
        last_auth_failure_at: null,
        last_failure_class: '',
      },
      telemetry: {},
      ...databricks,
    },
  }
}

const endpoint = {
  endpoint: 'projects/p/branches/b/endpoints/primary',
  last_minted_at: '2026-11-11T11:30:00Z',
  expires_at: '2026-11-11T12:30:00Z',
  consecutive_mint_failures: 0,
  last_error_class: '',
}

const telemetry = {
  orders: {
    state: 'polling',
    streams: [{
      stream: 'plan_history', forward_lag_seconds: 180, statements: 120,
      counters_since: '2026-11-11T10:00:00Z',
    }],
    delivery: [{
      consumer: 'history.v1', pending_rows: 10001, pending_rows_capped: true,
      oldest_pending_age_seconds: 3600,
    }],
  },
}

beforeEach(() => {
  vi.useFakeTimers({ toFake: ['Date'] })
  vi.setSystemTime(NOW)
  apiState = { data: null, loading: false, error: null }
})

afterEach(() => {
  vi.useRealTimers()
})

describe('DatabricksStatusCard', () => {
  it('reads the admin deployment status', () => {
    render(<DatabricksStatusCard />)
    expect(requested).toBe('/api/v1/deployment/status')
  })

  it('renders nothing outside Databricks', () => {
    apiState.data = status({ active: false })
    const { container } = render(<DatabricksStatusCard />)
    expect(container).toBeEmptyDOMElement()
  })

  it('renders nothing before the status loads or when it fails', () => {
    const { container, rerender } = render(<DatabricksStatusCard />)
    expect(container).toBeEmptyDOMElement()
    apiState = { data: null, loading: false, error: 'forbidden' }
    rerender(<DatabricksStatusCard />)
    expect(container).toBeEmptyDOMElement()
  })

  it('renders nothing for a status without a databricks block', () => {
    apiState.data = { runtime_liveness: 'not_reported' }
    const { container } = render(<DatabricksStatusCard />)
    expect(container).toBeEmptyDOMElement()
  })

  it('shows the credential expiry with no warning when minting is healthy', () => {
    apiState.data = status({ endpoints: [endpoint] })
    render(<DatabricksStatusCard />)
    expect(screen.getByText('endpoints/primary')).toBeInTheDocument()
    expect(screen.getByText(/expires in 30 min/)).toBeInTheDocument()
    expect(screen.queryByRole('alert')).not.toBeInTheDocument()
  })

  it('warns when credential minting has failed', () => {
    apiState.data = status({
      endpoints: [{ ...endpoint, consecutive_mint_failures: 3,
        last_error_class: 'throttled' }],
    })
    render(<DatabricksStatusCard />)
    const alert = screen.getByRole('alert')
    expect(alert).toHaveTextContent('3 consecutive mint failures')
    expect(alert).toHaveTextContent('throttled')
  })

  it('warns on an expired credential', () => {
    apiState.data = status({
      endpoints: [{ ...endpoint, expires_at: '2026-11-11T11:00:00Z' }],
    })
    render(<DatabricksStatusCard />)
    expect(screen.getByRole('alert')).toHaveTextContent('expired')
  })

  it('shows a Gateway failure newer than the last success', () => {
    apiState.data = status({
      gateway: {
        authentication_observed: false,
        last_auth_success_at: '2026-11-11T11:00:00Z',
        last_auth_failure_at: '2026-11-11T11:58:00Z',
        last_failure_class: 'authenticate',
      },
    })
    render(<DatabricksStatusCard />)
    const alert = screen.getByRole('alert')
    expect(alert).toHaveTextContent('Gateway authentication failing')
    expect(alert).toHaveTextContent('authenticate')
  })

  it('shows a Gateway that has not been used yet', () => {
    apiState.data = status({
      gateway: {
        authentication_observed: false, last_auth_success_at: null,
        last_auth_failure_at: null, last_failure_class: '',
      },
    })
    render(<DatabricksStatusCard />)
    expect(screen.getByText(/no Gateway request yet/)).toBeInTheDocument()
    expect(screen.queryByRole('alert')).not.toBeInTheDocument()
  })

  it('shows per-stream lag, backlog and statements per hour', () => {
    apiState.data = status({ telemetry })
    render(<DatabricksStatusCard />)
    expect(screen.getByText('plan_history')).toBeInTheDocument()
    expect(screen.getByText('3 min')).toBeInTheDocument()
    expect(screen.getByText('60/h')).toBeInTheDocument()
    expect(screen.getByText(/10001\+ pending/)).toBeInTheDocument()
    expect(screen.getByText(/oldest 60 min/)).toBeInTheDocument()
  })

  it('shows a dash for a stream that has not read yet', () => {
    apiState.data = status({
      telemetry: {
        orders: { state: 'binding', streams: [{ stream: 'logs', statements: 0,
          counters_since: '2026-11-11T12:00:00Z' }], delivery: [] },
      },
    })
    render(<DatabricksStatusCard />)
    expect(screen.getByText('logs')).toBeInTheDocument()
    expect(screen.getAllByText('—').length).toBeGreaterThanOrEqual(2)
  })
})
