// configbadges.ts: Settings payloads for settings-badges.spec.ts (config-03:
// the masked-override badge; config-07: the lifecycle banner wording).
// Each payload names the Go code whose JSON it copies. Register these
// routes AFTER mockAllAPIs: Playwright matches the newest first.

import { type Page } from '@playwright/test'
import { configBody, configState } from './settings'

// maskedTrust copies the config.MaskedKey of
// TestConfigGlobalGetListsMaskedOverrides: the file sets observation, a
// stored UI override sets advisory, and trust keeps the more restrictive
// of the two (config.RestrictTrust), so observation is in force.
export const maskedTrust = {
  key: 'trust.level', file_value: 'observation', override_value: 'advisory',
  effective_value: 'observation', updated_by: 1, updated_at: '2026-11-13T08:00:00Z',
}

// MASKED_TRUST_TITLE is MaskedBadge's title for maskedTrust.
export const MASKED_TRUST_TITLE = 'The config file, environment or a flag sets observation; ' +
  'a UI override (user 1, 2026-11-13T08:00:00Z) sets advisory. Effective: observation.'

export const maskedModel = {
  key: 'llm.model', file_value: 'gpt-4o', override_value: 'gpt-4o-mini',
  effective_value: 'gpt-4o-mini', updated_by: 1, updated_at: '2026-11-13T09:00:00Z',
}

// LIFECYCLE_BANNER is the Settings banner config-07 settled on: owner-less
// reconfigure fields are restart-bound (reports/t1/config-07.md).
export const LIFECYCLE_BANNER = 'Saved fields follow server lifecycle metadata. ' +
  'The LLM connection and YAML fleet databases rebuild in place, trust.level and the ' +
  'trust ceilings apply live; every other field is reported as pending restart.'

// badgeBody is configGlobalGetHandler's answer with masked entries: each
// masked key's config entry shows its effective value, source override and
// masked: true (internal/api/config_masking.go). extra adds fields a newer
// server may send, such as an unknown restart reason.
export function badgeBody(masked: Record<string, string>[],
  extra: Record<string, unknown> = {}) {
  const body = configBody(configState({ masked }))
  const config: Record<string, unknown> = { ...body.config }
  for (const m of masked) {
    config[m.key] = { value: m.effective_value, source: 'override', masked: true }
  }
  return { ...body, config, ...extra }
}

export async function routeBadges(page: Page, body: () => unknown) {
  await page.route('**/api/v1/config/global', route => route.fulfill({ json: body() }))
}
