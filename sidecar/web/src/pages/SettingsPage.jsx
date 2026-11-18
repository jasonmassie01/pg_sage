/* eslint-disable react-refresh/only-export-components */
import { LoadingSpinner } from '../components/LoadingSpinner'
import { ErrorBanner } from '../components/ErrorBanner'
import { QueryState } from '../components/QueryState'
import { TabPanel } from '../components/Tabs'
import { queryBlocked } from '../lib/queryState'
import { MaskedOverridesContext } from './settings/SourceBadge'
import { PendingRestartPanel } from './settings/PendingRestartPanel'
import { FeedbackBanner } from './settings/fields'
import { TabContent } from './settings/tabs/TabContent'
import { useSettingsConfig, useSettingsMode } from './settings/useSettingsConfig'
import { useSettingsSave } from './settings/useSettingsSave'
import { useRestartNow } from './settings/useRestartNow'
import { SavedYAML } from '../components/ConfigDiff'
import { CARD_STYLE } from './settings/styles'
import {
  LifecycleBanner, SaveBar, ScopeNotice, SettingsDialogs, SettingsHeader, TabCards,
} from './settings/SettingsSections'

// The page keeps only the composition (dashboard-30): the load and edit
// state is useSettingsConfig, saving is useSettingsSave, and each tab is a
// file under settings/tabs. isMaskedSecret stays importable from here.
export { isMaskedSecret } from './settings/secretMask'

function fieldPropsOf(s) {
  return {
    getVal: s.getVal, setVal: s.setVal, getSource: s.getSource,
    isDatabaseScope: s.isDatabaseScope, configUrl: s.configUrl, readOnly: s.readOnly,
  }
}

export function SettingsPage({ database, databaseId }) {
  const view = useSettingsMode()
  const s = useSettingsConfig({ database, databaseId, tab: view.tab })
  const save = useSettingsSave(s)
  const restart = useRestartNow()
  if (queryBlocked(s.config)) {
    return <QueryState query={s.config} skeleton={<LoadingSpinner />} />
  }
  if (s.selectedDatabase && !s.isDatabaseScope && !s.isStandaloneScope) {
    // Whether the database is missing rests on the deployment mode; after a
    // failed refresh that mode is not current, so the failure is shown.
    return <ErrorBanner onRetry={s.refetch}
      message={s.config.error || `Selected database "${database}" is not available`} />
  }
  const fieldProps = { ...fieldPropsOf(s), resetField: save.resetField }
  return (
    <MaskedOverridesContext.Provider value={s.masked}>
    <QueryState query={s.config}><div className="space-y-4 max-w-3xl">
      <SettingsDialogs s={s} save={save} database={database} />
      <SettingsHeader view={view} />
      <ScopeNotice s={s} database={database} />
      <LifecycleBanner supported={s.data?.restart_supported === true} restart={restart} />
      <PendingRestartPanel pending={s.data?.pending} />
      <TabPanel id="settings" active={view.tab} className="space-y-4">
        <TabCards tab={view.tab} database={database}
          databaseId={s.isDatabaseScope ? s.numericDatabaseId : undefined} />
        {s.feedback && <FeedbackBanner {...s.feedback} />}
        <div className="rounded p-5" style={CARD_STYLE}>
          <TabContent mode={view.mode} tab={view.tab} data={s.data}
            database={database} fieldProps={fieldProps} />
        </div>
        {view.tab !== 'General' && s.hasEdits && <SaveBar save={save} onDiscard={s.discard} />}
        {/* cli-01b: outside the save bar, which hides once a save clears the edits. */}
        <SavedYAML keys={save.savedKeys} onDismiss={save.dismissSaved} />
      </TabPanel>
    </div></QueryState>
    </MaskedOverridesContext.Provider>
  )
}
