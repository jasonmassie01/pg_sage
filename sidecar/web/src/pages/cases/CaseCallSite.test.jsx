import { fireEvent, render, screen } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { CaseCallSite } from './CaseCallSite'
import { stubFetch } from '../../test/fetchStub'

// pr-01a: a case names the call site of its query on request. The reply
// copies internal/api/call_site_db_test.go. No concurrent access tests:
// one GET after one click.

afterEach(() => vi.unstubAllGlobals())

const exact = {
  configured: true, database: 'shop', queryid: '42', repo: 'acme/shop', host: 'fake',
  summary: 'Called from app/models/order.rb:3 (exact)',
  call_site: { path: 'app/models/order.rb', line: 3, confidence: 'exact',
    method: 'sqlcommenter file and line' },
}

describe('CaseCallSite', () => {
  it('asks the code host only when the operator asks', async () => {
    const fetch = stubFetch({ '/api/v1/queries/42/call-site': { json: exact } })
    render(<CaseCallSite queryid="42" database="shop" />)
    expect(fetch).not.toHaveBeenCalled()
    fireEvent.click(screen.getByRole('button', { name: 'Find the call site in the code' }))
    expect(await screen.findByTestId('query-call-site'))
      .toHaveTextContent('Called from app/models/order.rb:3 (exact)')
    expect(fetch).toHaveBeenCalledTimes(1)
    expect(screen.queryByRole('button', { name: /call site/ })).toBeNull()
  })

  it('offers nothing without a numeric queryid or one database', () => {
    const fetch = stubFetch({})
    const { container, rerender } = render(<CaseCallSite queryid="12abc" database="shop" />)
    expect(container).toBeEmptyDOMElement()
    rerender(<CaseCallSite queryid="42" database="all" />)
    expect(container).toBeEmptyDOMElement()
    rerender(<CaseCallSite queryid="42" database="" />)
    expect(container).toBeEmptyDOMElement()
    rerender(<CaseCallSite queryid={undefined} database="shop" />)
    expect(container).toBeEmptyDOMElement()
    expect(fetch).not.toHaveBeenCalled()
  })
})
