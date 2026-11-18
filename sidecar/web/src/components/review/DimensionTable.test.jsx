import { fireEvent, render, screen, within } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import { DimensionTable } from './DimensionTable'
import { dimension, gradedRun } from '../../test/reviewFixtures'

// review-01b: eight rows, each with its letter or "Unknown: <reason>", and
// the checks behind it. No concurrent access tests: a pure render.

describe('DimensionTable', () => {
  it('lists the eight dimensions in rubric order with their letters', () => {
    render(<DimensionTable dimensions={gradedRun().grade.dimensions} />)
    const rows = screen.getAllByTestId('dimension-row')
    expect(rows).toHaveLength(8)
    expect(rows.map(r => within(r).getByTestId('dimension-title').textContent)).toEqual([
      'Recoverability', 'Change safety', 'Workload and plan stability', 'Schema',
      'Capacity and cost', 'Security and access', 'Configuration', 'Lifecycle'])
    expect(rows.map(r => within(r).getByTestId('letter-badge').textContent))
      .toEqual(['C', 'A', 'B', 'B', 'A', 'B', 'A', 'A'])
  })

  it('shows the checks behind a dimension with their status and reason', () => {
    const dims = [dimension('schema', 'Schema', null)]
    render(<DimensionTable dimensions={dims} />)
    const row = screen.getByTestId('dimension-row')
    expect(within(row).getByTestId('letter-badge'))
      .toHaveTextContent('Unknown: coverage 40%, below 60%')
    fireEvent.click(within(row).getByText('2 checks'))
    const checks = within(row).getAllByTestId('dimension-check')
    expect(checks.map(c => c.textContent)).toEqual([
      expect.stringContaining('Pass'), expect.stringContaining('Unknown')])
    expect(checks[0]).toHaveTextContent('schema.first')
    expect(checks[1]).toHaveTextContent('the producer has not run yet')
  })

  it('renders an invalid letter or score as "Unknown: invalid grade"', () => {
    const dims = [
      dimension('schema', 'Schema', 'Z'),
      dimension('lifecycle', 'Lifecycle', 'A', { score: 101 }),
      dimension('configuration', 'Configuration', 'A', { score: -1 }),
    ]
    render(<DimensionTable dimensions={dims} />)
    for (const badge of screen.getAllByTestId('letter-badge')) {
      expect(badge).toHaveTextContent('Unknown: invalid grade')
    }
  })

  it('shows a 100-character reason in full and truncates 101 with a title', () => {
    const full = 'r'.repeat(100)
    const long = 's'.repeat(101)
    const dims = [dimension('schema', 'Schema', 'B', { checks: [
      { check: 'schema.a', status: 'unknown', weight: 1, evidence: [], reason: full },
      { check: 'schema.b', status: 'unknown', weight: 1, evidence: [], reason: long },
    ] })]
    render(<DimensionTable dimensions={dims} />)
    fireEvent.click(screen.getByText('2 checks'))
    const reasons = screen.getAllByTestId('check-reason')
    expect(reasons[0].textContent).toBe(full)
    expect(reasons[0]).not.toHaveAttribute('title')
    expect(reasons[1].textContent).toHaveLength(100)
    expect(reasons[1]).toHaveAttribute('title', long)
  })

  it('says so when there are no dimensions instead of an empty table', () => {
    render(<DimensionTable dimensions={[]} />)
    expect(screen.getByText('This grade has no dimensions.')).toBeInTheDocument()
    expect(screen.queryAllByTestId('dimension-row')).toHaveLength(0)
  })

  it('treats a missing check list as no checks', () => {
    render(<DimensionTable dimensions={[{ dimension: 'x', title: 'X', letter: 'A', score: 95,
      coverage: 100 }]} />)
    expect(screen.getByTestId('dimension-row')).toHaveTextContent('0 checks')
  })
})
