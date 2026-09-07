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

test('error filtering preserves malformed locations and unrelated status digits', () => {
  const base = 'http://localhost:8086';
  for (const url of ['not a URL', '/api/v1/auth/me', 'http://[invalid']) {
    expect(isExpectedError('Failed to load resource: 401', url)).toBe(false);
  }
  for (const message of ['Parser failed at line 401', 'Failed to load resource: 1401']) {
    expect(isExpectedError(message, base + '/api/v1/auth/me')).toBe(false);
  }
  expect(isExpectedError('Parser failed at line 404', base + '/favicon.ico')).toBe(false);
  expect(isExpectedError(
    'Failed to load resource: the server responded with a status of 401 (Unauthorized)',
    base + '/api/v1/auth/login',
  )).toBe(true);
  expect(isExpectedError(
    'Failed to load resource: the server responded with a status of 404 (Not Found)',
    base + '/favicon.ico',
  )).toBe(true);
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
