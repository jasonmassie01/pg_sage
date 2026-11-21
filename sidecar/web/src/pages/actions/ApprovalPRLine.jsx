// cli-02b: a card's approval pull request (card.approval_pr). With
// policy.approval_prs on, a change the remote approval cap allows is also
// open as a PR on the config repository and a merge by a mapped operator
// approves it; this line links the PR, or says why there is none (app_only
// above the cap, a failed open pg_sage retries, PR mode turned off) or how
// the PR was decided.

const MUTED = { color: 'var(--text-secondary)' }

function PRLink({ note }) {
  const label = `${note.repo}#${note.pr_number}`
  if (!note.pr_url) return label
  return <a href={note.pr_url} target="_blank" rel="noreferrer" className="underline"
    style={{ color: 'var(--accent)' }}>{label}</a>
}

function lineBody(note) {
  switch (note.state) {
    case 'open':
      return <>Also open as <PRLink note={note} />: merging it approves this change.</>
    case 'opening':
      return <>Opening a pull request on {note.repo}.</>
    case 'open_failed':
      return <>{note.detail}; pg_sage retries.</>
    case 'app_only':
      return <>{note.detail}</>
    case 'disabled':
      return <>Approval pull requests are off: {note.detail}</>
    default:
      return <><PRLink note={note} />: {note.state}{note.detail ? ` (${note.detail})` : ''}</>
  }
}

export function ApprovalPRLine({ note }) {
  if (!note) return null
  return (
    <div data-testid="approval-pr-line" className="text-xs" style={MUTED}>
      {lineBody(note)}
    </div>
  )
}
