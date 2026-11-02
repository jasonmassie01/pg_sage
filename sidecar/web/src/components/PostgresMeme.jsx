import { useEffect, useRef, useSyncExternalStore } from 'react'
import { Pause, Play, Shuffle } from 'lucide-react'
import { templates, memes } from './postgresMemes'
import { memeRotation, MEME_ROTATION_MS } from './memeRotation'

const pages = [
  ['#/advanced/findings', 'Findings explorer'], ['#/advanced/actions', 'Action history'],
  ['#/advanced/runbooks', 'Runbooks'], ['#/advanced/autonomy', 'Earned autonomy'],
  ['#/advanced', 'Snapshot & metrics'], ['#/manage-databases', 'Fleet'],
  ['#/database', 'Database'], ['#/cases', 'Cases'], ['#/findings', 'Findings'],
  ['#/actions', 'Actions'], ['#/slos', 'SLOs'], ['#/trust', 'Trust'], ['#/facts', 'Facts'],
  ['#/ask', 'Ask Sage'], ['#/settings', 'Settings'], ['#/notifications', 'Notifications'],
  ['#/alerts', 'Alert log'], ['#/alert-log', 'Alert log'], ['#/users', 'Users'],
  ['#/profile', 'Profile'], ['#/agents', 'Agents'], ['#/mcp-tokens', 'MCP tokens'],
  ['#/link-sso', 'Identity'], ['#/login', 'Sign in'], ['#/forecasts', 'Forecasts'],
  ['#/query-hints', 'Query hints'], ['#/schema-health', 'Schema health'],
  ['#/incidents', 'Incidents'], ['#/', 'Value'],
]

export function PostgresMeme({ route = window.location.hash, compact = false, rotation = memeRotation, guide }) {
  const path = String(route || '#/').split('?')[0]
  const page = pages.find(([prefix]) => path === prefix || (prefix !== '#/' && path.startsWith(`${prefix}/`)))?.[1]
    || 'Postgres operations'
  const { meme, paused } = useSyncExternalStore(rotation.subscribe, rotation.getSnapshot)
  const template = templates[meme.template]
  const figure = useRef(null)

  useEffect(() => rotation.visit(path), [rotation, path])
  useEffect(() => {
    if (paused) return
    const timer = window.setInterval(() => {
      // Do not change a meme someone is interacting with or consume cards in
      // background tabs. Rotation makes no API/model/database requests.
      if (document.visibilityState !== 'hidden' && !figure.current?.contains(document.activeElement)) rotation.next()
    }, MEME_ROTATION_MS)
    return () => window.clearInterval(timer)
  }, [rotation, paused, meme.id])

  return (
    <figure ref={figure} className={`postgres-meme${compact ? ' postgres-meme-compact' : ''}${guide ? ' postgres-meme-with-guide' : ''}`}
      data-testid="postgres-meme" data-meme-id={meme.id}
      aria-label={`PostgreSQL meme for ${page}`}>
      <div className="postgres-meme-media">
        <div className={`postgres-meme-poster postgres-meme-${meme.template}`}
          style={{ '--meme-ratio': template.width / template.height }}>
          <img src={`/memes/${template.file}`} alt={template.alt}
            width={template.width} height={template.height} loading="eager" decoding="async" />
          {meme.captions.map((caption, index) => (
            <span key={index} className={`postgres-meme-label postgres-meme-label-${index + 1}`}>
              <span className="sr-only">{template.roles[index]}: </span>{caption}
            </span>
          ))}
        </div>
      </div>
      {guide && (
        <aside className="postgres-tab-intro" data-testid="tab-introduction"
          aria-label={`About ${guide.title}`}>
          <div className="postgres-tab-intro-eyebrow">WHAT’S HERE</div>
          <h2>About {guide.title}</h2>
          <p>{guide.description}</p>
        </aside>
      )}
      <figcaption className="postgres-meme-copy">
        <div className="postgres-meme-eyebrow">THE DBA BREAKROOM</div>
        <div className="postgres-meme-template">{template.name} · {memes.length} in the mix</div>
        <div className="postgres-meme-readable" aria-hidden="true">
          {meme.captions.map((caption, index) => <p key={index}>{caption}</p>)}
        </div>
        <div className="postgres-meme-controls">
          <button type="button" onClick={rotation.next} className="postgres-meme-next">
            <Shuffle size={13} aria-hidden="true" /> Another meme
          </button>
          <button type="button" onClick={rotation.togglePaused}
            aria-label={paused ? 'Resume meme rotation' : 'Pause meme rotation'}>
            {paused ? <Play size={12} aria-hidden="true" /> : <Pause size={12} aria-hidden="true" />}
            {paused ? 'Paused' : 'Auto · 60s'}
          </button>
        </div>
        <a className="postgres-meme-credits" href="/memes/CREDITS.md">Template credits</a>
      </figcaption>
    </figure>
  )
}
