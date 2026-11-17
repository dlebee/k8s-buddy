// Per-kind summary components for the resource drawer. Each renderer takes the object as JSON
// (from /api/object) and returns HTML; actions (shell, port-forward, open related) are bound after.
import { esc, ago, api } from '/static/common.js';
import { describeCron, nextRuns } from '/static/cron.js';
import { mountHistory } from '/static/charts.js';
import { verSpan, versionCardHTML } from '/static/versions.js';
import { pickForward, pickShell, forwardsFor, openForward } from '/static/forwards.js';

// ---------- small helpers ----------
const arr = (v) => (Array.isArray(v) ? v : []);
const obj = (v) => (v && typeof v === 'object' ? v : {});
const plural = (n, w) => `${n} ${w}${n === 1 ? '' : 's'}`;
const when = (ts) => (ts ? `${ago(ts)} ago` : '–');

function until(ts) {
  const ms = new Date(ts).getTime() - Date.now();
  const a = Math.abs(ms) / 1000;
  const s = a < 3600 ? `${Math.max(1, Math.round(a / 60))}m` : a < 172800 ? `${Math.round(a / 3600)}h` : `${Math.round(a / 86400)}d`;
  return ms >= 0 ? `in ${s}` : `${s} ago`;
}

const UNITS = { n: 1e-9, u: 1e-6, m: 1e-3, k: 1e3, K: 1e3, M: 1e6, G: 1e9, T: 1e12, P: 1e15, E: 1e18, Ki: 1024, Mi: 1024 ** 2, Gi: 1024 ** 3, Ti: 1024 ** 4, Pi: 1024 ** 5, Ei: 1024 ** 6 };
function qty(s) {
  const m = /^([0-9.]+)(e[0-9]+)?([a-zA-Z]{0,2})$/.exec(String(s ?? '').trim());
  if (!m) return NaN;
  return parseFloat(m[1] + (m[2] || '')) * (UNITS[m[3]] ?? 1);
}
function bytes(n) {
  if (!isFinite(n)) return '–';
  const u = ['B', 'KiB', 'MiB', 'GiB', 'TiB', 'PiB'];
  let i = 0;
  while (n >= 1024 && i < u.length - 1) { n /= 1024; i++; }
  return `${n >= 100 || i === 0 ? Math.round(n) : n.toFixed(1)} ${u[i]}`;
}
const cores = (n) => (isFinite(n) ? (n >= 10 ? Math.round(n) : +n.toFixed(2)) + ' cores' : '–');

const pill = (text, tone = '') => `<span class="pill ${tone}">${esc(text)}</span>`;
const chip = (k, v) => `<span class="rv-chip" title="${esc(k)}${v != null ? '=' + esc(v) : ''}">${v != null ? `<i>${esc(k)}</i>${esc(v)}` : esc(k)}</span>`;
const chips = (o, empty = '<span class="muted">none</span>') => {
  const e = Object.entries(obj(o));
  return e.length ? `<div class="rv-chips">${e.map(([k, v]) => chip(k, v)).join('')}</div>` : empty;
};
const section = (title, body, right = '') => `<section class="rv-section"><div class="rv-section-head"><h3>${title}</h3>${right}</div>${body}</section>`;
const facts = (items) => `<div class="rv-facts">${items.filter(Boolean).map(([label, value, sub, tone]) =>
  `<div class="rv-fact ${tone || ''}"><div class="l">${esc(label)}</div><div class="v" title="${esc(String(value).replace(/<[^>]+>/g, ''))}">${value}</div>${sub ? `<div class="s">${sub}</div>` : ''}</div>`).join('')}</div>`;

function image(ref) {
  const s = String(ref || '');
  const at = s.indexOf('@');
  const base = at >= 0 ? s.slice(0, at) : s;
  const digest = at >= 0 ? s.slice(at + 1) : '';
  const slash = base.lastIndexOf('/');
  const colon = base.lastIndexOf(':');
  const tag = colon > slash ? base.slice(colon + 1) : digest ? '' : 'latest';
  const path = colon > slash ? base.slice(0, colon) : base;
  const repo = path.slice(path.lastIndexOf('/') + 1);
  const prefix = path.slice(0, path.lastIndexOf('/') + 1);
  return `<span class="rv-image" title="${esc(s)}"><span class="muted">${esc(prefix)}</span><b>${esc(repo)}</b>${tag ? `<span class="rv-tag">${esc(tag)}</span>` : ''}${digest ? `<span class="rv-tag muted">${esc(digest.slice(0, 19))}</span>` : ''}</span>`;
}

function conditionChips(conds, goodFalse = []) {
  return `<div class="rv-chips">${arr(conds).map((c) => {
    const good = goodFalse.includes(c.type) ? c.status === 'False' : c.status === 'True';
    const tone = good ? 'ok' : c.status === 'Unknown' ? 'warn' : goodFalse.includes(c.type) ? 'bad' : 'warn';
    const tip = [c.reason, c.message, c.lastTransitionTime && `since ${when(c.lastTransitionTime)}`].filter(Boolean).join(' · ');
    return `<span class="pill ${tone}" title="${esc(tip)}">${good ? '✓' : '✕'} ${esc(c.type)}</span>`;
  }).join('') || '<span class="muted">none reported</span>'}</div>`;
}

function podDots(pods, ctx) {
  if (!pods.length) return '<p class="muted rv-tight">No pods.</p>';
  return `<div class="rv-pods">${pods.map((p) => `<button class="rv-pod" data-open-pod="${esc(p.name)}" title="${esc(p.status)} · ${esc(p.ready)} ready · ${p.restarts} restarts${p.node ? ' · ' + esc(p.node) : ''}">
    <span class="dot ${p.health}"></span><span class="n">${esc(p.name)}</span>${p.restarts ? `<span class="r">↻${p.restarts}</span>` : ''}<span class="st">${esc(p.status)}</span></button>`).join('')}</div>`;
}

function replicasBar({ desired, ready, updated, available }) {
  const total = Math.max(desired, ready, updated, 1);
  const slots = Math.min(total, 40);
  const scale = total / slots;
  const segs = Array.from({ length: slots }, (_, i) => {
    const n = (i + 1) * scale;
    const cls = n <= ready ? 'ok' : n <= updated ? 'warn' : n <= desired ? 'empty' : 'extra';
    return `<span class="${cls}"></span>`;
  }).join('');
  const tone = desired === 0 ? '' : available >= desired ? 'ok' : available === 0 ? 'bad' : 'warn';
  return `<div class="rv-replicas">
    <div class="rv-rep-big ${tone}"><b>${ready}</b><span>/ ${desired}</span><small>ready</small></div>
    <div class="rv-rep-right">
      <div class="rv-slots">${segs}</div>
      <div class="rv-rep-legend">
        <span><i class="ok"></i>${ready} ready</span><span><i class="warn"></i>${updated} up-to-date</span><span><i class="acc"></i>${available} available</span>
      </div>
    </div></div>`;
}

// slot marks a region that re-renders whenever the list of port forwards changes.
function slot(ctx, name, fn) {
  (ctx.slots ||= {})[name] = fn;
  return `<div data-slot="${name}">${fn()}</div>`;
}

const actionsBar = (buttons) => (buttons.filter(Boolean).length ? `<div class="rv-actions">${buttons.filter(Boolean).join('')}</div>` : '');
const btnForward = `<button class="btn sm" data-act="forward">⇄ Port-forward</button>`;
const btnShell = `<button class="btn sm" data-act="shell">❯_ Shell</button>`;

function activeForwards(target) {
  const fs = forwardsFor(target).filter((f) => f.state === 'active');
  return fs.length ? `<div class="rv-forwards">${fs.map((f) => `<span class="rv-fwd"><span class="dot ok"></span>:${f.remotePort} →
    <a href="${esc(f.url)}" data-open-fwd="${f.id}">${esc(f.url.replace('http://', ''))}</a></span>`).join('')}</div>` : '';
}

function warnings(events) {
  if (!events?.length) return '';
  return section(`Recent warnings <span class="muted">${events.length}</span>`, `<div class="rv-events">${events.slice(0, 8).map((e) => `
    <div class="rv-event"><div class="t"><b>${esc(e.status)}</b><span class="muted">${ago(e.created)} ago${+e.cols[2] > 1 ? ` · ×${esc(e.cols[2])}` : ''}</span></div><div class="m">${esc(e.cols[1])}</div></div>`).join('')}</div>`);
}

function metadata(md) {
  const labels = obj(md.labels);
  const owners = arr(md.ownerReferences);
  return section('Labels', chips(labels)) +
    (owners.length ? `<p class="muted rv-tight">Owned by ${owners.map((o) => `<b>${esc(o.kind)}</b> ${esc(o.name)}`).join(', ')}</p>` : '');
}

