import { describe, expect, it } from 'vitest'
import { canOperate, runningStatus } from './runbookStatus'

// The status line says which version runs and what the latest version
// waits for (playbooks-10).

const signedV = n => ({ version: n, signed_at: 't', signature_valid: true })

describe('runningStatus', () => {
  it('names the running version and the staged draft', () => {
    expect(runningStatus({ status: 'signed', latest_version: 3, running_version: 3,
      latest: signedV(3) })).toBe('v3 signed and running')
    expect(runningStatus({ status: 'draft', latest_version: 4, running_version: 3,
      latest: { version: 4, signed_at: null } }))
      .toBe('v3 signed and running; v4 draft awaiting signature')
  })

  it('says nothing runs when no version was ever signed', () => {
    expect(runningStatus({ status: 'draft', latest_version: 1, running_version: 0,
      latest: { version: 1, signed_at: null } })).toBe('v1 draft: runbook not running')
    expect(runningStatus({ status: 'draft', latest_version: 1, latest: {} }))
      .toBe('v1 draft: runbook not running')
  })

  it('flags a tampered latest signature while the older version runs', () => {
    expect(runningStatus({ status: 'invalid', latest_version: 2, running_version: 1,
      latest: { version: 2, signed_at: 't', signature_valid: false } }))
      .toBe('v1 signed and running; v2 signature invalid, not running')
    expect(runningStatus({ status: 'invalid', latest_version: 2, running_version: 1,
      latest: signedV(2) })).toBe('v1 signed and running; v2 invalid, not running')
    expect(runningStatus({ status: 'invalid', latest_version: 1, running_version: 0,
      latest: { version: 1, signed_at: 't', signature_valid: false } }))
      .toBe('v1 invalid: runbook not running')
  })

  it('reports a retired runbook as not running whatever its versions', () => {
    expect(runningStatus({ status: 'retired', latest_version: 2, running_version: 0,
      latest: signedV(2) })).toBe('retired: runbook not running')
  })
})

describe('canOperate', () => {
  it('allows operators and admins only', () => {
    expect(canOperate({ role: 'admin' })).toBe(true)
    expect(canOperate({ role: 'operator' })).toBe(true)
    expect(canOperate({ role: 'viewer' })).toBe(false)
    expect(canOperate(null)).toBe(false)
  })
})
