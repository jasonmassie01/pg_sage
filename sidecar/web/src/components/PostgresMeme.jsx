const templates = {
  distracted: {
    name: 'Distracted Boyfriend', file: 'distracted-boyfriend.jpg', width: 720, height: 480,
    alt: 'Distracted Boyfriend: a man looks at another woman while his girlfriend looks offended.',
    roles: ['The tempting distraction', 'The distracted person', 'The ignored partner'],
  },
  buttons: {
    name: 'Two Buttons', file: 'two-buttons.jpg', width: 475, height: 720,
    alt: 'Two Buttons: a sweating man struggles to choose between two red buttons.',
    roles: ['First button', 'Second button', 'The person choosing'],
  },
  drake: {
    name: 'Drake Hotline Bling', file: 'drake.jpg', width: 720, height: 720,
    alt: 'Drake Hotline Bling: Drake rejects the top option and approves the bottom option.',
    roles: ['Drake rejects', 'Drake approves'],
  },
  disaster: {
    name: 'Disaster Girl', file: 'disaster-girl.jpg', width: 720, height: 540,
    alt: 'Disaster Girl: a girl gives the camera a knowing smile in front of a burning house.',
    roles: ['Setup', 'Punchline'],
  },
  change: {
    name: 'Change My Mind', file: 'change-my-mind.jpg', width: 720, height: 539,
    alt: 'Change My Mind: a man sits behind a table with a sign inviting people to change his mind.',
    roles: ['The sign'],
  },
  fine: {
    name: 'This Is Fine', file: 'this-is-fine.jpg', width: 720, height: 350,
    alt: 'This Is Fine: a cartoon dog sits in a burning room and says, “This is fine.”',
    roles: ['Setup', 'Punchline'],
  },
}