// ---------- Pod ----------
function containerCard(c, st, init) {
  const s = obj(st);
  const state = obj(s.state);
  const last = obj(obj(s.lastState).terminated);
  let badge, tone;
  if (state.running) { badge = s.ready === false ? 'Running · not ready' : 'Running'; tone = s.ready === false ? 'warn' : 'ok'; }
  else if (state.waiting) { badge = state.waiting.reason || 'Waiting'; tone = /BackOff|Err|Invalid/.test(badge) ? 'bad' : 'warn'; }
  else if (state.terminated) { badge = state.terminated.reason || `Exit ${state.terminated.exitCode}`; tone = state.terminated.exitCode === 0 ? '' : 'bad'; }
  else { badge = 'Pending'; tone = 'warn'; }
  const ports = arr(c.ports);
  const res = obj(c.resources);
  const reqLim = [['cpu', 'CPU'], ['memory', 'Mem']].map(([k, l]) => {
    const r = obj(res.requests)[k], li = obj(res.limits)[k];
    return r || li ? `<span class="rv-chip"><i>${l}</i>${esc(r || '–')}${li ? ' / ' + esc(li) : ''}</span>` : '';
  }).join('');
  return `<div class="rv-container ${tone}">
    <div class="rv-c-head">
      <span class="dot ${tone === 'ok' ? 'ok' : tone === 'bad' ? 'bad' : tone === 'warn' ? 'warn' : 'neutral'}"></span>
      <b class="rv-c-name">${esc(c.name)}</b>${init ? pill('init') : ''}
      ${pill(badge, tone)}
      <span class="spacer"></span>
      <span class="rv-restarts ${s.restartCount ? 'warn' : ''}" title="restarts">↻ ${s.restartCount || 0}</span>
    </div>
    <div class="rv-c-body">
      <div>${image(c.image)}</div>
      <div class="rv-c-meta">
        ${state.running ? `<span>started ${when(state.running.startedAt)}</span>` : ''}
        ${state.waiting?.message ? `<span class="rv-c-msg">${esc(state.waiting.message)}</span>` : ''}
        ${state.terminated ? `<span>exit ${state.terminated.exitCode}${state.terminated.finishedAt ? ' · ' + when(state.terminated.finishedAt) : ''}</span>` : ''}
      </div>
      ${last.reason || last.exitCode != null ? `<div class="rv-last"><span class="muted">Last termination</span> ${pill(last.reason || 'Exited', last.reason === 'Completed' ? '' : 'bad')}
        <span class="muted">exit ${esc(last.exitCode)}${last.finishedAt ? ' · ' + when(last.finishedAt) : ''}</span></div>` : ''}
      ${ports.length || reqLim ? `<div class="rv-chips">${ports.map((p) => chip(p.name || 'port', `${p.containerPort}/${p.protocol || 'TCP'}`)).join('')}${reqLim}</div>` : ''}
    </div>
  </div>`;
}

// ---------- requests / limits / live usage ----------
const fmtCPU = (m) => m < 0 ? '–' : m >= 1000 ? `${+(m / 1000).toFixed(m >= 10000 ? 0 : 2)}` : `${m}m`;
const fmtMem = (b) => b < 0 ? '–' : bytes(b);
const sumOf = (list, f) => { let any = false, t = 0; for (const x of list) { const v = f(x); if (v >= 0) { any = true; t += v; } } return any ? t : -1; };

// usageBar draws live usage against request (dashed tick) and limit (solid tick).
function usageBar(label, used, req, lim, fmt, isMem) {
  const scale = Math.max(lim, req, used ?? 0, 1) * (lim > 0 ? 1 : 1.25);
  const pct = (v) => Math.min(100, Math.max(0, (v / scale) * 100));
  const has = used != null;
  const tone = !has ? '' : lim > 0 && used >= lim * 0.9 ? 'bad' : req > 0 && used > req ? (isMem ? 'warn' : 'over') : 'ok';
  const notes = [];
  if (has && req > 0) notes.push(`${Math.round((used / req) * 100)}% of request`);
  if (has && lim > 0) notes.push(`${Math.round((used / lim) * 100)}% of limit`);
  if (lim < 0) notes.push('no limit');
  if (req < 0) notes.push('no request');
  const why = tone === 'bad' ? (isMem ? 'close to the memory limit: the container gets OOM-killed when it reaches it' : 'close to the CPU limit: the container is being throttled')
    : tone === 'warn' ? 'above its memory request: first in line for eviction when the node runs short' : tone === 'over' ? 'bursting above its CPU request (fine while the node has spare CPU)' : '';
  return `<div class="rv-use ${tone}" ${why ? `title="${esc(why)}"` : ''}>
    <div class="rv-use-top"><b>${label}</b><span>${has ? fmt(used) : '<span class="muted">no data</span>'}<span class="muted"> · req ${fmt(req)} · lim ${fmt(lim)}</span></span></div>
    <div class="rv-use-bar">${has ? `<span class="fill" style="width:${pct(used)}%"></span>` : ''}${req > 0 ? `<i class="req" style="left:${pct(req)}%" title="request ${fmt(req)}"></i>` : ''}${lim > 0 ? `<i class="lim" style="left:${pct(lim)}%" title="limit ${fmt(lim)}"></i>` : ''}</div>
    <div class="muted rv-small">${notes.join(' · ')}</div></div>`;
}

function usageHTML(rep, single) {
  if (!rep.pods.length) return '<p class="muted">No pods.</p>';
  const note = rep.metrics ? `<p class="muted rv-small">Live usage from the metrics API, averaged over ${esc(rep.window || 'the last minute')} · refreshes every 15s</p>`
    : `<p class="muted rv-small">⚠ ${esc(rep.metricsError || 'Metrics API unavailable')} — showing requests and limits only.</p>`;
  if (single) {
    const p = rep.pods[0];
    return note + p.containers.map((c) => `<div class="rv-use-c">${p.containers.length > 1 ? `<div class="rv-c-name">${esc(c.name)}</div>` : ''}<div class="rv-use-grid">
      ${usageBar('CPU', c.usage?.cpu, c.requests.cpu, c.limits.cpu, fmtCPU)}${usageBar('Memory', c.usage?.memory, c.requests.memory, c.limits.memory, fmtMem, true)}</div></div>`).join('');
  }
  const agg = (p, k, f) => sumOf(p.containers, (c) => f(c)?.[k] ?? -1);
  const rows = rep.pods.map((p) => {
    const u = (k) => { const v = agg(p, k, (c) => c.usage); return v < 0 ? null : v; };
    return `<div class="rv-use-row"><button class="rv-topo-pod ${esc(p.health)}" data-open-pod="${esc(p.name)}" title="${esc(p.name + (p.containers.length > 1 ? '\ncontainers: ' + p.containers.map((c) => c.name).join(', ') : ''))}"><i></i>${esc(p.name)}</button>
      ${usageBar('CPU', u('cpu'), agg(p, 'cpu', (c) => c.requests), agg(p, 'cpu', (c) => c.limits), fmtCPU)}
      ${usageBar('Memory', u('memory'), agg(p, 'memory', (c) => c.requests), agg(p, 'memory', (c) => c.limits), fmtMem, true)}</div>`;
  }).join('');
  const total = (k, f) => sumOf(rep.pods, (p) => agg(p, k, f));
  const tu = (k) => { const v = total(k, (c) => c.usage); return v < 0 ? null : v; };
  return note + `<div class="rv-use-total">${usageBar(`CPU · ${rep.pods.length} pods`, tu('cpu'), total('cpu', (c) => c.requests), total('cpu', (c) => c.limits), fmtCPU)}
    ${usageBar('Memory', tu('memory'), total('memory', (c) => c.requests), total('memory', (c) => c.limits), fmtMem, true)}</div>
    <div class="rv-use-rows">${rows}</div>`;
}

function podOwner(md) {
  const o = arr(md.ownerReferences).find((x) => x.controller) || arr(md.ownerReferences)[0];
  if (!o) return null;
  const hash = obj(md.labels)['pod-template-hash'];
  if (o.kind === 'ReplicaSet' && hash && o.name.endsWith('-' + hash)) return { kind: 'deployments', label: 'Deployment', name: o.name.slice(0, -hash.length - 1) };
  const map = { StatefulSet: 'statefulsets', DaemonSet: 'daemonsets', Job: 'jobs', ReplicaSet: 'cr:apps/replicasets' };
  return { kind: map[o.kind], label: o.kind, name: o.name };
}

