// Presentation only: retain the reported measurements and exact source text in
// technical evidence. Missing evidence is never a reassuring default.
export function caseID(row) {
  return row.case_id || row.id
}

export function caseLink(page, row) {
  const query = new URLSearchParams({ case: caseID(row) })
  if (row.database_name) query.set('database', row.database_name)
  return `#/${page}?${query}`
}

function categoryOf(row) {
  const prefix = `finding:${row.database_name}:`
  if (row.identity_key?.startsWith(prefix)) {
    return row.identity_key.slice(prefix.length).split(':')[0]
  }
  return null
}

function measurement(value) {
  return typeof value === 'number' && Number.isFinite(value) ? value : null
}

const GLOSSARY = {
  sequence: { term: 'a sequence', definition: 'An automatic counter that generates values, often used for row IDs.' },
  index: { term: 'an index', definition: 'A lookup structure that can help the database find rows without reading through the table.' },
  statistics: { term: 'table statistics', definition: 'Estimates about a table’s rows and values that help the database choose how to run a query.' },
}

export function casePresentation(row) {
  const category = categoryOf(row)
  const detail = row.evidence?.find(item => item.detail)?.detail || {}
  const usage = measurement(detail.usage_pct)
  const blocks = measurement(detail.delta_temp_blks_written)
  const seconds = measurement(detail.interval_seconds)
  let title = row.title
  let observed = 'See the reported condition and supporting evidence in this case.'
  let consequence = 'The effect on applications and users has not been established.'
  let glossary = null

  if (category === 'sequence_exhaustion') {
    glossary = GLOSSARY.sequence
    title = 'An ID counter is nearly full'
    observed = usage === null
      ? 'The reported sequence is approaching its range limit; a measured percentage is unavailable.'
      : `This sequence has used about ${Number(usage.toFixed(1))}% of its reported range.`
    consequence = 'At its limit, requests for another sequence value can fail. Time to limit and affected applications have not been established.'
  } else if (category === 'temp_spill') {
    title = 'A query used temporary disk space'
    const interval = seconds !== null && seconds > 0
      ? ` during an approximately ${Number(seconds.toFixed(1))}-second collection interval`
      : ' during the collection interval (duration unknown)'
    observed = blocks === null
      ? 'Temporary disk activity was reported; the measured block count is unavailable.'
      : `${blocks.toLocaleString('en-US')} temporary blocks were written${interval}.`
    consequence = 'This may add disk work; the effect on users has not been established. No memory setting has been validated by this finding.'
  } else if (category === 'duplicate_index') {
    glossary = GLOSSARY.index
    title = 'Two indexes appear to duplicate each other'
    observed = 'The analyzer reports matching index definitions. Exact definitions are available in technical evidence.'
    consequence = 'Keeping both may add storage and write work. The measured cost and effect on users have not been established; review dependencies before removal.'
  } else if (category === 'unused_index') {
    glossary = GLOSSARY.index
    title = 'An index may no longer be needed'
    observed = 'The analyzer reports an index with little or no recorded use.'
    consequence = 'An unused index may add storage and write work. Recorded use alone does not establish that removal is safe.'
  } else if (category === 'missing_fk_index' || category === 'missing_index') {
    glossary = GLOSSARY.index
    title = 'A table may benefit from an additional index'
    observed = 'The analyzer identified a possible missing index. Review the affected table and query evidence.'
    consequence = 'An index may reduce work for some queries, but also adds storage and write work. The effect on this workload needs review.'
  } else if (category === 'stale_statistics' || category === 'stale_stats') {
    glossary = GLOSSARY.statistics
    title = 'Query planning information may be out of date'
    observed = 'The analyzer reports outdated table statistics, which help the database choose how to run a query.'
    consequence = 'Outdated statistics may lead to less efficient query plans. The effect on users has not been established.'
  } else if (category === 'table_bloat') {
    title = 'A table has old row versions awaiting cleanup'
    const live = measurement(detail.n_live_tup)
    const dead = measurement(detail.n_dead_tup)
    const ratio = measurement(detail.dead_ratio)
    const share = ratio !== null && ratio >= 0 && ratio <= 1
      ? ` (about ${Number((ratio * 100).toFixed(1))}% old versions)` : ''
    observed = `Reported estimates: ${live === null ? 'live-row count unavailable' : `${live.toLocaleString('en-US')} live rows`}; ${dead === null ? 'old-version count unavailable' : `${dead.toLocaleString('en-US')} old row versions`}${share}.`
    consequence = 'Updates and deletes can leave old row versions for cleanup. These may add storage and read work; the recoverable disk space and effect on users have not been established.'
    if (detail.io_saturated === true) {
      consequence += ' The analyzer also reports high disk I/O pressure; review workload conditions before cleanup.'
    }
  } else if (category === 'seq_scan_heavy') {
    glossary = GLOSSARY.index
    title = 'A table is often read without using an index'
    const sequential = measurement(detail.seq_scan)
    const indexed = measurement(detail.idx_scan)
    const live = measurement(detail.n_live_tup)
    observed = `Recorded scans: ${sequential === null ? 'sequential-scan count unavailable' : `${sequential.toLocaleString('en-US')} sequential scans (reading table rows directly)`}; ${indexed === null ? 'index-scan count unavailable' : `${indexed.toLocaleString('en-US')} index scans`}. The counter window is unknown.`
    if (live !== null) observed += ` Reported live-row estimate: ${live.toLocaleString('en-US')}.`
    consequence = 'Reading many rows may add work, but a sequential scan can be appropriate. Query plans and the effect on users need review before adding an index.'
  } else if (category === 'invalid_index') {
    glossary = GLOSSARY.index
    title = 'An index is marked invalid'
    observed = typeof detail.table === 'string' && detail.table
      ? `The database reports this index as invalid on table ${detail.table}. Its definition is in technical evidence.`
      : 'The database reports this index as invalid. The affected table is not supplied in the evidence.'
    consequence = 'It may be unavailable for query planning while still requiring storage or write work. The cause and effect on users have not been established; review its definition and dependencies before rebuilding or removing it.'
  } else if (category === 'slow_query') {
    title = 'A query is taking longer than the configured threshold'
    const mean = measurement(detail.mean_exec_ms)
    observed = mean === null
      ? 'The analyzer reports query latency above its configured threshold.'
      : `Reported average execution time: ${mean.toLocaleString('en-US')} milliseconds.`
    consequence = 'This may delay requests that use the query. The affected applications and cause have not been established.'
  }

  const whyNow = row.why_now?.trim()
  const assessed = whyNow && !/^(not urgent|critical severity requires immediate review)$/i.test(whyNow)
  return {
    title, observed, consequence, glossary,
    attention: assessed ? whyNow : 'Urgency not assessed. Severity is the analyzer’s classification; it does not establish a deadline or user impact.',
    category,
  }
}

export function actionAvailability(row) {
  const candidate = row.action_candidates?.[0]
  if (!candidate) return 'No proposed change is attached. Review the evidence or ask Sage to explain what needs investigation.'
  const reason = candidate.blocked_reason || candidate.policy_decision?.blocked_reason || ''
  if (/observation/i.test(reason)) return 'Database changes are disabled in Observation mode. You can review this proposal in Actions.'
  if (/provider_unsupported|provider unsupported/i.test(reason)) return 'pg_sage cannot run this proposed action for this connection. Review the proposal and available procedure in Actions.'
  if (/permission|privilege/i.test(reason)) return 'This connection lacks permission for the proposed change. Review the required authority in Actions.'
  if (reason || candidate.policy_decision?.decision === 'blocked') return 'The proposed change is blocked. Review the recorded restriction in Actions before choosing a next step.'
  return 'A proposed change is attached. Review its evidence, restrictions and decision requirements in Actions.'
}
