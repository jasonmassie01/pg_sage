import { DatabaseOnlySetting, Field, SectionHeading } from '../fields'
import { EXECUTION_OPTIONS, TRUST_OPTIONS } from './options'

function SafetySection(props) {
  return (
    <div>
      <SectionHeading>Safety controls</SectionHeading>
      <Field label="Trust Level" configKey="trust.level" type="select"
        options={TRUST_OPTIONS}
        help={'Controls how much autonomy pg_sage has. Start with Observation to'
          + ' just watch, then graduate to Advisory for safe recommendations.'}
        {...props} />
      {props.isDatabaseScope ? (
        <Field label="Execution Mode" configKey="execution_mode" type="select"
          options={EXECUTION_OPTIONS}
          help={'How pg_sage handles approved actions. Auto executes immediately,'
            + ' Approval waits for your OK.'}
          {...props} />
      ) : (
        <DatabaseOnlySetting label="Execution Mode" />
      )}
      <Field label="CPU Ceiling (%)" configKey="safety.cpu_ceiling_pct"
        help={'pg_sage pauses all automated actions when CPU usage exceeds this'
          + ' threshold. Protects your database during peak load.'}
        {...props} />
    </div>
  )
}

export function SimpleMonitoringTab(props) {
  return (
    <div className="space-y-6">
      <div>
        <SectionHeading>How pg_sage monitors</SectionHeading>
        <Field label="Collector Interval (seconds)" configKey="collector.interval_seconds"
          help={'How often pg_sage checks your database health. Lower = more'
            + ' responsive but slightly more overhead.'}
          {...props} />
        <Field label="Slow Query Threshold (ms)" configKey="analyzer.slow_query_threshold_ms"
          help={'Queries taking longer than this are flagged for review. 1000ms is a'
            + ' good default.'}
          {...props} />
        <Field label="Unused Index Window (days)"
          configKey="analyzer.unused_index_window_days"
          help={'How many days an index must go unused before pg_sage flags it. A'
            + ' longer window avoids false positives from seasonal workloads.'}
          {...props} />
      </div>
      <SafetySection {...props} />
    </div>
  )
}
