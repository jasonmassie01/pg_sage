import { GeneralTab } from './GeneralTab'
import { CollectorTab } from './CollectorTab'
import { AnalyzerTab } from './AnalyzerTab'
import { TrustSafetyTab } from './TrustSafetyTab'
import { LLMTab } from './LLMTab'
import { AlertingTab } from './AlertingTab'
import { RetentionTab } from './RetentionTab'
import { SimpleMonitoringTab } from './SimpleMonitoringTab'
import { SimpleAIAlertsTab } from './SimpleAIAlertsTab'
import { PolicySets } from '../PolicySets'

const SIMPLE = {
  Monitoring: SimpleMonitoringTab,
  'AI & Alerts': SimpleAIAlertsTab,
}

const ADVANCED = {
  Collector: CollectorTab,
  Analyzer: AnalyzerTab,
  'Trust & Safety': TrustSafetyTab,
  LLM: LLMTab,
  Alerting: AlertingTab,
  Retention: RetentionTab,
}

// TabContent renders the selected tab of the selected mode. General is the
// same in both modes and edits nothing; Policy sets (fleetpol-01c) edits
// no config either; every other tab gets fieldProps.
export function TabContent({ mode, tab, data, database, fieldProps, user }) {
  if (tab === 'General') {
    return (
      <GeneralTab
        mode={data?.mode} databases={data?.databases}
        database={database}
      />
    )
  }
  if (tab === 'Policy sets' && mode !== 'simple') return <PolicySets user={user} />
  const Tab = (mode === 'simple' ? SIMPLE : ADVANCED)[tab]
  return Tab ? <Tab {...fieldProps} /> : null
}
