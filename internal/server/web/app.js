import { api, post, stream, esc, ago, copy, debounce, RANK, TOKEN } from '/static/common.js';
import { logsTab } from '/static/logs.js';
import { loadReleases, verSpan } from '/static/versions.js';
import { renderSummary } from '/static/summary.js';
import { mountForwardsDock } from '/static/forwards.js';
import { setScope, scopeParam, inScope, picker, pruneClusters, scopeChip } from '/static/scope.js';
import { mountExposure } from '/static/exposure.js';

const view = document.getElementById('view');
const VIEWS = ['overview', 'resources', 'exposure', 'alerts', 'history', 'doctor', 'settings'];
const HEALTH_ORDER = { bad: 0, warn: 1, neutral: 2, ok: 3 };
const SEV_PILL = { critical: 'bad', warning: 'warn', notice: 'notice', info: '' };
let current = null;
let kinds = [];
let clustersCache = null;

// ---------- routing ----------
function route() {
  const [name, qs] = location.hash.slice(1).split('?');
  return { name: VIEWS.includes(name) ? name : 'overview', params: new URLSearchParams(qs || '') };
}
export function go(name, params = {}) {
  const qs = new URLSearchParams(Object.entries(params).filter(([, v]) => v !== '' && v != null && v !== false)).toString();
  location.hash = name + (qs ? '?' + qs : '');
}
window.buddyShowTab = (tab) => go(VIEWS.includes(tab) ? tab : 'overview');

async function render() {
  let r = route();
  if (r.params.has('cluster') || r.params.has('namespace')) {
    if (r.params.get('cluster')) setScope('clusters', [r.params.get('cluster')], { notify: false });
    if (r.params.get('namespace')) setScope('namespaces', [r.params.get('namespace')], { notify: false });
    r.params.delete('cluster'); r.params.delete('namespace');
    const qs = r.params.toString();
    history.replaceState(null, '', `#${r.name}${qs ? '?' + qs : ''}`);
    r = route();
  }
  document.querySelectorAll('nav a[data-view]').forEach((a) => a.classList.toggle('on', a.dataset.view === r.name));
  subnav.hidden = r.name !== 'resources';
  current = r.name;
  closeDrawer();
  exposureHandle?.destroy(); exposureHandle = null;
  try {
    await ({ overview, resources, exposure, alerts: () => alertsView('active'), history: () => alertsView('all'), doctor, settings })[r.name](r.params);
  } catch (e) {
    view.innerHTML = `<div class="card empty"><span class="big">⚠️</span>${esc(e.message)}</div>`;
  }
}
document.querySelectorAll('nav a[data-view]').forEach((a) => a.addEventListener('click', () => go(a.dataset.view)));
window.addEventListener('hashchange', render);
document.addEventListener('buddy:scope', () => { if (drawer.hidden) render(); });
document.addEventListener('click', (e) => { if (e.target.closest('[data-scope-clear]')) setScope('clusters', []); });

// mountPickers fills toolbar placeholders with the shared cluster/namespace pickers.
function mountPickers(root, clusterNames, namespaceNames, { namespaced = true } = {}) {
  root.querySelector('[data-pick=clusters]')?.replaceWith(picker('clusters', clusterNames.map((value) => ({ value }))));
  root.querySelector('[data-pick=namespaces]')?.replaceWith(picker('namespaces', namespaceNames.map((value) => ({ value })),
    { disabled: !namespaced, note: 'This kind is cluster-wide, so the namespace filter doesn\'t apply' }));
}
const PICKERS = '<span data-pick="clusters"></span><span data-pick="namespaces"></span>';

// ---------- resizable sidebar ----------
(() => {
  const root = document.documentElement;
  const handle = document.getElementById('sbResize');
  const set = (w) => root.style.setProperty('--sidebar-w', `${Math.max(180, Math.min(520, w))}px`);
  try { const w = +localStorage.getItem('sidebarWidth'); if (w) set(w); } catch { /* storage unavailable */ }
  handle.addEventListener('pointerdown', (e) => {
    e.preventDefault();
    handle.setPointerCapture(e.pointerId);
    document.body.classList.add('resizing');
    const move = (ev) => set(ev.clientX);
    const up = () => {
      handle.removeEventListener('pointermove', move);
      document.body.classList.remove('resizing');
      try { localStorage.setItem('sidebarWidth', parseInt(getComputedStyle(root).getPropertyValue('--sidebar-w'), 10)); } catch { /* ignore */ }
    };
    handle.addEventListener('pointermove', move);
    handle.addEventListener('pointerup', up, { once: true });
  });
  handle.addEventListener('dblclick', () => { root.style.removeProperty('--sidebar-w'); try { localStorage.removeItem('sidebarWidth'); } catch { /* ignore */ } });
})();

// ---------- helpers ----------
function toast(msg) {
  const t = document.getElementById('toast');
  t.textContent = msg; t.classList.add('on');
  clearTimeout(t._h); t._h = setTimeout(() => t.classList.remove('on'), 1400);
}
window.buddyToast = toast;
async function copyCmd(text) { await copy(text); toast('Copied to clipboard'); }
const healthPill = (h, text) => `<span class="pill ${h === 'ok' ? 'ok' : h === 'bad' ? 'bad' : h === 'warn' ? 'warn' : ''}">${esc(text)}</span>`;
const clusterHealth = (c) => c.status === 'excluded' ? 'neutral' : c.status === 'error' ? 'bad' : c.status === 'signin' ? 'warn'
  : (c.stats && (c.stats.nodesReady < c.stats.nodes || c.stats.podsProblem || c.stats.pvcPending || c.stats.lbPending)) || c.alerts ? 'warn' : 'ok';
const statusLabel = { connected: 'Connected', syncing: 'Syncing', connecting: 'Connecting', error: 'Unreachable', excluded: 'Not monitored', signin: 'Needs Azure sign-in' };
const plural = (n, w) => `${n} ${w}${n === 1 ? '' : 's'}`;

async function loadClusters() { clustersCache = await api('/api/clusters'); return clustersCache; }

function head(title, sub, right = '') {
  return `<div class="page-head"><div><h1>${title}</h1>${sub ? `<p>${sub}</p>` : ''}</div><div class="spacer"></div>${right}</div>`;
}

// ---------- exposure ----------
let exposureHandle = null;
async function exposure(params) {
  const ov = clustersCache || await loadClusters();
  if (current !== 'exposure') return;
  const monitored = ov.clusters.filter((c) => c.status !== 'excluded').map((c) => c.name);
  pruneClusters(monitored);
  view.innerHTML = `
    ${head('Exposure', 'What each cluster exposes to the outside and where that traffic lands: entry point → ingress → service → workload → pod.', '<span data-pick="clusters"></span>')}
    ${scopeChip(monitored.length)}
    <div id="xp"></div>`;
  mountPickers(view, monitored, []);
  exposureHandle = mountExposure(view.querySelector('#xp'), {
    clusters: monitored.filter((n) => inScope('clusters', n)),
    params,
    onState: (p) => {
      const qs = new URLSearchParams(Object.entries(p).filter(([, v]) => v)).toString();
      history.replaceState(null, '', '#exposure' + (qs ? '?' + qs : ''));
    },
    openResource: (kind, t) => openRelated(kind, t),
  });
}

