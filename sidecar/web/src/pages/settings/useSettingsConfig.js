import { useCallback, useMemo, useState } from 'react'
import { useAPI } from '../../hooks/useAPI'
import { useToast } from '../../components/Toast'
import { readPref, writePref } from '../../lib/storage'
import { maskedByKey } from './SourceBadge'

// The Settings page's load and edit state (dashboard-30): which config the
// page reads, the simple/advanced mode, the unsaved edits and the trust
// raise that waits for TrustLevelConfirm. Saving lives in useSettingsSave.

export const ADVANCED_TABS = [
  'General', 'Collector', 'Analyzer', 'Trust & Safety',
  'LLM', 'Alerting', 'Retention', 'Policy sets',
]

export const SIMPLE_TABS = ['General', 'Monitoring', 'AI & Alerts']

export const MODE_PREF = 'pg_sage_settings_mode'

// settingsScope picks the config endpoint for the selected database.
// Standalone runtimes appear in the monitoring inventory without a managed
// registration ID. Read the deployment mode before treating that as a
// missing database; only standalone may use instance-wide configuration.
export function settingsScope(database, databaseId) {
  const selectedDatabase = Boolean(database && database !== 'all')
  const numericDatabaseId = Number(databaseId)
  const isDatabaseScope = selectedDatabase
    && Number.isSafeInteger(numericDatabaseId) && numericDatabaseId > 0
  const needsStandaloneConfig = selectedDatabase
    && (databaseId == null || numericDatabaseId === 0)
  const configUrl = isDatabaseScope
    ? `/api/v1/config/databases/${numericDatabaseId}`
    : !selectedDatabase || needsStandaloneConfig
      ? '/api/v1/config/global'
      : null
  return {
    selectedDatabase, numericDatabaseId, isDatabaseScope, needsStandaloneConfig, configUrl,
  }
}

// isTrustEscalation is true for observation -> advisory/autonomous and
// advisory -> autonomous. De-escalation is always safe and never confirmed.
export function isTrustEscalation(from, to) {
  if (from === to) return false
  return (from === 'observation' && (to === 'advisory' || to === 'autonomous'))
    || (from === 'advisory' && to === 'autonomous')
}

function initialMode() {
  return readPref(MODE_PREF, 'simple') === 'advanced' ? 'advanced' : 'simple'
}

// LINKED_TABS are the advanced tabs a link may open with ?tab= (the inbox
// links a policy exception to Policy sets, fleetpol-01c).
const LINKED_TABS = { 'policy-sets': 'Policy sets' }

// settingsTabFromRoute is the route's ?tab= value, '' when it has none.
export function settingsTabFromRoute(route) {
  return new URLSearchParams((route || '').split('?')[1] || '').get('tab') || ''
}

// useSettingsMode is the simple/advanced switch, stored with writePref,
// and the selected tab, which falls back to General when the mode hides it.
// linkedTab (a LINKED_TABS key) opens that tab in advanced mode.
export function useSettingsMode(linkedTab) {
  const linked = Object.hasOwn(LINKED_TABS, linkedTab || '') ? LINKED_TABS[linkedTab] : null
  const [mode, setMode] = useState(() => (linked ? 'advanced' : initialMode()))
  const [tab, setTab] = useState(linked || 'General')
  const tabs = mode === 'simple' ? SIMPLE_TABS : ADVANCED_TABS
  const toggleMode = () => {
    const next = mode === 'simple' ? 'advanced' : 'simple'
    setMode(next)
    writePref(MODE_PREF, next)
    if (!(next === 'simple' ? SIMPLE_TABS : ADVANCED_TABS).includes(tab)) setTab('General')
  }
  return { mode, toggleMode, tabs, tab, setTab }
}

// useEdits holds the unsaved values over the loaded config.
export function useEdits(cfg) {
  const [edits, setEdits] = useState({})
  const getVal = useCallback((key) => {
    if (key in edits) return edits[key]
    return cfg[key]?.value ?? ''
  }, [cfg, edits])
  const getSource = useCallback((key) => {
    if (key in edits) return 'modified'
    return cfg[key]?.source ?? 'file/default'
  }, [cfg, edits])
  const dropEdit = useCallback(key => setEdits(prev => {
    const next = { ...prev }
    delete next[key]
    return next
  }), [])
  const discard = useCallback(() => setEdits({}), [])
  const hasEdits = Object.keys(edits).length > 0
  return { edits, setEdits, getVal, getSource, dropEdit, discard, hasEdits }
}

// useTrustGate holds a trust raise until TrustLevelConfirm answers.
function useTrustGate(setEdits, setFeedback) {
  const [pendingTrust, setPendingTrust] = useState(null)
  const applyPendingTrust = () => {
    if (!pendingTrust) return
    setEdits(prev => ({ ...prev, [pendingTrust.key]: pendingTrust.val }))
    setFeedback(null)
    setPendingTrust(null)
  }
  const cancelPendingTrust = () => setPendingTrust(null)
  return { pendingTrust, setPendingTrust, applyPendingTrust, cancelPendingTrust }
}

// useSetVal is the one way a field changes: refused when read-only, refused
// with a self-clearing hint for keys a database scope cannot override, and
// held for confirmation when it raises trust.
function useSetVal({ readOnly, isDatabaseScope, getVal, setEdits, setFeedback, gate }) {
  const toast = useToast()
  return (key, val) => {
    if (readOnly) return
    if (isDatabaseScope && key !== 'trust.level' && key !== 'execution_mode') {
      // A hint on each rejected keystroke: it clears itself rather than
      // stacking up like the error toasts that wait to be dismissed.
      toast.error(
        'Per-database overrides currently support only trust and execution mode',
        { duration: 4000 },
      )
      return
    }
    if (key === 'trust.level') {
      const from = getVal('trust.level')
      if (isTrustEscalation(from, val)) {
        gate.setPendingTrust({ key, val, from })
        return
      }
    }
    setEdits(prev => ({ ...prev, [key]: val }))
    setFeedback(null)
  }
}

// useSettingsConfig loads the scoped config and owns the edit state. The
// edits and the save notice clear whenever the tab or the scope changes.
export function useSettingsConfig({ database, databaseId, tab }) {
  const scope = settingsScope(database, databaseId)
  const config = useAPI(scope.configUrl, 0)
  const { data } = config
  const isStandaloneScope = scope.needsStandaloneConfig && data?.mode === 'standalone'
  const readOnly = data?.read_only === true
  const cfg = useMemo(() => data?.config || {}, [data?.config])
  const masked = useMemo(
    () => maskedByKey(data?.masked_by_override), [data?.masked_by_override],
  )
  const [feedback, setFeedback] = useState(null)
  const editState = useEdits(cfg)
  const { setEdits } = editState
  // Reset while rendering, not in an effect, so no frame shows the old
  // tab's edits under the new one.
  const view = `${tab}|${scope.configUrl}`
  const [shownView, setShownView] = useState(view)
  if (shownView !== view) {
    setShownView(view)
    setEdits({})
    setFeedback(null)
  }
  const gate = useTrustGate(setEdits, setFeedback)
  const setVal = useSetVal({
    readOnly, isDatabaseScope: scope.isDatabaseScope, getVal: editState.getVal,
    setEdits, setFeedback, gate,
  })
  return {
    ...scope, ...editState, ...gate, config, data, refetch: config.refetch,
    isStandaloneScope, readOnly, cfg, masked, feedback, setFeedback, setVal,
  }
}
