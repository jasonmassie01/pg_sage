import { render, screen, within } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { IndexAdmissionPanel } from './IndexAdmissionPanel'

// D6: each database shows whether autonomous index builds are admitted,
// which evidence decided it, and how far the IO baseline has been learned.

let payload = null
let requested = []
vi.mock('../hooks/useAPI', () => ({
  useAPI: (url) => {
    requested.push(url)
    return { data: payload, refetch: vi.fn() }
  },
}))

const learning = {
  database: 'orders', ok: false, reason: 'learning_baseline',
  mode: 'learned_baseline', detail: 'learning IO baseline: 3.0/7 days',
  baseline_required_days: 7, baseline_observed_days: 3, baseline_samples: 4320,
  missing_evidence: ['host_cpu'], withheld_findings: 2,
}

const declared = {
  database: 'billing', ok: true, reason: 'load_within_ceiling',
  mode: 'declared_capacity', detail: '', baseline_required_days: 7,
  baseline_observed_days: 0.5, baseline_samples: 720,
  missing_evidence: [], withheld_findings: 0,
}

const unavailable = {
  database: 'legacy', ok: false, reason: 'load_unavailable', mode: 'unavailable',
  detail: 'pg-side IO sampler is not running', baseline_required_days: 0,
  baseline_observed_days: 0, baseline_samples: 0,
  missing_evidence: ['pg_io_rate', 'host_cpu'], withheld_findings: 0,
}

describe('IndexAdmissionPanel', () => {
  beforeEach(() => {
    payload = null
    requested = []
  })

  it('shows baseline learning progress for a withheld database', () => {
    payload = { databases: [learning] }
    render(<IndexAdmissionPanel database="all" />)
    const row = screen.getByTestId('index-admission-orders')
    expect(within(row).getByText('Withheld')).toBeInTheDocument()
    expect(within(row).getByText('Learned baseline')).toBeInTheDocument()
    expect(within(row).getByText('learning IO baseline: 3.0/7 days')).toBeInTheDocument()
    const bar = within(row).getByRole('progressbar')
    expect(bar).toHaveAttribute('aria-valuenow', '3')
    expect(bar).toHaveAttribute('aria-valuemax', '7')
    expect(within(row).getByText(/2 findings withheld/)).toBeInTheDocument()
  })

  it('shows declared-capacity admission without a learning bar', () => {
    payload = { databases: [declared] }
    render(<IndexAdmissionPanel database="all" />)
    const row = screen.getByTestId('index-admission-billing')
    expect(within(row).getByText('Admitted')).toBeInTheDocument()
    expect(within(row).getByText('Declared capacity')).toBeInTheDocument()
    expect(within(row).queryByRole('progressbar')).toBeNull()
  })

  it('names the missing evidence when admission is unavailable', () => {
    payload = { databases: [unavailable] }
    render(<IndexAdmissionPanel database="all" />)
    const row = screen.getByTestId('index-admission-legacy')
    expect(within(row).getByText('No IO evidence')).toBeInTheDocument()
    expect(within(row).getByText(/pg_io_rate, host_cpu/)).toBeInTheDocument()
  })

  it('requests only the selected database', () => {
    payload = { databases: [learning] }
    render(<IndexAdmissionPanel database="orders" />)
    expect(requested).toContain('/api/v1/admission?database=orders')
    render(<IndexAdmissionPanel database="all" />)
    expect(requested).toContain('/api/v1/admission')
  })

  it('stays hidden with no databases', () => {
    payload = { databases: [] }
    render(<IndexAdmissionPanel database="all" />)
    expect(screen.queryByTestId('index-admission-panel')).toBeNull()
    payload = null
    render(<IndexAdmissionPanel database="all" />)
    expect(screen.queryByTestId('index-admission-panel')).toBeNull()
  })
})
