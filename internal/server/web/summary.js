// Per-kind summary components for the resource drawer. Each renderer takes the object as JSON
// (from /api/object) and returns HTML; actions (shell, port-forward, open related) are bound after.
import { esc, ago } from '/static/common.js';
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
        <div class="rv-paths">${arr(obj(r.http).paths).map((p) => `<div class="rv-path"><code>${esc(p.path || '/')}</code><span class="muted rv-ptype">${esc((p.pathType || '').replace('ImplementationSpecific', 'impl'))}</span><span class="rv-arrow"></span>${backend(p.backend)}</div>`).join('') || '<div class="rv-path muted">no paths</div>'}</div>
      </div>`).join('')}
      ${spec.defaultBackend ? `<div class="rv-route"><div class="rv-host"><span class="muted">default</span></div><div class="rv-paths"><div class="rv-path"><code>*</code><span class="rv-arrow"></span>${backend(spec.defaultBackend)}</div></div></div>` : ''}
      ${!rules.length && !spec.defaultBackend ? '<p class="muted">No rules.</p>' : ''}
    </div>`)}
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
      ['Kubelet', esc(info.kubeletVersion || '–'), esc(info.containerRuntimeVersion || '')],
      ['OS', esc(info.osImage || '–'), `${esc(info.kernelVersion || '')} · ${esc(info.architecture || '')}`],
      ['Address', esc((addrs.find((a) => a.type === 'InternalIP') || addrs[0] || {}).address || '–'), esc((addrs.find((a) => a.type === 'Hostname') || {}).address || '')],
      ['Age', esc(ago(md.creationTimestamp))],
    ])}
    ${section('Capacity <span class="muted">allocatable of total</span>', `<div class="rv-meters">
      ${meter('CPU', qty(alloc.cpu), qty(cap.cpu), cores, reserved('cpu', cores))}
      ${meter('Memory', qty(alloc.memory), qty(cap.memory), bytes, reserved('memory', bytes))}
      ${ctx.podCount != null ? meter('Pods', ctx.podCount, qty(alloc.pods), (n) => String(Math.round(n)), 'scheduled of allocatable') : ''}
      ${cap['ephemeral-storage'] ? meter('Ephemeral storage', qty(alloc['ephemeral-storage']), qty(cap['ephemeral-storage']), bytes) : ''}
    </div>`)}
    ${section('Conditions', conditionChips(status.conditions, ['MemoryPressure', 'DiskPressure', 'PIDPressure', 'NetworkUnavailable']))}
    ${section(`Taints <span class="muted">${arr(spec.taints).length}</span>`, arr(spec.taints).length ? `<div class="rv-chips">${arr(spec.taints).map((t) => `<span class="rv-chip taint ${t.effect === 'NoExecute' ? 'bad' : ''}"><i>${esc(t.key)}${t.value ? '=' + esc(t.value) : ''}</i>${esc(t.effect)}</span>`).join('')}</div>` : '<span class="muted">none</span>')}
    ${warnings(ctx.events)}
    ${metadata(md)}`;
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
  const h = ctx.history || [];
  const cur = h[0] || {};
  const tone = (s) => (s === 'deployed' ? 'ok' : s === 'failed' ? 'bad' : /^pending/.test(s) ? 'warn' : '');
  return `
    ${facts([
      ['Status', pill(cur.status || '?', tone(cur.status))],
      ['Revision', String(cur.revision ?? '–'), plural(h.length, 'stored revision')],
      ['Updated', esc(ago(cur.updated)), cur.updated ? new Date(cur.updated).toLocaleString() : ''],
    ])}
    ${section('History', `<ol class="rv-timeline">${h.map((r, i) => `<li class="${tone(r.status)} ${i === 0 ? 'current' : ''}">
      <span class="node"></span><div class="rv-tl-main"><b>Revision ${r.revision}</b>${pill(r.status, tone(r.status))}${i === 0 ? '<span class="muted">current</span>' : ''}</div>
      <div class="muted rv-small">${new Date(r.updated).toLocaleString()} · ${ago(r.updated)} ago</div></li>`).join('')}</ol>`)}
    <p class="muted rv-small">Only release metadata is read; values and manifests stay in their secrets. Use the Console tab for <code>helm get values</code>.</p>`;
}

const RENDERERS = {
  pods: podSummary, deployments: workloadSummary('deployments'), statefulsets: workloadSummary('statefulsets'), daemonsets: workloadSummary('daemonsets'),
  services: serviceSummary, ingresses: ingressSummary, pvcs: pvcSummary, pvs: pvSummary, nodes: nodeSummary,
  'cr:cert-manager.io/certificates': certSummary, helm: helmSummary,
};

export const hasRichSummary = (kind) => kind in RENDERERS;

// renderSummary returns { html, bind } for the drawer. d is the /api/object response, events the warning events.
export function renderSummary(kind, d, { row, events, open, fallback }) {
  const ctx = { row, events, pods: d.pods, services: d.services, history: d.history, podCount: d.podCount, base: { cluster: d.cluster, namespace: d.namespace || '' } };
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
        root.querySelectorAll('[data-open-pod]').forEach((b) => b.onclick = () => open('pods', { cluster: d.cluster, namespace: d.namespace, name: b.dataset.openPod }));
        root.querySelectorAll('[data-open-fwd]').forEach((a) => a.onclick = (e) => { e.preventDefault(); openForward(a.dataset.openFwd, a.href); });
        root.querySelectorAll('[data-act="forward"]').forEach((b) => b.onclick = () => pickForward(b, target, ctx.forward.ports));
        root.querySelectorAll('[data-forward-port]').forEach((b) => b.onclick = () => pickForward(b, target, [{ port: +b.dataset.forwardPort }]));
        root.querySelectorAll('[data-act="shell"]').forEach((b) => b.onclick = () => pickShell(b, ctx.base, ctx.shell));
      };
      wire();
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
