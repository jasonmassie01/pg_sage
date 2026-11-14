import { DatabaseOnlySetting, Field, SectionHeading } from '../fields'
import { MaintenanceWindowPreview } from '../MaintenanceWindowPreview'
import { EXECUTION_OPTIONS, TRUST_OPTIONS } from './options'

function TrustSection(props) {
  return (
    <div>
      <SectionHeading>Trust</SectionHeading>
      <Field label="Trust Level" configKey="trust.level"
        type="select" options={TRUST_OPTIONS} {...props} />
      {props.isDatabaseScope ? (
        <Field label="Execution Mode" configKey="execution_mode"
          type="select" options={EXECUTION_OPTIONS} {...props} />
      ) : (
        <DatabaseOnlySetting label="Execution Mode" />
      )}
      <Field label="Tier 3: Safe" configKey="trust.tier3_safe"
        type="toggle" {...props} />
      <Field label="Tier 3: Moderate" configKey="trust.tier3_moderate"
        type="toggle" {...props} />
      <Field label="Maintenance Window"
        configKey="trust.maintenance_window" type="text" {...props} />
      <MaintenanceWindowPreview
        expression={props.getVal('trust.maintenance_window')} />
    </div>
  )
}

export function TrustSafetyTab(props) {
  return (
    <div className="space-y-6">
      <TrustSection {...props} />
      <div>
        <SectionHeading>Safety</SectionHeading>
        <Field label="CPU Ceiling (%)"
          configKey="safety.cpu_ceiling_pct" {...props} />
        <Field label="Query Timeout (ms)"
          configKey="safety.query_timeout_ms" {...props} />
        <Field label="DDL Timeout (seconds)"
          configKey="safety.ddl_timeout_seconds" {...props} />
        <Field label="Lock Timeout (ms)"
          configKey="safety.lock_timeout_ms" {...props} />
      </div>
      <div>
        <SectionHeading>Rollback</SectionHeading>
        <Field label="Rollback Threshold (%)"
          configKey="trust.rollback_threshold_pct" {...props} />
        <Field label="Rollback Window (minutes)"
          configKey="trust.rollback_window_minutes" {...props} />
        <Field label="Rollback Cooldown (days)"
          configKey="trust.rollback_cooldown_days" {...props} />
        <Field label="Cascade Cooldown (cycles)"
          configKey="trust.cascade_cooldown_cycles" {...props} />
      </div>
    </div>
  )
}
