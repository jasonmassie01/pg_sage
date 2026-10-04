import { SQLBlock } from '../../components/SQLBlock'

// FindingFactSections renders what binding facts (roadmap 2.3) add to a
// finding: the source-fix packet when a confirmed fact redirected the
// change to the application's own migrations, and the cleanup batch of a
// test-fixture cleanup finding. pg_sage runs neither.

const label = { color: 'var(--text-secondary)' }
const strong = { color: 'var(--text-primary)' }
const box = { border: '1px solid var(--border)', background: 'var(--bg-primary)' }

function isObject(value) {
  return value !== null && typeof value === 'object' && !Array.isArray(value)
}

export function FindingFactSections({ row }) {
  const detail = row?.detail
  if (!isObject(detail)) return null
  const fix = isObject(detail.source_fix) ? detail.source_fix : null
  const schemas = Array.isArray(detail.schemas) ? detail.schemas : []
  const cleanup = row.category === 'test_fixture_cleanup'
    && (Boolean(detail.cleanup_sql) || schemas.length > 0)
  if (!fix && !cleanup) return null
  return (
    <div className="space-y-3">
      {fix && <SourceFixPacket fix={fix} />}
      {cleanup && <CleanupBatch sql={detail.cleanup_sql} schemas={schemas} />}
    </div>
  )
}

function SourceFixPacket({ fix }) {
  return (
    <section data-testid="source-fix-packet" className="rounded p-3 space-y-2 text-sm"
      style={box}>
      <div>
        <div className="font-medium" style={strong}>Source-fix packet</div>
        <div className="text-xs" style={label}>
          add this to the application&apos;s migrations; pg_sage will not run it
        </div>
      </div>
      {fix.summary && <p style={strong}>{fix.summary}</p>}
      {(fix.fact || fix.provenance) && (
        <div className="text-xs" style={label}>
          {fix.fact}{fix.fact && fix.provenance ? ' · ' : ''}{fix.provenance}
        </div>
      )}
      {fix.migration && (
        <div data-testid="source-fix-migration">
          <div className="text-xs font-medium mb-1" style={label}>Migration</div>
          <SQLBlock sql={fix.migration} />
        </div>
      )}
      {fix.down && (
        <div data-testid="source-fix-down">
          <div className="text-xs font-medium mb-1" style={label}>Down (undo)</div>
          <SQLBlock sql={fix.down} />
        </div>
      )}
    </section>
  )
}

function CleanupBatch({ sql, schemas }) {
  return (
    <section data-testid="cleanup-batch" className="rounded p-3 space-y-2 text-sm"
      style={box}>
      <div className="font-medium" style={strong}>
        cleanup batch: review and run once; pg_sage never drops schemas itself
      </div>
      {schemas.length > 0 && (
        <div data-testid="cleanup-schemas" className="text-xs" style={label}>
          Schemas ({schemas.length}): {schemas.join(', ')}
        </div>
      )}
      {sql && (
        <div data-testid="cleanup-sql"><SQLBlock sql={sql} /></div>
      )}
    </section>
  )
}
