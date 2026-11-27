// Attempts of one trigger (sre-01). A run that expired in the queue or on
// stale evidence while its incident stayed open is followed by a new
// attempt, which the expired run links to; a later attempt names the run
// it replaces. After the last attempt (5) none starts by itself.

const MAX_ATTEMPTS = 5
const RETRIED = new Set(['queue_expired', 'stale_evidence'])

const strong = { color: 'var(--text-primary)' }
const linkStyle = { color: 'var(--accent)' }

function AttemptLink({ id, label, onOpen }) {
  return (
    <a href={`#investigation-${id}`} className="ml-1 underline" style={linkStyle}
      onClick={e => { e.preventDefault(); onOpen(id) }}>
      {label}
    </a>
  )
}

export function AttemptNotice({ investigation, onOpen }) {
  if (!investigation) return null
  const attempt = Number(investigation.attempt) || 1
  const next = investigation.superseded_by
  const exhausted = !next && investigation.state === 'expired' &&
    RETRIED.has(investigation.failure_code) && attempt >= MAX_ATTEMPTS
  return (
    <>
      {next && (
        <div data-testid="investigation-new-attempt" style={strong}>
          A new attempt was started because this run expired while the incident stayed open.
          <AttemptLink id={next} label={`Open attempt ${attempt + 1}`} onOpen={onOpen} />
        </div>
      )}
      {attempt > 1 && (
        <div data-testid="investigation-attempt" style={strong}>
          Attempt {attempt}: it replaces an earlier run that expired.
          {investigation.supersedes && (
            <AttemptLink id={investigation.supersedes} label="Open the earlier run"
              onOpen={onOpen} />
          )}
        </div>
      )}
      {exhausted && (
        <div data-testid="investigation-attempts-exhausted" style={strong}>
          This was attempt {attempt} of {MAX_ATTEMPTS}. No further attempt starts
          automatically; an operator can start a new investigation of the issue.
        </div>
      )}
    </>
  )
}