// Captions are HTML, not baked into the image: readable by assistive technology,
// selectable, and sharp at every size. The photos/comics are bundled locally.
const memes = [
  ['#/advanced/findings', 'Findings explorer', 'buttons', ['ADD AN INDEX', 'ADD TWO INDEXES', 'ME DISCOVERING BTREE']],
  ['#/advanced/actions', 'Action history', 'disaster', ['“JUST ONE QUICK DDL”', 'FRIDAY. 4:59 PM.']],
  ['#/advanced/runbooks', 'Runbooks', 'drake', ['“ASK DAVE”', 'A RUNBOOK THAT WORKS WHILE DAVE IS ON PTO']],
  ['#/advanced/autonomy', 'Earned autonomy', 'buttons', ['AUTOMATE THE FIX', 'KEEP MY PAGER', 'DBA EVALUATING AUTONOMY']],
  ['#/advanced', 'Snapshot & metrics', 'fine', ['CPU: 100%. CACHE HIT: 99.9%.', '“SO IT’S MOSTLY GREEN.”']],
  ['#/manage-databases', 'Fleet', 'disaster', ['I SCHEDULED MAINTENANCE', 'ON ALL PRIMARIES AT ONCE']],
  ['#/database', 'Database', 'distracted', ['SHARDING', 'MY APP', 'ONE MISSING INDEX']],
  ['#/cases', 'Cases', 'distracted', ['A NEW EXTENSION', 'ME', 'THE OPEN SEV-1']],
  ['#/findings', 'Findings', 'buttons', ['ADD AN INDEX', 'ADD TWO INDEXES', 'ME DISCOVERING BTREE']],
  ['#/actions', 'Actions', 'disaster', ['“JUST ONE QUICK DDL”', 'FRIDAY. 4:59 PM.']],
  ['#/slos', 'SLOs', 'fine', ['P99 IS IN SECONDS.', '“BUT THE AVERAGE IS FINE.”']],
  ['#/trust', 'Trust', 'drake', ['“TRUST ME, I’M AN ADMIN”', 'A ROLLBACK WE’VE ACTUALLY TESTED']],
  ['#/facts', 'Facts', 'change', ['“WORKED IN STAGING” IS NOT EVIDENCE.']],
  ['#/ask', 'Ask Sage', 'drake', ['“IT’S PROBABLY THE CACHE”', 'EXPLAIN (ANALYZE, BUFFERS)']],
  ['#/settings', 'Settings', 'drake', ['1 GB work_mem FOR EVERYONE', 'FIXING THE ONE QUERY THAT SPILLS']],
  ['#/notifications', 'Notifications', 'drake', ['EMAIL EVERY DEAD TUPLE', 'PAGE ON SOMETHING ACTIONABLE']],
  ['#/alerts', 'Alert log', 'fine', ['THE ALERT STOPPED FIRING.', '“I TURNED THE ALERT OFF.”']],
  ['#/alert-log', 'Alert log', 'fine', ['THE ALERT STOPPED FIRING.', '“I TURNED THE ALERT OFF.”']],
  ['#/users', 'Users', 'distracted', ['SUPERUSER', 'NEW APP', 'THE SELECT GRANT IT NEEDS']],
  ['#/profile', 'Profile', 'change', ['YOUR JOB TITLE IS NOT A POSTGRES ROLE.']],
  ['#/agents', 'Agents', 'buttons', ['APPROVE THE AGENT', 'READ THE SQL', 'THE HUMAN IN THE LOOP']],
  ['#/mcp-tokens', 'MCP tokens', 'disaster', ['“I LOGGED THE ENTIRE REQUEST”', 'INCLUDING THE BEARER TOKEN']],
  ['#/link-sso', 'Identity', 'drake', ['“BUT I OWN THIS DATABASE”', 'A VALID SESSION']],
  ['#/login', 'Sign in', 'drake', ['“BUT I OWN THIS DATABASE”', 'A VALID SESSION']],
  ['#/forecasts', 'Forecasts', 'buttons', ['ADD CAPACITY NOW', 'WAIT FOR THE PAGER', '“HOW FAST IS IT GROWING?”']],
  ['#/query-hints', 'Query hints', 'distracted', ['FORCING THE PLAN', 'ME', 'UPDATING STATISTICS']],
  ['#/schema-health', 'Schema health', 'disaster', ['“FOREIGN KEYS GET AN INDEX, RIGHT?”', 'POSTGRES: “THAT’S A YOU PROBLEM.”']],
  ['#/incidents', 'Incidents', 'buttons', ['FIX THE INCIDENT', 'FIND WHO CAUSED IT', 'THE FIRST FIVE MINUTES']],
  ['#/', 'Value', 'distracted', ['NEW HARDWARE', 'MY TEAM', 'CREATE INDEX']],
]

function memeForRoute(route) {
  const path = String(route || '#/').split('?')[0]
  return memes.find(([prefix]) => path === prefix || (prefix !== '#/' && path.startsWith(`${prefix}/`)))
    || ['#/', 'Postgres operations', 'fine', ['THE PAGE IS MISSING.', '“MUST BE THE QUERY PLANNER.”']]
}

export function PostgresMeme({ route = window.location.hash, compact = false }) {
  const [, page, templateKey, captions] = memeForRoute(route)
  const template = templates[templateKey]
  return (
    <figure className={`postgres-meme${compact ? ' postgres-meme-compact' : ''}`}
      data-testid="postgres-meme" aria-label={`PostgreSQL meme for ${page}`}>
      <figcaption className="postgres-meme-heading">
        <span className="postgres-meme-eyebrow">THE DBA BREAKROOM</span>
        <span className="postgres-meme-template">{template.name}</span>
      </figcaption>
      <div className={`postgres-meme-poster postgres-meme-${templateKey}`}>
        <img src={`/memes/${template.file}`} alt={template.alt}
          width={template.width} height={template.height} loading="lazy" decoding="async" />
        {captions.map((caption, index) => (
          <span key={index} className={`postgres-meme-label postgres-meme-label-${index + 1}`}>
            <span className="sr-only">{template.roles[index]}: </span>{caption}
          </span>
        ))}
      </div>
      <a className="postgres-meme-credits" href="/memes/CREDITS.md">Template credits</a>
    </figure>
  )
}
