// ConversationList: the caller's own Ask Sage conversations for this
// database, newest first (server order). Selecting one loads its thread.

const muted = { color: 'var(--text-secondary)' }

export function ConversationList({ resource, selectedId, onSelect }) {
  const items = Array.isArray(resource.data?.conversations)
    ? resource.data.conversations : []
  return (
    <nav data-testid="ask-conversations" aria-label="Conversations" className="space-y-1">
      <h2 className="text-xs font-semibold uppercase" style={muted}>Conversations</h2>
      {resource.error && (
        <p data-testid="ask-conversations-error" className="text-xs"
          style={{ color: 'var(--red)' }}>
          {resource.error}
        </p>
      )}
      {!resource.error && resource.data && items.length === 0 && (
        <p data-testid="ask-conversations-empty" className="text-xs" style={muted}>
          No conversations yet.
        </p>
      )}
      {items.map(c => (
        <button key={c.id} type="button" data-testid={`ask-conversation-${c.id}`}
          aria-pressed={c.id === selectedId} onClick={() => onSelect(c.id)}
          className="block w-full text-left rounded px-2 py-1 text-sm"
          style={{ color: c.id === selectedId ? 'var(--accent)' : 'var(--text-primary)',
            background: c.id === selectedId ? 'var(--bg-hover)' : 'transparent' }}>
          {c.title || c.id}
        </button>
      ))}
    </nav>
  )
}
