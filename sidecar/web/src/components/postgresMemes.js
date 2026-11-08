export const templates = {
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

// Original captions on the six locally bundled classic templates. IDs are stable
// identifiers for the deck; no URL, identity or database information is stored.
export const memes = [
  { id: 'cache-alibi', template: 'drake', captions: ['“IT’S PROBABLY THE CACHE”', 'EXPLAIN (ANALYZE, BUFFERS)'] },
  { id: 'dave-on-pto', template: 'drake', captions: ['“ASK DAVE”', 'A RUNBOOK THAT WORKS WHILE DAVE IS ON PTO'] },
  { id: 'whole-server-mem', template: 'drake', captions: ['1 GB work_mem FOR EVERYONE', 'FIXING THE ONE QUERY THAT SPILLS'] },
  { id: 'dead-tuple-mail', template: 'drake', captions: ['EMAIL EVERY DEAD TUPLE', 'PAGE ON SOMETHING ACTIONABLE'] },
  { id: 'tested-rollback', template: 'drake', captions: ['“TRUST ME, I’M AN ADMIN”', 'A ROLLBACK WE’VE ACTUALLY TESTED'] },
  { id: 'restore-drill', template: 'drake', captions: ['A GREEN BACKUP JOB', 'A RESTORE THAT FINISHES BEFORE RETIREMENT'] },
  { id: 'n-plus-one', template: 'drake', captions: ['A BIGGER CONNECTION POOL', 'NOT RUNNING 400 QUERIES TO DRAW ONE PAGE'] },
  { id: 'hardware-budget', template: 'drake', captions: ['BUY MORE RAM', 'STOP SELECTING THE ENTIRE TABLE'] },
  { id: 'planner-seance', template: 'drake', captions: ['ARGUING WITH THE PLANNER', 'INTRODUCING IT TO CURRENT STATISTICS'] },
  { id: 'where-clause', template: 'drake', captions: ['“DELETE IS FASTER WITHOUT WHERE”', 'CONTINUING TO HAVE A JOB'] },
  { id: 'sharding-distraction', template: 'distracted', captions: ['SHARDING', 'MY APP', 'ONE MISSING INDEX'] },
  { id: 'extension-distraction', template: 'distracted', captions: ['A NEW EXTENSION', 'ME', 'THE OPEN SEV-1'] },
  { id: 'grant-envy', template: 'distracted', captions: ['SUPERUSER', 'NEW APP', 'THE SELECT GRANT IT NEEDS'] },
  { id: 'new-hardware', template: 'distracted', captions: ['NEW HARDWARE', 'MY TEAM', 'CREATE INDEX'] },
  { id: 'friday-migration', template: 'distracted', captions: ['ONE LAST MIGRATION', 'ME AT 4:59', 'MY WEEKEND'] },
  { id: 'orm-distraction', template: 'distracted', captions: ['ANOTHER ORM', 'MY TEAM', 'THE SQL THIS ONE GENERATES'] },
  { id: 'replica-distraction', template: 'distracted', captions: ['MORE READ REPLICAS', 'MY APP', 'THAT QUERY RETURNS 8 GB'] },
  { id: 'partition-distraction', template: 'distracted', captions: ['PARTITION EVERYTHING', 'ME', 'A RETENTION POLICY'] },
  { id: 'btree-discovery', template: 'buttons', captions: ['ADD AN INDEX', 'ADD TWO INDEXES', 'ME DISCOVERING BTREE'] },
  { id: 'human-in-loop', template: 'buttons', captions: ['APPROVE THE AGENT', 'READ THE SQL', 'THE HUMAN IN THE LOOP'] },
  { id: 'incident-priorities', template: 'buttons', captions: ['FIX THE INCIDENT', 'FIND WHO CAUSED IT', 'THE FIRST FIVE MINUTES'] },
  { id: 'capacity-pager', template: 'buttons', captions: ['ADD CAPACITY NOW', 'WAIT FOR THE PAGER', '“HOW FAST IS IT GROWING?”'] },
  { id: 'vacuum-standoff', template: 'buttons', captions: ['VACUUM AGAIN', 'END THE 3-DAY TRANSACTION', '“WHY ISN’T SPACE BEING REUSED?”'] },
  { id: 'duplicate-courage', template: 'buttons', captions: ['DROP THE DUPLICATE', 'KEEP IT JUST IN CASE', 'THE INDEX HAS NEVER BEEN USED'] },
  { id: 'write-amplification', template: 'buttons', captions: ['FASTER READS', 'FASTER WRITES', 'ME ADDING MY 19TH INDEX'] },
  { id: 'temporary-permanent', template: 'buttons', captions: ['DELETE THE TEMP TABLE', 'ASK IF ANYONE USES IT', 'temp_orders_2017'] },
  { id: 'all-primaries', template: 'disaster', captions: ['I SCHEDULED MAINTENANCE', 'ON ALL PRIMARIES AT ONCE'] },
  { id: 'quick-ddl', template: 'disaster', captions: ['“JUST ONE QUICK DDL”', 'FRIDAY. 4:59 PM.'] },
  { id: 'verbose-secret', template: 'disaster', captions: ['“I LOGGED THE ENTIRE REQUEST”', 'INCLUDING THE BEARER TOKEN'] },
  { id: 'foreign-key-surprise', template: 'disaster', captions: ['“FOREIGN KEYS GET AN INDEX, RIGHT?”', 'POSTGRES: “THAT’S A YOU PROBLEM.”'] },
  { id: 'forgot-where', template: 'disaster', captions: ['THE DELETE FINISHED INSTANTLY.', 'SO DID EVERY CUSTOMER’S ACCOUNT.'] },
  { id: 'table-lock', template: 'disaster', captions: ['“IT’S ONLY A SCHEMA CHANGE”', 'THE LOCK QUEUE WOULD LIKE A WORD.'] },
  { id: 'final-index', template: 'disaster', captions: ['I FIXED THE DUPLICATE INDEX.', 'I RENAMED IT index_final_v2.'] },
  { id: 'cascade-confidence', template: 'disaster', captions: ['“THE DROP FAILED ON DEPENDENCIES”', 'SO I ADDED CASCADE.'] },
  { id: 'green-cache', template: 'fine', captions: ['CPU: 100%. CACHE HIT: 99.9%.', '“SO IT’S MOSTLY GREEN.”'] },
  { id: 'average-alibi', template: 'fine', captions: ['P99 IS IN SECONDS.', '“BUT THE AVERAGE IS FINE.”'] },
  { id: 'silenced-alert', template: 'fine', captions: ['THE ALERT STOPPED FIRING.', '“I TURNED THE ALERT OFF.”'] },
  { id: 'replica-yesterday', template: 'fine', captions: ['THE REPLICA IS AN HOUR BEHIND.', '“FINALLY, STABLE REPORTING.”'] },
  { id: 'idle-transaction', template: 'fine', captions: ['IDLE IN TRANSACTION SINCE MONDAY.', '“AT LEAST IT’S NOT USING CPU.”'] },
  { id: 'full-disk-logs', template: 'fine', captions: ['DISK IS 99% FULL.', '“ENABLE DEBUG LOGGING.”'] },
  { id: 'untested-backup', template: 'fine', captions: ['WE’VE NEVER TESTED A RESTORE.', '“BUT THE BACKUP EMAIL IS GREEN.”'] },
  { id: 'one-blocker', template: 'fine', captions: ['50 QUERIES WAITING ON ONE LOCK.', '“ONLY ONE SESSION IS BUSY.”'] },
  { id: 'staging-evidence', template: 'change', captions: ['“WORKED IN STAGING” IS NOT EVIDENCE.'] },
  { id: 'job-title', template: 'change', captions: ['YOUR JOB TITLE IS NOT A POSTGRES ROLE.'] },
  { id: 'fk-wishful-thinking', template: 'change', captions: ['“SURELY THE FOREIGN KEY INDEXES ITSELF” IS A SCHEMA STRATEGY.'] },
  { id: 'rollback-document', template: 'change', captions: ['A WORD DOCUMENT CALLED ROLLBACK IS NOT A ROLLBACK PLAN.'] },
  { id: 'select-star-contract', template: 'change', captions: ['SELECT * IS A SURPRISE SUBSCRIPTION TO FUTURE COLUMNS.'] },
  { id: 'backup-schrodinger', template: 'change', captions: ['YOUR BACKUP IS SCHRÖDINGER’S DATABASE UNTIL YOU RESTORE IT.'] },
  { id: 'index-rent', template: 'change', captions: ['EVERY INDEX YOU ADD CHARGES RENT ON EVERY WRITE.'] },
  { id: 'timezone-truce', template: 'change', captions: ['“WE’LL FIX TIME ZONES LATER” IS HOW LATER BECOMES YESTERDAY.'] },
]

// Fisher–Yates gives each cycle a new random order. Keep the deck in a caller's
// stable ref/module so unrelated renders cannot draw extra cards.
export function createMemeDeck(random = Math.random) {
  let remaining = []
  let previousID
  return {
    next() {
      if (remaining.length === 0) {
        remaining = [...memes]
        for (let i = remaining.length - 1; i > 0; i--) {
          const j = Math.floor(random() * (i + 1))
          ;[remaining[i], remaining[j]] = [remaining[j], remaining[i]]
        }
        // The end of one cycle and start of the next must also feel fresh.
        const last = remaining.length - 1
        if (remaining[last].id === previousID) {
          ;[remaining[0], remaining[last]] = [remaining[last], remaining[0]]
        }
      }
      const meme = remaining.pop()
      previousID = meme.id
      return meme
    },
  }
}
