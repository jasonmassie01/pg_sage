import { render, screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import { ApprovalPRLine } from './ApprovalPRLine'

// cli-02b: the inbox card's approval pull request line. No concurrent
// access tests: a pure render of one prop.

describe('ApprovalPRLine', () => {
  it('renders nothing without a note', () => {
    const { container } = render(<ApprovalPRLine note={null} />)
    expect(container).toBeEmptyDOMElement()
  })

  it('links an open PR and says a merge approves', () => {
    render(<ApprovalPRLine note={{ state: 'open', repo: 'acme/config', pr_number: 3,
      pr_url: 'https://github.com/acme/config/pull/3' }} />)
    const line = screen.getByTestId('approval-pr-line')
    expect(line).toHaveTextContent('Also open as acme/config#3')
    expect(line).toHaveTextContent('merging it approves this change')
    expect(screen.getByRole('link', { name: 'acme/config#3' }))
      .toHaveAttribute('href', 'https://github.com/acme/config/pull/3')
  })

  it('says why a change stays app-only', () => {
    render(<ApprovalPRLine note={{ state: 'app_only',
      detail: 'Not opened as a pull request: risk tier high is approved in pg_sage only; ' +
        'approve it here.' }} />)
    expect(screen.getByTestId('approval-pr-line'))
      .toHaveTextContent('risk tier high is approved in pg_sage only')
  })

  it('shows a failed open with its status and the retry', () => {
    render(<ApprovalPRLine note={{ state: 'open_failed', repo: 'acme/config',
      detail: 'PR not opened: HTTP 503' }} />)
    const line = screen.getByTestId('approval-pr-line')
    expect(line).toHaveTextContent('PR not opened: HTTP 503')
    expect(line).toHaveTextContent('pg_sage retries')
  })

  it('shows PR mode turned off', () => {
    render(<ApprovalPRLine note={{ state: 'disabled',
      detail: 'the code host refused the token (HTTP 401)' }} />)
    expect(screen.getByTestId('approval-pr-line'))
      .toHaveTextContent('Approval pull requests are off: the code host refused the token')
  })

  it('names a decided PR and why', () => {
    render(<ApprovalPRLine note={{ state: 'refused', repo: 'acme/config', pr_number: 4,
      pr_url: 'https://github.com/acme/config/pull/4',
      detail: 'merged by octo, who lacks the operator role on orders' }} />)
    const line = screen.getByTestId('approval-pr-line')
    expect(line).toHaveTextContent('acme/config#4: refused')
    expect(line).toHaveTextContent('lacks the operator role')
  })
})