function podSummary(o, ctx) {
  const spec = obj(o.spec), status = obj(o.status), md = obj(o.metadata);
  const byName = Object.fromEntries(arr(status.containerStatuses).map((s) => [s.name, s]));
  const initBy = Object.fromEntries(arr(status.initContainerStatuses).map((s) => [s.name, s]));
  const restarts = arr(status.containerStatuses).reduce((n, s) => n + (s.restartCount || 0), 0);
  const ready = arr(status.containerStatuses).filter((s) => s.ready).length;
  const owner = podOwner(md);
  const ports = arr(spec.containers).flatMap((c) => arr(c.ports).filter((p) => !p.protocol || p.protocol === 'TCP').map((p) => ({ port: p.containerPort, label: `${c.name}${p.name ? ' · ' + p.name : ''}` })));
  ctx.forward = { target: { kind: 'pods', name: md.name }, ports };
  ctx.shell = arr(spec.containers).map((c) => ({ pod: md.name, container: c.name, running: !!obj(byName[c.name]?.state).running, status: Object.keys(obj(byName[c.name]?.state))[0] }));
  const stateReason = arr(status.containerStatuses).map((c) => obj(c.state).waiting?.reason || obj(c.state).terminated?.reason).find(Boolean);
  const phaseTone = stateReason ? (/BackOff|Err|OOM|Invalid/.test(stateReason) ? 'bad' : stateReason === 'Completed' ? '' : 'warn')
    : status.phase === 'Running' ? (ready === arr(spec.containers).length ? 'ok' : 'warn') : status.phase === 'Failed' ? 'bad' : status.phase === 'Succeeded' ? '' : 'warn';
  const initDone = arr(spec.initContainers).every((c) => obj(initBy[c.name]?.state).terminated?.exitCode === 0);
  return `
    ${actionsBar([btnShell, btnForward])}${slot(ctx, 'fwd', () => activeForwards({ ...ctx.base, kind: 'pods', name: md.name }))}
    ${facts([
      ['Status', pill(ctx.row?.status || stateReason || status.phase || '?', ctx.row ? toneOf(ctx.row.health) : phaseTone), status.reason ? esc(status.reason) : ''],
      ['Ready', `${ready}<small> / ${arr(spec.containers).length}</small>`, 'containers', ready < arr(spec.containers).length ? 'warn' : ''],
      ['Restarts', String(restarts), restarts ? `across containers` : 'none', restarts > 3 ? 'bad' : restarts ? 'warn' : ''],
      ['Node', spec.nodeName ? `<a data-open="nodes" data-name="${esc(spec.nodeName)}">${esc(spec.nodeName)}</a>` : '<span class="muted">unscheduled</span>'],
      ['Pod IP', esc(status.podIP || '–'), status.hostIP ? `host ${esc(status.hostIP)}` : ''],
      ['QoS', esc(status.qosClass || '–'), spec.priorityClassName ? esc(spec.priorityClassName) : ''],
      ['Age', esc(ago(md.creationTimestamp)), new Date(md.creationTimestamp).toLocaleString()],
      owner && ['Controlled by', owner.kind ? `<a data-open="${esc(owner.kind)}" data-name="${esc(owner.name)}">${esc(owner.name)}</a>` : esc(owner.name), esc(owner.label)],
    ])}
    ${section(`Containers <span class="muted">${arr(spec.containers).length}</span>`, arr(spec.containers).map((c) => containerCard(c, byName[c.name])).join(''))}
    ${status.phase === 'Running' || status.phase === 'Pending' ? section('Resources', '<div data-usage="1"><p class="muted">Loading usage…</p></div>') : ''}
    ${section('Usage over time', '<div class="uc" data-hist="pod"><p class="muted">Loading…</p></div>')}
    ${arr(spec.initContainers).length ? `<details class="rv-details" ${initDone ? '' : 'open'}><summary>Init containers <span class="muted">${arr(spec.initContainers).length}${initDone ? ' · all completed' : ''}</span></summary>
      ${arr(spec.initContainers).map((c) => containerCard(c, initBy[c.name], true)).join('')}</details>` : ''}
    ${section('Conditions', conditionChips(status.conditions))}
    ${warnings(ctx.events)}
    ${metadata(md)}`;
}

const toneOf = (h) => (h === 'ok' ? 'ok' : h === 'bad' ? 'bad' : h === 'warn' ? 'warn' : '');

// ---------- Deployment / StatefulSet / DaemonSet ----------
function workloadSummary(kind) {
  return (o, ctx) => {
    const spec = obj(o.spec), status = obj(o.status), md = obj(o.metadata);
    const tpl = obj(obj(spec.template).spec);
    let counts, strategy;
    if (kind === 'daemonsets') {
      counts = { desired: status.desiredNumberScheduled || 0, ready: status.numberReady || 0, updated: status.updatedNumberScheduled || 0, available: status.numberAvailable || 0 };
      strategy = obj(spec.updateStrategy).type;
    } else {
      const desired = spec.replicas ?? 1;
      counts = { desired, ready: status.readyReplicas || 0, updated: status.updatedReplicas || 0, available: status.availableReplicas ?? status.readyReplicas ?? 0 };
      strategy = kind === 'statefulsets' ? obj(spec.updateStrategy).type : obj(spec.strategy).type;
    }
    const conds = arr(status.conditions);
    const progressing = conds.find((c) => c.type === 'Progressing');
    const stuck = progressing?.reason === 'ProgressDeadlineExceeded';
    const rolling = counts.updated < counts.desired || (status.observedGeneration != null && md.generation > status.observedGeneration);
    const rollout = stuck ? ['Rollout stuck', 'bad'] : rolling ? ['Rolling out', 'warn'] : counts.desired === 0 ? ['Scaled to 0', ''] : counts.available >= counts.desired ? ['Rolled out', 'ok'] : ['Degraded', 'warn'];
    const pods = ctx.pods || [];
    const ports = arr(tpl.containers).flatMap((c) => arr(c.ports).filter((p) => !p.protocol || p.protocol === 'TCP').map((p) => ({ port: p.containerPort, label: `${c.name}${p.name ? ' · ' + p.name : ''}` })));
    if (kind !== 'daemonsets') ctx.forward = { target: { kind, name: md.name }, ports };
    ctx.shell = pods.flatMap((p) => p.containers.map((c) => ({ pod: p.name, container: c, running: /^Running/.test(p.status), status: p.status })));
    return `
      ${actionsBar([ctx.shell.length && btnShell, kind !== 'daemonsets' && btnForward])}${kind !== 'daemonsets' ? slot(ctx, 'fwd', () => activeForwards({ ...ctx.base, kind, name: md.name })) : ''}
      <div class="rv-hero">
        ${replicasBar(counts)}
        <div class="rv-rollout">
          <div>${pill(rollout[0], rollout[1])} ${strategy ? `<span class="muted">${esc(strategy)}</span>` : ''}</div>
          ${progressing ? `<div class="rv-rollout-msg" title="${esc(progressing.message || '')}">${esc(progressing.reason || '')}${progressing.message ? ' — ' + esc(progressing.message) : ''}</div>` : ''}
          ${progressing?.lastUpdateTime ? `<div class="muted rv-small">updated ${when(progressing.lastUpdateTime)}</div>` : ''}
        </div>
      </div>
      ${section('Images', `<div class="rv-images">${arr(tpl.containers).map((c) => `<div class="rv-img-row"><span class="rv-c-name">${esc(c.name)}</span>${image(c.image)}</div>`).join('')}</div>`)}
      ${pods.length ? section('Resources', '<div data-usage="0"><p class="muted">Loading usage…</p></div>') : ''}
      ${section('Usage over time <span class="muted">all pods</span>', '<div class="uc" data-hist="workload"><p class="muted">Loading…</p></div>')}
      ${section(`Pods <span class="muted">${pods.length}</span>`, podDots(pods, ctx), pods.length ? `<span class="rv-pod-sum">${['ok', 'warn', 'bad'].map((h) => { const n = pods.filter((p) => p.health === h).length; return n ? `<span><span class="dot ${h}"></span>${n}</span>` : ''; }).join('')}</span>` : '')}
      ${section('Selector', chips(obj(spec.selector).matchLabels))}
      ${conds.length ? section('Conditions', conditionChips(conds)) : ''}
      ${warnings(ctx.events)}
      ${metadata(md)}`;
  };
}

// ---------- Service ----------
function serviceSummary(o, ctx) {
  const spec = obj(o.spec), status = obj(o.status), md = obj(o.metadata);
  const pods = ctx.pods || [];
  const readyPods = pods.filter((p) => p.health === 'ok').length;
  const lb = arr(obj(status.loadBalancer).ingress).map((i) => i.ip || i.hostname);
  const ext = [...lb, ...arr(spec.externalIPs)];
  const hasSel = Object.keys(obj(spec.selector)).length > 0;
  const tcp = arr(spec.ports).filter((p) => !p.protocol || p.protocol === 'TCP');
  ctx.forward = { target: { kind: 'services', name: md.name }, ports: tcp.map((p) => ({ port: p.port, label: p.name || '' })) };
  return `
    ${actionsBar([tcp.length && btnForward])}
    ${facts([
      ['Type', esc(spec.type || 'ClusterIP'), spec.sessionAffinity && spec.sessionAffinity !== 'None' ? `affinity ${esc(spec.sessionAffinity)}` : ''],
      ['Cluster IP', esc(spec.clusterIP || '–'), arr(spec.ipFamilies).join(', ')],
      spec.type === 'LoadBalancer' || ext.length ? ['External', ext.length ? esc(ext.join(', ')) : '<span class="pill warn">pending</span>', spec.loadBalancerClass ? esc(spec.loadBalancerClass) : '', ext.length ? '' : 'warn'] : null,
      spec.type === 'ExternalName' ? ['External name', esc(spec.externalName)] : null,
      hasSel ? ['Endpoints', `${readyPods}<small> / ${pods.length}</small>`, 'ready pods selected', pods.length === 0 ? 'bad' : readyPods < pods.length ? 'warn' : ''] : ['Endpoints', '<span class="muted">manual</span>', 'no selector'],
    ])}
    ${section('Ports', arr(spec.ports).length ? slot(ctx, 'ports', () => `<div class="rv-ports">${arr(spec.ports).map((p) => {
      const fwds = forwardsFor({ ...ctx.base, kind: 'services', name: md.name }).filter((f) => f.state === 'active');
      const f = fwds.find((x) => x.remotePort === p.port);
      return `<div class="rv-port">
        <span class="rv-port-name">${esc(p.name || '')}</span>
        <span class="rv-port-box svc">${p.port}<small>${esc(p.protocol || 'TCP')}</small></span>
        <span class="rv-arrow"></span>
        <span class="rv-port-box pod" title="target port on the pods">${esc(p.targetPort ?? p.port)}<small>pods</small></span>
        ${p.nodePort ? `<span class="rv-port-box node" title="node port">${p.nodePort}<small>node</small></span>` : '<span></span>'}
        ${f ? `<a class="rv-fwd-link" href="${esc(f.url)}" data-open-fwd="${f.id}">${esc(f.url.replace('http://', ''))}</a>`
          : (!p.protocol || p.protocol === 'TCP') ? `<button class="btn sm" data-forward-port="${p.port}">⇄ Forward</button>` : '<span></span>'}
      </div>`;
    }).join('')}</div>`) : '<p class="muted">No ports.</p>')}
    ${hasSel ? section(`Selects ${plural(pods.length, 'pod')}`, (pods.length ? '' : '<p class="rv-warnline">⚠ The selector matches no pods, so this service has no endpoints.</p>') + podDots(pods, ctx)) : ''}
    ${section('Selector', chips(spec.selector))}
    ${warnings(ctx.events)}
    ${metadata(md)}`;
}

