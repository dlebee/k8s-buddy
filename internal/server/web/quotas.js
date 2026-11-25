// Resource Quotas Optimizer: requests and limits suggested per container from recorded usage.
// Requests follow the median (typical) use, limits the highest reading, each with headroom.
import { api, esc } from '/static/common.js';
import { inScope, scope } from '/static/scope.js';
import { mountHistory } from '/static/charts.js';

const GOOD_DAYS = 7;
const fmtCPU = (m) => (m < 0 ? '–' : m >= 1000 ? `${+(m / 1000).toFixed(2)}` : `${Math.round(m)}m`);
const fmtMem = (b) => { if (b < 0) return '–'; const u = ['B', 'Ki', 'Mi', 'Gi', 'Ti']; let i = 0; while (b >= 1024 && i < u.length - 1) { b /= 1024; i++; } return `${b >= 100 || i === 0 ? Math.round(b) : b.toFixed(1)}${u[i]}`; };
// days: an amount of recorded time, given in days: "45 min" under an hour, "3.5 h" under a day, then days.
const days = (d) => {
  const m = Math.round(d * 1440);
  if (m < 60) return `${m} min`;
  if (d < 1) return `${+(d * 24).toFixed(d * 24 >= 10 ? 0 : 1)} h`;
  return `${d.toFixed(d >= 10 ? 0 : 1)} d`;
};
const load = (k, d) => { try { return JSON.parse(localStorage.getItem(k)) ?? d; } catch { return d; } };
const store = (k, v) => { try { localStorage.setItem(k, JSON.stringify(v)); } catch { /* ignore */ } };

const VERDICT = {
  risk: ['At risk', 'bad', 'OOM-killed often (more than once per replica in two weeks, or twice in a day), or CPU held at its limit (throttled)'],
  missing: ['Missing', 'warn', 'No requests or limits set'],
  under: ['Under-requested', 'warn', 'Typical memory use is above the request (allowed while under the limit): first to be evicted if the node runs short'],
  over: ['Over-provisioned', 'notice', 'Requests well above what it uses'],
  ok: ['Fits', 'ok', 'Within range of its use'],
};

// pair shows current → suggested, highlighting a change.
// exact: a suggested value as you'd write it in a spec (7Gi, 6400Mi, 250m, 1.75).
const exactMem = (b) => (b >= 1 << 30 && b % (1 << 30) === 0 ? `${b / (1 << 30)}Gi` : `${Math.round(b / (1 << 20))}Mi`);
function pair(cur, rec, fmt) {
  const changed = cur < 0 || Math.abs(cur - rec) / Math.max(rec, 1) > 0.25;
  const show = fmt === fmtMem ? exactMem : fmt;
  return `<span class="${cur < 0 ? 'q-none' : ''}">${cur < 0 ? 'none' : fmt(cur)}</span> → <b class="${changed ? 'q-new' : ''}">${show(rec)}</b>`;
}

function resCell(r, fmt) {
  return `<div class="q-res"><span class="q-k">req</span>${pair(r.request.current, r.request.recommended, fmt)}</div>
    <div class="q-res"><span class="q-k">lim</span>${pair(r.limit.current, r.limit.recommended, fmt)}</div>
    <div class="q-use muted">use ${fmt(r.median)} typical · ${fmt(r.max)} max</div>`;
}
const memTags = (x) => (x.ooms ? `<span class="q-tag ${x.oomRare ? 'q-notice' : 'q-bad'}" title="${x.oomRare ? 'Rare: the limit is doing its job. ' : ''}Last ${new Date(x.lastOom * 1000).toLocaleString()}">OOM-killed ×${x.ooms}${x.oomRare ? ' · rare' : ''}</span>`
  : x.fillsLimit ? '<span class="q-tag q-notice" title="Uses most of its memory limit without OOM kills: likely sizes itself to it (JVM, databases, caches)">fills its limit</span>' : '');

function detail(r) {
  return `<div class="q-detail">
    <div class="q-grid">
      <div><h4>Why</h4><ul class="q-find">${r.findings.length ? r.findings.map((f) => `<li class="lv-${f.level}">${esc(f.text)}</li>`).join('') : '<li>Requests and limits are in line with what it uses.</li>'}</ul>
        <p class="muted small">Per replica, from ${days(r.days)} of recorded data${r.spanDays - r.days >= 1 ? ` over ${days(r.spanDays)} (gaps skipped)` : ''}. Requests: median use + 20%. Limits: highest use + 30% CPU / + 25% memory. Rounded up: CPU to 25m steps (quarter cores above 1 core), memory to a multiple of 8Mi (8Mi steps under 1Gi, 128Mi up to 8Gi, 512Mi above).${r.podLevel ? ' This pod has several containers and per-container data is still being collected, so these figures are for the whole pod.' : ''}</p>
        <table class="ptable q-nums"><thead><tr><th></th><th>Typical (median)</th><th>p95</th><th>Max</th></tr></thead><tbody>
          <tr><td>CPU</td><td>${fmtCPU(r.cpu.median)}</td><td>${fmtCPU(r.cpu.p95)}</td><td>${fmtCPU(r.cpu.max)}</td></tr>
          <tr><td>Memory</td><td>${fmtMem(r.mem.median)}</td><td>${fmtMem(r.mem.p95)}</td><td>${fmtMem(r.mem.max)}</td></tr>
        </tbody></table></div>
      <div><h4>Usage over time <span class="muted">whole workload</span></h4><div class="q-chart"></div></div>
    </div>
    <h4>Apply it <span class="muted">(a suggestion to run yourself; nothing runs from here)</span></h4>
    <div class="plan-cmds"><div class="cmd"><pre class="code">${esc(r.command)}</pre><button class="btn sm" data-copy="${esc(r.command.split('\n').filter((l) => !l.startsWith('#')).join('\n') || r.command)}">Copy</button></div></div>
    <button class="btn sm" data-open>Open ${esc(r.kind)} ${esc(r.workload)}</button>
  </div>`;
}