// ---------- overview ----------
async function overview() {
  const [ov, pending] = await Promise.all([loadClusters(), api('/api/alerts?' + new URLSearchParams({ scope: 'pending', limit: '3000', cluster: scopeParam('clusters') })), loadReleases()]);
  if (current !== 'overview') return;
  pruneClusters(ov.clusters.filter((c) => c.status !== 'excluded').map((c) => c.name));
  const allMonitored = ov.clusters.filter((c) => c.status !== 'excluded');
  const monitored = allMonitored.filter((c) => inScope('clusters', c.name));
  const shown = ov.clusters.filter((c) => inScope('clusters', c.name));
  const latest = pending.slice(0, 6);
  const sum = { pending: pending.length, clusters: new Set(pending.map((a) => a.cluster)).size };
  const up = monitored.filter((c) => c.stats);
  const total = (k) => up.reduce((n, c) => n + (c.stats[k] || 0), 0);
  const unreachable = monitored.filter((c) => c.status === 'error').length;
  const signin = monitored.filter((c) => c.status === 'signin').length;
  const nodes = total('nodes'), nodesReady = total('nodesReady'), unhealthy = total('podsProblem');
  view.innerHTML = `
    ${head('Overview', `${plural(allMonitored.length, 'cluster')} monitored${unreachable ? ` · <span style="color:var(--bad)">${unreachable} unreachable</span>` : ''}`,
      `<span data-pick="clusters"></span><button class="btn" id="toDoctor">✚ Run doctor</button>`)}
    ${scopeChip(allMonitored.length)}
    ${ov.kubeconfigError ? `<div class="card empty" style="color:var(--bad)">Kubeconfig error: ${esc(ov.kubeconfigError)}</div>` : ''}
    <div class="stats">
      <div class="card stat ${sum.pending ? 'warn' : ''}"><div class="label">Needs a look</div><div class="value">${sum.pending}</div><div class="foot">on ${plural(sum.clusters, 'cluster')}</div></div>
      <div class="card stat"><div class="label">Clusters online</div><div class="value">${up.length}<small> / ${monitored.length}</small></div><div class="foot">${[unreachable && plural(unreachable, 'unreachable'), signin && `${signin} waiting for Azure sign-in`].filter(Boolean).join(' · ') || 'all reachable'}</div></div>
      <div class="card stat ${nodesReady < nodes ? 'bad' : ''}"><div class="label">Nodes ready</div><div class="value">${nodesReady}<small> / ${nodes}</small></div><div class="foot">across all clusters</div></div>
      <div class="card stat"><div class="label">Pods running</div><div class="value">${total('podsRunning')}<small> / ${total('pods') - total('podsDone')}</small></div><div class="foot">+ ${total('podsDone')} completed · ${total('warnings1h')} warnings/1h</div></div>
      <div class="card stat ${unhealthy ? 'bad' : ''}"><div class="label">Unhealthy pods</div><div class="value">${unhealthy}</div><div class="foot">${total('pvcPending')} PVC · ${total('lbPending')} LB pending</div></div>
    </div>
    ${latest.length ? `<div class="section-title">Needs a look <small>most important first</small><span class="spacer" style="flex:1"></span><a style="font-weight:500;font-size:12.5px;cursor:pointer" id="allAlerts">See all →</a></div>
      <div class="card alerts">${latest.map(alertRow).join('')}</div>` : ''}
    <div class="section-title">Clusters <small>click one to browse its resources</small></div>
    <div class="clusters">${shown.map(clusterCard).join('')}</div>`;
  mountPickers(view, allMonitored.map((c) => c.name), []);
  view.querySelector('#toDoctor').onclick = () => go('doctor');
  view.querySelector('#allAlerts')?.addEventListener('click', () => go('alerts'));
  view.querySelectorAll('.cluster').forEach((el) => el.onclick = () => {
    if (el.dataset.status === 'signin') { main.scrollTo({ top: 0, behavior: 'smooth' }); return; }
    if (el.dataset.status === 'excluded' || el.dataset.status === 'error') { go('settings'); return; }
    setScope('clusters', [el.dataset.name], { notify: false });
    go('resources', { kind: 'pods' });
  });
  bindAlertRows(view, latest);
}

function clusterCard(c) {
  const h = clusterHealth(c), s = c.stats;
  const flags = [];
  if (s?.podsProblem) flags.push(`<span class="pill bad">${plural(s.podsProblem, 'unhealthy pod')}</span>`);
  if (s?.pvcPending) flags.push(`<span class="pill warn">${s.pvcPending} PVC pending</span>`);
  if (s?.lbPending) flags.push(`<span class="pill warn">${s.lbPending} LB pending</span>`);
  if (c.alerts) flags.push(`<span class="pill accent">${plural(c.alerts, 'alert')}</span>`);
  if (c.resourceErrors) flags.push(`<span class="pill warn" title="${esc(accessTooltip(c.resourceErrors))}">limited access</span>`);
  const active = s ? s.pods - s.podsDone : 0;
  const podPct = active ? (s.podsRunning / active) * 100 : 0, badPct = active ? (s.podsProblem / active) * 100 : 0;
  return `<div class="card cluster h-${h}" data-name="${esc(c.name)}" data-status="${c.status}">
    <div class="top"><span class="dot ${c.status === 'connected' ? h : c.status}"></span><span class="name" title="${esc(c.name)}">${esc(c.name)}</span>${verSpan(c.version)}</div>
    ${s ? `<div class="metrics">
        <div><b class="${s.nodesReady < s.nodes ? 'bad' : ''}">${s.nodesReady}/${s.nodes}</b>nodes</div>
        <div title="${s.podsDone} completed job pods not counted"><b>${s.podsRunning}/${s.pods - s.podsDone}</b>pods${s.podsDone ? ` · ${s.podsDone} done` : ''}</div>
        <div><b>${s.warnings1h}</b>warnings/1h</div></div>
      <div class="bar"><span style="width:${podPct - badPct}%;background:var(--ok)"></span><span style="width:${badPct}%;background:var(--bad)"></span></div>
      <div class="flags">${flags.join('') || '<span class="muted" style="font-size:12px">All good</span>'}</div>`
    : `<div class="muted" style="margin-top:8px;font-size:12.5px">${statusLabel[c.status] || c.status}</div>${c.error ? `<div class="err">${esc(c.error)}</div>` : ''}`}
  </div>`;
}

function accessTooltip(errs) {
  const entries = Object.entries(errs);
  const denied = entries.filter(([, e]) => /forbidden/i.test(e)).map(([r]) => r);
  const other = entries.filter(([, e]) => !/forbidden/i.test(e));
  const lines = [];
  if (denied.length) lines.push(`No access to: ${denied.join(', ')}`);
  for (const [r, e] of other) lines.push(`${r}: ${e.replace(/^.*?: /, '').slice(0, 140)}`);
  const sample = entries.find(([, e]) => /forbidden/i.test(e));
  if (sample) lines.push('', sample[1].replace(/^.*?forbidden: /i, '').slice(0, 200));
  return lines.join('\n');
}

// ---------- resources ----------
const resState = { sort: null, asc: true };
let crds = [];
let crdsAt = 0;
let lastList = null;
const subnav = document.getElementById('subnav');

async function loadKinds() {
  if (!kinds.length) kinds = await api('/api/kinds');
  if (Date.now() - crdsAt > 5 * 60 * 1000) {
    crdsAt = Date.now();
    crds = await api('/api/crds').catch(() => crds);
  }
}

const crKind = (c) => `cr:${c.group}/${c.resource}`;
function kindLabel(id) {
  const k = kinds.find((x) => x.id === id);
  if (k) return k.label;
  const c = crds.find((x) => crKind(x) === id);
  return c ? `${c.kind}s` : id.replace(/^cr:/, '');
}

function renderSubnav(active) {
  const sections = [...new Set(kinds.map((k) => k.section))];
  const groups = {};
  crds.forEach((c) => (groups[c.group] ||= []).push(c));
  const filter = subnav.dataset.filter || '';
  const groupNames = Object.keys(groups).sort().filter((g) => !filter || g.includes(filter) || groups[g].some((c) => c.kind.toLowerCase().includes(filter)));
  subnav.innerHTML = `
    ${sections.map((sec) => `<div class="sub-section">${esc(sec)}</div>
      ${kinds.filter((k) => k.section === sec).map((k) => `<a data-kind="${k.id}" class="${k.id === active ? 'on' : ''}">${esc(k.label)}</a>`).join('')}`).join('')}
    <div class="sub-section">Custom resources ${crds.length ? `<span class="muted">${crds.length}</span>` : ''}</div>
    ${crds.length ? `<input class="sub-filter" id="crdFilter" placeholder="Filter CRDs…" value="${esc(filter)}" spellcheck="false">` : '<div class="sub-empty">None found</div>'}
    ${groupNames.map((g) => {
      const items = groups[g].filter((c) => !filter || g.includes(filter) || c.kind.toLowerCase().includes(filter));
      const open = filter || items.some((c) => crKind(c) === active);
      return `<details class="sub-group" ${open ? 'open' : ''}><summary title="${esc(g)}">${esc(g)}</summary>
        ${items.map((c) => `<a data-kind="${esc(crKind(c))}" class="${crKind(c) === active ? 'on' : ''}" title="${esc(c.kind)} · on ${c.clusters.length} cluster(s)">${esc(c.kind)}<span class="muted">${c.clusters.length}</span></a>`).join('')}
      </details>`;
    }).join('')}`;
  subnav.querySelectorAll('a[data-kind]').forEach((a) => a.onclick = () => {
    resState.sort = null;
    const p = route().params;
    go('resources', { kind: a.dataset.kind, problems: p.get('problems') || '' });
  });
  const fi = subnav.querySelector('#crdFilter');
  if (fi) {
    fi.oninput = debounce(() => { subnav.dataset.filter = fi.value.toLowerCase(); renderSubnav(active); subnav.querySelector('#crdFilter')?.focus(); }, 150);
    if (filter) fi.setSelectionRange(filter.length, filter.length);
  }
  subnav.querySelector('a.on')?.scrollIntoView({ block: 'nearest' });
}

