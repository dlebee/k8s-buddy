// Spikes: workloads and nodes whose CPU or memory went above a share of what they're allowed (a
// pod's limit, a node's allocatable). The shares are the viewer's to pick.
// Only for looking: it never changes a recommendation.
import { api, esc, ago } from '/static/common.js';
import { inScope } from '/static/scope.js';
import { mountHistory } from '/static/charts.js';

const fmtCPU = (m) => (m < 0 ? '–' : m >= 1000 ? `${+(m / 1000).toFixed(2)} cores` : `${Math.round(m)}m`);
const fmtMem = (b) => { if (b < 0) return '–'; const u = ['B', 'Ki', 'Mi', 'Gi', 'Ti']; let i = 0; while (b >= 1024 && i < u.length - 1) { b /= 1024; i++; } return `${b >= 100 || i === 0 ? Math.round(b) : b.toFixed(1)}${u[i]}`; };
const fmt = (x, v) => (x.resource === 'cpu' ? fmtCPU(v) : fmtMem(v));
const load = (k, d) => { try { return JSON.parse(localStorage.getItem(k)) ?? d; } catch { return d; } };
const store = (k, v) => { try { localStorage.setItem(k, JSON.stringify(v)); } catch { /* ignore */ } };
const when = (s) => new Date(s * 1000).toLocaleString([], { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' });
const mins = (m) => (m < 60 ? `${m} min` : `${+(m / 60).toFixed(m >= 600 ? 0 : 1)} h`);

export async function spikesView(view, { head, current, monitored, mountPickers, scopeChip, openWorkload }) {
  let daysWin = load('buddy.spikes.days', 7);
  let res = load('buddy.spikes.res', 'all');
  let what = load('buddy.spikes.what', 'all');
  let system = load('buddy.quotas.system', false);
  const DEF = { nodeCpu: 75, nodeMem: 75, podCpu: 75, podMem: 75 };
  let limits = { ...DEF, ...load('buddy.spikes.limits', {}) };
  let q = '';
  let open = null;
  const title = (extra = '') => head('Usage spikes', 'Where CPU or memory went above a share of what it\'s allowed: a pod\'s limit, a node\'s allocatable. Pick the shares below. For looking: Quotas recommendations don\'t use it.', extra);
  view.innerHTML = `${title()}<div class="card empty">Reading usage history…</div>`;
  const clusters = monitored.filter((n) => inScope('clusters', n));

  const fetchAndDraw = async () => {
    const r = await api('/api/spikes?' + new URLSearchParams({ cluster: clusters.join(','), days: daysWin, ...limits }));
    if (current() !== 'spikes') return;
    const shell = (body) => `${title('<span data-pick="clusters"></span><span data-pick="namespaces"></span>')}${scopeChip(monitored.length)}${body}`;
    if (!r.available) {
      view.innerHTML = shell('<div class="card empty"><span class="big">📈</span>Usage history is off, so there are no spikes to show.<br><a class="btn sm" style="margin-top:10px" href="#settings">Open Settings</a></div>');
      mountPickers(view, monitored, []);
          return;
    }
    const nsNames = [...new Set(r.rows.map((x) => x.namespace).filter(Boolean))].sort();
    const draw = () => {
      const base = r.rows.filter((x) => (x.kind === 'Node' || inScope('namespaces', x.namespace)) && (system || !x.system));
      const shown = base.filter((x) => (res === 'all' || x.resource === res)
        && (what === 'all' || (what === 'nodes') === (x.kind === 'Node'))
        && (!q || `${x.cluster} ${x.namespace} ${x.kind} ${x.name}`.toLowerCase().includes(q)));
      const count = (f) => base.filter(f).length;
      const pctBox = (k) => `<input class="input s-pct" type="number" min="1" max="100" step="5" data-lim="${k}" value="${r.limits[k] ?? limits[k]}" aria-label="${k}">%`;
      const seg = (id, cur, opts) => `<div class="seg" data-seg="${id}">${opts.map(([k, l, n]) => `<button class="${cur === k ? 'on' : ''}" data-v="${k}">${l}${n === undefined ? '' : ` <small>${n}</small>`}</button>`).join('')}</div>`;
      view.innerHTML = shell(`
        <div class="q-tools">
          ${seg('days', daysWin, [[1, 'Last 24 h'], [7, 'Last 7 days'], [r.retentionDays, `All (${r.retentionDays} d)`]].filter(([d], i) => i === 0 || d <= r.retentionDays && (i < 2 || d > 7)))}
          ${seg('res', res, [['all', 'All', base.length], ['cpu', 'CPU', count((x) => x.resource === 'cpu')], ['memory', 'Memory', count((x) => x.resource === 'memory')]])}
          ${seg('what', what, [['all', 'All'], ['workloads', 'Workloads', count((x) => x.kind !== 'Node')], ['nodes', 'Nodes', count((x) => x.kind === 'Node')]])}
          <label class="toggle"><input type="checkbox" id="sSys" ${system ? 'checked' : ''}> Include system namespaces</label>
          <span class="spacer"></span>
          <input class="input" id="sFind" placeholder="Filter…" value="${esc(q)}" spellcheck="false">
        </div>
        <div class="s-limits">Counts as a spike above
          <span><b>Nodes</b> CPU ${pctBox('nodeCpu')} memory ${pctBox('nodeMem')} of allocatable</span>
          <span><b>Pods</b> CPU ${pctBox('podCpu')} memory ${pctBox('podMem')} of their limit</span>
          ${Object.keys(DEF).some((k) => limits[k] !== DEF[k]) ? '<button class="btn sm" id="sReset">Defaults (75%)</button>' : ''}
          ${r.noLimitCpu || r.noLimitMem ? `<span class="muted small">Pods without a limit can't be judged: ${r.noLimitCpu} without a CPU limit, ${r.noLimitMem} without a memory limit.</span>` : ''}
        </div>
        ${shown.length ? `<div class="card q-table"><table><thead><tr><th>Workload or node</th><th>Resource</th><th>Usual → peak</th><th>Peak vs limit</th><th>Spikes</th><th>Last spike</th></tr></thead><tbody>
          ${shown.map((x, i) => { const id = `${x.cluster}/${x.kind}/${x.namespace}/${x.name}/${x.resource}`; const pct = x.limit > 0 ? Math.round(100 * x.peak / x.limit) : -1; return `
            <tr data-row="${i}" class="${open === id ? 'on' : ''}">
              <td><b>${esc(x.name)}</b><div class="muted small">${esc(x.kind)}${x.namespace ? ` · ${esc(x.namespace)}` : ''} · ${esc(x.cluster)}</div></td>
              <td>${x.resource === 'cpu' ? 'CPU' : 'Memory'}</td>
              <td>${fmt(x, x.usual)} → <b>${fmt(x, x.peak)}</b><div class="muted small">${x.usual > 0 ? `${+(x.peak / x.usual).toFixed(x.peak / x.usual >= 10 ? 0 : 1)}× usual` : 'from idle'}</div></td>
              <td>${pct < 0 ? '<span class="muted">no limit</span>' : `<span class="${pct >= 100 ? 'q-tag q-bad' : 'q-tag q-notice'}">${pct}%</span>`}<div class="muted small">${x.limit > 0 ? `of ${fmt(x, x.limit)}${x.kind === 'Node' ? ' allocatable' : ' limit'}` : ''}</div></td>
              <td>${x.spikes}<div class="muted small">${mins(x.minutes)} in all${x.pods > 1 ? ` · ${x.pods} pods` : ''}</div></td>
              <td>${esc(ago(new Date(x.lastAt * 1000)))} ago<div class="muted small">${when(x.lastAt)}</div></td>
            </tr>${open === id ? `<tr class="q-open"><td colspan="6"><div class="q-detail">
              <p>${x.peakPod ? `Chart of <b>${esc(x.peakPod)}</b>, the pod that went highest. ` : ''}Peak <b>${fmt(x, x.peak)}</b> on ${when(x.peakAt)}, against a usual ${fmt(x, x.usual)}. ${x.spikes} time${x.spikes === 1 ? '' : 's'} above ${x.kind === 'Node' ? limits[x.resource === 'cpu' ? 'nodeCpu' : 'nodeMem'] : limits[x.resource === 'cpu' ? 'podCpu' : 'podMem']}% of its ${x.kind === 'Node' ? 'allocatable' : 'limit'} in the last ${daysWin === 1 ? '24 hours' : `${daysWin} days`}, ${mins(x.minutes)} in all.</p>
              <div class="q-chart"></div>
              ${x.kind !== 'Node' && x.kind !== 'Pod' ? `<button class="btn sm" data-open>Open ${esc(x.kind)} ${esc(x.name)}</button>` : ''}
            </div></td></tr>` : ''}`; }).join('')}
        </tbody></table></div>` : `<div class="card empty">${base.length ? 'Nothing matches this filter.' : `Nothing went above these shares in the last ${daysWin === 1 ? '24 hours' : `${daysWin} days`} in this selection.`}</div>`}`);
      mountPickers(view, monitored, nsNames);
          view.querySelectorAll('[data-seg]').forEach((g) => g.querySelectorAll('[data-v]').forEach((b) => b.onclick = () => {
        const v = b.dataset.v;
        if (g.dataset.seg === 'days') { daysWin = +v; store('buddy.spikes.days', daysWin); open = null; fetchAndDraw(); return; }
        if (g.dataset.seg === 'res') { res = v; store('buddy.spikes.res', res); }
        if (g.dataset.seg === 'what') { what = v; store('buddy.spikes.what', what); }
        draw();
      }));
      view.querySelectorAll('[data-lim]').forEach((i) => i.onchange = () => {
        const v = Math.min(100, Math.max(1, Math.round(+i.value || DEF[i.dataset.lim])));
        limits = { ...limits, [i.dataset.lim]: v };
        store('buddy.spikes.limits', limits);
        open = null;
        fetchAndDraw();
      });
      const reset = view.querySelector('#sReset');
      if (reset) reset.onclick = () => { limits = { ...DEF }; store('buddy.spikes.limits', limits); open = null; fetchAndDraw(); };
      view.querySelector('#sSys').onchange = (e) => { system = e.target.checked; store('buddy.quotas.system', system); draw(); };
      const find = view.querySelector('#sFind');
      find.oninput = () => { q = find.value.toLowerCase().trim(); draw(); const f = view.querySelector('#sFind'); f.focus(); f.setSelectionRange(f.value.length, f.value.length); };
      view.querySelectorAll('[data-row]').forEach((tr) => tr.onclick = () => {
        const x = shown[+tr.dataset.row];
        const id = `${x.cluster}/${x.kind}/${x.namespace}/${x.name}/${x.resource}`;
        open = open === id ? null : id;
        draw();
      });
      const openRow = view.querySelector('.q-open');
      if (openRow) {
        const x = shown[+openRow.previousElementSibling.dataset.row];
        const chart = openRow.querySelector('.q-chart');
        chart.dataset.hours = String(daysWin * 24);
        // A workload's spike is one pod against its own limit: chart that pod, so the line matches.
        const node = x.kind === 'Node';
        const pct = node ? { cpu: limits.nodeCpu, mem: limits.nodeMem } : { cpu: limits.podCpu, mem: limits.podMem };
        const now = Date.now() / 1000;
        mountHistory(chart, { cluster: x.cluster, kind: x.peakPod ? 'pod' : x.historyKind, namespace: x.namespace || '', name: x.peakPod || x.historyName,
          allocatable: node, overPct: pct, focus: { from: x.peakAt - 3 * 3600, to: Math.min(now, x.peakAt + 3 * 3600) } });
        const b = openRow.querySelector('[data-open]');
        if (b) b.onclick = () => openWorkload({ kind: x.kind, cluster: x.cluster, namespace: x.namespace, workload: x.name });
      }
    };
    draw();
  };
  await fetchAndDraw();
}
