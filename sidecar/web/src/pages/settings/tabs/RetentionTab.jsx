import { Field, SectionHeading } from '../fields'

export function RetentionTab(props) {
  return (
    <div>
      <SectionHeading>Retention</SectionHeading>
      <Field label="Snapshots (days)"
        configKey="retention.snapshots_days" {...props} />
      <Field label="Findings (days)"
        configKey="retention.findings_days" {...props} />
      <Field label="Actions (days)"
        configKey="retention.actions_days" {...props} />
      <Field label="Explains (days)"
        configKey="retention.explains_days" {...props} />
    </div>
  )
}
