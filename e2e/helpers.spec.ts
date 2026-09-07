import { test, expect } from '@playwright/test';
import { getConsoleErrors, isExpectedError } from './helpers';

test('error filtering keeps API failures and narrowly permits expected responses', () => {
  const base = 'http://localhost:8086';
  expect(isExpectedError('Failed to load resource: 404', base + '/favicon.ico')).toBe(true);
  expect(isExpectedError('Failed to load resource: 401', base + '/api/v1/auth/me')).toBe(true);
  for (const text of ['Failed to fetch', 'net::ERR_CONNECTION_REFUSED', '404', '401']) {
    expect(isExpectedError(text, base + '/api/v1/cases')).toBe(false);
  }
  expect(isExpectedError('favicon.ico parser crashed', base + '/assets/main.js')).toBe(false);
  expect(isExpectedError('Failed to load resource: 500', base + '/favicon.ico')).toBe(false);
  expect(isExpectedError('401')).toBe(false);
});

test('console collector captures application errors and uncaught exceptions', async ({ page }) => {
  const errors = getConsoleErrors(page);
  await page.goto('about:blank');
  await page.evaluate(() => {
    console.error('Failed to fetch application data');
    setTimeout(() => { throw new Error('uncaught regression sentinel'); }, 0);
  });
  await expect.poll(() => errors).toEqual([
    'Failed to fetch application data', 'uncaught regression sentinel',
  ]);
});
