import { render, screen } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { QueryCallSite } from './QueryCallSite'
import { stubFetch } from '../test/fetchStub'

// pr-01a: the query page names the line of application code that sends
// the statement. Replies copy internal/api/call_site_db_test.go and
// call_site_test.go. No concurrent access tests: one polled GET.

afterEach(() => vi.unstubAllGlobals())

const url = '/api/v1/queries/42/call-site'

function renderCallSite(reply) {
  const fetch = stubFetch({ [url]: reply })
  render(<QueryCallSite queryid="42" database="shop db" />)
  return fetch
}

describe('QueryCallSite', () => {
  it('shows an exact call site with its confidence', async () => {
    const fetch = renderCallSite({ json: {
      configured: true, database: 'shop db', queryid: '42', repo: 'acme/shop',
      host: 'github api.github.com', summary: 'Called from app/models/order.rb:42 (exact)',
      call_site: { query_id: '42', path: 'app/models/order.rb', line: 42,
        method: 'sqlcommenter file and line', confidence: 'exact' },
    } })
    const line = await screen.findByTestId('query-call-site')
    expect(line).toHaveTextContent('Called from app/models/order.rb:42 (exact)')
    expect(line).toHaveTextContent('acme/shop')
    expect(line).toHaveTextContent('sqlcommenter file and line')
    expect(fetch.mock.calls[0][0]).toBe(`${url}?database=shop%20db`)
  })

  it('lists several likely candidates without picking one', async () => {
    renderCallSite({ json: {
      configured: true, repo: 'acme/shop', host: 'fake',
      summary: 'Called from one of 2 places (likely): a.rb:3, b.rb:6',
      call_site: { method: 'sqlcommenter controller, action or route search',
        confidence: 'likely',
        candidates: [{ path: 'a.rb', line: 3 }, { path: 'b.rb', line: 6 }] },
    } })
    const items = await screen.findAllByTestId('call-site-candidate')
    expect(items.map(i => i.textContent)).toEqual(['a.rb:3', 'b.rb:6'])
    expect(screen.getByTestId('query-call-site')).toHaveTextContent('(likely)')
  })

  it('gives the guidance when no call site is found', async () => {
    renderCallSite({ json: {
      configured: true, repo: 'acme/shop', host: 'fake',
      summary: 'No call site found (none): add sqlcommenter tags to the queries',
      call_site: { method: 'none', confidence: 'none',
        note: 'add sqlcommenter tags to the queries' },
    } })
    expect(await screen.findByTestId('query-call-site'))
      .toHaveTextContent('add sqlcommenter tags')
  })

  it('says how to turn lookups on when no code host is configured', async () => {
    renderCallSite({ json: { configured: false, database: 'shop db', queryid: '42',
      note: 'No code host is configured: set agents.code_host.provider and repo.' } })
    expect(await screen.findByTestId('query-call-site-off'))
      .toHaveTextContent('agents.code_host')
    expect(screen.queryByTestId('query-call-site')).toBeNull()
  })

  it('shows the code host error instead of a call site', async () => {
    renderCallSite({ status: 502, json: {
      error: 'codehost: access denied: token cannot read acme/shop (HTTP 403)' } })
    expect(await screen.findByTestId('query-call-site-error'))
      .toHaveTextContent('token cannot read acme/shop')
    expect(screen.queryByTestId('query-call-site')).toBeNull()
  })
})