// ---------- CPU / memory cells (pods) ----------
const fmtCPU = (m) => (m >= 1000 ? `${+(m / 1000).toFixed(m >= 10000 ? 0 : 1)} cores` : `${m}m`);
const fmtMem = (b) => { const u = ['B', 'Ki', 'Mi', 'Gi', 'Ti']; let i = 0; while (b >= 1024 && i < u.length - 1) { b /= 1024; i++; } return `${b >= 100 || i === 0 ? Math.round(b) : b.toFixed(1)}${u[i]}`; };
function usageCell(used, req, lim, fmt, isMem) {
  const has = used >= 0;
  const ref = lim > 0 ? lim : req > 0 ? req : 0;
  const pct = has && ref ? Math.min(100, (used / ref) * 100) : 0;
  const tone = !has ? '' : lim > 0 && used >= lim * 0.9 ? 'bad' : req > 0 && used > req ? (isMem ? 'warn' : 'over') : 'ok';
  const tip = [has ? `using ${fmt(used)}` : 'no live sample', req > 0 ? `request ${fmt(req)}` : 'no request', lim > 0 ? `limit ${fmt(lim)}` : 'no limit',
    has && ref ? `${Math.round((used / ref) * 100)}% of ${lim > 0 ? 'limit' : 'request'}` : ''].filter(Boolean).join(' · ');
  return `<td class="uc ${tone}" title="${esc(tip)}"><span>${has ? fmt(used) : req > 0 ? `<span class="muted">req ${fmt(req)}</span>` : '<span class="muted">–</span>'}</span>${ref ? `<i><b style="width:${pct}%"></b>${lim > 0 && req > 0 ? `<u style="left:${Math.min(100, (req / lim) * 100)}%"></u>` : ''}</i>` : ''}</td>`;
}
const usageCells = (u, which) => !u ? '<td></td>' : which === 'cpu' ? usageCell(u.cpu, u.cpuReq, u.cpuLim, fmtCPU) : usageCell(u.mem, u.memReq, u.memLim, fmtMem, true);

// diskCell colours "3/16" as the node nears its attach limit.
function diskCell(v) {
  const [a, l] = String(v).split('/').map(Number);
  const tone = l > 0 ? (a >= l ? 'bad' : a >= l * 0.8 ? 'warn' : '') : '';
  return `<td class="dkc ${tone}" title="${l > 0 ? `${a} disks attached of ${l} the node can take` : `${a} disks attached (no attach limit advertised)`}">${esc(v)}</td>`;
}

// ---------- column picker ----------
// Columns hidden by default per kind; everything else shows. People's own picks override these.
const DEFAULT_HIDDEN = { pods: ['c:IP'], nodes: ['c:OS image'] };
function hiddenColumns(kind) {
  try { const v = localStorage.getItem('buddy.cols.' + kind); if (v) return new Set(JSON.parse(v)); } catch { /* storage unavailable */ }
  return new Set(DEFAULT_HIDDEN[kind] || []);
}
function saveHidden(kind, set) { try { localStorage.setItem('buddy.cols.' + kind, JSON.stringify([...set])); } catch { /* ignore */ } }
function resetHidden(kind) { try { localStorage.removeItem('buddy.cols.' + kind); } catch { /* ignore */ } }

async function resources(params) {
  await Promise.all([loadKinds(), loadReleases()]);
  const ov = clustersCache || await loadClusters();
  const kind = params.get('kind') || 'pods';
  const f = { kind, q: params.get('q') || '', problems: params.get('problems') === '1', sort: params.get('sort') || '' };
  const clusterNames = ov.clusters.filter((c) => c.stats).map((c) => c.name);
  renderSubnav(kind);
  const crd = crds.find((c) => crKind(c) === kind);
  view.innerHTML = `
    ${head(esc(kindLabel(kind)), crd ? `<code>${esc(crd.group)}/${esc(crd.version)}</code> · custom resource on ${plural(crd.clusters.length, 'cluster')}` : 'Across all your clusters.')}
    <div class="toolbar">
      <span id="fPickers">${PICKERS}</span>
      <input class="input search" id="fQ" placeholder="Search name, status, image, IP…" value="${esc(f.q)}" autocomplete="off" spellcheck="false">
      <label class="toggle"><input type="checkbox" id="fProblems" ${f.problems ? 'checked' : ''}> Only problems</label>
      <span class="colpick"><button class="btn sm" id="fCols" title="Choose columns">Columns</button><div class="colpick-menu" id="fColsMenu" hidden></div></span>
      <span class="spacer" style="flex:1"></span><span class="muted" id="resCount"></span>
    </div>
    <div class="card"><div class="table-wrap" id="resTable"><div class="empty">Loading…</div></div><div class="table-foot" id="resFoot" hidden></div></div>`;

  const update = (patch) => go('resources', { ...f, problems: f.problems ? '1' : '', ...patch });
  resources.setSort = (s) => update({ sort: s });
  view.querySelector('#fProblems').onchange = (e) => update({ problems: e.target.checked ? '1' : '' });
  const q = view.querySelector('#fQ');
  q.oninput = debounce(() => { f.q = q.value; history.replaceState(null, '', '#resources?' + new URLSearchParams(Object.entries({ ...f, problems: f.problems ? '1' : '' }).filter(([, v]) => v))); load(); }, 250);
  if (f.q) { q.focus(); q.setSelectionRange(f.q.length, f.q.length); }

  async function load() {
    const qs = new URLSearchParams({ kind: f.kind, cluster: scopeParam('clusters'), namespace: scopeParam('namespaces'), q: f.q, problems: f.problems ? '1' : '', sort: f.sort });
    const list = await api('/api/resources?' + qs);
    if (current !== 'resources' || route().params.get('kind') && route().params.get('kind') !== f.kind) return;
    lastList = list;
    const pick = view.querySelector('#fPickers');
    pick.innerHTML = PICKERS;
    mountPickers(pick, clusterNames, list.namespaces, { namespaced: list.namespaced });
    view.querySelector('#resCount').textContent = `${list.total.toLocaleString()} ${list.total === 1 ? 'item' : 'items'}${list.problems ? ` · ${list.problems} with problems` : ''}`;
    renderTable(list);
  }

  // Columns are descriptors so people can pick which ones show; the choice is kept per kind.
  function renderColumnPicker(all, list) {
    const btn = view.querySelector('#fCols'), menu = view.querySelector('#fColsMenu');
    if (!btn) return;
    const hidden = hiddenColumns(f.kind);
    const pickable = all.filter((c) => !c.fixed);
    menu.innerHTML = `<div class="colpick-head">Columns<button class="btn sm" data-reset>Defaults</button></div>${pickable.map((c) => `<label><input type="checkbox" data-col="${esc(c.id)}" ${hidden.has(c.id) ? '' : 'checked'}> ${esc(c.label)}</label>`).join('')}`;
    menu.querySelectorAll('[data-col]').forEach((cb) => cb.onchange = () => {
      const h = hiddenColumns(f.kind);
      cb.checked ? h.delete(cb.dataset.col) : h.add(cb.dataset.col);
      saveHidden(f.kind, h);
      renderTable(list);
    });
    menu.querySelector('[data-reset]').onclick = () => { resetHidden(f.kind); renderTable(list); };
    btn.onclick = (e) => { e.stopPropagation(); menu.hidden = !menu.hidden; };
    menu.onclick = (e) => e.stopPropagation();
    if (!resources.closePicker) { resources.closePicker = true; document.addEventListener('click', () => { const m = document.getElementById('fColsMenu'); if (m) m.hidden = true; }); }
  }

  function columnsFor(list) {
    const usage = list.items.some((r) => r.usage);
    const nodes = f.kind === 'nodes';
    const cols = [
      { id: 'health', label: '', fixed: true, td: (r) => `<td style="width:18px"><span class="dot ${r.health}"></span></td>`, key: (r) => HEALTH_ORDER[r.health] },
      { id: 'name', label: 'Name', fixed: true, td: (r) => `<td class="name" title="${esc(r.name)}">${esc(r.name)}</td>`, key: (r) => r.name },
      ...(list.namespaced ? [{ id: 'namespace', label: 'Namespace', td: (r) => `<td>${esc(r.namespace)}</td>`, key: (r) => r.namespace }] : []),
      { id: 'cluster', label: 'Cluster', td: (r) => `<td class="muted">${esc(r.cluster)}</td>`, key: (r) => r.cluster },
      { id: 'status', label: 'Status', td: (r) => `<td>${healthPill(r.health, r.status)}</td>`, key: (r) => r.status },
      ...(usage ? [
        { id: 'cpu', label: 'CPU', server: 'cpu', td: (r) => usageCells(r.usage, 'cpu'), key: (r) => -(r.usage?.cpu ?? -1) },
        { id: 'memory', label: 'Memory', server: 'memory', td: (r) => usageCells(r.usage, 'mem'), key: (r) => -(r.usage?.mem ?? -1) },
      ] : []),
      ...list.columns.map((label, ci) => ({
        id: 'c:' + label, label,
        td: (r) => {
          const v = r.cols[ci] ?? '';
          if (nodes && label === 'Version') return `<td>${verSpan(v)}</td>`;
          if (nodes && label === 'Disks') return diskCell(v);
          return `<td title="${esc(v)}">${esc(v)}</td>`;
        },
        key: (r) => { const v = r.cols[ci]; const n = parseFloat(v); return isNaN(n) || String(n) !== String(v).split('/')[0] ? v : n; },
      })),
      { id: 'age', label: 'Age', td: (r) => `<td class="muted">${ago(r.created)}</td>`, key: (r) => -new Date(r.created).getTime() },
    ];
    const hidden = hiddenColumns(f.kind);
    return { all: cols, shown: cols.filter((c) => c.fixed || !hidden.has(c.id)) };
  }

  function renderTable(list) {
    const { all, shown } = columnsFor(list);
    const items = [...list.items];
    const sortCol = shown.find((c) => c.id === resState.sort);
    if (sortCol && !(sortCol.server && f.sort)) {
      items.sort((a, b) => { const x = sortCol.key(a), y = sortCol.key(b); return (x < y ? -1 : x > y ? 1 : 0) * (resState.asc ? 1 : -1); });
    }
    renderColumnPicker(all, list);
    const wrap = view.querySelector('#resTable');
    if (!items.length) {
      const waiting = list.unavailable.some((u) => u.reason === 'loading');
      const denied = list.unavailable.length && list.unavailable.every((u) => u.reason === 'no access');
      wrap.innerHTML = `<div class="empty"><span class="big">${waiting ? '⏳' : denied ? '🔒' : f.problems ? '✨' : '🔍'}</span>${waiting ? 'Loading…' : denied ? "You don't have access to list these on any cluster." : f.problems ? 'No problems here.' : 'Nothing here.'}</div>`;
    } else {
      const sortedId = f.sort ? (f.sort === 'cpu' ? 'cpu' : 'memory') : resState.sort;
      wrap.innerHTML = `<table><thead><tr>${shown.map((c) => `<th data-id="${esc(c.id)}" class="${sortedId === c.id ? 'sorted' + (resState.asc || f.sort ? '' : ' asc') : ''}"${c.server ? ' title="Sort heaviest first, across all pods"' : ''}>${esc(c.label)}</th>`).join('')}</tr></thead><tbody>
        ${items.map((r, idx) => `<tr data-idx="${idx}">${shown.map((c) => c.td(r)).join('')}</tr>`).join('')}
      </tbody></table>`;
      wrap.querySelectorAll('th').forEach((th) => th.onclick = () => {
        const c = shown.find((x) => x.id === th.dataset.id);
        // CPU/Memory sort on the server so "heaviest" covers every pod, not just the rows shown.
        if (c.server) { resState.sort = null; resources.setSort(f.sort === c.server ? '' : c.server); return; }
        if (f.sort) { f.sort = ''; history.replaceState(null, '', '#resources?' + new URLSearchParams(Object.entries({ ...f, problems: f.problems ? '1' : '' }).filter(([, v]) => v))); }
        resState.asc = resState.sort === c.id ? !resState.asc : true;
        resState.sort = c.id;
        renderTable(list);
      });
      wrap.querySelectorAll('tbody tr').forEach((tr) => tr.onclick = () => {
        const r = items[+tr.dataset.idx];
        if (f.kind === 'events') eventDrawer(r); else resourceDrawer(f.kind, r, list.columns);
      });
    }
    const usage = shown.some((c) => c.server);
    const foot = view.querySelector('#resFoot');
    const notes = [];
    if (list.total > list.items.length) notes.push(`Showing first ${list.items.length.toLocaleString()} of ${list.total.toLocaleString()} — narrow it down with filters.`);
    const by = (r) => list.unavailable.filter((u) => u.reason === r).map((u) => u.cluster);
    if (by('loading').length) notes.push(`Loading from ${plural(by('loading').length, 'cluster')}… (first look at this kind takes a few seconds)`);
    if (by('no access').length) notes.push(`No access on ${by('no access').join(', ')}`);
    if (by('error').length) notes.push(`Errors on ${by('error').join(', ')}`);
    if (usage && list.noMetrics?.length) notes.push(`No live usage from ${list.noMetrics.join(', ')} (metrics API unavailable) — showing requests only`);
    if (by('loading').length) setTimeout(() => current === 'resources' && resources.refresh?.(), 2500);
    foot.hidden = !notes.length; foot.textContent = notes.join(' · ');
    foot.title = list.unavailable.filter((u) => u.detail).map((u) => `${u.cluster}: ${u.detail}`).join('\n');
  }
  await load();
  resources.refresh = load;
}

