import test from 'node:test';
import assert from 'node:assert/strict';
import { createSpec, summarize, progress, filterWorkflows, chartPaths } from './assets/model.mjs';

test('parallel batches have unique IDs and sufficient successful execution budgets', () => {
  const spec = createSpec('  Batch  ', 'batch', 2200);
  assert.equal(spec.name, 'Batch');
  assert.equal(spec.tasks.length, 2200);
  assert.equal(new Set(spec.tasks.map(task => task.id)).size, 2200);
  for (const task of spec.tasks) {
    assert.ok(task.steps * task.tokens_per_step <= task.token_limit);
    assert.ok(task.steps * task.step_ms < task.runtime_ms);
    assert.equal(task.runaway, undefined);
  }
});

test('pipeline dependencies resolve to earlier tasks', () => {
  const { tasks } = createSpec('Pipeline', 'pipeline', 'ignored');
  const completed = new Set();
  for (const task of tasks) {
    for (const dependency of task.depends_on || []) assert.ok(completed.has(dependency));
    completed.add(task.id);
  }
  assert.deepEqual([...completed], ['research', 'analyze', 'summarize']);
});

test('breaker presets produce bounded intentional failures', () => {
  const token = createSpec('Token test', 'token', 1).tasks[0];
  const runtime = createSpec('Runtime test', 'runtime', 1).tasks[0];
  assert.equal(token.runaway, true);
  assert.equal(token.max_attempts, 1);
  assert.equal(token.token_limit / token.tokens_per_step * token.step_ms, 320);
  assert.equal(runtime.runaway, true);
  assert.equal(runtime.runtime_ms, 1500);
  assert.ok(runtime.runtime_ms / runtime.step_ms * runtime.tokens_per_step < runtime.token_limit);
});

test('invalid or excessive inputs fail before submission', () => {
  for (const count of [0, -1, 1.5, 2201, Infinity, NaN, 'oops']) assert.throws(() => createSpec('name', 'batch', count));
  for (const name of ['', ' ', 'a'.repeat(121), '界'.repeat(41)]) assert.throws(() => createSpec(name, 'batch', 1));
  for (const preset of ['unknown', '__proto__', 'toString']) assert.throws(() => createSpec('name', preset, 1));
});

test('progress includes all terminal outcomes without counting pending tasks', () => {
  const counts = { succeeded: 2, failed: 1, cancelled: 1, skipped: 1, running: 2, pending: 3 };
  assert.deepEqual(progress({ task_count: 10, counts }), { done: 5, total: 10, percent: 50 });
  const totals = summarize([{ counts }], [{ capacity: 8, active: 2 }, { capacity: 4, active: 1 }]);
  assert.equal(totals.capacity, 12); assert.equal(totals.active, 3); assert.equal(totals.workers, 2);
  assert.equal(totals.failed, 1); assert.equal(totals.succeeded, 2);
  assert.equal(progress({ task_count: 0, counts: {} }).percent, 0);
});

test('workflow search and state filters compose', () => {
  const items = [{ id: 'one', name: 'Research', state: 'running' }, { id: 'two', name: 'Research again', state: 'succeeded' }];
  assert.deepEqual(filterWorkflows(items, ' RESEARCH ', 'running'), [items[0]]);
  assert.deepEqual(filterWorkflows(items, 'two', 'all'), [items[1]]);
  assert.deepEqual(filterWorkflows(items, 'missing', 'all'), []);
});

test('charts use only supplied samples and handle zero and single-point histories', () => {
  assert.equal(chartPaths([]).line, 'M0 149H600');
  assert.equal(chartPaths([0]).max, 10);
  assert.equal(chartPaths([0, 20, 0]).max, 20);
  for (const samples of [[], [0], [5], [0, 10, 0]]) {
    const chart = chartPaths(samples);
    assert.ok(!chart.line.includes('NaN')); assert.ok(!chart.area.includes('Infinity'));
  }
});
