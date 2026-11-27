import { PRIMARY_TEXT, SECONDARY_TEXT } from '../styles'

// Emergency stop/resume moved to the header control (D8), which operators
// can reach and which follows the database picker.
export function GeneralTab({ mode, databases, database }) {
  const scoped = database && database !== 'all'
  return (
    <div className="space-y-4">
      <h3 className="text-sm font-medium"
        style={SECONDARY_TEXT}>
        System Info
      </h3>
      <div className="grid grid-cols-2 gap-3 text-sm">
        {scoped ? (
          <>
            <div style={SECONDARY_TEXT}>Scope</div>
            <div style={PRIMARY_TEXT}>
              Database override
            </div>
            <div style={SECONDARY_TEXT}>Database</div>
            <div style={PRIMARY_TEXT}>
              {database}
            </div>
          </>
        ) : (
          <>
            <div style={SECONDARY_TEXT}>Mode</div>
            <div style={PRIMARY_TEXT}>
              {mode || 'unknown'}
            </div>
            <div style={SECONDARY_TEXT}>Databases</div>
            <div style={PRIMARY_TEXT}>
              {databases ?? 0}
            </div>
          </>
        )}
      </div>
      <h3 className="text-sm font-medium mt-6"
        style={SECONDARY_TEXT}>
        Emergency Controls
      </h3>
      <p className="text-xs" data-testid="emergency-scope"
        style={SECONDARY_TEXT}>
        Stop and Resume are in the page header next to the database
        picker, for operators and admins. Current scope: {scoped
          ? `database ${database} only`
          : 'all databases in the fleet'}.
      </p>
    </div>
  )
}
