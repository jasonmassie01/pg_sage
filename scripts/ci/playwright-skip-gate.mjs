#!/usr/bin/env node
// Fails a CI step when a Playwright JSON report shows a skipped, failed or
// unloadable test, or no passing test at all (ci-08). A skipped browser test
// is not a passing one: the full-surface fixture provides everything the
// suites need, so any skip means a spec or the fixture is broken.
//
// Usage: node scripts/ci/playwright-skip-gate.mjs <report.json>
// Write the report with: PLAYWRIGHT_JSON_OUTPUT_NAME=<report.json> \
//   npx playwright test --reporter=list,json
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';

const counters = ['expected', 'skipped', 'unexpected', 'flaky'];

function isReport(report) {
  if (report === null || typeof report !== 'object' || Array.isArray(report)) return false;
  const { stats } = report;
  if (stats === null || typeof stats !== 'object') return false;
  return counters.every((key) => Number.isInteger(stats[key] ?? 0));
}

// skippedTitles walks the suite tree: stats alone would miss a skip that a
// retry or a reporter quirk left out of the totals.
function skippedTitles(suites, parents = [], found = []) {
  for (const suite of suites ?? []) {
    const path = suite.title && suite.title !== suite.file ? [...parents, suite.title] : parents;
    for (const spec of suite.specs ?? []) {
      if ((spec.tests ?? []).some((t) => t.status === 'skipped')) {
        found.push(`${spec.file}: ${[...path, spec.title].join(' > ')}`);
      }
    }
    skippedTitles(suite.suites, path, found);
  }
  return found;
}

export function evaluateReport(report) {
  if (!isReport(report)) {
    return { ok: false, passed: 0, flaky: 0, skipped: [],
      problems: ['input is not a Playwright JSON report (no numeric stats)'] };
  }
  const stats = Object.fromEntries(counters.map((key) => [key, report.stats[key] ?? 0]));
  const skipped = skippedTitles(report.suites);
  const problems = [];
  const skipCount = Math.max(stats.skipped, skipped.length);
  if (skipCount > 0) problems.push(`${skipCount} skipped test(s); a skip is not a pass`);
  if (stats.unexpected > 0) problems.push(`${stats.unexpected} failed test(s)`);
  for (const error of report.errors ?? []) {
    problems.push(`report error: ${error?.message ?? JSON.stringify(error)}`);
  }
  if (stats.expected + stats.flaky === 0) problems.push('no test passed; did the suite run?');
  return { ok: problems.length === 0, passed: stats.expected, flaky: stats.flaky,
    problems, skipped };
}

function readReport(file) {
  let text;
  try {
    text = readFileSync(file, 'utf8');
  } catch (error) {
    throw new Error(`cannot read Playwright report ${file}: ${error.message}`);
  }
  try {
    return JSON.parse(text);
  } catch (error) {
    throw new Error(`Playwright report ${file} is not valid JSON: ${error.message}`);
  }
}

function main(argv) {
  if (argv.length !== 1) {
    console.error('usage: playwright-skip-gate.mjs <playwright-report.json>');
    return 2;
  }
  let report;
  try {
    report = readReport(argv[0]);
  } catch (error) {
    console.error(error.message);
    return 1;
  }
  const result = evaluateReport(report);
  const skipCount = Math.max(Number(report?.stats?.skipped) || 0, result.skipped.length);
  console.log(`playwright gate: ${result.passed} passed, ${skipCount} skipped, ` +
    `${result.flaky} flaky`);
  if (result.flaky > 0) console.log(`::warning::${result.flaky} test(s) passed only on retry`);
  if (result.ok) return 0;
  for (const title of result.skipped) console.error(`skipped: ${title}`);
  for (const problem of result.problems) console.error(`::error::${problem}`);
  return 1;
}

if (process.argv[1] === fileURLToPath(import.meta.url)) {
  process.exitCode = main(process.argv.slice(2));
}
