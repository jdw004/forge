export const states = ['pending', 'running', 'succeeded', 'failed', 'cancelled', 'skipped'];
export const terminal = value => ['succeeded', 'failed', 'cancelled', 'skipped'].includes(value);
export const presets = {
  batch: 'Independent agents with bounded tokens and runtime. Each task takes about 5 seconds.',
  pipeline: 'Research → analyze → summarize. Each task waits for its dependency to succeed.',
  token: 'Intentional runaway agents stop when another step would exceed 100 tokens.',
  runtime: 'Intentional runaway agents stop at a 1.5-second runtime budget.',
};

export function createSpec(name, preset, count) {
  name = name.trim();
  if (!name || new TextEncoder().encode(name).length > 120) throw new Error('Use a workflow name of 1–120 bytes.');
  if (!Object.hasOwn(presets, preset)) throw new Error('Choose a supported preset.');
  count = preset === 'pipeline' ? 3 : Number(count);
  if (!Number.isInteger(count) || count < 1 || count > 2200) throw new Error('Choose between 1 and 2,200 tasks.');
  const base = { steps: 50, step_ms: 100, tokens_per_step: 8, token_limit: 1000, runtime_ms: 15000, max_attempts: 2 };
  const tasks = Array.from({ length: count }, (_, i) => ({ id: `task-${String(i + 1).padStart(4, '0')}`, ...base }));
  if (preset === 'pipeline') {
    ['research', 'analyze', 'summarize'].forEach((id, i, ids) => {
      tasks[i] = { ...base, id, steps: 12, ...(i ? { depends_on: [ids[i - 1]] } : {}) };
    });
  }
  for (const task of tasks) {
    if (preset === 'token') Object.assign(task, { runaway: true, step_ms: 80, tokens_per_step: 25, token_limit: 100, max_attempts: 1 });
    if (preset === 'runtime') Object.assign(task, { runaway: true, step_ms: 50, token_limit: 1000000, runtime_ms: 1500, max_attempts: 1 });
  }
  return { name, tasks };
}

export function summarize(workflows, nodes) {
  const counts = Object.fromEntries(states.map(state => [state, 0]));
  for (const workflow of workflows) for (const state of states) counts[state] += workflow.counts[state] || 0;
  return { ...counts, workers: nodes.length, capacity: nodes.reduce((n, node) => n + node.capacity, 0), active: nodes.reduce((n, node) => n + node.active, 0) };
}

export function progress(workflow) {
  const done = states.filter(terminal).reduce((n, state) => n + (workflow.counts[state] || 0), 0);
  return { done, total: workflow.task_count, percent: workflow.task_count ? Math.round(100 * done / workflow.task_count) : 0 };
}

export function filterWorkflows(workflows, query, state) {
  query = query.trim().toLowerCase();
  return workflows.filter(item => (state === 'all' || item.state === state) && `${item.name} ${item.id}`.toLowerCase().includes(query));
}

export function chartPaths(samples) {
  if (!samples.length) return { line: 'M0 149H600', area: 'M0 149H600V150H0Z', max: 10 };
  const max = Math.max(10, ...samples);
  const points = samples.map((value, i) => `${samples.length === 1 ? 0 : (i * 600 / (samples.length - 1)).toFixed(1)} ${(149 - Math.max(0, value) / max * 145).toFixed(1)}`);
  if (points.length === 1) points.push(`600 ${points[0].split(' ')[1]}`);
  const line = `M${points.join(' L')}`;
  return { line, area: `${line} L600 150 L0 150 Z`, max };
}
