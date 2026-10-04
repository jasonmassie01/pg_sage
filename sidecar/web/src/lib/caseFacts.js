// caseFactTarget derives the database object of a finding-sourced case
// from its identity key, "finding:<database>:<category>:<object_type>:
// <object>[:<rule_id>]" (internal/cases/identity.go), so the case can show
// the binding facts about that object. It returns null when no object can
// be derived safely:
//   - query cases: the key holds a normalized query, not an object;
//   - schema_lint cases: the category ("schema_lint:<rule>") and the
//     trailing rule id both contain ':', so the object is ambiguous;
//   - migration cases: the object is a synthetic id followed by a rule id.
// The object itself may contain ':' (e.g. "slot:cdc"), so everything after
// the object type is the object.

const SKIPPED_OBJECT_TYPES = new Set(['query', 'migration'])

export function caseFactTarget(caseRow) {
  const key = caseRow?.identity_key
  const database = caseRow?.database_name
  if (typeof key !== 'string' || !database) return null
  const prefix = `finding:${database}:`
  if (!key.startsWith(prefix)) return null
  const rest = key.slice(prefix.length)
  const [category, objectType] = rest.split(':', 2)
  if (!category || !objectType || category.startsWith('schema_lint')) return null
  if (SKIPPED_OBJECT_TYPES.has(objectType)) return null
  const object = rest.slice(category.length + objectType.length + 2)
  if (!object) return null
  return { database, category, objectType, object }
}