// ---------- Ingress ----------
function ingressSummary(o, ctx) {
  const spec = obj(o.spec), status = obj(o.status), md = obj(o.metadata);
  const ann = obj(md.annotations);
  const cls = spec.ingressClassName || ann['kubernetes.io/ingress.class'] || '';
  const tlsHosts = new Set(arr(spec.tls).flatMap((t) => arr(t.hosts)));
  const lb = arr(obj(status.loadBalancer).ingress).map((i) => i.ip || i.hostname);
  const svcs = ctx.services || {};
  const backend = (b) => {
    const s = obj(obj(b).service);
    if (!s.name) return obj(b).resource ? pill(`${obj(b).resource.kind}/${obj(b).resource.name}`) : pill('?');
    const port = obj(s.port).number ?? obj(s.port).name ?? '';
    const known = s.name in svcs;
    const missing = known && svcs[s.name] == null;
    const badPort = known && !missing && typeof port === 'number' && !svcs[s.name].includes(port);
    const tone = missing || badPort ? 'bad' : 'accent';
    const tip = missing ? 'service not found' : badPort ? `service has no port ${port}` : 'open service';
    return `<a class="rv-backend ${tone}" data-open="services" data-name="${esc(s.name)}" title="${esc(tip)}">${esc(s.name)}<small>:${esc(port)}</small>${missing ? ' <b>missing</b>' : badPort ? ' <b>bad port</b>' : ''}</a>`;
  };
  const rules = arr(spec.rules);
  return `
    ${facts([
      ['Class', cls ? esc(cls) : '<span class="muted">default</span>'],
      ['Address', lb.length ? esc(lb.join(', ')) : '<span class="pill warn">none yet</span>', '', lb.length ? '' : 'warn'],
      ['TLS', tlsHosts.size ? `🔒 ${tlsHosts.size} host${tlsHosts.size === 1 ? '' : 's'}` : '<span class="muted">off</span>', arr(spec.tls).map((t) => esc(t.secretName || '')).filter(Boolean).join(', ')],
      ['Rules', String(rules.length), plural(rules.reduce((n, r) => n + arr(obj(r.http).paths).length, 0), 'path')],
    ])}
    ${section('Routes', `<div class="rv-routes">
      ${rules.map((r) => `<div class="rv-route">
        <div class="rv-host ${tlsHosts.has(r.host) ? 'tls' : ''}">${tlsHosts.has(r.host) ? '<span title="TLS">🔒</span>' : '<span class="muted" title="plain HTTP">○</span>'}<span>${esc(r.host || '*')}</span></div>
        <div class="rv-paths">${arr(obj(r.http).paths).map((p) => `<div class="rv-path">${r.host && !r.host.startsWith('*') ? `<button class="rv-visit" data-visit-host="${esc(r.host)}" data-visit-path="${esc(p.path || '')}" title="Open ${tlsHosts.has(r.host) ? 'https' : 'http'}://${esc(r.host)}${esc(p.path || '/')} in your browser">↗</button>` : ''}<code>${esc(p.path || '/')}</code><span class="muted rv-ptype">${esc((p.pathType || '').replace('ImplementationSpecific', 'impl'))}</span><span class="rv-arrow"></span>${backend(p.backend)}</div>`).join('') || '<div class="rv-path muted">no paths</div>'}</div>
      </div>`).join('')}
      ${spec.defaultBackend ? `<div class="rv-route"><div class="rv-host"><span class="muted">default</span></div><div class="rv-paths"><div class="rv-path"><code>*</code><span class="rv-arrow"></span>${backend(spec.defaultBackend)}</div></div></div>` : ''}
      ${!rules.length && !spec.defaultBackend ? '<p class="muted">No rules.</p>' : ''}
    </div>`)}
    ${warnings(ctx.events)}
    ${metadata(md)}
    ${ingressTopology(md, rules, spec.defaultBackend, tlsHosts, ctx)}`;
}

// ingressTopology draws ingress → service → pods; a backend that leads nowhere ends in a red ✕.
function ingressTopology(md, rules, defaultBackend, tlsHosts, ctx) {
  const svcs = ctx.services || {}, bpods = ctx.backendPods || {};
  const routes = new Map(); // service → ["host/path", ...]
  const add = (b, r) => { const n = obj(obj(b).service).name; if (n) { if (!routes.has(n)) routes.set(n, []); routes.get(n).push(r); } };
  for (const r of rules) for (const p of arr(obj(r.http).paths)) add(p.backend, (r.host || '*') + (p.path || '/'));
  if (defaultBackend) add(defaultBackend, 'default backend');
  const hosts = rules.map((r) => r.host).filter(Boolean);
  const lock = hosts.some((h) => tlsHosts.has(h)) ? '🔒 ' : '';
  return trafficPath('Ingress', md.name, lock + (hosts.length ? hosts[0] + (hosts.length > 1 ? ` +${hosts.length - 1}` : '') : '*'), routes, ctx);
}

// trafficPath draws <head> → services → pods. routes maps a service key ("name", or "ns/name" for
// another namespace) to the host/paths that reach it.
function trafficPath(head, title, sub, routes, ctx) {
  if (!routes.size) return '';
  const svcs = ctx.services || {}, bpods = ctx.backendPods || {};
  const MAXP = 8;
  // Pods share their workload's name; show just the distinguishing tail ("…-7d9f-abcde").
  const short = (pods, svc) => {
    const names = pods.map((p) => p.name);
    let pre = names.length > 1 ? names.reduce((a, b) => { let i = 0; while (i < a.length && a[i] === b[i]) i++; return a.slice(0, i); }) : names[0].startsWith(svc) ? svc : '';
    pre = pre.slice(0, pre.lastIndexOf('-') + 1);
    return (n) => pre.length > 6 && n.length > pre.length ? '…-' + n.slice(pre.length) : n;
  };
  const rows = [...routes].map(([key, rs]) => {
    const [ns, name] = key.includes('/') ? key.split('/') : [null, key];
    const nsAttr = ns ? ` data-ns="${esc(ns)}"` : '';
    const known = key in svcs, missing = known && svcs[key] == null, pods = bpods[key];
    const dangling = missing || (pods && !pods.length);
    const svcBox = missing
      ? `<span class="rv-topo-box svc bad dashed" title="Service not found">${esc(name)}<small>${ns ? esc(ns) + ' · ' : ''}missing</small></span>`
      : `<a class="rv-topo-box svc ${dangling ? 'bad' : ''}" data-open="services" data-name="${esc(name)}"${nsAttr} title="${esc(rs.join('\n'))}">${esc(name)}<small>${esc(rs.length === 1 ? rs[0] : plural(rs.length, 'route'))}</small></a>`;
    const right = missing ? '<span class="rv-topo-x" title="Service not found">✕</span>'
      : !known ? '<span class="muted rv-small">syncing…</span>'
        : !pods ? '<span class="muted rv-small">no selector · manual endpoints</span>'
          : !pods.length ? '<span class="rv-topo-x" title="No pods match this service selector">✕</span><span class="rv-topo-why">no pods match</span>'
            : `<div class="rv-topo-pods">${pods.slice(0, MAXP).map((p, _, __, sh = short(pods, name)) => `<button class="rv-topo-pod ${esc(p.health)}" data-open-pod="${esc(p.name)}"${ns ? ` data-pod-ns="${esc(ns)}"` : ''} title="${esc(`${p.name}\n${p.status} · ${p.ready} ready${p.restarts ? ` · ${p.restarts} restarts` : ''}`)}"><i></i>${esc(sh(p.name))}</button>`).join('')}${pods.length > MAXP ? `<span class="muted rv-small">+${pods.length - MAXP} more</span>` : ''}</div>`;
    return `<div class="rv-topo-row ${dangling ? 'dangling' : ''}"><span class="rv-topo-wire"></span>${svcBox}<span class="rv-topo-wire ${dangling ? 'stub' : ''}"></span>${right}</div>`;
  }).join('');
  return section('Traffic path', `<div class="rv-topo">
    <div class="rv-topo-head"><span>${esc(head)}</span><span>Service</span><span>Pods</span></div>
    <div class="rv-topo-body">
      <div class="rv-topo-box ing">${esc(title)}<small>${esc(sub)}</small></div>
      <div class="rv-topo-rows">${rows}</div>
    </div></div>`);
}

// ---------- Gateway API ----------
const condTone = (c) => (c.status === 'True' ? 'ok' : c.status === 'False' ? 'bad' : 'warn');
const condPills = (conds) => arr(conds).length
  ? `<div class="rv-chips">${arr(conds).map((c) => `<span class="pill ${condTone(c)}" title="${esc([c.reason, c.message].filter(Boolean).join(': '))}">${esc(c.type)}</span>`).join('')}</div>`
  : '<span class="muted">no status yet — no controller has picked this up</span>';

