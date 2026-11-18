import { Save, RotateCcw } from 'lucide-react'
import { ConfigDiff } from '../../components/ConfigDiff'
import { AuditCard } from './AuditCard'
import { DatabricksStatusCard } from './DatabricksStatusCard'
import { DerivedSettings } from './DerivedSettings'
import { Footprint } from './Footprint'
import { RestartControl } from './PendingRestartPanel'
import { StandingPolicy } from './StandingPolicy'
import { TrustLevelConfirm } from './TrustLevelConfirm'
import { TabBar } from './fields'
import { CARD_STYLE, QUIET_BUTTON_STYLE, SECONDARY_TEXT } from './styles'

// The parts of the Settings page around the tab content (dashboard-30), in
// the order SettingsPage renders them. s is useSettingsConfig's result and
// save is useSettingsSave's.

// SettingsDialogs: the trust raise is confirmed before it becomes an edit,
// and every save is reviewed in the diff before it is sent.
export function SettingsDialogs({ s, save, database }) {
  return (
    <>
      {s.pendingTrust && (
        <TrustLevelConfirm
          from={s.pendingTrust.from}
          to={s.pendingTrust.val}
          database={database}
          onConfirm={s.applyPendingTrust}
          onCancel={s.cancelPendingTrust}
        />
      )}
      {save.showDiff && (
        <ConfigDiff
          edits={s.edits}
          cfg={s.cfg}
          saving={save.saving}
          error={save.saveError}
          onConfirm={save.confirmSave}
          onCancel={save.cancelSave}
        />
      )}
    </>
  )
}

export function SettingsHeader({ view }) {
  return (
    <div className="flex items-center justify-between">
      <TabBar tabs={view.tabs} active={view.tab} onSelect={view.setTab} />
      <button
        data-testid="settings-mode-toggle"
        onClick={view.toggleMode}
        className="text-xs px-3 py-1 rounded whitespace-nowrap ml-4"
        style={QUIET_BUTTON_STYLE}
      >
        {view.mode === 'simple' ? 'Show Advanced' : 'Show Simple'}
      </button>
    </div>
  )
}

function scopeLabel(s, database) {
  if (s.isDatabaseScope) return `Database ${database} (ID ${s.numericDatabaseId})`
  if (s.isStandaloneScope) return `Instance settings for ${database} (standalone)`
  return 'Global defaults'
}

export function ScopeNotice({ s, database }) {
  return (
    <>
      <div className="text-xs"
        data-testid="settings-scope"
        style={SECONDARY_TEXT}>
        Scope: {scopeLabel(s, database)}
      </div>
      {s.isStandaloneScope && (
        <p className="text-xs" data-testid="settings-standalone-scope"
          style={SECONDARY_TEXT}>
          This deployment monitors one database. These settings apply to the
          pg_sage instance; a managed database registration is not required.
        </p>
      )}
      {s.readOnly && (
        <div
          data-testid="settings-read-only"
          className="rounded p-3 text-xs"
          style={{
            color: 'var(--yellow)',
            border: '1px solid var(--yellow)',
            background: 'var(--bg-card)',
          }}
        >
          Configuration is read-only in YAML fleet mode. To change these
          values, {s.data?.write_guidance || 'edit the YAML file'}.
        </div>
      )}
    </>
  )
}

export function LifecycleBanner({ supported, restart }) {
  return (
    <div
      className="rounded p-3 text-xs flex items-center justify-between gap-3"
      style={{ ...CARD_STYLE, color: 'var(--text-secondary)' }}
    >
      <span data-testid="settings-lifecycle-banner">
        Saved fields follow server lifecycle metadata. The LLM connection
        and YAML fleet databases rebuild in place, trust.level and the trust
        ceilings apply live; every other field is reported as pending
        restart.
      </span>
      <RestartControl supported={supported}
        restarting={restart.restarting} onRestart={restart.restartNow} />
      {restart.dialog}
    </div>
  )
}

// TabCards are the read-mostly panels above the fields of a tab.
export function TabCards({ tab, database, databaseId }) {
  const general = tab === 'General'
  return (
    <>
      {general && <DatabricksStatusCard />}
      {general && <DerivedSettings database={database} />}
      {general && <Footprint database={database} />}
      {tab === 'Trust & Safety' && <StandingPolicy databaseId={databaseId} />}
      {general && <AuditCard database={database} />}
    </>
  )
}

export function SaveBar({ save, onDiscard }) {
  return (
    <div className="flex gap-3">
      <button onClick={save.requestSave} disabled={save.saving}
        data-testid="settings-save"
        className="flex items-center gap-2 px-4 py-2 rounded text-sm font-medium"
        style={{ background: 'var(--accent)', color: '#fff' }}>
        <Save size={16} />
        {save.saving ? 'Saving...' : 'Review & Save'}
      </button>
      <button onClick={onDiscard}
        data-testid="settings-discard"
        className="flex items-center gap-2 px-4 py-2 rounded text-sm"
        style={QUIET_BUTTON_STYLE}>
        <RotateCcw size={16} /> Discard
      </button>
    </div>
  )
}
