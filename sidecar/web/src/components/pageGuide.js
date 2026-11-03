const guides = [
  ['#/advanced/findings', 'Findings explorer', 'Explore database issues, their evidence and proposed next steps. Filter cases to focus on the signals that need attention.'],
  ['#/advanced/actions', 'Action history', 'Review proposed changes, pending decisions and recorded outcomes. The execution mode determines whether an approved change can run.'],
  ['#/advanced/runbooks', 'Runbooks', 'Review versioned investigation guides. An administrator must sign each version before use; runbooks propose changes without executing them.'],
  ['#/advanced/autonomy', 'Earned autonomy', 'Review evidence for giving pg_sage more independence on specific tasks. Administrators approve proposed increases in autonomy.'],
  ['#/advanced', 'Snapshot & metrics', 'Survey collected database health, performance metrics and recent recommendations. Provider readiness shows supported capabilities and blockers.'],
  ['#/manage-databases', 'Fleet', 'See the databases pg_sage monitors and their connection health. Open a database to inspect its collected snapshot.'],
  ['#/database', 'Database', 'Inspect the latest collected snapshot for one database, including the measurements and raw details used for analysis.'],
  ['#/cases', 'Cases', 'See database issues, the evidence behind them and what remains unknown. Open a case to investigate it or review a proposed next step.'],
  ['#/findings', 'Cases', 'See database issues, the evidence behind them and what remains unknown. Open a case to investigate it or review a proposed next step.'],
  ['#/actions', 'Actions', 'Follow proposed database changes through review, approval and outcome checks. The execution mode determines whether a change can run.'],
  ['#/slos', 'SLOs', 'Check reliability targets, remaining error budgets and how quickly failures consume them. Missing evidence appears as unknown.'],
  ['#/trust', 'Trust', 'See what pg_sage is allowed to do and the evidence behind each autonomy level. Review proposed increases before granting more control.'],
  ['#/facts', 'Facts', 'Record context pg_sage cannot discover, such as application-owned indexes. Proposed facts need confirmation before they influence decisions.'],
  ['#/ask', 'Ask Sage', 'Ask questions about the selected database and inspect the cited evidence. Answers never execute or approve database changes.'],
  ['#/settings', 'Settings', 'Inspect monitoring, AI and safety settings. Scope labels show whether changes affect the pg_sage instance or one database.'],
  ['#/notifications', 'Notifications', 'Configure alert delivery channels and rules, then inspect delivery history to see what was sent or failed.'],
  ['#/alerts', 'Alerts', 'Review alert history for the selected database, including severity, timing and delivery details.'],
  ['#/users', 'Users', 'Manage who can use pg_sage and what each role can do. App access and database permissions are separate.'],
  ['#/profile', 'Account', 'Review your signed-in identity and account preferences. Your pg_sage role determines which controls you can use.'],
  ['#/login', 'Account', 'Review your signed-in identity and account preferences. Your pg_sage role determines which controls you can use.'],
  ['#/agents', 'Agents', 'Review registered agents, their permissions and activity. Access depends on your role and the deployment’s authentication mode.'],
  ['#/mcp-tokens', 'MCP tokens', 'Manage credentials for external tools that connect through MCP. Token access is limited by its scope and the account’s permissions.'],
  ['#/forecasts', 'Cases · Forecasts', 'Review cases based on forecasts, including predicted capacity risks and the evidence behind them.'],
  ['#/query-hints', 'Cases · Query hints', 'Review cases about query-plan hints and their evidence. Provider support determines which proposals are available.'],
  ['#/schema-health', 'Cases · Schema health', 'Review cases from schema checks, such as missing or duplicate indexes, and inspect the supporting evidence.'],
  ['#/incidents', 'Cases · Incidents', 'Review incident investigations, collected evidence and proposed next steps. Open a case to follow its progress.'],
  ['#/', 'Value', 'Compare verified outcomes and estimated DBA effort saved, and see recommendations still waiting for review.'],
]

export function pageGuide(route, { pageTitle, authMode } = {}) {
  if (pageTitle === 'Access denied') return {
    title: 'Access denied', description: 'Your current role cannot open this tab. An app administrator can review the access you need.',
  }
  const path = String(route || '#/').split('?')[0]
  const match = guides.find(([prefix]) => path === prefix || (prefix !== '#/' && path.startsWith(`${prefix}/`)))
  if (!match || pageTitle === 'Not found') return {
    title: 'Page not found', description: 'This address does not match a page. Choose a tab in the navigation to return to your database workspace.',
  }
  const [, title, description] = match
  if (authMode === 'databricks_app' && (path === '#/agents' || path === '#/mcp-tokens')) return {
    title, description: 'This App uses Databricks sign-in. Agent and MCP token access are unavailable in this deployment.',
  }
  if (authMode === 'databricks_app' && (path === '#/profile' || path === '#/login')) return {
    title, description: 'Review your Databricks identity and pg_sage role. Databricks manages sign-in for this App.',
  }
  return { title, description }
}
