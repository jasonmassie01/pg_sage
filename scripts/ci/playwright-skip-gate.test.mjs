// Tests for playwright-skip-gate.mjs (ci-08). Run: node --test scripts/ci/
// No concurrent access tests: the gate reads one report file and keeps no state.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { mkdtempSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { spawnSync } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import { evaluateReport } from './playwright-skip-gate.mjs';

const gate = fileURLToPath(new URL('./playwright-skip-gate.mjs', import.meta.url));

function spec(file, title, status) {
  return { title, file, line: 1, tests: [{ status, projectName: 'chromium' }] };
}

function report(stats, specs = [], suites = []) {
  return {
    stats: { expected: 0, skipped: 0, unexpected: 0, flaky: 0, ...stats },
    suites: [{ title: 'a.spec.ts', file: 'a.spec.ts', specs, suites }],
  };
}

function runGate(content) {
  const dir = mkdtempSync(join(tmpdir(), 'skip-gate-'));
  const file = join(dir, 'report.json');
  if (content !== undefined) writeFileSync(file, content);
  return spawnSync(process.execPath, [gate, file], { encoding: 'utf8' });
}

test('happy path: every test passed, no skip', () => {
  const result = evaluateReport(report({ expected: 87 }));
  assert.deepEqual(result, { ok: true, passed: 87, flaky: 0, problems: [], skipped: [] });
});

test('one skipped test fails the gate and is named with its file', () => {
  const nested = [{ title: 'Fleet', file: 'f.spec.ts',
    specs: [spec('f.spec.ts', 'aggregates three targets', 'skipped')], suites: [] }];
  const result = evaluateReport(report({ expected: 86, skipped: 1 }, [], nested));
  assert.equal(result.ok, false);
  assert.deepEqual(result.skipped, ['f.spec.ts: Fleet > aggregates three targets']);
  assert.match(result.problems.join('\n'), /1 skipped/);
});

test('skips counted in stats but not found in the tree still fail', () => {
  const result = evaluateReport(report({ expected: 3, skipped: 2 }));
  assert.equal(result.ok, false);
  assert.match(result.problems.join('\n'), /2 skipped/);
});

test('a skipped spec missing from stats still fails (tree walk is authoritative too)', () => {
  const result = evaluateReport(report({ expected: 3 }, [spec('a.spec.ts', 'x', 'skipped')]));
  assert.equal(result.ok, false);
  assert.deepEqual(result.skipped, ['a.spec.ts: x']);
});

test('failed tests fail the gate', () => {
  const result = evaluateReport(report({ expected: 3, unexpected: 1 }));
  assert.equal(result.ok, false);
  assert.match(result.problems.join('\n'), /1 failed/);
});

test('boundary: zero tests run fails closed', () => {
  const result = evaluateReport(report({}));
  assert.equal(result.ok, false);
  assert.match(result.problems.join('\n'), /no test passed/);
});

test('boundary: exactly one passing test is enough', () => {
  assert.equal(evaluateReport(report({ expected: 1 })).ok, true);
});

test('flaky tests pass the gate but are counted', () => {
  const result = evaluateReport(report({ expected: 4, flaky: 1 }));
  assert.equal(result.ok, true);
  assert.equal(result.flaky, 1);
});

test('report-level errors (e.g. a spec that fails to load) fail the gate', () => {
  const broken = { ...report({ expected: 2 }), errors: [{ message: 'SyntaxError in x.spec.ts' }] };
  const result = evaluateReport(broken);
  assert.equal(result.ok, false);
  assert.match(result.problems.join('\n'), /SyntaxError in x\.spec\.ts/);
});

test('invalid input: null, array and missing stats are rejected', () => {
  for (const value of [null, [], {}, { stats: null }, { stats: { expected: 'many' } }]) {
    const result = evaluateReport(value);
    assert.equal(result.ok, false, JSON.stringify(value));
    assert.match(result.problems.join('\n'), /not a Playwright JSON report/);
  }
});

test('CLI: exit 0 and a summary line on a clean report', () => {
  const run = runGate(JSON.stringify(report({ expected: 51 })));
  assert.equal(run.status, 0, run.stderr);
  assert.match(run.stdout, /51 passed, 0 skipped, 0 flaky/);
});

test('CLI: exit 1 and the skipped title on stderr', () => {
  const run = runGate(JSON.stringify(report({ expected: 1, skipped: 1 },
    [spec('a.spec.ts', 'needs fixture', 'skipped')])));
  assert.equal(run.status, 1);
  assert.match(run.stderr, /a\.spec\.ts: needs fixture/);
});

test('CLI: missing file, malformed JSON and missing argument exit non-zero', () => {
  const missing = runGate(undefined);
  assert.notEqual(missing.status, 0);
  assert.match(missing.stderr, /cannot read Playwright report/);
  const malformed = runGate('{"stats": ');
  assert.notEqual(malformed.status, 0);
  assert.match(malformed.stderr, /not valid JSON/);
  const noArg = spawnSync(process.execPath, [gate], { encoding: 'utf8' });
  assert.equal(noArg.status, 2);
  assert.match(noArg.stderr, /usage/);
});
