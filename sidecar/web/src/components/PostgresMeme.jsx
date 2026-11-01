const memes = [
  ['#/advanced/findings', 'Findings explorer', 'Just one harmless sequential scan.', 'One million executions later: we should talk.', '-- the planner remembers'],
  ['#/advanced/actions', 'Action history', 'I have a rollback plan.', 'Excellent. Does it contain more than COMMIT?', '-- receipts > reassurance'],
  ['#/advanced/runbooks', 'Runbooks', 'Did you try turning it off and on?', 'This is why the DBA writes the runbook.', '-- step 0: know your primary'],
  ['#/advanced/autonomy', 'Earned autonomy', 'Trust me. I tested the query.', 'In a production-shaped daydream?', '-- trust, then EXPLAIN'],
  ['#/advanced', 'Snapshot & metrics', 'The cache hit ratio is 99.9%.', 'The other 0.1% has opened a ticket.', '-- averages have an alibi'],
  ['#/manage-databases', 'Fleet', 'Every database is fine.', 'Each has a different definition of fine.', '-- SELECT * FROM reality;'],
  ['#/database', 'Database', 'It is only a small table.', 'Small is a state of mind. And a stale statistic.', '-- ANALYZE your assumptions'],
  ['#/cases', 'Cases', 'It only happens sometimes.', 'Excellent. A reproducible feeling.', '-- correlation has entered the chat'],
  ['#/findings', 'Findings', 'Just one harmless sequential scan.', 'One million executions later: we should talk.', '-- the planner remembers'],
  ['#/actions', 'Actions', 'I have a rollback plan.', 'Excellent. Does it contain more than COMMIT?', '-- receipts > reassurance'],
  ['#/slos', 'SLOs', 'Everything is green.', 'The denominator has left the chat.', '-- no data is not zero errors'],
  ['#/trust', 'Trust', 'Can we enable autonomous mode?', 'First, show me the WHERE clause.', '-- least privilege. maximum side-eye.'],
  ['#/facts', 'Facts', 'It worked on my laptop.', 'Great. Is your laptop the primary?', '-- evidence beats folklore'],
  ['#/ask', 'Ask Sage', 'Just one quick question.', 'Postgres: define “quick.”', '-- EXPLAIN yourself'],
  ['#/settings', 'Settings', 'It says work_mem.', 'It did not say whole_server_mem.', '-- per operation. yes, really.'],
  ['#/notifications', 'Notifications', 'We solved alert fatigue.', 'Now every alert whispers “VACUUM.”', '-- quiet is a feature'],
  ['#/alert-log', 'Alert log', 'The alert resolved itself.', 'So did my confidence in that alert.', '-- retain the evidence'],
  ['#/users', 'Users', 'Can I have superuser?', 'Best I can do is least privilege.', '-- GRANT good_judgment TO everyone;'],
  ['#/profile', 'Profile', 'It is always DNS.', 'Until it is your role membership.', '-- current_user has questions'],
  ['#/agents', 'Agents', 'The agent says it is safe.', 'The approval ledger would like a word.', '-- humans still own COMMIT'],
  ['#/mcp-tokens', 'MCP tokens', 'I put the token somewhere safe.', 'Please tell me “somewhere” is not a screenshot.', '-- secrets do not belong in query plans'],
  ['#/link-sso', 'Identity', 'It is always DNS.', 'Until it is your identity provider.', '-- authentication is not authorization'],
  ['#/login', 'Sign in', 'I know who I am.', 'The database would like some evidence.', '-- authentication is not authorization'],
  ['#/', 'Value', 'How much time did the index save?', 'Enough to finally drink the coffee warm.', '-- measure twice. CREATE INDEX once.'],
]

function memeForRoute(route) {
  const path = String(route || '#/').split('?')[0]
  return memes.find(([prefix]) => path === prefix || (prefix !== '#/' && path.startsWith(`${prefix}/`)))
    || ['#/', 'Postgres operations', 'We need to go deeper.', 'There is always another nested loop.', '-- welcome to the query plan']
}

function ElephantDBA() {
  return (
    <svg viewBox="0 0 148 104" role="img"
      aria-label="An elephant DBA with a headset, a query terminal, and a coffee"
      className="postgres-meme-art">
      <path d="M12 91h125" stroke="currentColor" opacity=".2" />
      <path d="M42 81c-15-4-24-16-23-34 1-17 14-29 31-25l16 13-3 41Z"
        fill="#28657a" stroke="#7ac9d7" strokeWidth="1.5" />
      <path d="M49 25c13-12 35-11 48 3 9 10 11 26 7 38l-9 21H54L43 65c-6-17-6-30 6-40Z"
        fill="#65a8b6" stroke="#a7e2df" strokeWidth="1.5" />
      <path d="M82 51c4 8 9 14 10 25 1 9-3 15-11 16-8 1-12-5-9-12 2-5 6-7 11-5"
        fill="none" stroke="#a7e2df" strokeWidth="10" strokeLinecap="round" />
      <path d="M47 41c5-14 24-21 39-11" fill="none" stroke="#162d39" strokeWidth="6" strokeLinecap="round" />
      <rect x="40" y="43" width="9" height="21" rx="4" fill="#172c38" stroke="#72e6c7" />
      <path d="m45 61 6 9h14" fill="none" stroke="#72e6c7" strokeWidth="2" />
      <circle cx="67" cy="70" r="3" fill="#72e6c7" />
      <path d="m57 43 8-1m18 1 8 2" stroke="#173744" strokeWidth="2.5" strokeLinecap="round" />
      <circle cx="63" cy="50" r="2.5" fill="#10222d" /><circle cx="87" cy="52" r="2.5" fill="#10222d" />
      <path d="m52 83 12-7 8 9" fill="#264b5d" />
      <rect x="14" y="74" width="46" height="25" rx="4" fill="#0a1822" stroke="#3e6978" />
      <path d="m20 82 4 3-4 3m8 1h12" fill="none" stroke="#72e6c7" strokeWidth="1.5" />
      <path d="M11 99h52" stroke="#73a5b0" strokeWidth="2" strokeLinecap="round" />
      <path d="M112 76h15v17h-15Zm15 3h4c6 0 6 10 0 10h-4" fill="#d7aa72" stroke="#f4d5a6" strokeWidth="1.5" />
      <path d="m117 69-1-5 3-5m5 9-1-4 2-4" fill="none" stroke="#d7aa72" strokeWidth="1.5" strokeLinecap="round" opacity=".7" />
      <path d="m112 25 4-5m5 10 6-2" stroke="#72e6c7" strokeWidth="1.5" strokeLinecap="round" />
    </svg>
  )
}

/** Original, route-aware DBA humor. It is a captioned illustration, never a status signal. */
export function PostgresMeme({ route = window.location.hash, compact = false }) {
  const [, page, setup, punchline, comment] = memeForRoute(route)
  return (
    <figure className={`postgres-meme${compact ? ' postgres-meme-compact' : ''}`}
      data-testid="postgres-meme" aria-label={`PostgreSQL meme for ${page}`}>
      <div className="postgres-meme-illustration"><ElephantDBA /></div>
      <figcaption className="postgres-meme-caption">
        <div className="postgres-meme-eyebrow"><span>THE DBA BREAKROOM</span><span>pg_sage originals</span></div>
        <p className="postgres-meme-setup">{setup}</p>
        <p className="postgres-meme-punchline">{punchline}</p>
      </figcaption>
      <code className="postgres-meme-comment">{comment}</code>
    </figure>
  )
}
