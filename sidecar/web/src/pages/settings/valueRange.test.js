import { describe, expect, it } from 'vitest'
import { firstRangeProblem, rangeProblem } from './valueRange'

// config-02b: the Settings page checks config_meta.json's min, max and
// enum (the API's own contract) before it sends a PUT.
describe('rangeProblem', () => {
  it('accepts the bounds and refuses the values just outside them', () => {
    expect(rangeProblem('collector.interval_seconds', '5')).toBeNull()
    expect(rangeProblem('collector.interval_seconds', '1073741824')).toBeNull()
    expect(rangeProblem('collector.interval_seconds', '4')).toBe(
      'collector.interval_seconds must be 5-1073741824, got 4')
    expect(rangeProblem('collector.interval_seconds', '1073741825')).toMatch(
      /^collector.interval_seconds must be 5-1073741824/)
  })

  it('accepts 0 and 100 on a percentage key', () => {
    expect(rangeProblem('analyzer.regression_threshold_pct', '0')).toBeNull()
    expect(rangeProblem('analyzer.regression_threshold_pct', '100')).toBeNull()
    expect(rangeProblem('analyzer.regression_threshold_pct', '-1')).not.toBeNull()
  })

  it('refuses a non-number and a fraction on an integer key', () => {
    expect(rangeProblem('retention.snapshots_days', 'abc')).toBe(
      'retention.snapshots_days must be a number, got "abc"')
    expect(rangeProblem('retention.snapshots_days', '')).not.toBeNull()
    expect(rangeProblem('retention.snapshots_days', '1.5')).toBe(
      'retention.snapshots_days must be a whole number, got "1.5"')
  })

  it('takes fractions on a float key', () => {
    expect(rangeProblem('analyzer.cache_hit_ratio_warning', '0.95')).toBeNull()
    expect(rangeProblem('analyzer.cache_hit_ratio_warning', '1.01')).toBe(
      'analyzer.cache_hit_ratio_warning must be 0-1, got 1.01')
  })

  it('checks enums', () => {
    expect(rangeProblem('mcp.transport', 'http')).toBeNull()
    expect(rangeProblem('mcp.transport', 'grpc')).toBe(
      'mcp.transport must be one of stdio, http, got "grpc"')
  })

  it('leaves keys without a contract alone', () => {
    expect(rangeProblem('llm.model', 'anything')).toBeNull()
    expect(rangeProblem('execution_mode', 'auto')).toBeNull()
    expect(rangeProblem('no.such_key', '-5')).toBeNull()
  })
})

describe('firstRangeProblem', () => {
  it('returns null for no edits and the first problem otherwise', () => {
    expect(firstRangeProblem({})).toBeNull()
    expect(firstRangeProblem(null)).toBeNull()
    expect(firstRangeProblem({ 'llm.model': 'x', 'retention.snapshots_days': '0' }))
      .toBe('retention.snapshots_days must be 1-1073741824, got 0')
  })
})
