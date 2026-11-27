import { render, screen, within } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import { citationKey, citationNumbers } from '../../lib/ask'
import { CitationList, CitationMarkers } from './Citations'

// mcp-05: a citation links to the object it names (the finding's case,
// the action, the investigation), and two reads of one id stay two
// citations, each with its own digest. Answers stored before aliases
// existed still render. No concurrency tests: pure render of props.

const reads = [
  { id: 'finding:42', alias: 'E1', kind: 'finding', ref: '42', label: 'finding:42 ok',
    digest: 'aaaaaaaaaaaaaaaa' },
  { id: 'findings:open', alias: 'E2', kind: 'findings', ref: 'open',
    label: 'findings:open ok', digest: 'bbbbbbbbbbbbbbbb' },
  { id: 'findings:open', alias: 'E3', kind: 'findings', ref: 'open',
    label: 'findings:open ok', digest: 'cccccccccccccccc' },
  { id: 'action:7', alias: 'E4', kind: 'action', ref: '7', label: 'action:7 success',
    digest: 'dddddddddddddddd' },
  { id: 'investigation:9b2e', alias: 'E5', kind: 'investigation', ref: '9b2e',
    label: 'investigation:9b2e done', digest: 'eeeeeeeeeeeeeeee' },
  { id: 'table:public.orders', alias: 'E6', kind: 'table', ref: 'public.orders',
    label: 'table:public.orders ok', digest: 'ffffffffffffffff' },
]

function markers(ids, citations, database = 'orders') {
  const byKey = new Map(citations.map(c => [citationKey(c), c]))
  return render(<p data-testid="st"><CitationMarkers ids={ids}
    numbers={citationNumbers(citations)} byId={byKey} database={database} /></p>)
}