// ---------- drawer ----------
const drawer = document.getElementById('drawer');
let drawerSeq = 0;
// The open tab's cleanup (a log stream, a timer) runs when another tab or drawer replaces it.
let drawerCleanup = null;
const runDrawerCleanup = () => { const f = drawerCleanup; drawerCleanup = null; f?.(); };
function openDrawer({ eyebrow, title, tabs }) {
  runDrawerCleanup();
  const seq = ++drawerSeq;
  document.getElementById('dEyebrow').innerHTML = eyebrow;
  document.getElementById('dTitle').textContent = title;
  const tabsEl = document.getElementById('dTabs');
  const body = document.getElementById('dBody');
  tabsEl.innerHTML = tabs.map((t, i) => `<button data-i="${i}" class="${i === 0 ? 'on' : ''}">${esc(t.label)}</button>`).join('');
  const bind = () => {
    body.querySelectorAll('[data-copy]').forEach((b) => b.onclick = () => copyCmd(b.dataset.copy));
    body.querySelectorAll('[data-azure-login]').forEach((b) => b.onclick = () => { startAzureLogin(); closeDrawer(); });
  };
  const show = async (i) => {
    runDrawerCleanup();
    tabsEl.querySelectorAll('button').forEach((b) => b.classList.toggle('on', +b.dataset.i === i));
    const t = tabs[i];
    body.scrollTop = 0;
    if (t.load && t.html == null) {
      body.innerHTML = '<div class="empty">Loading…</div>';
      try { t.html = await t.load(); } catch (e) { t.html = `<div class="empty" style="color:var(--bad)">${esc(e.message)}</div>`; }
      if (seq !== drawerSeq || !tabsEl.querySelector(`button[data-i="${i}"].on`)) return;
    }
    body.innerHTML = t.html;
    bind();
    t.bind?.(body);
    drawerCleanup = t.cleanup || null;
  };
  tabsEl.querySelectorAll('button').forEach((b) => b.onclick = () => show(+b.dataset.i));
  show(0);
  drawer.hidden = false;
}
function closeDrawer() { runDrawerCleanup(); drawer.hidden = true; }

// The drawer's width follows the grip on its left edge and is remembered between sessions.
(() => {
  const grip = drawer.querySelector('.drawer-grip');
  const set = (w) => { drawer.style.setProperty('--drawer-w', `${Math.round(w)}px`); };
  try { const w = +localStorage.getItem('buddy.drawer.w'); if (w >= 360) set(w); } catch { /* storage unavailable */ }
  grip.addEventListener('pointerdown', (e) => {
    e.preventDefault();
    grip.setPointerCapture(e.pointerId);
    drawer.classList.add('resizing');
    const move = (ev) => set(Math.min(Math.max(window.innerWidth - ev.clientX, 360), window.innerWidth * 0.96));
    const up = () => {
      grip.removeEventListener('pointermove', move);
      drawer.classList.remove('resizing');
      const w = parseInt(drawer.style.getPropertyValue('--drawer-w'), 10);
      try { if (w) localStorage.setItem('buddy.drawer.w', String(w)); } catch { /* ignore */ }
    };
    grip.addEventListener('pointermove', move);
    grip.addEventListener('pointerup', up, { once: true });
  });
  grip.addEventListener('dblclick', () => { drawer.style.removeProperty('--drawer-w'); try { localStorage.removeItem('buddy.drawer.w'); } catch { /* ignore */ } });
})();
drawer.querySelectorAll('[data-close]').forEach((el) => el.onclick = closeDrawer);
document.addEventListener('keydown', (e) => {
  if (e.key === 'Escape' && !drawer.hidden) closeDrawer();
  if (e.key === '/' && current === 'resources' && document.activeElement?.tagName !== 'INPUT') { e.preventDefault(); view.querySelector('#fQ')?.focus(); }
});

const cmdBlock = (cmds) => cmds.map((c) => `<div class="cmd"><pre class="code">${esc(c)}</pre><button class="btn sm" data-copy="${esc(c)}">Copy</button></div>`).join('');