export async function quotasView(view, { head, current, monitored, mountPickers, scopeChip, openWorkload }) {
  let filter = load('buddy.quotas.filter', 'all');
  let system = load('buddy.quotas.system', false);
  let q = '';
  let open = null;
  view.innerHTML = `${head('Resource Quotas Optimizer', 'Suggested requests and limits for every container, from the CPU and memory it actually used.')}<div class="card empty">Reading usage history…</div>`;
  const clusters = monitored.filter((n) => inScope('clusters', n));
  const r = await api('/api/optimizer?' + new URLSearchParams({ cluster: clusters.join(',') }));
  if (current() !== 'quotas') return;

  const shell = (body) => `
    ${head('Resource Quotas Optimizer', 'Suggested requests and limits for every container, from the CPU and memory it actually used: requests from its typical (median) use, limits from its highest use, each with headroom.', '<span data-pick="clusters"></span><span data-pick="namespaces"></span>')}
    ${scopeChip(monitored.length)}${body}`;
  if (!r.available) {
    view.innerHTML = shell(`<div class="card empty"><span class="big">📈</span>Usage history is off, so there's nothing to learn from.<br><span class="muted">Turn on "Usage history" in Settings; suggestions appear once a few days are recorded.</span><br><a class="btn sm" style="margin-top:10px" href="#settings">Open Settings</a></div>`);
    mountPickers(view, monitored, []);
    return;
  }
  const nsNames = [...new Set(r.rows.map((x) => x.namespace))].sort();
  const recorded = Math.max(r.days, (r.recordedMinutes || 0) / 1440);
  const toGo = Math.max(0, (r.minMinutes || 60) - (r.recordedMinutes || 0));
  const banner = !r.rows.length && r.collecting
    ? `<div class="q-banner">⏳ <div><b>${recorded ? `${days(recorded)} of usage recorded so far.` : 'No usage recorded yet.'}</b> Recommendations start once a workload has 1 hour of recorded usage${toGo ? `: about ${toGo} min to go` : ', in the next few minutes'}. They get better with more data: it's best to wait for at least ${GOOD_DAYS} days before changing anything. Only the time k8s-buddy is running counts; gaps are skipped.</div></div>`
    : r.days < GOOD_DAYS
    ? `<div class="q-banner">⏳ <div><b>Only ${days(r.days)} of usage recorded so far.</b> Suggestions get better with more data: it's best to wait until at least ${GOOD_DAYS} days are recorded before changing anything (busy days and weekly jobs need to be seen). Only the time k8s-buddy was running counts; gaps are skipped. History is kept ${r.retentionDays} days (Settings).</div></div>`
    : `<div class="q-banner ok">✓ <div>Based on up to ${days(r.days)} of recorded usage (kept ${r.retentionDays} days). Gaps while k8s-buddy wasn't running are skipped.</div></div>`;

  const draw = () => {
    const base = r.rows.filter((x) => inScope('namespaces', x.namespace) && (system || !x.system));
    const count = (v) => base.filter((x) => x.verdict === v).length;
    const shown = base.filter((x) => (filter === 'all' || x.verdict === filter)
      && (!q || `${x.cluster} ${x.namespace} ${x.workload} ${x.container}`.toLowerCase().includes(q)));
    const over = base.filter((x) => x.verdict === 'over');
    const freeCPU = over.reduce((n, x) => n + Math.max(0, x.freeCpu), 0), freeMem = over.reduce((n, x) => n + Math.max(0, x.freeMem), 0);
    view.innerHTML = shell(`
      ${banner}
      <div class="stats q-stats">
        <div class="card stat"><div class="label">Containers analysed</div><div class="value">${base.length}</div><div class="foot">${r.collecting ? `${r.collecting} workload${r.collecting === 1 ? '' : 's'} still collecting (need 1 h)` : `in ${r.clusters.length} cluster${r.clusters.length === 1 ? '' : 's'}`}</div></div>
        <div class="card stat ${count('risk') ? 'bad' : ''}"><div class="label">At risk</div><div class="value">${count('risk')}</div><div class="foot">OOM-killed often, or CPU at its limit</div></div>
        <div class="card stat ${count('under') ? 'warn' : ''}"><div class="label">Under-requested</div><div class="value">${count('under')}</div><div class="foot">memory use above request, under limit</div></div>
        <div class="card stat ${count('missing') ? 'warn' : ''}"><div class="label">Missing requests/limits</div><div class="value">${count('missing')}</div><div class="foot">suggested from their use</div></div>
        <div class="card stat"><div class="label">Over-provisioned</div><div class="value">${count('over')}</div><div class="foot">could free ${fmtCPU(freeCPU)} CPU · ${fmtMem(freeMem)} requested</div></div>
      </div>
      <div class="q-tools">
        <div class="seg">${[['all', 'All', base.length], ...Object.entries(VERDICT).map(([k, v]) => [k, v[0], count(k)])].map(([k, label, n]) => `<button class="${filter === k ? 'on' : ''}" data-f="${k}">${label} <small>${n}</small></button>`).join('')}</div>
        <label class="toggle"><input type="checkbox" id="qSys" ${system ? 'checked' : ''}> Include system namespaces</label>
        <span class="spacer"></span>
        <input class="input" id="qFind" placeholder="Filter workloads…" value="${esc(q)}" spellcheck="false">
      </div>
      ${shown.length ? `<div class="card q-table"><table><thead><tr><th>Workload</th><th>Verdict</th><th>Container</th><th>Data</th><th>CPU <span class="muted">now → suggested</span></th><th>Memory <span class="muted">now → suggested</span></th></tr></thead><tbody>
        ${shown.map((x, i) => { const v = VERDICT[x.verdict]; const id = `${x.cluster}/${x.namespace}/${x.kind}/${x.workload}/${x.container}`; return `
          <tr data-row="${i}" class="${open === id ? 'on' : ''}">
            <td><b>${esc(x.workload)}</b><div class="muted small">${esc(x.kind)} · ${esc(x.namespace)} · ${esc(x.cluster)}${x.helm ? ' · Helm' : ''}</div></td>
            <td><span class="q-verdict q-${v[1]}" title="${esc(v[2])}">${v[0]}</span></td>
            <td class="mono small">${esc(x.container)}<div class="muted">×${x.replicas} replica${x.replicas === 1 ? '' : 's'}</div></td>
            <td><span class="conf conf-${x.confidence}" title="${x.confidence} confidence">${days(x.days)}</span>${x.days < GOOD_DAYS ? '<div class="muted small">wait for 7 d</div>' : ''}</td>
            <td>${resCell(x.cpu, fmtCPU)}</td>
            <td>${resCell(x.mem, fmtMem)}${memTags(x)}</td>
          </tr>${open === id ? `<tr class="q-open"><td colspan="6">${detail(x)}</td></tr>` : ''}`; }).join('')}
      </tbody></table></div>` : `<div class="card empty">${base.length ? 'Nothing matches this filter.' : r.collecting ? `Still collecting: recommendations start at the 1-hour mark${toGo ? ` (about ${toGo} min to go)` : ''}.` : 'No workloads with recorded usage in this selection.'}</div>`}`);
    mountPickers(view, monitored, nsNames);
    view.querySelectorAll('[data-f]').forEach((b) => b.onclick = () => { filter = b.dataset.f; store('buddy.quotas.filter', filter); draw(); });
    view.querySelector('#qSys').onchange = (e) => { system = e.target.checked; store('buddy.quotas.system', system); draw(); };
    const find = view.querySelector('#qFind');
    find.oninput = () => { q = find.value.toLowerCase().trim(); draw(); const f = view.querySelector('#qFind'); f.focus(); f.setSelectionRange(f.value.length, f.value.length); };
    view.querySelectorAll('[data-row]').forEach((tr) => tr.onclick = () => {
      const x = shown[+tr.dataset.row];
      const id = `${x.cluster}/${x.namespace}/${x.kind}/${x.workload}/${x.container}`;
      open = open === id ? null : id;
      draw();
    });
    const openRow = view.querySelector('.q-open');
    if (openRow) {
      const x = shown[+openRow.previousElementSibling.dataset.row];
      const chart = openRow.querySelector('.q-chart');
      chart.dataset.hours = String(Math.min(24 * r.retentionDays, 24 * 7));
      mountHistory(chart, { cluster: x.cluster, kind: x.historyKind, namespace: x.namespace, name: x.historyName });
      openRow.querySelector('[data-open]').onclick = () => openWorkload(x);
      openRow.querySelectorAll('[data-copy]').forEach((b) => b.onclick = (e) => {
        e.stopPropagation();
        navigator.clipboard?.writeText(b.dataset.copy);
        b.textContent = 'Copied';
        setTimeout(() => { b.textContent = 'Copy'; }, 1200);
      });
    }
  };
  draw();
  void scope;
}