function gatewaySummary(o, ctx) {
  const spec = obj(o.spec), status = obj(o.status), md = obj(o.metadata);
  const addrs = arr(status.addresses).map((a) => a.value).filter(Boolean);
  const lstat = Object.fromEntries(arr(status.listeners).map((l) => [l.name, l]));
  const programmed = arr(status.conditions).find((c) => c.type === 'Programmed');
  return `
    ${facts([
      ['Class', `<a data-open="cr:gateway.networking.k8s.io/gatewayclasses" data-name="${esc(spec.gatewayClassName || '')}" data-ns="">${esc(spec.gatewayClassName || '–')}</a>`],
      ['Address', addrs.length ? esc(addrs.join(', ')) : '<span class="pill warn">none yet</span>', '', addrs.length ? '' : 'warn'],
      ['Listeners', String(arr(spec.listeners).length)],
      ['Status', programmed ? pill(programmed.status === 'True' ? 'programmed' : 'not programmed', condTone(programmed)) : pill('pending', 'warn'), programmed && programmed.status !== 'True' ? esc(programmed.reason || '') : '', programmed?.status === 'True' ? '' : 'warn'],
    ])}
    ${section('Listeners', `<div class="rv-routes">${arr(spec.listeners).map((l) => {
      const st = lstat[l.name] || {};
      const bad = arr(st.conditions).find((c) => c.status === 'False' && ['Accepted', 'Programmed', 'ResolvedRefs'].includes(c.type));
      return `<div class="rv-route"><div class="rv-host ${l.protocol === 'HTTPS' || l.protocol === 'TLS' ? 'tls' : ''}">${l.protocol === 'HTTPS' || l.protocol === 'TLS' ? '<span title="TLS">🔒</span>' : '<span class="muted">○</span>'}<span>${esc(l.hostname || '*')}</span></div>
        <div class="rv-paths"><div class="rv-path"><code>${esc(l.name)}</code><span class="muted rv-ptype">${esc(l.protocol)} :${esc(l.port)}</span>
          <span class="muted rv-small">${st.attachedRoutes != null ? plural(st.attachedRoutes, 'route') : ''}</span>${bad ? `<span class="pill bad" title="${esc(bad.message || '')}">${esc(bad.reason || bad.type)}</span>` : ''}</div></div></div>`;
    }).join('') || '<p class="muted">No listeners.</p>'}</div>`)}
    ${section('Conditions', condPills(status.conditions))}
    ${warnings(ctx.events)}
    ${metadata(md)}`;
}

function routeSummary(o, ctx) {
  const spec = obj(o.spec), status = obj(o.status), md = obj(o.metadata);
  const kind = o.kind || 'Route';
  const hosts = arr(spec.hostnames);
  const routes = new Map();
  const rules = arr(spec.rules);
  rules.forEach((r) => {
    const paths = arr(r.matches).map((m) => obj(m.path).value || (obj(m.method).service ? `/${m.method.service}/${m.method.method || '*'}` : '')).filter(Boolean);
    for (const b of arr(r.backendRefs)) {
      if ((b.kind || 'Service') !== 'Service' || !b.name) continue;
      const key = b.namespace && b.namespace !== md.namespace ? `${b.namespace}/${b.name}` : b.name;
      if (!routes.has(key)) routes.set(key, []);
      for (const h of hosts.length ? hosts : ['*']) for (const p of paths.length ? paths : ['/']) routes.get(key).push(h + p);
    }
  });
  const parents = arr(status.parents);
  return `
    ${facts([
      ['Hostnames', hosts.length ? esc(hosts[0]) + (hosts.length > 1 ? ` <small>+${hosts.length - 1}</small>` : '') : '<span class="muted">from listener</span>'],
      ['Gateways', String(arr(spec.parentRefs).length)],
      ['Rules', String(rules.length), plural(routes.size, 'backend service')],
    ])}
    ${section('Attached to', arr(spec.parentRefs).map((p) => {
      const ns = p.namespace || md.namespace;
      const st = parents.find((x) => obj(x.parentRef).name === p.name && (obj(x.parentRef).namespace || md.namespace) === ns);
      return `<div class="rv-path"><a class="rv-backend accent" data-open="cr:gateway.networking.k8s.io/gateways" data-name="${esc(p.name)}" data-ns="${esc(ns)}">${esc(ns)}/${esc(p.name)}${p.sectionName ? `<small>:${esc(p.sectionName)}</small>` : ''}</a>${st ? condPills(st.conditions) : '<span class="pill warn">not accepted yet</span>'}</div>`;
    }).join('') || '<p class="muted">No parent gateways.</p>')}
    ${section('Rules', `<div class="rv-routes">${rules.map((r) => `<div class="rv-route"><div class="rv-paths">
      ${(arr(r.matches).length ? arr(r.matches) : [{}]).map((m) => `<div class="rv-path"><code>${esc(obj(m.path).value || (obj(m.method).service ? `${m.method.service}/${m.method.method || '*'}` : '/'))}</code><span class="muted rv-ptype">${esc(obj(m.path).type || (m.method ? 'gRPC' : 'PathPrefix'))}</span><span class="rv-arrow"></span>${arr(r.backendRefs).map((b) => `<span class="rv-backend accent">${esc(b.name)}<small>${b.port ? ':' + esc(b.port) : ''}${b.weight != null ? ` w${esc(b.weight)}` : ''}</small></span>`).join(' ') || '<span class="muted">no backends</span>'}</div>`).join('')}
    </div></div>`).join('') || '<p class="muted">No rules.</p>'}</div>`)}
    ${warnings(ctx.events)}
    ${metadata(md)}
    ${trafficPath(kind, md.name, hosts.length ? hosts[0] + (hosts.length > 1 ? ` +${hosts.length - 1}` : '') : '*', routes, ctx)}`;
}

// ---------- Secret ----------
const SECRET_TYPES = { Opaque: 'generic', 'kubernetes.io/tls': 'TLS certificate', 'kubernetes.io/dockerconfigjson': 'registry credentials', 'kubernetes.io/service-account-token': 'service account token', 'kubernetes.io/basic-auth': 'basic auth', 'kubernetes.io/ssh-auth': 'SSH key', 'helm.sh/release.v1': 'Helm release record' };
function secretSummary(o, ctx) {
  const md = obj(o.metadata);
  const keys = Object.keys(obj(o.stringData)).sort();
  const sizes = ctx.secretSizes || {};
  return `
    ${facts([
      ['Type', esc(o.type || 'Opaque'), esc(SECRET_TYPES[o.type || 'Opaque'] || '')],
      ['Keys', String(keys.length)],
      ['Immutable', o.immutable ? 'yes' : 'no'],
      ['Age', esc(ago(md.creationTimestamp)), new Date(md.creationTimestamp).toLocaleString()],
    ])}
    ${section('Data', keys.length ? `<p class="muted rv-small">Values aren't loaded. Reveal reads one key from the cluster when you click it and hides it again after 60s.</p>
      <div class="sv-keys">${keys.map((k) => `<div class="sv-key" data-key="${esc(k)}">
        <div class="sv-head"><code>${esc(k)}</code><span class="muted rv-small">${sizes[k] != null ? bytes(sizes[k]) : ''}</span><span class="spacer"></span>
          <button class="btn sm" data-reveal="${esc(k)}">Reveal</button></div>
        <div class="sv-val" hidden></div></div>`).join('')}</div>` : '<p class="muted">This secret has no keys.</p>')}
    ${warnings(ctx.events)}
    ${metadata(md)}`;
}

