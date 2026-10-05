import { useState } from 'react'
import { MAX_QUESTION_LENGTH } from '../../lib/ask'

// Composer: the question box. Ctrl/Cmd+Enter submits, a plain Enter adds a
// new line. Input longer than the limit is cut to the limit. onAsk returns
// true when the question was answered, which clears the box unless the
// user has typed something new meanwhile.

const muted = { color: 'var(--text-secondary)' }

export function Composer({ busy, onAsk, onNewConversation }) {
  const [text, setText] = useState('')
  const question = text.trim()
  const canAsk = question !== '' && !busy

  async function submit() {
    if (!canAsk) return
    // Keep anything typed while the request was in flight.
    if (await onAsk(question)) setText(t => (t.trim() === question ? '' : t))
  }

  function onKeyDown(e) {
    if (e.key === 'Enter' && (e.ctrlKey || e.metaKey)) {
      e.preventDefault()
      submit()
    }
  }

  return (
    <div className="space-y-2">
      <textarea data-testid="ask-input" aria-label="Question" rows={3}
        maxLength={MAX_QUESTION_LENGTH} value={text}
        onChange={e => setText(e.target.value.slice(0, MAX_QUESTION_LENGTH))}
        onKeyDown={onKeyDown}
        placeholder="Ask about this database (Ctrl+Enter to send)"
        className="w-full rounded p-2 text-sm"
        style={{ background: 'var(--bg-card)', color: 'var(--text-primary)',
          border: '1px solid var(--border)' }} />
      <div className="flex items-center gap-2">
        <button type="button" data-testid="ask-submit" disabled={!canAsk} onClick={submit}
          className="rounded px-3 py-1 text-sm"
          style={{ background: 'var(--accent)', color: '#fff', opacity: canAsk ? 1 : 0.5 }}>
          {busy ? 'Asking…' : 'Ask'}
        </button>
        <button type="button" data-testid="ask-new-conversation" onClick={onNewConversation}
          className="rounded px-3 py-1 text-sm"
          style={{ ...muted, border: '1px solid var(--border)' }}>
          New conversation
        </button>
        <span data-testid="ask-char-count" className="ml-auto text-xs" style={muted}>
          {text.length} / {MAX_QUESTION_LENGTH}
        </span>
      </div>
    </div>
  )
}