async function describeText(kind, r) {
  const u = new URL('/api/describe', location.origin);
  Object.entries({ cluster: r.cluster, kind, namespace: r.namespace || '', name: r.name }).forEach(([k, v]) => u.searchParams.set(k, v));
  const res = await fetch(u, { headers: { 'X-Buddy-Token': TOKEN } });
  const text = await res.text();
  if (!res.ok) throw new Error(text.trim() || res.statusText);
  return text;
}

const RUNNABLE = /^(kubectl|helm|cmctl)\s/;
// shQuote quotes a value for display in a command line (kubeconfig context names can contain anything).
const shQuote = (s) => /^[A-Za-z0-9_.\/:@-]+$/.test(s) ? s : `'${String(s).replace(/'/g, `'\\''`)}'`;
const consoleHistory = [];

// consoleTab renders suggested commands with Run buttons plus a free-form prompt; output stays in the tab.
function consoleTab(cmds, cluster, namespace) {
  const prefill = `kubectl --context ${shQuote(cluster)}${namespace ? ' -n ' + shQuote(namespace) : ''} `;
  const lines = cmds.filter((c) => c.trim());
  return {
    label: 'Console',
    html: `<p class="muted" style="margin-top:0">Runs on this Mac with your kubeconfig. Read-only commands run right away; anything that changes the cluster asks first.</p>
      ${lines.map((c) => RUNNABLE.test(c)
        ? `<div class="cmd"><pre class="code">${esc(c)}</pre><button class="btn sm" data-copy="${esc(c)}">Copy</button><button class="btn sm primary" data-run="${esc(c)}">Run</button></div>`
        : `<p class="muted">${esc(c)}</p>`).join('')}
      <div class="console">
        <div class="console-log" id="cLog"></div>
        <form class="console-prompt" id="cForm"><span>$</span><input id="cIn" value="${esc(prefill)}" spellcheck="false" autocomplete="off"><button class="btn sm primary">Run</button></form>
      </div>`,
    bind(root) {
      const log = root.querySelector('#cLog');
      const input = root.querySelector('#cIn');
      let hist = consoleHistory.length;
      const run = async (command) => {
        const block = document.createElement('div');
        block.className = 'console-block';
        block.innerHTML = `<div class="console-cmd">$ ${esc(command)}</div><pre class="console-out">running…</pre>`;
        log.appendChild(block);
        block.scrollIntoView({ block: 'nearest' });
        consoleHistory.push(command); hist = consoleHistory.length;
        const out = block.querySelector('.console-out');
        const exec = (confirm) => api('/api/exec', { method: 'POST', body: JSON.stringify({ command, confirm }) });
        try {
          let res = await exec(false);
          if (res.needsConfirm) {
            // In-page confirmation: the Mac app's web view doesn't show window.confirm dialogs.
            out.innerHTML = `<span class="console-warn">⚠ ${esc(res.confirmReason)}</span>
              <span class="console-confirm"><button class="btn sm danger" data-yes>Run it</button><button class="btn sm" data-no>Cancel</button></span>`;
            const ok = await new Promise((resolve) => {
              out.querySelector('[data-yes]').onclick = () => resolve(true);
              out.querySelector('[data-no]').onclick = () => resolve(false);
            });
            if (!ok) { out.textContent = 'cancelled'; out.classList.add('muted'); return; }
            out.textContent = 'running…';
            res = await exec(true);
          }
          out.textContent = res.error ? res.error : (res.output || '(no output)') + (res.truncated ? '\n… output truncated' : '');
          block.querySelector('.console-cmd').insertAdjacentHTML('beforeend',
            `<span class="console-meta ${res.exitCode === 0 ? 'ok' : 'bad'}">${res.error && res.exitCode === -1 ? 'blocked' : 'exit ' + res.exitCode}${res.durationMs ? ' · ' + (res.durationMs / 1000).toFixed(1) + 's' : ''}</span>`);
          if (res.exitCode !== 0) out.classList.add('bad');
        } catch (e) { out.textContent = e.message; out.classList.add('bad'); }
        block.scrollIntoView({ block: 'nearest' });
      };
      root.querySelectorAll('[data-run]').forEach((b) => b.onclick = () => run(b.dataset.run));
      root.querySelector('#cForm').onsubmit = (e) => { e.preventDefault(); const c = input.value.trim(); if (c) run(c); };
      input.onkeydown = (e) => {
        if (e.key === 'ArrowUp' && hist > 0) { input.value = consoleHistory[--hist]; e.preventDefault(); }
        if (e.key === 'ArrowDown' && hist < consoleHistory.length) { input.value = consoleHistory[++hist] ?? prefill; e.preventDefault(); }
      };
    },
  };
}

// Maps an object kind (as found on alerts and doctor findings) to a describable resource id.
const KIND_IDS = {
  Pod: 'pods', Deployment: 'deployments', StatefulSet: 'statefulsets', DaemonSet: 'daemonsets', ReplicaSet: 'cr:apps/replicasets',
  Job: 'jobs', CronJob: 'cronjobs', Service: 'services', Ingress: 'ingresses', PersistentVolumeClaim: 'pvcs', PersistentVolume: 'pvs',
  Node: 'nodes', ConfigMap: 'configmaps', HorizontalPodAutoscaler: 'hpas', PodDisruptionBudget: 'pdbs',
  Certificate: 'cr:cert-manager.io/certificates', Issuer: 'cr:cert-manager.io/issuers', ClusterIssuer: 'cr:cert-manager.io/clusterissuers',
};

function describeTab(kindId, target) {
  return { label: 'Describe', load: async () => {
    const text = await describeText(kindId, target);
    return `<div style="display:flex;justify-content:flex-end;margin-bottom:8px"><button class="btn sm" data-copy="${esc(text)}">Copy</button></div><pre class="code yaml">${esc(text)}</pre>`;
  } };
}

// routingTab draws every ingress that shares a host with the given one: which is served, which is
// ignored, and where an ignored ingress's paths really end up.
function routingTab(cluster, namespace, name) {
  return { label: 'Routing', load: async () => {
    const hosts = await api('/api/ingress-routes?' + new URLSearchParams({ cluster, namespace, name }));
    if (!hosts.length) return '<p class="muted">This ingress has no host rules.</p>';
    const me = `${namespace}/${name}`;
    return hosts.map((h) => `
      <div class="rm">
        <div class="rm-host"><span class="rm-globe">🌐</span><b>${esc(h.host)}</b><span class="muted">class ${esc(h.class || '—')}${h.controller ? ' · ' + esc(h.controller) : ''}</span></div>
        <div class="rm-list">
        ${h.ingresses.map((ri) => {
          const id = `${ri.namespace}/${ri.name}`;
          const ok = ri.served && !ri.reason;
          return `<div class="rm-ing ${ok ? 'ok' : 'bad'} ${id === me ? 'me' : ''}">
            <div class="rm-ing-head">
              <span class="dot ${ok ? 'ok' : 'bad'}"></span><b title="${esc(id)}">${esc(ri.name)}</b>
              <span class="pill">${esc(ri.type)}</span>${ri.tls ? '<span class="pill">TLS</span>' : ''}
              <span class="rm-state">${ok ? `served · ${esc(ri.address)}` : 'ignored'}</span>
            </div>
            ${ri.namespace !== namespace ? `<div class="muted" style="font-size:12px">namespace ${esc(ri.namespace)}</div>` : ''}
            ${ri.paths.length ? ri.paths.map((p) => `
              <div class="rm-path">
                <code>${esc(p.path)}</code><span class="rm-arrow ${ok ? '' : 'dead'}">${ok ? '──▶' : '─ ─▶'}</span>
                <span class="rm-svc ${ok ? '' : 'dead'}">${esc(p.service || '—')}${p.port ? ':' + esc(p.port) : ''}</span>
              </div>
              ${p.fallsTo ? `<div class="rm-fall">↳ actually answered by <b>${esc(p.fallsTo.ingress.split('/')[1])}</b> <code>${esc(p.fallsTo.path)}</code> ──▶ <span class="rm-svc">${esc(p.fallsTo.service)}</span></div>`
                : (!ok ? '<div class="rm-fall bad">↳ no served route matches: requests get a 404</div>' : '')}`).join('')
            : `<div class="muted" style="font-size:12.5px">${ri.type === 'master' ? 'master: holds TLS and server settings, paths come from minions' : 'no paths'}</div>`}
            ${ri.ignoredAnnotations?.length ? `<div class="rm-ignored">not applied by this controller: ${ri.ignoredAnnotations.map((a) => `<span class="pill warn">${esc(a)}</span>`).join(' ')}</div>` : ''}
            ${ri.reason ? `<div class="rm-reason">${esc(ri.reason)}</div>` : ''}
          </div>`;
        }).join('')}
        </div>
      </div>`).join('');
  } };
}

const objectQuery = (kind, t) => new URLSearchParams({ cluster: t.cluster, kind, namespace: t.namespace || '', name: t.name });

// summaryTab leads every drawer: a per-kind summary built from the live object (see summary.js).
function summaryTab(kindId, target, { row, detail, fallback }) {
  const tab = { label: 'Summary' };
  tab.load = async () => {
    const [d, events] = await Promise.all([
      api('/api/object?' + objectQuery(kindId, target)).catch((e) => ({ error: e })),
      detail ? Promise.resolve(detail.events) : api('/api/resource?' + objectQuery(kindId, target)).then((x) => x.events, () => []),
    ]);
    if (d.error) {
      const msg = d.error.message.replace(/^\d+ /, '');
      const gone = /not found/i.test(msg), denied = /forbidden/i.test(msg);
      const note = `<p class="muted">${gone ? '🔍 This object no longer exists.' : denied ? "🔒 You don't have access to read this object." : esc(msg)}</p>`;
      return fallback ? note + fallback : `<div class="empty">${note}</div>`;
    }
    const res = renderSummary(kindId, d, { row, events, open: openRelated, fallback });
    tab.bind = res.bind;
    return res.html;
  };
  return tab;
}

const singular = (l) => l.replace(/ies$/, 'y').replace(/(sses|ches|shes|xes)$/, (m) => m.slice(0, -2)).replace(/s$/, '');

const LOG_KINDS = new Set(['pods', 'deployments', 'statefulsets', 'daemonsets', 'jobs']);

// openRelated opens the drawer for an object linked from a summary (a pod, node, volume...).
function openRelated(kind, t) {
  resourceDrawer(kind, { cluster: t.cluster, namespace: t.namespace || '', name: t.name, health: '', status: '', cols: [] }, [])
    .catch((e) => toast(e.message.replace(/^\d+ /, '').slice(0, 140)));
}

// objectDrawer opens summary + describe + details + console for something that points at a Kubernetes
// object; ingresses lead with the routing map.
function objectDrawer({ eyebrow, title, details, cluster, namespace, kind, name, commands }) {
  const kindId = KIND_IDS[kind];
  const tabs = [];
  if (kind === 'Ingress' && name) tabs.push(routingTab(cluster, namespace, name));
  if (kindId && name) {
    const target = { cluster, namespace, name };
    tabs.push(summaryTab(kindId, target, { fallback: details }));
    if (LOG_KINDS.has(kindId)) tabs.push(logsTab(kindId, target));
    tabs.push(describeTab(kindId, target));
  }
  tabs.push({ label: 'Details', html: details });
  tabs.push(consoleTab(commands, cluster, namespace));
  openDrawer({ eyebrow, title, tabs });
}

async function resourceDrawer(kind, r, columns) {
  const d = await api('/api/resource?' + objectQuery(kind, r));
  const kv = [...(r.status ? [['Status', healthPill(r.health, r.status)]] : []), ['Cluster', esc(r.cluster)], ...(r.namespace ? [['Namespace', esc(r.namespace)]] : []),
    ...columns.map((c, i) => [esc(c), esc(r.cols[i] || '–')]), ...(r.created ? [['Created', `${new Date(r.created).toLocaleString()} (${ago(r.created)} ago)`]] : [])];
  const generic = `<dl class="kv">${kv.map(([k, v]) => `<dt>${k}</dt><dd>${v}</dd>`).join('')}</dl>
    <div class="section-title" style="margin-top:6px">Recent warnings</div>
    ${d.events.length ? `<div class="card alerts">${d.events.map((e) => `<div class="alert warning"><div class="stripe"></div><div><div class="meta"><b>${esc(e.status)}</b><span>${ago(e.created)} ago</span><span>×${esc(e.cols[2])}</span></div><div class="detail">${esc(e.cols[1])}</div></div><div></div></div>`).join('')}</div>`
      : '<p class="muted">No warning events for this object.</p>'}`;
  const tabs = [summaryTab(kind, r, { row: r.status ? r : null, detail: d, fallback: generic })];
  if (LOG_KINDS.has(kind)) tabs.push(logsTab(kind, r));
  if (kind !== 'helm') tabs.push(describeTab(kind, r));
  tabs.push(
    { label: kind === 'helm' ? 'Release' : 'YAML', html: `<div style="display:flex;justify-content:flex-end;margin-bottom:8px"><button class="btn sm" data-copy="${esc(d.yaml)}">Copy</button></div><pre class="code yaml">${esc(d.yaml)}</pre>` },
    consoleTab(d.commands, r.cluster, r.namespace),
  );
  openDrawer({
    eyebrow: `<span class="pill accent">${esc(singular(kindLabel(kind)))}</span>${esc(r.cluster)}${r.namespace ? ' · ' + esc(r.namespace) : ''}`,
    title: r.name,
    tabs,
  });
}

function eventDrawer(r) {
  openDrawer({
    eyebrow: `<span class="pill warn">Warning event</span>${esc(r.cluster)} · ${esc(r.namespace)}`,
    title: r.status + ' · ' + r.cols[0],
    tabs: [{ label: 'Summary', html: `<dl class="kv"><dt>Object</dt><dd>${esc(r.cols[0])}</dd><dt>Count</dt><dd>${esc(r.cols[2])}</dd><dt>Last seen</dt><dd>${esc(r.cols[3])} ago</dd></dl><pre class="code">${esc(r.cols[1])}</pre>` },
      // involvedObject isn't validated by the API server: only suggest a command for plain names.
      consoleTab(/^[a-z][a-z0-9]{0,62}\/[a-z0-9]([-a-z0-9.]{0,251}[a-z0-9])?$/.test(r.cols[0])
        ? [`kubectl --context ${shQuote(r.cluster)} -n ${shQuote(r.namespace)} describe ${r.cols[0]}`] : [], r.cluster, r.namespace)],
  });
}

// ---------- alerts ----------
function alertRow(a) {
  return `<div class="alert ${a.severity} ${a.resolved ? 'resolved' : ''}" data-id="${a.id}">
    <div class="stripe"></div>
    <div>
      <div class="meta"><span class="pill ${SEV_PILL[a.severity]}">${a.severity}</span><b style="color:var(--text)">${esc(a.cluster)}</b>${a.namespace ? `<span>${esc(a.namespace)}</span>` : ''}
        <span>${a.resolved ? 'resolved ' + ago(a.resolvedAt) + ' ago' : 'seen ' + ago(a.lastSeen) + ' ago'}</span>${a.count > 1 ? `<span>×${a.count}</span>` : ''}${a.ackedAt && !a.resolved ? '<span class="pill">snoozed</span>' : ''}</div>
      <div class="title">${esc(a.title)}</div>
      ${a.detail ? `<div class="detail">${esc(a.detail)}</div>` : ''}
    </div>
    <div class="actions">
      <button class="btn sm primary" data-act="open">Open</button>
      ${!a.resolved ? `<button class="btn sm" data-act="ack">Snooze</button>` : ''}
      <button class="btn sm" data-act="mute" title="Never show this exact alert again">Mute</button>
    </div></div>`;
}

function alertDrawer(a) {
  const details = `<dl class="kv">
      <dt>Severity</dt><dd><span class="pill ${SEV_PILL[a.severity]}">${a.severity}</span></dd>
      <dt>Cluster</dt><dd>${esc(a.cluster)}</dd>${a.namespace ? `<dt>Namespace</dt><dd>${esc(a.namespace)}</dd>` : ''}
      ${a.kind ? `<dt>Object</dt><dd>${esc(a.kind)}${a.name ? ' / ' + esc(a.name) : ''}</dd>` : ''}
      <dt>First seen</dt><dd>${new Date(a.firstSeen).toLocaleString()} (${ago(a.firstSeen)} ago)</dd>
      <dt>Last seen</dt><dd>${ago(a.lastSeen)} ago${a.count > 1 ? ` · ×${a.count}` : ''}</dd>
      <dt>State</dt><dd>${a.resolved ? 'resolved ' + ago(a.resolvedAt) + ' ago' : a.ackedAt ? 'snoozed' : 'open'}</dd>
    </dl>
    ${a.detail ? `<pre class="code">${esc(a.detail)}</pre>` : ''}
    ${a.source === 'azure' && !a.resolved ? '<p><button class="btn primary" data-azure-login>Sign in to Azure</button></p>' : ''}
    <p class="muted mono" style="margin-top:14px">key: ${esc(a.key)}</p>`;
  objectDrawer({
    eyebrow: `<span class="pill ${SEV_PILL[a.severity]}">${a.severity}</span>${esc(a.cluster)}${a.namespace ? ' · ' + esc(a.namespace) : ''}`,
    title: a.title, details, cluster: a.cluster, namespace: a.namespace, kind: a.kind, name: a.name,
    commands: (a.hint || '').split('\n'),
  });
}

function bindAlertRows(root, alerts, after = render) {
  root.querySelectorAll('.alert[data-id]').forEach((el) => {
    const a = alerts.find((x) => x.id === el.dataset.id);
    el.style.cursor = 'pointer';
    el.onclick = () => alertDrawer(a);
    el.querySelectorAll('[data-act]').forEach((b) => b.onclick = async (e) => {
      e.stopPropagation();
      const act = b.dataset.act;
      if (act === 'open') return alertDrawer(a);
      await post(`/api/alerts/${a.id}/${act}`);
      toast(act === 'ack' ? 'Snoozed' : 'Muted — undo in Settings');
      after();
    });
  });
}

async function alertsView(scope) {
  const p = route().params;
  const f = { sev: p.get('sev') || '', q: p.get('q') || '' };
  const ov = clustersCache || await loadClusters();
  const all = await api('/api/alerts?' + new URLSearchParams({ scope, limit: '3000', cluster: scopeParam('clusters') }));
  if (current !== (scope === 'all' ? 'history' : 'alerts')) return;
  const needle = f.q.toLowerCase();
  const namespaces = [...new Set(all.map((a) => a.namespace).filter(Boolean))].sort();
  const items = all.filter((a) => inScope('namespaces', a.namespace) && (!f.sev || RANK[a.severity] >= RANK[f.sev]) &&
    (!needle || [a.cluster, a.namespace, a.title, a.detail, a.category].join(' ').toLowerCase().includes(needle)));
  const name = scope === 'all' ? 'history' : 'alerts';
  view.innerHTML = `
    ${head(scope === 'all' ? 'History' : 'Alerts', scope === 'all' ? 'Everything k8s-buddy has seen, including resolved and snoozed.' : 'Open problems, including snoozed ones.',
      scope === 'all' ? '' : `<button class="btn" id="ackAll">Snooze all</button>`)}
    <div class="toolbar">
      ${PICKERS}
      <div class="seg" id="aSev">${[['', 'All'], ['notice', 'Notice+'], ['warning', 'Warning+'], ['critical', 'Critical']].map(([v, l]) => `<button data-v="${v}" class="${f.sev === v ? 'on' : ''}">${l}</button>`).join('')}</div>
      <input class="input search" id="aQ" placeholder="Search alerts…" value="${esc(f.q)}">
      <span class="spacer" style="flex:1"></span><span class="muted">${plural(items.length, 'alert')}</span>
    </div>
    <div class="card alerts">${items.length ? items.map(alertRow).join('') : `<div class="empty"><span class="big">${scope === 'all' ? '🗂️' : '😌'}</span>${scope === 'all' ? 'No history yet.' : 'Nothing needs your attention.'}</div>`}</div>`;
  const upd = (patch) => go(name, { ...f, ...patch });
  mountPickers(view, ov.clusters.filter((c) => c.status !== 'excluded').map((c) => c.name), namespaces);
  view.querySelectorAll('#aSev button').forEach((b) => b.onclick = () => upd({ sev: b.dataset.v }));
  const q = view.querySelector('#aQ');
  q.oninput = debounce(() => upd({ q: q.value }), 300);
  if (f.q) { q.focus(); q.setSelectionRange(f.q.length, f.q.length); }
  view.querySelector('#ackAll')?.addEventListener('click', async () => { await post('/api/alerts/ack-all?' + new URLSearchParams({ cluster: scopeParam('clusters') })); toast('Snoozed'); render(); });
  bindAlertRows(view, items);
}

// ---------- doctor ----------
let doctorPoll;
// Finding groups start collapsed; remember the ones the user opened across re-renders and re-runs.
const doctorOpen = new Set();
async function doctor() {
  const rep = await api('/api/doctor');
  if (current !== 'doctor') return;
  const findings = rep.findings.filter((x) => inScope('clusters', x.cluster) && inScope('namespaces', x.namespace));
  const byCheck = Object.fromEntries(rep.checks.map((c) => [c.id, []]));
  findings.forEach((x) => (byCheck[x.check] ||= []).push(x));
  const ran = !!rep.finishedAt;
  const clusters = [...new Set(rep.findings.map((x) => x.cluster))].sort();
  document.getElementById('navDoctor').textContent = ran ? String(rep.findings.length) : '';
  view.innerHTML = `
    ${head('Doctor', 'An on-demand checkup for things that are quietly wrong.')}
    <div class="card hero">
      <div><h2>${rep.running ? 'Examining your clusters…' : ran ? `${plural(rep.findings.length, 'finding')} across ${plural(rep.clusters.length, 'cluster')}` : 'Give your clusters a checkup'}</h2>
        <p>${rep.running ? 'Looking for dangling services, broken ingresses, blocking PDBs, orphaned volumes and more.'
          : ran ? `Last run ${ago(rep.finishedAt)} ago, took ${Math.max(1, Math.round((new Date(rep.finishedAt) - new Date(rep.startedAt)) / 1000))}s.`
          : 'Looks for dangling services and ingresses, PDBs that will block upgrades, orphaned disks, unused claims and leftover pods. Read-only.'}</p></div>
      <div class="spacer"></div>
      <button class="btn primary" id="runDoctor" ${rep.running ? 'disabled' : ''}>${rep.running ? '<span class="spinner"></span>Running…' : ran ? 'Run again' : 'Run checkup'}</button>
    </div>
    ${ran && clusters.length ? `<div class="toolbar" style="margin-top:16px">${PICKERS}${findings.length !== rep.findings.length ? `<span class="muted">${findings.length} of ${rep.findings.length} findings in scope</span>` : ''}</div>` : ''}
    ${Object.keys(rep.errors || {}).length ? `<p class="muted" style="margin-top:12px">Some checks were skipped: ${Object.entries(rep.errors).map(([c, e]) => `<b>${esc(c)}</b> (${esc(e)})`).join(', ')}</p>` : ''}
    ${ran ? rep.checks.map((c) => {
      const list = byCheck[c.id] || [];
      const worst = list.reduce((m, x) => Math.max(m, RANK[x.severity]), -1);
      return `<details class="card check" data-check="${esc(c.id)}" ${list.length && doctorOpen.has(c.id) ? 'open' : ''}>
        <summary><span class="pill ${!list.length ? 'ok' : worst >= 2 ? 'warn' : worst === 1 ? 'notice' : ''}">${list.length || '✓'}</span>
          <div><div class="t">${esc(c.title)}</div><div class="d">${esc(c.description)}</div></div><span class="chev">›</span></summary>
        ${list.map((x) => `<div class="finding"><div class="what">${esc(x.name)} <span class="muted" style="font-weight:400">· ${esc(x.cluster)}${x.namespace ? ' / ' + esc(x.namespace) : ''}</span></div>
          <div class="msg">${esc(x.message)}</div>
          <div class="act"><button class="btn sm primary" data-finding="${rep.findings.indexOf(x)}">Open</button></div></div>`).join('')}
      </details>`;
    }).join('') : ''}`;
  view.querySelectorAll('details.check').forEach((d) => d.ontoggle = () => { d.open ? doctorOpen.add(d.dataset.check) : doctorOpen.delete(d.dataset.check); });
  view.querySelector('#runDoctor').onclick = async () => { await post('/api/doctor'); doctor(); };
  mountPickers(view, clusters, [...new Set(rep.findings.map((x) => x.namespace).filter(Boolean))].sort());
  view.querySelectorAll('[data-copy]').forEach((b) => b.onclick = () => copyCmd(b.dataset.copy));
  view.querySelectorAll('[data-finding]').forEach((b) => b.onclick = () => {
    const x = rep.findings[+b.dataset.finding];
    const check = rep.checks.find((c) => c.id === x.check);
    objectDrawer({
      eyebrow: `<span class="pill ${SEV_PILL[x.severity]}">${esc(check?.title || x.check)}</span>${esc(x.cluster)}${x.namespace ? ' · ' + esc(x.namespace) : ''}`,
      title: x.name, cluster: x.cluster, namespace: x.namespace, kind: x.kind, name: x.check === 'leftover-pods' ? '' : x.name,
      details: `<p>${esc(x.message)}</p>${check ? `<p class="muted">${esc(check.description)}</p>` : ''}`,
      commands: [x.hint || ''],
    });
  });
  clearTimeout(doctorPoll);
  if (rep.running) doctorPoll = setTimeout(() => current === 'doctor' && doctor(), 1500);
}

// ---------- settings ----------
async function settings() {
  const [s, ov] = await Promise.all([api('/api/settings'), loadClusters(), loadReleases()]);
  if (current !== 'settings') return;
  view.innerHTML = `
    ${head('Settings', 'Saved to your user config directory.')}
    <div class="card form">
      <div class="row"><div class="lbl"><b>Clusters to watch</b><span>Uncheck contexts you don't care about. Contexts you delete from your kubeconfig disappear here automatically.</span></div>
        <div>
          <div class="toolbar"><button class="btn sm" id="selNone">Uncheck all</button><button class="btn sm" id="selAll">Check all</button><button class="btn sm danger" id="selBad">Uncheck unreachable</button></div>
          <div class="ctx-list">${ov.clusters.map((c) => `<label><input type="checkbox" data-ctx="${esc(c.name)}" ${s.excludedContexts.includes(c.name) ? '' : 'checked'}>
            <span class="dot ${c.status === 'connected' ? clusterHealth(c) : c.status}"></span><span class="n">${esc(c.name)}</span>${c.version ? verSpan(c.version, 'ctx-ver') : ''}<span class="muted" style="font-size:12px">${statusLabel[c.status] || c.status}</span></label>`).join('')}</div>
        </div></div>
      <div class="row"><div class="lbl"><b>Kubeconfig</b><span>Empty uses $KUBECONFIG or ~/.kube/config.</span></div>
        <input class="input" id="kubeconfig" style="width:100%" value="${esc(s.kubeconfig)}" placeholder="~/.kube/config"></div>
      <div class="row"><div class="lbl"><b>Nudge me for</b><span>The buddy bounces and the menu bar count goes up at this level and above.</span></div>
        <div class="seg" id="nudge" style="width:max-content">${['info', 'notice', 'warning', 'critical'].map((l) => `<button data-v="${l}" class="${l === s.nudgeLevel ? 'on' : ''}">${l[0].toUpperCase() + l.slice(1)}+</button>`).join('')}</div></div>
      <div class="row"><div class="lbl"><b>Buddy</b><span>The floating character on your screen.</span></div>
        <div><label class="toggle" style="color:var(--text)"><input type="checkbox" id="showCharacter" ${s.showCharacter ? 'checked' : ''}> Show the buddy</label><br>
          <label class="toggle" style="color:var(--text);margin-top:6px"><input type="checkbox" id="showBubble" ${s.showBubble ? 'checked' : ''}> Show a speech bubble when nudging</label></div></div>
      <div class="row"><div class="lbl"><b>Snooze & history</b><span>How long "Snooze" hides an alert, and how many alerts to remember.</span></div>
        <div style="display:flex;gap:16px;flex-wrap:wrap;align-items:center">
          <label class="toggle" style="color:var(--text)">Snooze for <input class="input" type="number" id="snoozeHours" min="1" max="168" value="${s.snoozeHours}" style="width:80px"> hours</label>
          <label class="toggle" style="color:var(--text)">Keep <input class="input" type="number" id="historySize" min="50" max="20000" value="${s.historySize}" style="width:100px"> alerts</label></div></div>
      <div class="row"><div class="lbl"><b>Ignored namespaces</b><span>One per line. Globs work, e.g. <code>preview-*</code>.</span></div>
        <textarea class="input" id="ignoredNamespaces" placeholder="kube-system">${esc(s.ignoredNamespaces.join('\n'))}</textarea></div>
      <div class="row"><div class="lbl"><b>Muted alerts</b><span>Alerts you chose to never see again.</span></div>
        <div id="muted">${s.mutedKeys.length ? s.mutedKeys.map((k) => `<div style="display:flex;gap:8px;align-items:center;margin-bottom:6px"><code style="flex:1;word-break:break-all">${esc(k)}</code><button class="btn sm" data-unmute="${esc(k)}">Unmute</button></div>`).join('') : '<span class="muted">None</span>'}</div></div>
    </div>
    <div class="save-bar"><span class="muted" id="dirty"></span><button class="btn primary" id="save">Save changes</button></div>`;
  const boxes = () => [...view.querySelectorAll('[data-ctx]')];
  const byName = Object.fromEntries(ov.clusters.map((c) => [c.name, c]));
  const dirty = () => (view.querySelector('#dirty').textContent = 'Unsaved changes');
  view.querySelector('.form').addEventListener('input', dirty);
  view.querySelector('#selAll').onclick = () => { boxes().forEach((b) => (b.checked = true)); dirty(); };
  view.querySelector('#selNone').onclick = () => { boxes().forEach((b) => (b.checked = false)); dirty(); };
  view.querySelector('#selBad').onclick = () => { boxes().forEach((b) => { if (byName[b.dataset.ctx]?.status === 'error') b.checked = false; }); dirty(); };
  let nudgeLevel = s.nudgeLevel;
  view.querySelectorAll('#nudge button').forEach((b) => b.onclick = () => { nudgeLevel = b.dataset.v; view.querySelectorAll('#nudge button').forEach((x) => x.classList.toggle('on', x === b)); dirty(); });
  let muted = [...s.mutedKeys];
  view.querySelectorAll('[data-unmute]').forEach((b) => b.onclick = () => { muted = muted.filter((k) => k !== b.dataset.unmute); b.parentElement.remove(); dirty(); });
  view.querySelector('#save').onclick = async () => {
    const next = {
      ...s, nudgeLevel, mutedKeys: muted,
      excludedContexts: boxes().filter((b) => !b.checked).map((b) => b.dataset.ctx),
      kubeconfig: view.querySelector('#kubeconfig').value.trim(),
      showBubble: view.querySelector('#showBubble').checked,
      showCharacter: view.querySelector('#showCharacter').checked,
      snoozeHours: +view.querySelector('#snoozeHours').value,
      historySize: +view.querySelector('#historySize').value,
      ignoredNamespaces: view.querySelector('#ignoredNamespaces').value.split('\n').map((x) => x.trim()).filter(Boolean),
    };
    await api('/api/settings', { method: 'PUT', body: JSON.stringify(next) });
    toast('Settings saved');
    view.querySelector('#dirty').textContent = '';
    setTimeout(loadClusters, 1500);
  };
}

// ---------- live updates ----------
// ---------- Azure sign-in ----------
const main = document.querySelector('main');
const azureBanner = document.getElementById('azureBanner');
let azurePoll;

async function refreshAzure() {
  const az = await api('/api/azure').catch(() => null);
  if (!az) return;
  clearTimeout(azurePoll);
  if (az.login.running) azurePoll = setTimeout(refreshAzure, 2000);
  azureBanner.hidden = !az.needed && !az.login.running && !az.login.error;
  if (azureBanner.hidden) return;
  if (!az.needed && !az.login.running) {
    azureBanner.innerHTML = `<span class="banner-ico">⚠️</span><div><b>Azure sign-in didn't finish</b><small>${esc(az.login.error)}</small></div>
      <button class="btn primary" data-azure-login>Try again</button>`;
  } else {
    azureBanner.innerHTML = `<span class="banner-ico">🔑</span>
      <div><b>Azure sign-in expired · ${plural(az.clusters.length, 'cluster')} waiting</b>
        <small>${az.login.running ? 'Finish signing in in the browser window that just opened. Clusters reconnect as soon as you are done.'
          : esc(az.reason || 'kubelogin uses your Azure CLI session, which needs a fresh sign-in.')}
          ${az.login.error ? `<br><span style="color:var(--bad)">Last attempt: ${esc(az.login.error)}</span>` : ''}</small></div>
      <button class="btn primary" data-azure-login ${az.login.running ? 'disabled' : ''}>${az.login.running ? '<span class="spinner"></span>Waiting for browser…' : 'Sign in to Azure'}</button>`;
  }
  azureBanner.querySelector('[data-azure-login]')?.addEventListener('click', startAzureLogin);
}

async function startAzureLogin() {
  await post('/api/azure/login');
  toast('Opening Azure sign-in in your browser…');
  refreshAzure();
}

async function refreshBadges() {
  const sum = await api('/api/summary');
  document.getElementById('navAlerts').textContent = sum.pending ? String(sum.pending) : '';
  document.getElementById('brandSub').textContent = sum.pending ? `${plural(sum.pending, 'thing')} to look at` : 'all quiet';
}
const onChange = debounce(() => {
  refreshBadges();
  refreshAzure();
  if ((current === 'alerts' || current === 'history') && drawer.hidden) render();
}, 600);
stream({ summary: onChange });
setInterval(() => {
  if (document.hidden || !drawer.hidden) return;
  if (current === 'overview') overview();
  else if (current === 'resources') resources.refresh?.();
}, 8000);

refreshBadges();
refreshAzure();
mountForwardsDock();
render();

// Dev mode (K8S_BUDDY_WEB_DIR set): reload the page when a web file changes. Release builds answer 404
// here, so this stops after one request.
(async () => {
  let v;
  try { v = await api('/api/dev/version'); } catch { return; }
  setInterval(async () => { try { if ((await api('/api/dev/version')) !== v) location.reload(); } catch { /* app restarting */ } }, 1000);
})();
