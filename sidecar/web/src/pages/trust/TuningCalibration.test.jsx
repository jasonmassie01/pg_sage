import { render, screen, waitFor } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { TuningCalibration } from './TuningCalibration'

// Roadmap 2.2: the tuning agent's calibrated confidence. Per action class
// and prediction method, the reliability bins map predicted improvement
// to what the outcome ledger observed; below the minimum number of
// decided outcomes a class is "uncalibrated" and shows no confidence.

const bins = [
  { label: '0-10', n: 0, hits: 0, rate: 0, tolerance_met: 0 },
  { label: '10-25', n: 0, hits: 0, rate: 0, tolerance_met: 0 },
  { label: '25-50', n: 6, hits: 5, rate: 0.8333, wilson_low: 0.436, wilson_high: 0.97,
    mean_predicted: 38, mean_observed: 30, tolerance_met: 4, regressed: 0 },
  { label: '50+', n: 0, hits: 0, rate: 0, tolerance_met: 0 },
]

function mockFetch(body, ok = true, status = 200) {
  globalThis.fetch = vi.fn(() => Promise.resolve({
    ok, status, json: () => Promise.resolve(body),
  }))
}

afterEach(() => {
  delete globalThis.fetch
})

describe('TuningCalibration', () => {
  it('shows each class and method with its reliability bins', async () => {
    mockFetch({ database: 'orders', min_outcomes: 5, window_days: 180, excluded: 2,
      classes: [
        { class: 'index_create', method: 'hypopg', n: 6, hits: 5, regressed: 0,
          rate: 0.8333, wilson_low: 0.436, wilson_high: 0.97, status: 'calibrated', bins },
        { class: 'guc', method: 'model', n: 2, hits: 1, regressed: 1, rate: 0.5,
          status: 'uncalibrated', bins: bins.map(b => ({ ...b, n: 0, hits: 0 })) },
      ] })
    render(<TuningCalibration database="orders" />)
    const row = await screen.findByTestId('tuning-calibration-index_create-hypopg')
    expect(globalThis.fetch).toHaveBeenCalledWith(
      '/api/v1/tuning/calibration?database=orders',
      expect.objectContaining({ credentials: 'include' }))
    expect(row).toHaveTextContent('index_create')
    expect(row).toHaveTextContent('hypopg')
    expect(row).toHaveTextContent('5 of 6 improved')
    expect(row).toHaveTextContent('25-50%: 5/6')
    expect(row).toHaveTextContent('predicted 38%, observed 30%')
    const guc = screen.getByTestId('tuning-calibration-guc-model')
    expect(guc).toHaveTextContent('uncalibrated')
    expect(guc).toHaveTextContent('2 of 5 outcomes')
    expect(guc).not.toHaveTextContent('%:')
  })

  it('says when there are no outcomes yet', async () => {
    mockFetch({ database: 'orders', min_outcomes: 5, window_days: 180, classes: [] })
    render(<TuningCalibration database="orders" />)
    expect(await screen.findByTestId('tuning-calibration-empty'))
      .toHaveTextContent('uncalibrated')
  })

  it('asks for one database instead of mixing ledgers', () => {
    globalThis.fetch = vi.fn()
    render(<TuningCalibration database="all" />)
    expect(screen.getByTestId('tuning-calibration-pick')).toBeInTheDocument()
    expect(globalThis.fetch).not.toHaveBeenCalled()
  })

  it('reports a failed load', async () => {
    mockFetch({ error: 'boom' }, false, 500)
    render(<TuningCalibration database="orders" />)
    await waitFor(() => expect(screen.getByRole('alert'))
      .toHaveTextContent('Could not load the tuning calibration'))
  })
})
