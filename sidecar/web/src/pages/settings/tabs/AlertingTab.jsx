import { Field, SectionHeading } from '../fields'

export function AlertingTab(props) {
  return (
    <div>
      <SectionHeading>Alerting</SectionHeading>
      <Field label="Enabled" configKey="alerting.enabled"
        type="toggle" {...props} />
      <Field label="Slack Webhook URL"
        configKey="alerting.slack_webhook_url" type="text" {...props} />
      <Field label="PagerDuty Routing Key"
        configKey="alerting.pagerduty_routing_key"
        type="password" {...props} />
      <Field label="Check Interval (seconds)"
        configKey="alerting.check_interval_seconds" {...props} />
      <Field label="Cooldown (minutes)"
        configKey="alerting.cooldown_minutes" {...props} />
      <Field label="Quiet Hours Start"
        configKey="alerting.quiet_hours_start" type="text"
        help="HH:MM format, e.g. 22:00" {...props} />
      <Field label="Quiet Hours End"
        configKey="alerting.quiet_hours_end" type="text"
        help="HH:MM format, e.g. 06:00" {...props} />
    </div>
  )
}
