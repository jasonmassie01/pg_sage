import { Component } from 'react'

// Render errors only: event-handler and fetch errors never reach a boundary
// and keep their own toasts and banners.

function errorText(error) {
  if (error instanceof Error) return error.message || 'Unknown error'
  if (error === null || error === undefined || error === '') return 'Unknown error'
  return String(error)
}

const card = {
  background: 'var(--bg-card)',
  border: '1px solid var(--border)',
}

function PageRenderError({ error, onRetry }) {
  return (
    <div role="alert" data-testid="page-error" className="rounded p-5" style={card}>
      <h2 className="text-lg font-semibold mb-2" style={{ color: 'var(--text-primary)' }}>
        This page failed to render
      </h2>
      <p className="text-sm mb-1" style={{ color: 'var(--text-secondary)' }}>
        The header, navigation and emergency stop still work.
      </p>
      <pre className="text-xs whitespace-pre-wrap mb-4" data-testid="page-error-message"
        style={{ color: 'var(--red, #e53e3e)' }}>
        {errorText(error)}
      </pre>
      <div className="flex items-center gap-4">
        <button type="button" onClick={onRetry} className="px-3 py-1.5 rounded text-sm"
          style={{ background: 'var(--accent)', color: '#fff' }}>
          Retry
        </button>
        <a href="#/" className="text-sm underline" style={{ color: 'var(--accent)' }}>
          Go to Inbox
        </a>
      </div>
    </div>
  )
}

// PageErrorBoundary contains a render error to the page area. A new
// resetKey (the route) clears the error without remounting a healthy page.
// Retry clears it too; the fallback already unmounted the page, so the page
// mounts afresh.
export class PageErrorBoundary extends Component {
  constructor(props) {
    super(props)
    this.state = { failed: false, error: null, resetKey: props.resetKey }
    this.retry = this.retry.bind(this)
  }

  static getDerivedStateFromError(error) {
    return { failed: true, error }
  }

  static getDerivedStateFromProps(props, state) {
    if (props.resetKey === state.resetKey) return null
    return { failed: false, error: null, resetKey: props.resetKey }
  }

  retry() {
    this.setState({ failed: false, error: null })
  }

  render() {
    if (this.state.failed) {
      return <PageRenderError error={this.state.error} onRetry={this.retry} />
    }
    return this.props.children
  }
}

// RootErrorBoundary is the last resort when the shell itself fails: a plain
// Reload, since nothing above it can be trusted to render.
export class RootErrorBoundary extends Component {
  constructor(props) {
    super(props)
    this.state = { failed: false }
  }

  static getDerivedStateFromError() {
    return { failed: true }
  }

  render() {
    if (!this.state.failed) return this.props.children
    const reload = this.props.reload || (() => window.location.reload())
    return (
      <div role="alert" data-testid="app-error" className="p-6">
        <h1 className="text-lg font-semibold mb-2">pg_sage failed to load</h1>
        <button type="button" onClick={() => reload()} className="px-4 py-2 rounded"
          style={{ background: 'var(--accent)', color: '#fff' }}>
          Reload
        </button>
      </div>
    )
  }
}