function revealHTML(r) {
  let text = r.text, note = '';
  if (text == null) { text = r.base64; note = `binary · ${bytes(r.size)} · shown as base64`; }
  else if (/^\s*[{[]/.test(text)) { try { text = JSON.stringify(JSON.parse(text), null, 2); note = 'JSON'; } catch { /* not JSON */ } }
  return `<pre class="sv-pre">${esc(text)}</pre><div class="sv-act">${note ? `<span class="muted rv-small">${esc(note)}</span>` : ''}<span class="spacer"></span>
    <span class="muted rv-small" data-countdown></span><button class="btn sm" data-sv-copy>Copy</button><button class="btn sm" data-sv-hide>Hide</button></div>`;
}

// ---------- CronJob ----------
function cronSummary(o, ctx) {
  const spec = obj(o.spec), status = obj(o.status), md = obj(o.metadata);
  // Without spec.timeZone the schedule runs in the controller manager's zone, which is UTC on
  // managed clusters (AKS, EKS, GKE).
  const tz = spec.timeZone || 'UTC';
  let words, runs = [], err = '';
  try { words = describeCron(spec.schedule); runs = spec.suspend ? [] : nextRuns(spec.schedule, tz, 5); } catch (e) { err = e.message; }
  const localTZ = Intl.DateTimeFormat().resolvedOptions().timeZone;
  const fmt = (d, zone) => d.toLocaleString([], { timeZone: zone, weekday: 'short', year: 'numeric', month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit', hour12: false });
  const active = arr(status.active);
  const tpl = obj(obj(obj(obj(spec.jobTemplate).spec).template).spec);
  return `
    ${facts([
      ['Schedule', `<code>${esc(spec.schedule || '')}</code>`, esc(spec.timeZone ? spec.timeZone : 'UTC (cluster default)')],
      ['State', spec.suspend ? pill('suspended', 'warn') : active.length ? pill(`${active.length} running`, 'accent') : pill('waiting', 'ok')],
      ['Last run', status.lastScheduleTime ? esc(ago(status.lastScheduleTime)) + ' ago' : '<span class="muted">never</span>', status.lastScheduleTime ? new Date(status.lastScheduleTime).toLocaleString() : ''],
      ['Last success', status.lastSuccessfulTime ? esc(ago(status.lastSuccessfulTime)) + ' ago' : '<span class="muted">none yet</span>', status.lastSuccessfulTime ? new Date(status.lastSuccessfulTime).toLocaleString() : '',
        status.lastScheduleTime && (!status.lastSuccessfulTime || status.lastSuccessfulTime < status.lastScheduleTime) && !active.length ? 'warn' : ''],
    ])}
    ${section('When it runs', err ? `<p class="rv-warnline">⚠ Can't read this schedule: ${esc(err)}</p>` : `
      <div class="cr-words">${esc(words)}${spec.timeZone ? '' : ' <span class="muted">(UTC)</span>'}</div>
      ${spec.suspend ? '<p class="muted">Suspended: no runs are scheduled until it is resumed.</p>'
        : runs.length ? `<table class="cr-runs"><thead><tr><th></th><th>Your time <span class="muted">${esc(localTZ)}</span></th><th>UTC</th>${spec.timeZone && spec.timeZone !== 'UTC' ? `<th>${esc(spec.timeZone)}</th>` : ''}</tr></thead><tbody>
            ${runs.map((d) => `<tr><td class="muted">${esc(until(d.toISOString()))}</td><td>${esc(fmt(d, localTZ))}</td><td>${esc(fmt(d, 'UTC'))}</td>${spec.timeZone && spec.timeZone !== 'UTC' ? `<td>${esc(fmt(d, spec.timeZone))}</td>` : ''}</tr>`).join('')}
          </tbody></table>${runs.length < 5 ? `<p class="muted rv-small">Only ${runs.length} run${runs.length === 1 ? '' : 's'} in the next 30 years.</p>` : ''}`
          : '<p class="rv-warnline">⚠ This schedule never fires (the date does not exist).</p>'}`)}
    ${section('Policy', facts([
      ['Concurrency', esc(spec.concurrencyPolicy || 'Allow'), ({ Allow: 'runs may overlap', Forbid: 'skips a run while one is active', Replace: 'replaces the running job' })[spec.concurrencyPolicy || 'Allow']],
      ['History kept', `${spec.successfulJobsHistoryLimit ?? 3} ok · ${spec.failedJobsHistoryLimit ?? 1} failed`],
      spec.startingDeadlineSeconds != null ? ['Start deadline', `${spec.startingDeadlineSeconds}s`, 'missed runs older than this are skipped'] : null,
    ]))}
    ${active.length ? section('Running now', `<div class="rv-chips">${active.map((a) => `<a class="rv-backend accent" data-open="jobs" data-name="${esc(a.name)}">${esc(a.name)}</a>`).join('')}</div>`) : ''}
    ${section('Images', `<div class="rv-images">${arr(tpl.containers).map((c) => `<div class="rv-img-row"><span class="rv-c-name">${esc(c.name)}</span>${image(c.image)}</div>`).join('')}</div>`)}
    ${warnings(ctx.events)}
    ${metadata(md)}`;
}

// ---------- PVC / PV ----------
function pvcSummary(o, ctx) {
  const spec = obj(o.spec), status = obj(o.status), md = obj(o.metadata);
  const cap = obj(status.capacity).storage, req = obj(obj(spec.resources).requests).storage;
  const phase = status.phase || 'Pending';
  return `
    <div class="rv-hero">
      <div class="rv-disk"><div class="rv-disk-ico ${phase === 'Bound' ? 'ok' : phase === 'Lost' ? 'bad' : 'warn'}">◉</div>
        <div><div class="rv-disk-cap">${esc(cap || req || '?')}</div><div class="muted">${cap && req && cap !== req ? `requested ${esc(req)}` : cap ? 'capacity' : 'requested'}</div></div></div>
      <div class="rv-bind">
        <span class="rv-node-box">claim<b>${esc(md.name)}</b></span>
        <span class="rv-link ${phase === 'Bound' ? 'ok' : 'warn'}">${esc(phase)}</span>
        ${spec.volumeName ? `<a class="rv-node-box" data-open="pvs" data-name="${esc(spec.volumeName)}">volume<b>${esc(spec.volumeName)}</b></a>` : '<span class="rv-node-box dashed">no volume yet</span>'}
      </div>
    </div>
    ${facts([
      ['Storage class', spec.storageClassName ? esc(spec.storageClassName) : '<span class="muted">none</span>'],
      ['Access modes', arr(spec.accessModes).map((m) => pill(m.replace(/([a-z])([A-Z])/g, '$1 $2'), 'accent')).join(' ') || '–'],
      ['Volume mode', esc(spec.volumeMode || 'Filesystem')],
    ])}
    ${section(`Used by ${plural((ctx.pods || []).length, 'pod')}`, podDots(ctx.pods || [], ctx))}
    ${warnings(ctx.events)}
    ${metadata(md)}`;
}

function pvSummary(o, ctx) {
  const spec = obj(o.spec), status = obj(o.status), md = obj(o.metadata);
  const claim = obj(spec.claimRef);
  const src = spec.csi ? `CSI · ${spec.csi.driver}` : spec.hostPath ? `hostPath · ${spec.hostPath.path}` : spec.local ? `local · ${spec.local.path}` : spec.nfs ? `NFS · ${spec.nfs.server}:${spec.nfs.path}` :
    Object.keys(spec).find((k) => !['capacity', 'accessModes', 'claimRef', 'persistentVolumeReclaimPolicy', 'storageClassName', 'volumeMode', 'nodeAffinity', 'mountOptions'].includes(k)) || '–';
  const phase = status.phase || '?';
  return `
    <div class="rv-hero">
      <div class="rv-disk"><div class="rv-disk-ico ${phase === 'Bound' ? 'ok' : phase === 'Available' ? '' : phase === 'Released' ? 'warn' : 'bad'}">◉</div>
        <div><div class="rv-disk-cap">${esc(obj(spec.capacity).storage || '?')}</div><div class="muted">capacity</div></div></div>
      <div class="rv-bind">
        <span class="rv-node-box">volume<b>${esc(md.name)}</b></span>
        <span class="rv-link ${phase === 'Bound' ? 'ok' : 'warn'}">${esc(phase)}</span>
        ${claim.name ? `<a class="rv-node-box" data-open="pvcs" data-ns="${esc(claim.namespace)}" data-name="${esc(claim.name)}">claim<b>${esc(claim.namespace)}/${esc(claim.name)}</b></a>` : '<span class="rv-node-box dashed">unclaimed</span>'}
      </div>
    </div>
    ${facts([
      ['Storage class', spec.storageClassName ? esc(spec.storageClassName) : '<span class="muted">none</span>'],
      ['Access modes', arr(spec.accessModes).map((m) => pill(m.replace(/([a-z])([A-Z])/g, '$1 $2'), 'accent')).join(' ') || '–'],
      ['Reclaim policy', esc(spec.persistentVolumeReclaimPolicy || '–'), '', spec.persistentVolumeReclaimPolicy === 'Delete' ? '' : ''],
      ['Source', esc(src)],
    ])}
    ${warnings(ctx.events)}
    ${metadata(md)}`;
}

// ---------- Node ----------
function meter(label, used, total, fmt, sub) {
  const pct = total > 0 && isFinite(used) ? Math.min(100, (used / total) * 100) : 0;
  return `<div class="rv-meter"><div class="rv-meter-top"><b>${esc(label)}</b><span>${fmt(used)} <span class="muted">/ ${fmt(total)}</span></span></div>
    <div class="rv-meter-bar"><span style="width:${pct}%"></span></div>${sub ? `<div class="muted rv-small">${sub}</div>` : ''}</div>`;
}

// nodeUseBar: used now against allocatable, with a tick for what the node's pods request.
function nodeUseBar(label, used, req, alloc, fmt, unit) {
  const pct = (v) => (alloc > 0 ? Math.min(100, Math.max(0, (v / alloc) * 100)) : 0);
  const has = used != null;
  const tone = !has ? '' : pct(used) >= 90 ? 'bad' : pct(used) >= 75 ? 'warn' : 'ok';
  const notes = [];
  if (has) notes.push(`${Math.round(pct(used))}% of allocatable in use`);
  notes.push(`${Math.round(pct(req))}% requested by pods`);
  if (has && req > 0) notes.push(used > req ? `${fmt(used - req)}${unit} above requests` : `${fmt(req - used)}${unit} requested but idle`);
  const why = tone === 'bad' ? (label === 'Memory' ? 'nearly full: pods may be evicted or OOM-killed' : 'nearly saturated: pods are competing for CPU') : tone === 'warn' ? 'getting busy' : '';
  return `<div class="rv-use ${tone}" ${why ? `title="${esc(why)}"` : ''}>
    <div class="rv-use-top"><b>${label}</b><span>${has ? `used ${fmt(used)}` : '<span class="muted">no live data</span>'}<span class="muted"> / ${fmt(alloc)}${unit} · requested ${fmt(req)}</span></span></div>
    <div class="rv-use-bar">${has ? `<span class="fill" style="width:${pct(used)}%"></span>` : ''}${req > 0 ? `<i class="req" style="left:${pct(req)}%" title="requested by pods ${fmt(req)}${unit}"></i>` : ''}</div>
    <div class="muted rv-small">${notes.join(' · ')}</div></div>`;
}

function nodeUsageHTML(rep) {
  const note = rep.metrics ? `<p class="muted rv-small">Live from the metrics API, averaged over ${esc(rep.window || 'the last minute')} · ${plural(rep.pods, 'pod')} on the node · refreshes every 15s</p>`
    : `<p class="muted rv-small">⚠ ${esc(rep.metricsError || 'Metrics API unavailable')} — showing what pods request only.</p>`;
  const gi = (b) => (b / 1024 ** 3).toFixed(b >= 100 * 1024 ** 3 ? 0 : 1);
  const co = (m) => (m / 1000).toFixed(m >= 100000 ? 0 : m >= 1000 ? 1 : 2);
  return note + `<div class="rv-use-grid">
    ${nodeUseBar('CPU', rep.usage?.cpu, rep.requests.cpu, rep.allocatable.cpu, co, ' cores')}
    ${nodeUseBar('Memory', rep.usage?.memory, rep.requests.memory, rep.allocatable.memory, gi, ' GiB')}</div>`;
}

function nodeSummary(o, ctx) {
  const spec = obj(o.spec), status = obj(o.status), md = obj(o.metadata);
  const cap = obj(status.capacity), alloc = obj(status.allocatable), info = obj(status.nodeInfo);
  const roles = Object.keys(obj(md.labels)).filter((l) => l.startsWith('node-role.kubernetes.io/')).map((l) => l.split('/')[1]);
  const ready = arr(status.conditions).find((c) => c.type === 'Ready');
  const addrs = arr(status.addresses);
  const reserved = (k, f) => `${f(qty(cap[k]) - qty(alloc[k]))} reserved for the system`;
  return `
    ${spec.unschedulable ? '<p class="rv-warnline">⚠ Cordoned: new pods won\'t be scheduled here.</p>' : ''}
    ${facts([
      ['Status', pill(ready?.status === 'True' ? 'Ready' : 'NotReady', ready?.status === 'True' ? 'ok' : 'bad'), ready ? `since ${when(ready.lastTransitionTime)}` : ''],
      ['Roles', esc(roles.join(', ') || 'worker')],
      ['Kubelet', verSpan(info.kubeletVersion) || '–', esc(info.containerRuntimeVersion || '')],
      ['OS', esc(info.osImage || '–'), `${esc(info.kernelVersion || '')} · ${esc(info.architecture || '')}`],
      ['Address', esc((addrs.find((a) => a.type === 'InternalIP') || addrs[0] || {}).address || '–'), esc((addrs.find((a) => a.type === 'Hostname') || {}).address || '')],
      ['Age', esc(ago(md.creationTimestamp))],
    ])}
    ${info.kubeletVersion ? section('Kubernetes version', versionCardHTML(info.kubeletVersion)) : ''}
    ${section('Current usage <span class="muted">used now against allocatable</span>', '<div data-usage="node"><p class="muted">Loading…</p></div>')}
    ${section('Capacity <span class="muted">allocatable of total</span>', `<div class="rv-meters">
      ${meter('CPU', qty(alloc.cpu), qty(cap.cpu), cores, reserved('cpu', cores))}
      ${meter('Memory', qty(alloc.memory), qty(cap.memory), bytes, reserved('memory', bytes))}
      ${ctx.podCount != null ? meter('Pods', ctx.podCount, qty(alloc.pods), (n) => String(Math.round(n)), 'scheduled of allocatable') : ''}
      ${cap['ephemeral-storage'] ? meter('Ephemeral storage', qty(alloc['ephemeral-storage']), qty(cap['ephemeral-storage']), bytes) : ''}
    </div>`)}
    ${section('Usage over time', '<div class="uc" data-hist="node"><p class="muted">Loading…</p></div>')}
    ${ctx.disks ? section(`Disks <span class="muted">${ctx.disks.limit >= 0 ? `${ctx.disks.attached} of ${ctx.disks.limit} attachable` : `${ctx.disks.attached} attached`}</span>`, diskHTML(ctx.disks)) : ''}
    ${section('Conditions', conditionChips(status.conditions, ['MemoryPressure', 'DiskPressure', 'PIDPressure', 'NetworkUnavailable']))}
    ${section(`Taints <span class="muted">${arr(spec.taints).length}</span>`, arr(spec.taints).length ? `<div class="rv-chips">${arr(spec.taints).map((t) => `<span class="rv-chip taint ${t.effect === 'NoExecute' ? 'bad' : ''}"><i>${esc(t.key)}${t.value ? '=' + esc(t.value) : ''}</i>${esc(t.effect)}</span>`).join('')}</div>` : '<span class="muted">none</span>')}
    ${warnings(ctx.events)}
    ${metadata(md)}`;
}

// diskHTML shows attached volumes per storage driver, against the limit the driver advertises.
function diskHTML(dk) {
  if (!dk.drivers.length) return '<p class="muted">No volumes attached, and no driver advertises an attach limit.</p>';
  return `<div class="rv-meters">${dk.drivers.map((d) => {
    const tone = d.limit > 0 ? (d.attached >= d.limit ? 'bad' : d.attached >= d.limit * 0.8 ? 'warn' : '') : '';
    const vols = (d.volumes || []).map((v) => /^pvc-[0-9a-f-]+$/.test(v) ? `<a class="rv-chip" data-open="pvs" data-name="${esc(v)}" data-ns="">${esc(v)}</a>` : `<span class="rv-chip" title="${esc(v)}">${esc(v.length > 40 ? '…' + v.slice(-39) : v)}</span>`).join('');
    const head = d.limit >= 0 ? meter(d.driver, d.attached, d.limit, (n) => String(Math.round(n)), tone === 'bad' ? '⚠ at the attach limit: new volumes for pods on this node will wait' : tone === 'warn' ? 'close to the attach limit' : 'attached of the attach limit')
      : `<div class="rv-meter"><div class="rv-meter-top"><b>${esc(d.driver)}</b><span>${d.attached} <span class="muted">attached · no limit advertised</span></span></div></div>`;
    return `<div class="dk ${tone}">${head}${vols ? `<div class="rv-chips" style="margin-top:6px">${vols}</div>` : ''}</div>`;
  }).join('')}</div>`;
}

// ---------- cert-manager Certificate ----------
function certSummary(o, ctx) {
  const spec = obj(o.spec), status = obj(o.status), md = obj(o.metadata);
  const ready = arr(status.conditions).find((c) => c.type === 'Ready');
  const issuing = arr(status.conditions).find((c) => c.type === 'Issuing' && c.status === 'True');
  const nb = status.notBefore && new Date(status.notBefore).getTime(), na = status.notAfter && new Date(status.notAfter).getTime(), rn = status.renewalTime && new Date(status.renewalTime).getTime();
  const left = na ? na - Date.now() : null;
  const days = left == null ? null : Math.floor(left / 86400000);
  // Short-lived certs are fine as long as renewal hasn't been missed; long-lived ones get louder near expiry.
  const tone = left == null ? '' : left <= 0 ? 'bad' : rn && Date.now() > rn ? 'warn' : !rn && days < 3 ? 'bad' : !rn && days < 14 ? 'warn' : 'ok';
  const unit = left == null || left <= 0 ? null : days >= 1 ? [days, days === 1 ? 'day left' : 'days left']
    : left >= 3600000 ? [Math.floor(left / 3600000), 'hours left'] : [Math.max(1, Math.floor(left / 60000)), 'min left'];
  const span = nb && na ? na - nb : 0;
  const pos = (t) => (span ? Math.min(100, Math.max(0, ((t - nb) / span) * 100)) : 0);
  const issuer = obj(spec.issuerRef);
  return `
    <div class="rv-hero">
      <div class="rv-countdown ${tone}">${left == null ? '<b>–</b><small>not issued</small>' : left <= 0 ? `<b>${Math.max(1, Math.ceil(-left / 86400000))}</b><small>days expired</small>` : `<b>${unit[0]}</b><small>${unit[1]}</small>`}</div>
      <div class="rv-cert-line">
        ${span ? `<div class="rv-lifetime"><span class="fill ${tone}" style="width:${pos(Date.now())}%"></span>${rn ? `<span class="mark" style="left:${pos(rn)}%" title="renewal ${new Date(rn).toLocaleString()}"></span>` : ''}</div>
          <div class="rv-lifetime-labels"><span>issued ${when(status.notBefore)}</span>${rn ? `<span>renews ${until(status.renewalTime)}</span>` : ''}<span>expires ${until(status.notAfter)}</span></div>` : '<p class="muted">No certificate issued yet.</p>'}
        <div class="rv-ready">${ready ? pill(ready.status === 'True' ? 'Ready' : 'Not ready', ready.status === 'True' ? 'ok' : 'bad') : pill('Unknown', 'warn')} ${issuing ? pill('Issuing', 'notice') : ''}
          <span class="muted">${esc(ready?.reason || '')}</span></div>
        ${ready?.message ? `<div class="rv-rollout-msg">${esc(ready.message)}</div>` : ''}
      </div>
    </div>
    ${facts([
      ['Issuer', `<a data-open="${issuer.kind === 'ClusterIssuer' ? 'cr:cert-manager.io/clusterissuers' : 'cr:cert-manager.io/issuers'}" data-name="${esc(issuer.name)}" ${issuer.kind === 'ClusterIssuer' ? 'data-ns=""' : ''}>${esc(issuer.name || '–')}</a>`, esc(issuer.kind || 'Issuer')],
      ['Secret', esc(spec.secretName || '–')],
      ['Expires', na ? esc(new Date(na).toLocaleDateString()) : '–', na ? until(status.notAfter) : ''],
      ['Renewal', rn ? esc(new Date(rn).toLocaleDateString()) : '–', rn ? until(status.renewalTime) : ''],
      status.revision ? ['Revision', String(status.revision)] : null,
    ])}
    ${section('DNS names', arr(spec.dnsNames).length || spec.commonName ? `<div class="rv-chips">${[spec.commonName, ...arr(spec.dnsNames)].filter((x, i, a) => x && a.indexOf(x) === i).map((d) => chip(d)).join('')}</div>` : '<span class="muted">none</span>')}
    ${warnings(ctx.events)}
    ${metadata(md)}`;
}

// ---------- Helm ----------
function helmSummary(_, ctx) {
  const all = ctx.history || [];
  const h = all.slice(0, 10); // newest first; older revisions rarely matter and can number in the hundreds
  const cur = h[0] || {};
  const tone = (s) => (s === 'deployed' ? 'ok' : s === 'failed' ? 'bad' : /^pending/.test(s) ? 'warn' : '');
  const older = all.length - h.length;
  return `
    ${facts([
      ['Status', pill(cur.status || '?', tone(cur.status))],
      ['Revision', String(cur.revision ?? '–'), plural(all.length, 'stored revision')],
      ['Updated', esc(ago(cur.updated)), cur.updated ? new Date(cur.updated).toLocaleString() : ''],
    ])}
    ${section('Values', h.length ? `<div class="sv-act" style="justify-content:flex-start"><select class="lg-sel" data-hv-rev>${h.map((r, i) => `<option value="${r.revision}">revision ${r.revision}${i === 0 ? ' (current)' : ''} · ${esc(r.status)}</option>`).join('')}</select>
        <button class="btn sm" data-hv-show>Show values</button></div>
      <p class="muted rv-small">Not loaded until you click: reads the user-supplied values of one revision (like <code>helm get values</code>); manifests are never read.</p>
      <div data-hv-out></div>` : '<p class="muted">No revisions found.</p>')}
    ${section(`History <span class="muted">${older ? `latest ${h.length} of ${all.length}` : h.length}</span>`, `<ol class="rv-timeline">${h.map((r, i) => `<li class="${tone(r.status)} ${i === 0 ? 'current' : ''}">
      <span class="node"></span><div class="rv-tl-main"><b>Revision ${r.revision}</b>${pill(r.status, tone(r.status))}${i === 0 ? '<span class="muted">current</span>' : ''}</div>
      <div class="muted rv-small">${new Date(r.updated).toLocaleString()} · ${ago(r.updated)} ago</div></li>`).join('')}</ol>
      ${older ? `<p class="muted rv-small">${plural(older, 'older revision')} not shown — see <code>helm history</code> in the Console tab.</p>` : ''}`)}`;
}

const RENDERERS = {
  pods: podSummary, deployments: workloadSummary('deployments'), statefulsets: workloadSummary('statefulsets'), daemonsets: workloadSummary('daemonsets'),
  services: serviceSummary, ingresses: ingressSummary, pvcs: pvcSummary, pvs: pvSummary, nodes: nodeSummary,
  'cr:cert-manager.io/certificates': certSummary, helm: helmSummary, cronjobs: cronSummary, secrets: secretSummary,
  'cr:gateway.networking.k8s.io/gateways': gatewaySummary,
  'cr:gateway.networking.k8s.io/httproutes': routeSummary, 'cr:gateway.networking.k8s.io/grpcroutes': routeSummary,
};

export const hasRichSummary = (kind) => kind in RENDERERS;

// renderSummary returns { html, bind } for the drawer. d is the /api/object response, events the warning events.
export function renderSummary(kind, d, { row, events, open, fallback }) {
  const ctx = { row, events, pods: d.pods, services: d.services, backendPods: d.backendPods, secretSizes: d.secretSizes, history: d.history, podCount: d.podCount, disks: d.disks, base: { cluster: d.cluster, namespace: d.namespace || '' } };
  const fn = RENDERERS[kind];
  const html = fn ? `<div class="rv">${fn(d.object || {}, ctx)}</div>` : fallback;
  return {
    html,
    bind(root) {
      const target = ctx.forward && { ...ctx.base, ...ctx.forward.target };
      const wire = () => {
        root.querySelectorAll('[data-open]').forEach((a) => a.onclick = (e) => {
          e.preventDefault();
          open(a.dataset.open, { cluster: d.cluster, namespace: a.dataset.ns ?? d.namespace ?? '', name: a.dataset.name });
        });
        root.querySelectorAll('[data-open-pod]').forEach((b) => b.onclick = () => open('pods', { cluster: d.cluster, namespace: b.dataset.podNs || d.namespace, name: b.dataset.openPod }));
        root.querySelectorAll('[data-open-fwd]').forEach((a) => a.onclick = (e) => { e.preventDefault(); openForward(a.dataset.openFwd, a.href); });
        root.querySelectorAll('[data-act="forward"]').forEach((b) => b.onclick = () => pickForward(b, target, ctx.forward.ports));
        root.querySelectorAll('[data-forward-port]').forEach((b) => b.onclick = () => pickForward(b, target, [{ port: +b.dataset.forwardPort }]));
        root.querySelectorAll('[data-visit-host]').forEach((b) => b.onclick = async () => {
          try {
            await api('/api/ingress-open', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ cluster: d.cluster, namespace: d.namespace, name: d.name, host: b.dataset.visitHost, path: b.dataset.visitPath }) });
          } catch (e) { b.title = e.message; b.classList.add('err'); }
        });
        root.querySelectorAll('[data-reveal]').forEach((b) => b.onclick = async () => {
          const box = b.closest('.sv-key').querySelector('.sv-val');
          const hide = () => { clearInterval(box._t); box.innerHTML = ''; box.hidden = true; b.hidden = false; };
          b.disabled = true; b.textContent = 'Reading…';
          try {
            const r = await api('/api/secret/reveal', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ cluster: d.cluster, namespace: d.namespace, name: d.name, key: b.dataset.reveal }) });
            box.innerHTML = revealHTML(r); box.hidden = false; b.hidden = true;
            const value = r.text ?? r.base64;
            box.querySelector('[data-sv-copy]').onclick = (e) => { navigator.clipboard?.writeText(value); e.target.textContent = 'Copied'; setTimeout(() => { e.target.textContent = 'Copy'; }, 1200); };
            box.querySelector('[data-sv-hide]').onclick = hide;
            let left = 60; const cd = box.querySelector('[data-countdown]');
            box._t = setInterval(() => { if (!document.contains(box) || --left <= 0) return hide(); cd.textContent = `hides in ${left}s`; }, 1000);
            cd.textContent = `hides in ${left}s`;
          } catch (e) { box.hidden = false; box.innerHTML = `<p class="rv-warnline">⚠ ${esc(e.message.replace(/^\d+ /, ''))}</p>`; }
          b.disabled = false; b.textContent = 'Reveal';
        });
        root.querySelectorAll('[data-hv-show]').forEach((b) => b.onclick = async () => {
          const out = root.querySelector('[data-hv-out]'), rev = +root.querySelector('[data-hv-rev]').value;
          b.disabled = true; b.textContent = 'Reading…';
          try {
            const r = await api('/api/helm/values', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ cluster: d.cluster, namespace: d.namespace, name: d.name, revision: rev }) });
            out.innerHTML = `<div class="sv-act"><span class="muted rv-small">${esc(r.chart)} ${esc(r.chartVersion)}${r.appVersion ? ' · app ' + esc(r.appVersion) : ''} · revision ${r.revision} · ${esc(r.status)}</span><span class="spacer"></span>
              <button class="btn sm" data-hv-copy>Copy</button><button class="btn sm" data-hv-hide>Hide</button></div>
              ${r.empty ? '<p class="muted">No user-supplied values: this revision uses the chart defaults.</p>' : `<pre class="code yaml sv-pre" style="max-height:420px">${esc(r.yaml)}</pre>`}`;
            out.querySelector('[data-hv-copy]').onclick = (e) => { navigator.clipboard?.writeText(r.yaml); e.target.textContent = 'Copied'; setTimeout(() => { e.target.textContent = 'Copy'; }, 1200); };
            out.querySelector('[data-hv-hide]').onclick = () => { out.innerHTML = ''; };
          } catch (e) { out.innerHTML = `<p class="rv-warnline">⚠ ${esc(e.message.replace(/^\d+ /, ''))}</p>`; }
          b.disabled = false; b.textContent = 'Show values';
        });
        root.querySelectorAll('[data-act="shell"]').forEach((b) => b.onclick = () => pickShell(b, ctx.base, ctx.shell));
      };
      wire();
      const hb = root.querySelector('[data-hist]');
      if (hb && !hb.dataset.mounted) {
        hb.dataset.mounted = '1';
        const WK = { deployments: 'Deployment', statefulsets: 'StatefulSet', daemonsets: 'DaemonSet', jobs: 'Job' };
        const k = hb.dataset.hist;
        mountHistory(hb, { cluster: d.cluster, kind: k, namespace: k === 'node' ? '' : d.namespace, name: k === 'workload' ? `${WK[d.kind]}/${d.name}` : d.name, allocatable: k === 'node' });
      }
      const ub = root.querySelector('[data-usage]');
      if (ub) {
        const q = new URLSearchParams({ cluster: d.cluster, kind: d.kind, namespace: d.namespace || '', name: d.name });
        const load = async () => {
          if (!document.contains(ub)) return clearInterval(timer);
          try { ub.innerHTML = ub.dataset.usage === 'node' ? nodeUsageHTML(await api('/api/usage?' + q)) : usageHTML(await api('/api/usage?' + q), ub.dataset.usage === '1'); } catch (e) { ub.innerHTML = `<p class="muted">${esc(e.message.replace(/^\d+ /, ''))}</p>`; }
          wire();
        };
        const timer = setInterval(load, 15000);
        load();
      }
      if (ctx.slots) {
        const rerender = () => {
          const el = root.querySelector('.rv');
          if (!el || !document.contains(el)) return document.removeEventListener('buddy:forwards', rerender);
          for (const [name, fn] of Object.entries(ctx.slots)) {
            const sl = root.querySelector(`[data-slot="${name}"]`);
            const html = fn();
            if (sl && sl.innerHTML !== html) sl.innerHTML = html;
          }
          wire();
        };
        document.addEventListener('buddy:forwards', rerender);
      }
    },
  };
}
