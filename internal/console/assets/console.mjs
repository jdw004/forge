import { states, terminal, presets, createSpec, summarize, progress, filterWorkflows, chartPaths } from './model.mjs';

const $ = id => document.getElementById(id);
const format = new Intl.NumberFormat();
const dateFormat = new Intl.DateTimeFormat(undefined, { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' });
const state = { token: '', generation: 0, workflows: [], nodes: [], system: null, samples: [], connected: false, selected: null, detail: null, page: 0 };
let refreshing = false, refreshAgain = false, detailRequest = 0, submission = null, toastTimer;

function element(tag, className, text) {
  const item = document.createElement(tag);
  if (className) item.className = className;
  if (text !== undefined) item.textContent = String(text);
  return item;
}

function badge(value) {
  const known = states.includes(value) ? value : 'pending';
  return element('span', `state-badge state-${known}`, known);
}

function showNotice(message) { $('notice').textContent = message; $('notice').hidden = !message; }
function toast(message) {
  clearTimeout(toastTimer);
  $('toast').textContent = message;
  $('toast').hidden = false;
  toastTimer = setTimeout(() => { $('toast').hidden = true; }, 4500);
}

async function api(path, options = {}) {
  const controller = new AbortController();
  const timer = setTimeout(() => controller.abort(), 8000);
  const headers = { ...options.headers };
  if (state.token) headers.Authorization = `Bearer ${state.token}`;
  try {
    const response = await fetch(path, { ...options, headers, signal: controller.signal, cache: 'no-store' });
    const body = response.status === 204 ? null : await response.json().catch(() => null);
    if (!response.ok) {
      const error = new Error(response.status === 401 ? 'Authentication required. Enter your API token in Connection settings.' : body?.error?.message || `Request failed (${response.status}).`);
      error.status = response.status;
      throw error;
    }
    if (response.status !== 204 && body === null) throw new Error('The API returned an unexpected response.');
    return body;
  } catch (error) {
    if (error.name === 'AbortError') throw new Error('The API request timed out. Check the connection and try again.');
    throw error;
  } finally { clearTimeout(timer); }
}

function renderOverview() {
  const totals = summarize(state.workflows, state.nodes);
  for (const [id, value] of [['running-count', totals.running], ['completed-count', totals.succeeded], ['failed-count', totals.failed], ['worker-count', totals.workers]]) $(id).textContent = format.format(value);
  $('capacity-label').textContent = `${format.format(totals.capacity)} configured task slots`;
  $('mode-label').textContent = state.system.mode === 'demo' ? 'DEMO CLUSTER' : 'POSTGRESQL CLUSTER';
  $('worker-mode').textContent = state.system.mode === 'demo' ? 'Embedded pools' : 'Independent processes';
  $('worker-description').textContent = state.system.mode === 'demo' ? 'Worker pools in one process · heartbeat freshness under 10 seconds' : 'Independent processes · heartbeat freshness under 10 seconds';
  $('cloud-region').textContent = state.system.cloud.region;
  $('storage-label').textContent = state.system.storage === 'memory' ? 'In-memory queue' : 'PostgreSQL';
  $('tracing-label').textContent = state.system.tracing_configured ? 'Configured · delivery unverified' : 'Not configured';
  state.samples.push(totals.active);
  state.samples = state.samples.slice(-60);
  const paths = chartPaths(state.samples);
  $('chart-line').setAttribute('d', paths.line);
  $('chart-area').setAttribute('d', paths.area);
  $('chart-max').textContent = format.format(paths.max);
  $('chart-status').textContent = `${format.format(totals.active)} active · ${state.samples.length} samples`;
}

function renderWorkflows() {
  const filtered = filterWorkflows(state.workflows, $('workflow-search').value, $('workflow-filter').value);
  const focusedID = document.activeElement?.dataset?.workflow;
  const rows = document.createDocumentFragment();
  for (const workflow of filtered) {
    const row = element('tr', workflow.id === state.selected ? 'selected' : '');
    const nameCell = element('td');
    const name = element('button', 'workflow-name', workflow.name);
    name.title = workflow.name;
    name.dataset.workflow = workflow.id;
    name.addEventListener('click', () => selectWorkflow(workflow.id));
    nameCell.append(name, element('span', 'workflow-id', workflow.id.slice(0, 12)));
    const statusCell = element('td'); statusCell.append(badge(workflow.state));
    const amount = progress(workflow);
    const progressCell = element('td');
    const indicator = element('div', 'progress-cell');
    const bar = element('progress'); bar.max = amount.total || 1; bar.value = amount.done; bar.setAttribute('aria-label', `${amount.done} of ${amount.total} tasks finished`);
    indicator.append(bar, element('span', '', `${amount.percent}%`)); progressCell.append(indicator);
    const created = new Date(workflow.created_at);
    const actionCell = element('td');
    const action = element('button', 'text-button', 'Inspect ↗');
    action.setAttribute('aria-label', `Inspect ${workflow.name}`);
    action.addEventListener('click', () => selectWorkflow(workflow.id)); actionCell.append(action);
    row.append(nameCell, statusCell, progressCell, element('td', '', format.format(workflow.task_count)), element('td', '', Number.isNaN(created.getTime()) ? '—' : dateFormat.format(created)), actionCell);
    rows.append(row);
  }
  $('workflow-rows').replaceChildren(rows);
  if (focusedID) Array.from($('workflow-rows').querySelectorAll('[data-workflow]')).find(item => item.dataset.workflow === focusedID)?.focus({ preventScroll: true });
  $('workflow-count').textContent = format.format(state.workflows.length);
  $('workflow-empty').hidden = filtered.length > 0;
  const searching = state.workflows.length > 0;
  $('workflow-empty').querySelector('h3').textContent = searching ? 'No matching workflows' : 'No executions yet';
  $('workflow-empty').querySelector('p').textContent = searching ? 'Try a different name, ID, or state filter.' : 'Launch a workflow to see your agents in motion.';
  $('workflow-empty').querySelector('button').hidden = searching;
  $('workflow-scope').textContent = `${filtered.length} shown · latest 100 workflows · updates every 2 seconds`;
}

function renderWorkers() {
  const cards = document.createDocumentFragment();
  for (const node of state.nodes) {
    const card = element('article', 'worker-card');
    const top = element('div', 'worker-top'); top.append(element('span', 'worker-icon', '▦'), element('span', 'worker-status', '● Reporting'));
    const id = element('div', 'worker-id', node.id); id.title = node.id;
    const usage = element('progress'); usage.max = node.capacity || 1; usage.value = node.active; usage.setAttribute('aria-label', `${node.active} active out of ${node.capacity} slots`);
    card.append(top, id, element('div', 'worker-meta', `${format.format(node.active)} active / ${format.format(node.capacity)} slots`), usage);
    cards.append(card);
  }
  if (!state.nodes.length) cards.append(element('p', 'muted', 'No workers are reporting. Start a worker connected to this task store.'));
  $('worker-grid').replaceChildren(cards);
}

function renderDetail() {
  const workflow = state.detail;
  if (!workflow || workflow.id !== state.selected) return;
  $('detail-name').textContent = workflow.name;
  $('detail-id').textContent = workflow.id;
  $('cancel-workflow').hidden = terminal(workflow.state);
  $('cancel-workflow').disabled = !state.connected;
  const tokens = workflow.tasks.reduce((total, task) => total + task.tokens, 0);
  $('detail-summary').replaceChildren(badge(workflow.state), element('span', '', `${workflow.tasks.length} tasks`), element('span', '', `${format.format(tokens)} reported tokens`));
  const pages = Math.max(1, Math.ceil(workflow.tasks.length / 100));
  state.page = Math.min(state.page, pages - 1);
  const tasks = workflow.tasks.slice(state.page * 100, (state.page + 1) * 100);
  const rows = document.createDocumentFragment();
  for (const task of tasks) {
    const row = element('tr'); const status = element('td'); status.append(badge(task.state));
    row.append(element('td', 'mono', task.id), status, element('td', '', `${format.format(task.tokens)} / ${format.format(task.token_limit)}`), element('td', '', `${task.attempt} / ${task.max_attempts}`), element('td', 'mono', task.worker_id || '—'), element('td', '', task.reason || (task.depends_on?.length ? `After: ${task.depends_on.join(', ')}` : '—')));
    rows.append(row);
  }
  $('task-rows').replaceChildren(rows);
  $('task-scope').replaceChildren(element('span', '', `Page ${state.page + 1} of ${pages} · ${workflow.tasks.length} tasks`));
  const pager = element('div', 'detail-actions');
  for (const [label, offset] of [['Previous', -1], ['Next', 1]]) {
    const button = element('button', 'text-button', label);
    button.disabled = state.page + offset < 0 || state.page + offset >= pages;
    button.addEventListener('click', () => { state.page += offset; renderDetail(); }); pager.append(button);
  }
  $('task-scope').append(pager);
}

async function loadDetail(id, generation = state.generation) {
  const request = ++detailRequest;
  try {
    const detail = await api(`/v1/workflows/${encodeURIComponent(id)}`);
    if (generation !== state.generation || id !== state.selected || request !== detailRequest) return;
    state.detail = detail; renderDetail();
  } catch (error) {
    if (generation !== state.generation || id !== state.selected || request !== detailRequest) return;
    $('detail-summary').textContent = `Unable to refresh details: ${error.message}`;
    $('cancel-workflow').disabled = true;
  }
}

function selectWorkflow(id) {
  state.selected = id; state.detail = null; state.page = 0;
  $('workflow-detail').hidden = false;
  $('detail-name').textContent = 'Loading workflow…'; $('detail-id').textContent = id;
  $('task-rows').replaceChildren(); $('task-scope').replaceChildren(); $('detail-summary').replaceChildren(); $('cancel-workflow').hidden = true;
  renderWorkflows(); loadDetail(id);
  $('workflow-detail').scrollIntoView({ block: 'start' });
}

async function refresh(force = false) {
  if (document.hidden && !force) return;
  if (refreshing) { refreshAgain = true; return; }
  refreshing = true;
  const generation = state.generation;
  try {
    const [system, workflows, nodes] = await Promise.all([api('/v1/system'), api('/v1/workflows?limit=100'), api('/v1/nodes')]);
    if (generation !== state.generation) return;
    Object.assign(state, { system, workflows: workflows.workflows, nodes: nodes.nodes, connected: true });
    $('connection-indicator').className = 'connection-indicator online';
    $('connection-status').textContent = state.nodes.length ? 'Control plane online · receiving worker heartbeats' : 'Control plane online · no workers reporting';
    $('last-updated').textContent = `Updated ${new Date().toLocaleTimeString()}`;
    showNotice(''); renderOverview(); renderWorkflows(); renderWorkers();
    if (state.selected) await loadDetail(state.selected, generation);
  } catch (error) {
    if (generation !== state.generation) return;
    state.connected = false;
    $('connection-indicator').className = 'connection-indicator offline';
    $('connection-status').textContent = 'Control plane unavailable · displayed data may be stale';
    $('cancel-workflow').disabled = true;
    showNotice(error.message || 'Cannot reach the API. Check your connection.');
  } finally {
    refreshing = false;
    if (refreshAgain) { refreshAgain = false; queueMicrotask(() => refresh(true)); }
  }
}

document.querySelectorAll('[data-open]').forEach(button => button.addEventListener('click', () => $(button.dataset.open).showModal()));
document.querySelectorAll('[data-close]').forEach(button => button.addEventListener('click', () => button.closest('dialog').close()));
$('refresh-button').addEventListener('click', () => refresh(true));
$('workflow-search').addEventListener('input', renderWorkflows);
$('workflow-filter').addEventListener('change', renderWorkflows);
$('close-detail').addEventListener('click', () => { state.selected = null; state.detail = null; detailRequest++; $('workflow-detail').hidden = true; renderWorkflows(); });
$('workflow-preset').addEventListener('change', () => {
  const preset = $('workflow-preset').value;
  $('preset-description').textContent = presets[preset];
  $('task-count').disabled = preset === 'pipeline';
  if (preset === 'pipeline') $('task-count').value = '3';
});
$('connection-form').addEventListener('submit', event => {
  event.preventDefault();
  state.token = $('api-token').value.trim(); $('api-token').value = '';
  state.generation++; state.connected = false; state.samples = []; submission = null;
  $('connection-dialog').close(); refresh(true);
});
$('launch-form').addEventListener('submit', async event => {
  event.preventDefault();
  const generation = state.generation;
  $('launch-error').hidden = true; $('launch-submit').disabled = true;
  try {
    const body = JSON.stringify(createSpec($('workflow-name').value, $('workflow-preset').value, $('task-count').value));
    if (!submission || submission.body !== body) {
      const key = crypto.randomUUID?.() || Array.from(crypto.getRandomValues(new Uint8Array(16)), byte => byte.toString(16).padStart(2, '0')).join('');
      submission = { body, key };
    }
    const workflow = await api('/v1/workflows', { method: 'POST', body, headers: { 'Content-Type': 'application/json', 'Idempotency-Key': submission.key } });
    if (generation !== state.generation) return;
    submission = null; $('launch-dialog').close(); toast(`Workflow launched: ${workflow.name}`);
    selectWorkflow(workflow.id); await refresh(true);
  } catch (error) {
    if (generation !== state.generation) return;
    $('launch-error').textContent = error.message; $('launch-error').hidden = false;
  } finally { $('launch-submit').disabled = false; }
});
$('cancel-workflow').addEventListener('click', async () => {
  const id = state.selected, generation = state.generation;
  if (!id || !state.connected) return;
  $('cancel-workflow').disabled = true;
  try {
    await api(`/v1/workflows/${encodeURIComponent(id)}/cancel`, { method: 'POST' });
    if (generation !== state.generation) return;
    toast('Pending and running tasks cancelled.'); await refresh(true);
  } catch (error) { if (generation === state.generation) showNotice(error.message); }
  finally { if (id === state.selected) $('cancel-workflow').disabled = !state.connected; }
});
document.addEventListener('visibilitychange', () => { if (!document.hidden) refresh(true); });
setInterval(() => refresh(), 2000);
refresh(true);