describe('CitationList', () => {
  it('deep-links finding, action and investigation citations', () => {
    render(<CitationList citations={reads} database="orders" />)
    const items = screen.getAllByTestId('ask-citation')
    expect(items).toHaveLength(6)
    const href = i => within(items[i]).getByRole('link').getAttribute('href')
    expect(href(0)).toBe('#/cases?finding=42&database=orders')
    expect(href(3)).toBe('#/actions?action=7&database=orders')
    expect(href(4)).toBe('#/cases?investigation=9b2e&database=orders')
    for (const i of [0, 3, 4]) {
      expect(href(i)).not.toMatch(/^#\/(cases|actions)$/)
    }
    expect(within(items[5]).queryByRole('link')).toBeNull()
  })

  it('keeps two reads of one id as two citations with their own digests', () => {
    render(<CitationList citations={reads} database="orders" />)
    const items = screen.getAllByTestId('ask-citation')
    expect(items[1]).toHaveTextContent('[2]')
    expect(items[1]).toHaveTextContent('bbbbbbbbbbbb')
    expect(items[2]).toHaveTextContent('[3]')
    expect(items[2]).toHaveTextContent('cccccccccccc')
    expect(items[1]).not.toHaveTextContent('cccccccccccc')
  })

  it('renders a stored answer without aliases, linking by its ref', () => {
    const stored = [{ id: 'finding:17', kind: 'finding', ref: '17', label: 'Finding #17',
      digest: 'a1b2c3d4e5f6a7b8' }]
    render(<CitationList citations={stored} database="orders" />)
    const item = screen.getByTestId('ask-citation')
    expect(within(item).getByRole('link', { name: 'Finding #17' }))
      .toHaveAttribute('href', '#/cases?finding=17&database=orders')
    expect(item).toHaveTextContent('a1b2c3d4e5f6')
  })

  it('renders nothing for an empty list', () => {
    const { container } = render(<CitationList citations={[]} database="orders" />)
    expect(container).toBeEmptyDOMElement()
  })
})

describe('CitationMarkers', () => {
  it('numbers markers by alias and links each to its own object', () => {
    markers(['E1', 'E3', 'E6'], reads)
    const st = screen.getByTestId('st')
    expect(within(st).getByRole('link', { name: '[1]' }))
      .toHaveAttribute('href', '#/cases?finding=42&database=orders')
    expect(within(st).getByRole('link', { name: '[3]' })).toHaveAttribute('href', '#/cases')
    expect(within(st).getByText('[6]').closest('a')).toBeNull()
    expect(st).not.toHaveTextContent('[2]')
  })

  it('falls back to ids for a stored statement without aliases', () => {
    const stored = [{ id: 'action:7', kind: 'action', ref: '7', label: 'a' }]
    markers(['action:7', 'finding:999'], stored)
    const st = screen.getByTestId('st')
    expect(within(st).getByRole('link', { name: '[1]' }))
      .toHaveAttribute('href', '#/actions?action=7&database=orders')
    expect(within(st).getAllByText(/\[\d\]/)).toHaveLength(1)
  })

  it('encodes a hostile reference instead of routing it', () => {
    const hostile = [{ id: 'finding:x', alias: 'E1', kind: 'finding',
      ref: '1&action=2#/trust', label: 'x' }]
    markers(['E1'], hostile)
    const href = screen.getByRole('link', { name: '[1]' }).getAttribute('href')
    expect(href).toBe('#/cases?finding=1%26action%3D2%23%2Ftrust&database=orders')
  })

  it('renders no markers for nil ids', () => {
    markers(undefined, reads)
    expect(screen.getByTestId('st')).toBeEmptyDOMElement()
  })
})

// mcp-05b: "what Sage read". A citation with an excerpt gets a disclosure
// showing the text in a <pre>, through React (never as HTML); a truncated
// one says so; answers stored before excerpts render with no panel.
describe('CitationList excerpts', () => {
  const read = (extra) => [{ id: 'finding:42', alias: 'E1', kind: 'finding', ref: '42',
    label: 'finding:42 ok', digest: 'aaaaaaaaaaaaaaaa', ...extra }]

  it('shows what Sage read in a disclosure', () => {
    render(<CitationList citations={read({ excerpt: 'finding 42 [warning, open]\nseq 4200' })}
      database="orders" />)
    const item = screen.getByTestId('ask-citation')
    expect(within(item).getByText('What Sage read')).toBeInTheDocument()
    const pre = within(item).getByTestId('ask-citation-excerpt')
    expect(pre.tagName).toBe('PRE')
    expect(pre.textContent).toBe('finding 42 [warning, open]\nseq 4200')
    expect(item).not.toHaveTextContent('excerpt truncated')
  })

  it('renders a script in an excerpt as text, never as markup', () => {
    const hostile = '<script>window.pwned = 1</script><img src=x onerror="alert(1)">'
    const { container } = render(<CitationList citations={read({ excerpt: hostile })}
      database="orders" />)
    expect(screen.getByTestId('ask-citation-excerpt').textContent).toBe(hostile)
    expect(container.querySelector('script')).toBeNull()
    expect(container.querySelector('img')).toBeNull()
    expect(window.pwned).toBeUndefined()
  })

  it('says when the excerpt was truncated', () => {
    render(<CitationList citations={read({ excerpt: 'abc', excerpt_truncated: true })}
      database="orders" />)
    expect(screen.getByTestId('ask-citation-excerpt').textContent).toBe('abc')
    expect(screen.getByTestId('ask-citation')).toHaveTextContent('excerpt truncated')
  })

  it('says truncated with no panel when the answer budget left no text', () => {
    render(<CitationList citations={read({ excerpt_truncated: true })} database="orders" />)
    const item = screen.getByTestId('ask-citation')
    expect(item).toHaveTextContent('excerpt truncated')
    expect(within(item).queryByTestId('ask-citation-excerpt')).toBeNull()
    expect(within(item).queryByText('What Sage read')).toBeNull()
  })

  it('renders a stored answer without excerpts with no panel', () => {
    render(<CitationList citations={reads} database="orders" />)
    expect(screen.queryAllByTestId('ask-citation-excerpt')).toHaveLength(0)
    expect(screen.queryByText('What Sage read')).toBeNull()
    expect(screen.getAllByTestId('ask-citation')[0]).not.toHaveTextContent('truncated')
  })

  it('renders no panel for an empty excerpt', () => {
    render(<CitationList citations={read({ excerpt: '' })} database="orders" />)
    expect(screen.queryByTestId('ask-citation-excerpt')).toBeNull()
  })
})
