import { describe, expect, it } from 'vitest'
import { caseFactTarget } from './caseFacts'

// caseFactTarget derives, from a finding-sourced case's identity key
// ("finding:<database>:<category>:<object_type>:<object>[:<rule_id>]"),
// the database object the binding facts are asked about.

const kase = (identity, database = 'prod') => ({ identity_key: identity,
  database_name: database })

describe('caseFactTarget', () => {
  it('derives the object of a finding case', () => {
    expect(caseFactTarget(kase('finding:prod:unused_index:index:public.idx_orders_status')))
      .toEqual({ database: 'prod', category: 'unused_index', objectType: 'index',
        object: 'public.idx_orders_status' })
  })

  it("keeps ':' inside the object", () => {
    expect(caseFactTarget(kase('finding:prod:slot_inactive:slot:slot:cdc')).object)
      .toBe('slot:cdc')
  })

  it("handles a database name containing ':'", () => {
    expect(caseFactTarget(kase('finding:a:b:table_bloat:table:public.orders', 'a:b')))
      .toEqual({ database: 'a:b', category: 'table_bloat', objectType: 'table',
        object: 'public.orders' })
  })

  it('skips query cases, whose key holds a normalized query', () => {
    expect(caseFactTarget(kase('finding:prod:slow_query:query:select * from t where a = $1')))
      .toBeNull()
  })

  it('skips schema_lint cases, whose category and rule id hold colons', () => {
    expect(caseFactTarget(kase(
      'finding:prod:schema_lint:lint_int_pk:table:public.orders:lint_int_pk'))).toBeNull()
    expect(caseFactTarget(kase('finding:prod:schema_lint_x:table:public.orders')))
      .toBeNull()
  })

  it('skips migration cases, whose object is a synthetic id with a rule id', () => {
    expect(caseFactTarget(kase('finding:prod:migration_safety:migration:abc:ddl_rule')))
      .toBeNull()
  })

  it('skips non-finding cases and malformed keys', () => {
    for (const c of [
      kase('incident:prod:lock_storm'),
      kase('finding:other:unused_index:index:public.i'),
      kase('finding:prod:unused_index:index:'),
      kase('finding:prod:unused_index'),
      kase('finding:prod:unused_index:index:public.i', ''),
      kase(undefined),
      kase(42),
      {},
      null,
      undefined,
    ]) {
      expect(caseFactTarget(c)).toBeNull()
    }
  })
})
