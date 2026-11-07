// @vitest-environment node
import { mkdirSync, readFileSync, writeFileSync } from 'node:fs'
import { dirname } from 'node:path'
import { fileURLToPath } from 'node:url'
import { describe, expect, it } from 'vitest'
import { DEFAULT_FORM, requestBody } from './tokenApi'

// Contract (agentgov-01): the body the MCP Tokens page posts by default is
// written to testdata/default-create.json, and the Go test
// internal/api/mcp_token_contract_integration_test.go posts that file to the
// real handler on real PostgreSQL and expects 201. A mocked 201 in the page
// tests can no longer hide drift between the page and the server.
//
// The form is the page's default with only the required fields filled: a
// name and an owner. The Go test replaces owner_user_id with a real user.

const FIXTURE = fileURLToPath(new URL('./testdata/default-create.json', import.meta.url))

function defaultFilledBody() {
  return requestBody({ ...DEFAULT_FORM, name: 'contract-default-form', owner: '1' })
}

function readFixture() {
  try {
    return readFileSync(FIXTURE, 'utf8').replace(/\r\n/g, '\n')
  } catch (err) {
    if (err?.code === 'ENOENT') return null
    throw err
  }
}

describe('MCP tokens default-create contract fixture', () => {
  it('builds an operator body with an owner from the default form', () => {
    expect(defaultFilledBody()).toEqual({
      name: 'contract-default-form', kind: 'operator', scopes: ['read'],
      databases: ['*'], expires_in_days: 30, owner_user_id: 1,
    })
  })

  it('keeps testdata/default-create.json equal to the default body', () => {
    const want = `${JSON.stringify(defaultFilledBody(), null, 2)}\n`
    const have = readFixture()
    if (have !== want) {
      mkdirSync(dirname(FIXTURE), { recursive: true })
      writeFileSync(FIXTURE, want)
    }
    // A changed fixture fails this run once, so the regenerated file is
    // committed and the Go contract test posts what the page now sends.
    expect(have, `regenerated ${FIXTURE}; commit it`).toBe(want)
  })
})
