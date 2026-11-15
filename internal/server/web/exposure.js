// Exposure map: what each cluster exposes to the outside and where that traffic lands.
// Two views over one model: a zoomable "galaxy" of many clusters (canvas) and a left-to-right
// "flow" of a single cluster (SVG). Layers: entry point → ingress → service → workload → pod.
import { api, esc } from '/static/common.js';

const LAYER = { cluster: 0, controller: 1, lb: 1, nodeport: 1, ingress: 2, service: 3, workload: 4, pod: 5 };
const KLABEL = { cluster: 'Cluster', controller: 'Ingress controller', lb: 'LoadBalancer service', nodeport: 'NodePort service', ingress: 'Ingress', service: 'Service', workload: 'Workload', pod: 'Pod', source: 'Traffic source' };
const DEPTHS = [['Entry', 1], ['Ingress', 2], ['Service', 3], ['Workload', 4], ['Pods', 5]];
const GCOL = { cluster: '#dfe6ff', controller: '#5b93ff', lb: '#b08cff', nodeport: '#ffa94d', ingress: '#2dd4bf', service: '#8fa3bf', workload: '#34d399', pod: '#34d399' };
const HCOL = { ok: '#34d399', warn: '#fbbf24', bad: '#f87171', neutral: '#94a3b8' };
const RING = [0, 80, 150, 215, 280, 335];
const WORKLOAD_IDS = { Deployment: 'deployments', StatefulSet: 'statefulsets', DaemonSet: 'daemonsets', Job: 'jobs', Pod: 'pods' };
const REFRESH_MS = 30000;
const clamp = (v, a, b) => Math.max(a, Math.min(b, v));

function health(n) {
  if (n.kind === 'pod') return n.health === 'neutral' ? 'neutral' : n.ready ? 'ok' : n.health === 'bad' ? 'bad' : 'warn';
  if (n.kind === 'workload') return n.total === 0 || n.ready === 0 ? 'bad' : n.ready < n.total ? 'warn' : 'ok';
  return n.issue ? 'bad' : 'ok';
}
const colorOf = (n) => (n.kind === 'workload' || n.kind === 'pod') ? HCOL[health(n)] : GCOL[n.kind];
const cssColor = (n) => (n.kind === 'workload' || n.kind === 'pod') ? HCOL[health(n)] : `var(--k-${n.kind})`;
const label = (n) => n.kind === 'ingress' && n.hosts.length ? n.hosts[0] + (n.hosts.length > 1 ? ` +${n.hosts.length - 1}` : '') : n.name;

// routesOf lists an ingress's host/path rules with the service each lands on.
const routesOf = (ing) => ing.children.flatMap((s) => (s.routes || []).filter((r) => r.ing === ing.name).map((r) => ({ r: r.r, svc: s.name })));
const routeCount = (ing) => routesOf(ing).length;
const nk = (n) => n.ci + '|' + n.key; // node keys repeat across clusters

// wrap breaks a long name into lines at - . / _ boundaries (hard-splitting only when a part is too
// long); whatever doesn't fit in maxLines ends with an ellipsis.
function wrap(text, max, maxLines) {
  if (text.length <= max) return [text];
  const parts = text.split(/(?<=[-./_:])/).flatMap((p) => p.length > max ? p.match(new RegExp(`.{1,${max}}`, 'g')) : [p]);
  const lines = [''];
  for (const p of parts) {
    if ((lines.at(-1) + p).length > max && lines.at(-1)) lines.push('');
    lines[lines.length - 1] += p;
  }
  if (lines.length > maxLines) { lines.length = maxLines; lines[maxLines - 1] = lines[maxLines - 1].slice(0, max - 1) + '…'; }
  return lines;
}

// ---------- model ----------
function buildCluster(c, ci) {
  const nodes = new Map(), edges = [];
  const add = (key, o) => { let n = nodes.get(key); if (!n) { n = { key, ci, children: [], parents: [], ...o }; nodes.set(key, n); } return n; };
  const link = (a, b) => { if (a.children.includes(b)) return; a.children.push(b); b.parents.push(a); edges.push({ a, b }); };
  const root = add('root', { kind: 'cluster', name: c.cluster, status: c.status, error: c.error, pods: c.pods });
  const wl = (ns, w) => {
    const key = `w:${ns}/${w.kind}/${w.name}`, fresh = !nodes.has(key);
    const n = add(key, { kind: 'workload', name: w.name, ns, wkind: w.kind, total: w.total, ready: w.ready });
    if (fresh) for (const p of w.pods || []) link(n, add(`p:${ns}/${p.name}`, { kind: 'pod', name: p.name, ns, status: p.status, health: p.health, ready: p.ready, restarts: p.restarts, node: p.node, ip: p.ip }));
    return n;
  };
  const sum = (ws, k) => ws.reduce((s, w) => s + w[k], 0);
  for (const e of c.entries) {
    const pseudo = e.kind === 'class';
    const en = add(`e:${e.kind}:${e.namespace}/${e.name}`, {
      kind: pseudo ? 'controller' : e.kind, pseudo, name: pseudo ? `class ${e.name}` : e.name, ns: e.namespace, type: e.type,
      ports: e.ports, ip: e.addresses, internal: e.internal, ctrl: e.controller, total: sum(e.workloads, 'total'), ready: sum(e.workloads, 'ready'),
    });
    if (pseudo) en.note = 'Controller not exposed by a LoadBalancer or NodePort service in this cluster';
    else if (!e.workloads.length) { en.issue = 'No pods behind this service'; en.dangling = true; }
    else if (e.type === 'LoadBalancer' && !e.addresses.length) en.issue = 'LoadBalancer has no external address yet';
    link(root, en);
    for (const w of e.workloads) link(en, wl(e.namespace, w));
    for (const i of e.ingresses) {
      const ig = add(`i:${i.namespace}/${i.name}`, { kind: 'ingress', name: i.name, ns: i.namespace, cls: i.class, tls: i.tls, hosts: i.hosts });
      link(en, ig);
      for (const s of i.services) {
        const sv = add(`s:${s.namespace}/${s.name}`, { kind: 'service', name: s.name, ns: s.namespace, type: s.type, ports: s.ports, routes: [] });
        sv.routes.push(...s.routes.map((r) => ({ r, ing: i.name })));
        if (s.missing) { sv.issue = 'Backend service not found'; sv.dangling = true; }
        else if (!s.workloads.length) { sv.issue = 'No pods match this service selector'; sv.dangling = true; }
        link(ig, sv);
        for (const w of s.workloads) link(sv, wl(s.namespace, w));
      }
    }
  }
  const list = [...nodes.values()];
  const count = (k) => list.filter((n) => n.kind === k).length;
  const stats = {
    entries: list.filter((n) => LAYER[n.kind] === 1).length, ctrl: list.filter((n) => n.kind === 'controller' && !n.pseudo).length,
    lb: count('lb'), np: count('nodeport'), ing: count('ingress'), svc: count('service'), wl: count('workload'), pods: count('pod'),
    issues: list.filter((n) => n.kind !== 'pod' && n.kind !== 'cluster' && health(n) !== 'ok').length,
    hosts: new Set(list.filter((n) => n.kind === 'ingress').flatMap((n) => n.hosts)).size,
  };
  return { name: c.cluster, family: 'all', status: c.status, root, nodes: list, edges, stats }; // one field; never grouped by name
}

function layoutGalaxy(clusters) {
  for (const c of clusters) {
    const leaves = (n) => { n.tkids = n.children.filter((k) => k.parents[0] === n); n.leaves = n.tkids.length ? n.tkids.reduce((s, k) => s + leaves(k), 0) : 1; return n.leaves; };
    leaves(c.root);
    c.f = Math.max(1, Math.sqrt(c.root.leaves / 40));
    c.R = RING[5] * c.f + 40;
    const place = (n, a0, a1) => {
      n.ang = (a0 + a1) / 2; n.span = a1 - a0;
      const r = RING[LAYER[n.kind]] * c.f;
      n.lx = Math.cos(n.ang) * r; n.ly = Math.sin(n.ang) * r;
      let a = a0;
      for (const k of n.tkids) { const s = (a1 - a0) * k.leaves / n.leaves; place(k, a, a + s); a += s; }
    };
    place(c.root, -Math.PI / 2, Math.PI * 1.5);
  }
  const pack = (items, gap) => {
    const placed = [];
    for (const it of items) {
      if (!placed.length) { it.x = 0; it.y = 0; placed.push(it); continue; }
      for (let i = 1; ; i++) {
        const a = i * 0.37, d = i * 6, x = Math.cos(a) * d, y = Math.sin(a) * d;
        if (placed.every((p) => Math.hypot(p.x - x, p.y - y) >= p.r + it.r + gap)) { it.x = x; it.y = y; placed.push(it); break; }
      }
    }
    return placed.length ? Math.max(...placed.map((p) => Math.hypot(p.x, p.y) + p.r)) : 0;
  };
  const families = [...new Set(clusters.map((c) => c.family))].map((f) => ({ name: f, cl: clusters.filter((c) => c.family === f).sort((a, b) => b.R - a.R) }));
  for (const fam of families) { fam.items = fam.cl.map((c) => ({ c, r: c.R })); fam.r = pack(fam.items, 230); }
  families.sort((a, b) => b.r - a.r);
  pack(families, 700);
  for (const fam of families) for (const it of fam.items) { it.c.x = fam.x + it.x; it.c.y = fam.y + it.y; }
  const base = { cluster: 16, controller: 9, lb: 6.5, nodeport: 6.5, ingress: 5.5, service: 4.2, workload: 4, pod: 2.6 };
  for (const c of clusters) for (const n of c.nodes) {
    n.x = c.x + n.lx; n.y = c.y + n.ly;
    n.wr = base[n.kind] * Math.sqrt(c.f) + (n.kind === 'workload' ? Math.min(4, Math.sqrt(n.total)) : 0);
  }
  return families;
}

// ---------- page ----------
export function mountExposure(root, { clusters: scoped, params, onState, openResource }) {
  const S = {
    view: params.get('v') === 'flow' ? 'flow' : 'galaxy', depth: clamp(+params.get('d') || 4, 1, 5), lanes: new Set((params.get('open') || '').split(',').filter(Boolean)), lanesInit: params.has('open'),
    sel: null, related: null, collapsed: new Set(), expanded: new Set(), data: [], clusters: [], families: [], loaded: false,
  };
  let destroyed = false, timer = null;
  const cleanups = [];
  const on = (target, ev, fn, opts) => { target.addEventListener(ev, fn, opts); cleanups.push(() => target.removeEventListener(ev, fn, opts)); };

  root.innerHTML = `
    <div class="xp-bar">
      <div class="xp-seg" data-role="views"><button data-v="galaxy">Galaxy</button><button data-v="flow">Flow</button></div>
      <span class="xp-label">Depth</span>
      <div class="xp-seg" data-role="depth">${DEPTHS.map(([l, d]) => `<button data-d="${d}" title="Show down to ${l.toLowerCase()} (key ${d})">${l}</button>`).join('')}</div>
      <input class="input search" data-role="search" placeholder="Find host, service, pod, IP…  ⏎" autocomplete="off" spellcheck="false">
      <span class="spacer" style="flex:1"></span><span class="muted" data-role="summary"></span>
    </div>
    <div class="xp-stage">
      <div class="xp-views">
        <div class="xp-view xp-galaxy" data-role="galaxy">
          <canvas></canvas>
          <div class="xp-crumb" data-role="crumb"></div>
          <div class="xp-zoom"><button data-z="in" title="Zoom in">+</button><button data-z="out" title="Zoom out">−</button><button data-z="fit" title="Fit all (F)">⤢</button></div>
          <div class="xp-legend" data-role="glegend"></div>
          <div class="xp-note" data-role="gnote">Loading exposure…</div>
        </div>
        <div class="xp-view xp-flow" data-role="flow">
          <svg><g></g></svg>
          <div class="xp-zoom"><button data-fz="in">+</button><button data-fz="out">−</button><button data-fz="fit">⤢</button></div>
          <div class="xp-legend" data-role="flegend"></div>
          <div class="xp-note" data-role="fnote" hidden></div>
        </div>
      </div>
      <aside class="xp-panel" data-role="panel"></aside>
    </div>`;
  const $ = (r) => root.querySelector(`[data-role=${r}]`);
  const tip = document.createElement('div');
  tip.className = 'xp-tip';
  document.body.appendChild(tip);
  cleanups.push(() => tip.remove());

  const saveState = () => onState?.({ v: S.view === 'flow' ? 'flow' : '', d: S.depth === 4 ? '' : String(S.depth), open: S.view === 'flow' ? [...S.lanes].join(',') : '' });
  const allNodes = () => S.clusters.flatMap((c) => c.nodes);

  // A pod shows when the depth reaches pods, or its workload is expanded or selected.
  function visible(n) {
    if (n.kind !== 'pod') return LAYER[n.kind] <= S.depth;
    if (S.depth >= 5) return true;
    const w = n.parents[0];
    return S.expanded.has(nk(w)) || S.sel === w || S.sel === n;
  }

  function relatedSet(n) {
    const s = new Set([n]);
    const up = (x) => x.parents.forEach((p) => { if (!s.has(p)) { s.add(p); up(p); } });
    const down = (x) => x.children.forEach((k) => { if (!s.has(k)) { s.add(k); down(k); } });
    up(n); down(n);
    return s;
  }

  function select(n, opts = {}) {
    S.sel = n; S.related = n ? relatedSet(n) : null;
    if (n && n.kind === 'pod' && S.depth < 4) setDepth(4, false);
    else if (n && n.kind !== 'pod' && LAYER[n.kind] > S.depth) setDepth(LAYER[n.kind], false);
    renderPanel();
    if (S.view === 'flow') {
      if (n && !S.lanes.has(S.clusters[n.ci].name)) { S.lanes.add(S.clusters[n.ci].name); saveState(); }
      buildFlow(false);
      if (opts.reveal && n) flowReveal(n);
    } else {
      if (opts.fly && n) flyTo(n);
      gDirty();
    }
  }

  // ---------- data ----------
  async function load() {
    let data;
    try {
      data = await api('/api/exposure?' + new URLSearchParams({ cluster: scoped.join(',') }));
    } catch (e) {
      if (!destroyed) $('gnote').textContent = 'Could not load exposure: ' + e.message;
      return;
    }
    if (destroyed) return;
    const selKey = S.sel && [S.clusters[S.sel.ci]?.name, S.sel.key];
    S.data = data.filter((c) => !scoped.length || scoped.includes(c.cluster)).sort((a, b) => a.cluster.localeCompare(b.cluster));
    S.clusters = S.data.map(buildCluster);
    S.families = layoutGalaxy(S.clusters);
    const tot = S.clusters.reduce((a, c) => { for (const k in c.stats) a[k] = (a[k] || 0) + c.stats[k]; return a; }, {});
    const waiting = S.clusters.filter((c) => c.status !== 'ok').length;
    $('summary').textContent = S.clusters.length ? `${S.clusters.length} clusters · ${tot.entries} entry points · ${tot.ing} ingresses · ${tot.hosts} hosts · ${tot.pods} exposed pods${waiting ? ` · ${waiting} not synced yet` : ''}` : '';
    if (!S.lanesInit) { // first load: open every lane for a small selection, else just the first that exposes something
      S.lanesInit = true;
      const exposing = S.clusters.filter((c) => c.stats.entries);
      (S.clusters.length <= 3 ? exposing : exposing.slice(0, 1)).forEach((c) => S.lanes.add(c.name));
    }
    $('gnote').hidden = !!S.clusters.length;
    $('gnote').textContent = 'No connected clusters in this selection yet.';
    if (selKey) {
      const ci = S.clusters.findIndex((c) => c.name === selKey[0]);
      const n = ci >= 0 && S.clusters[ci].nodes.find((x) => x.key === selKey[1]);
      S.sel = n || null; S.related = n ? relatedSet(n) : null;
      renderPanel();
    }
    if (!S.loaded) { S.loaded = true; resize(); vw = fitAllView(); }
    if (S.view === 'flow') buildFlow(!S.flowBuilt);
    gDirty();
  }

  // ---------- galaxy ----------
  const gEl = $('galaxy'), cv = gEl.querySelector('canvas'), ctx = cv.getContext('2d');
  let W = 0, H = 0, DPR = 1, vw = { k: .05, x: 0, y: 0 }, anim = null, dirty = true;
  const stars = Array.from({ length: 380 }, () => ({ x: Math.random(), y: Math.random(), r: Math.random() * 1.1 + .2, a: Math.random() * .6 + .15, p: Math.random() * .04 + .005 }));
  const gDirty = () => { dirty = true; };
  function resize() {
    const r = cv.getBoundingClientRect();
    if (!r.width) return;
    W = r.width; H = r.height; DPR = devicePixelRatio || 1; cv.width = W * DPR; cv.height = H * DPR; gDirty();
  }
  const toS = (x, y) => [x * vw.k + vw.x, y * vw.k + vw.y];
  const fitBox = (x0, y0, x1, y1, pad = 60) => { const k = Math.min((W - pad * 2) / (x1 - x0 || 1), (H - pad * 2) / (y1 - y0 || 1)); return { k, x: W / 2 - (x0 + x1) / 2 * k, y: H / 2 - (y0 + y1) / 2 * k }; };
  function fitAllView() {
    if (!S.clusters.length || !W) return { k: .05, x: W / 2, y: H / 2 };
    const b = S.clusters.map((c) => [c.x - c.R, c.y - c.R, c.x + c.R, c.y + c.R]);
    const v = fitBox(Math.min(...b.map((a) => a[0])), Math.min(...b.map((a) => a[1])), Math.max(...b.map((a) => a[2])), Math.max(...b.map((a) => a[3])));
    return { ...v, k: Math.min(v.k, 1.2) };
  }
  function animateTo(t, ms = 650) {
    const s = { ...vw }, t0 = performance.now();
    anim = (now) => {
      let p = Math.min(1, (now - t0) / ms);
      p = p < .5 ? 4 * p * p * p : 1 - Math.pow(-2 * p + 2, 3) / 2;
      const k = Math.exp(Math.log(s.k) + (Math.log(t.k) - Math.log(s.k)) * p);
      const sc = [(W / 2 - s.x) / s.k, (H / 2 - s.y) / s.k], tc = [(W / 2 - t.x) / t.k, (H / 2 - t.y) / t.k];
      const cx = sc[0] + (tc[0] - sc[0]) * p, cy = sc[1] + (tc[1] - sc[1]) * p;
      vw = { k, x: W / 2 - cx * k, y: H / 2 - cy * k }; gDirty();
      if (p >= 1) anim = null;
    };
  }
  function flyTo(n) {
    const c = S.clusters[n.ci];
    if (n.kind === 'cluster') { animateTo(fitBox(c.x - c.R, c.y - c.R, c.x + c.R, c.y + c.R, 40)); return; }
    const xs = [...(S.related || [n])].filter((m) => m.kind !== 'cluster' && visible(m));
    const pad = 40;
    let b = fitBox(Math.min(...xs.map((m) => m.x)) - pad, Math.min(...xs.map((m) => m.y)) - pad, Math.max(...xs.map((m) => m.x)) + pad, Math.max(...xs.map((m) => m.y)) + pad, 80);
    if (b.k > 4) b = { k: 4, x: W / 2 - n.x * 4, y: H / 2 - n.y * 4 };
    animateTo(b);
  }

  function drawGalaxy() {
    ctx.setTransform(DPR, 0, 0, DPR, 0, 0);
    ctx.fillStyle = '#060913'; ctx.fillRect(0, 0, W, H);
    for (const s of stars) {
      const x = ((s.x * W + vw.x * s.p) % W + W) % W, y = ((s.y * H + vw.y * s.p) % H + H) % H;
      ctx.globalAlpha = s.a; ctx.fillStyle = '#b8c4ff'; ctx.fillRect(x, y, s.r, s.r);
    }
    ctx.globalAlpha = 1;
    const k = vw.k, rel = S.related;
    const boxes = []; // label collision avoidance: busier galaxies claim space first
    const fits = (x, y, w, h) => { if (boxes.some((b) => x < b[0] + b[2] && x + w > b[0] && y < b[1] + b[3] && y + h > b[1])) return false; boxes.push([x, y, w, h]); return true; };
    const famA = clamp((0.16 - k) / 0.06, 0, 1);
    if (famA > 0 && S.families.length > 1) {
      ctx.font = '600 13px -apple-system,sans-serif'; ctx.textAlign = 'center';
      for (const f of S.families) { const [x, y] = toS(f.x, f.y - f.r - 90); ctx.fillStyle = `rgba(160,180,255,${.55 * famA})`; ctx.fillText(`${f.name.toUpperCase()} · ${f.cl.length} cluster${f.cl.length > 1 ? 's' : ''}`, x, y); }
    }
    const order = [...S.clusters].sort((a, b) => b.stats.entries + b.stats.ing - a.stats.entries - a.stats.ing);
    for (const c of order) {
      const [cx, cy] = toS(c.x, c.y), sr = c.R * k;
      if (cx + sr < -50 || cx - sr > W + 50 || cy + sr < -50 || cy - sr > H + 50) continue;
      const dimC = rel && !rel.has(c.root) ? .25 : 1;
      const glowA = clamp(1 - (sr - 60) / 260, .12, 1) * dimC * (c.status === 'ok' ? 1 : .35);
      const g = ctx.createRadialGradient(cx, cy, 0, cx, cy, sr * 1.05);
      g.addColorStop(0, `rgba(140,165,255,${.42 * glowA})`); g.addColorStop(.35, `rgba(110,90,220,${.16 * glowA})`); g.addColorStop(1, 'rgba(60,40,140,0)');
      ctx.fillStyle = g; ctx.beginPath(); ctx.arc(cx, cy, sr * 1.05, 0, Math.PI * 2); ctx.fill();
      if (sr > 70) {
        ctx.lineWidth = 1;
        for (let L = 1; L <= Math.min(S.depth, 5); L++) { ctx.strokeStyle = `rgba(150,170,255,${.07 * dimC})`; ctx.beginPath(); ctx.arc(cx, cy, RING[L] * c.f * k, 0, Math.PI * 2); ctx.stroke(); }
      }
      const eA = clamp((sr - 50) / 150, 0, 1);
      if (eA > 0) {
        for (const e of c.edges) {
          if (!visible(e.b) || !visible(e.a) && e.a.kind !== 'cluster') continue;
          const hot = rel && rel.has(e.a) && rel.has(e.b), tree = e.b.parents[0] === e.a;
          const a = rel ? (hot ? .9 : .05) : (tree ? .28 : .16);
          const [x1, y1] = toS(e.a.x, e.a.y), [x2, y2] = toS(e.b.x, e.b.y);
          const dang = e.b.dangling, da = rel ? (hot ? 1 : .12) : .75;
          ctx.strokeStyle = dang ? `rgba(248,113,113,${da * eA})` : hot ? `rgba(160,190,255,${a * eA})` : `rgba(130,150,220,${a * eA})`;
          ctx.lineWidth = hot || dang ? 1.6 : 1;
          ctx.setLineDash(tree ? [] : [3, 4]);
          ctx.beginPath(); ctx.moveTo(x1, y1);
          if (e.a.kind === 'cluster') ctx.lineTo(x2, y2);
          else { const mx = (x1 + x2) / 2, my = (y1 + y2) / 2; ctx.quadraticCurveTo(mx + (cx - mx) * .12, my + (cy - my) * .12, x2, y2); }
          ctx.stroke();
        }
        // Dangling services: a red stub reaching out toward the workloads that aren't there.
        if (S.depth >= 4) for (const n of c.nodes) {
          if (!n.dangling || !visible(n)) continue;
          const lit = !rel || rel.has(n), r0 = RING[LAYER[n.kind]] * c.f, r1 = r0 + (RING[4] * c.f - r0) * .7;
          const [x1, y1] = toS(n.x, n.y), [x2, y2] = toS(c.x + Math.cos(n.ang) * r1, c.y + Math.sin(n.ang) * r1);
          ctx.strokeStyle = `rgba(248,113,113,${(lit ? .85 : .12) * eA})`; ctx.lineWidth = 1.6; ctx.setLineDash([4, 3]);
          ctx.beginPath(); ctx.moveTo(x1, y1); ctx.lineTo(x2, y2); ctx.stroke();
          ctx.setLineDash([]);
          const s = clamp(3 * Math.sqrt(k * c.f), 1.5, 5);
          ctx.beginPath(); ctx.moveTo(x2 - s, y2 - s); ctx.lineTo(x2 + s, y2 + s); ctx.moveTo(x2 + s, y2 - s); ctx.lineTo(x2 - s, y2 + s); ctx.stroke();
        }
        ctx.setLineDash([]);
      }
      for (const n of c.nodes) {
        if (!visible(n)) continue;
        const [x, y] = toS(n.x, n.y), r = clamp(n.wr * k, n.kind === 'cluster' ? 2.2 : .9, 26);
        const lit = !rel || rel.has(n);
        ctx.globalAlpha = (lit ? 1 : .14) * (n.kind === 'cluster' ? 1 : clamp(.35 + sr / 200, .35, 1));
        const col = colorOf(n);
        if (r > 3) { ctx.shadowColor = col; ctx.shadowBlur = Math.min(14, r * 1.6); }
        ctx.fillStyle = col; ctx.beginPath(); ctx.arc(x, y, r, 0, Math.PI * 2);
        if (n.pseudo) { ctx.lineWidth = 2; ctx.strokeStyle = col; ctx.fillStyle = 'rgba(6,9,19,.9)'; ctx.fill(); ctx.stroke(); } else ctx.fill();
        ctx.shadowBlur = 0;
        if (r > 5 && n.kind !== 'workload' && n.kind !== 'pod' && n.issue) { ctx.strokeStyle = HCOL.bad; ctx.lineWidth = 2; ctx.beginPath(); ctx.arc(x, y, r + 3, 0, Math.PI * 2); ctx.stroke(); }
        if (n.kind === 'workload' && r > 4 && n.total > 1) {
          ctx.strokeStyle = 'rgba(6,9,19,.9)'; ctx.lineWidth = 1.5;
          for (let i = 0; i < n.total; i++) { const a0 = -Math.PI / 2 + i * 2 * Math.PI / n.total; ctx.beginPath(); ctx.moveTo(x, y); ctx.lineTo(x + Math.cos(a0) * r, y + Math.sin(a0) * r); ctx.stroke(); }
        }
        if (S.sel === n) { ctx.strokeStyle = '#fff'; ctx.lineWidth = 2; ctx.beginPath(); ctx.arc(x, y, r + 4, 0, Math.PI * 2); ctx.stroke(); }
        if (!lit) { ctx.globalAlpha = 1; continue; }
        if (n.kind === 'cluster') {
          if (sr <= 8) { ctx.globalAlpha = 1; continue; }
          ctx.textAlign = 'center'; ctx.font = `600 ${clamp(sr / 14, 11, 15)}px -apple-system,sans-serif`; ctx.fillStyle = '#eef2ff';
          const ly = sr < 70 ? cy + sr * .55 + 12 : cy + r + 16;
          const nm = c.name.replace(/-cluster$/, ''), tw = ctx.measureText(nm).width;
          const issues = c.stats.issues;
          const full = sr > 22 && fits(cx - Math.max(tw, 150) / 2, ly - 12, Math.max(tw, 150), issues ? 46 : 32);
          if (!full && !fits(cx - tw / 2, ly - 12, tw, 16)) { ctx.globalAlpha = 1; continue; }
          ctx.fillText(nm, cx, ly);
          if (full) {
            ctx.font = '11px -apple-system,sans-serif'; ctx.fillStyle = '#93a3d6';
            ctx.fillText(c.status === 'ok' ? `${c.stats.entries} entry · ${c.stats.ing} ing · ${c.stats.pods} pods` : c.status === 'error' ? 'unreachable' : 'syncing…', cx, ly + 14);
            if (issues) { ctx.fillStyle = '#f87171'; ctx.fillText(`● ${issues} issue${issues > 1 ? 's' : ''}`, cx, ly + 28); }
          }
        } else if (S.sel === n || (sr > 120 && n.span * RING[LAYER[n.kind]] * c.f * k >= 12.5) || (n.kind === 'pod' && r >= 2.5 && n.span * RING[5] * c.f * k >= 11)) {
          const right = Math.cos(n.ang) >= 0;
          ctx.textAlign = right ? 'left' : 'right';
          ctx.font = (LAYER[n.kind] === 1 ? '600 ' : '') + '11px -apple-system,sans-serif';
          ctx.fillStyle = LAYER[n.kind] === 1 ? '#e6ebff' : '#b9c3e0';
          let t = label(n);
          if (n.kind === 'workload') t += `  ${n.ready}/${n.total}`;
          if (n.kind === 'pod' && n.status !== 'Running') t += `  ${n.status}`;
          const lines = wrap(t, 30, 2);
          const tw = Math.max(...lines.map((l) => ctx.measureText(l).width)), tx = x + (right ? r + 5 : -r - 5);
          const ty = y + 3.5 - (lines.length - 1) * 6.5;
          if (S.sel === n || fits(right ? tx : tx - tw, ty - 9.5, tw, lines.length * 13)) lines.forEach((l, i) => ctx.fillText(l, tx, ty + i * 13));
        }
        ctx.globalAlpha = 1;
      }
    }
    const focus = S.sel ? S.clusters[S.sel.ci] : nearestCluster();
    $('crumb').innerHTML = `<a data-c="all">All clusters</a>${focus && vw.k > .12 ? ` › <a data-c="${S.clusters.indexOf(focus)}">${esc(focus.name)}</a>` : ''}${S.sel && S.sel.kind !== 'cluster' ? ` › ${esc(S.sel.name)}` : ''}<span style="color:#6f7ca8"> · ${Math.round(vw.k * 100)}%</span>`;
  }
  function nearestCluster() {
    const cx = (W / 2 - vw.x) / vw.k, cy = (H / 2 - vw.y) / vw.k;
    let best = null, bd = Infinity;
    for (const c of S.clusters) { const d = Math.hypot(c.x - cx, c.y - cy) - c.R; if (d < bd) { bd = d; best = c; } }
    return bd < 0 ? best : null;
  }
  function loop(now) {
    if (destroyed) return;
    if (anim) anim(now);
    if (dirty && S.view === 'galaxy' && W) { dirty = false; drawGalaxy(); }
    requestAnimationFrame(loop);
  }
  function hit(mx, my) {
    let best = null, bd = Infinity;
    for (const n of allNodes()) {
      if (!visible(n)) continue;
      const [x, y] = toS(n.x, n.y), r = Math.max(n.wr * vw.k, n.kind === 'cluster' ? 10 : 3) + 4;
      const d = Math.hypot(x - mx, y - my);
      if (d < r && d < bd) { bd = d; best = n; }
    }
    if (!best && vw.k < .25) for (const c of S.clusters) { const [x, y] = toS(c.x, c.y); if (Math.hypot(x - mx, y - my) < c.R * vw.k * .8) return c.root; }
    return best;
  }
  const zoomAt = (mx, my, f) => { const k = clamp(vw.k * f, .01, 8); const wx = (mx - vw.x) / vw.k, wy = (my - vw.y) / vw.k; vw = { k, x: mx - wx * k, y: my - wy * k }; gDirty(); };
  // Mouse wheels zoom; trackpads pan with two fingers and zoom with a pinch (ctrl+wheel).
  function wheel(e, zoomFn, panFn) {
    e.preventDefault();
    const r = e.currentTarget.getBoundingClientRect();
    const mouseWheel = e.deltaMode === 1 || (e.deltaX === 0 && Math.abs(e.deltaY) >= 40 && Number.isInteger(e.deltaY));
    if (e.ctrlKey || mouseWheel) zoomFn(e.clientX - r.left, e.clientY - r.top, Math.exp(-e.deltaY * (e.ctrlKey ? .012 : .0022)));
    else panFn(-e.deltaX, -e.deltaY);
  }
  on(gEl, 'wheel', (e) => { anim = null; wheel(e, zoomAt, (dx, dy) => { vw.x += dx; vw.y += dy; gDirty(); }); }, { passive: false });
  let gdrag = null;
  on(cv, 'pointerdown', (e) => { anim = null; gdrag = { x: e.clientX, y: e.clientY, vx: vw.x, vy: vw.y, moved: false }; cv.setPointerCapture(e.pointerId); });
  on(cv, 'pointermove', (e) => {
    const r = cv.getBoundingClientRect();
    if (gdrag) {
      const dx = e.clientX - gdrag.x, dy = e.clientY - gdrag.y;
      if (Math.abs(dx) + Math.abs(dy) > 4) { gdrag.moved = true; gEl.classList.add('drag'); }
      if (gdrag.moved) { vw.x = gdrag.vx + dx; vw.y = gdrag.vy + dy; gDirty(); hideTip(); return; }
    }
    const n = hit(e.clientX - r.left, e.clientY - r.top);
    cv.style.cursor = n ? 'pointer' : '';
    n ? showTip(n, e.clientX, e.clientY) : hideTip();
  });
  on(cv, 'pointerup', (e) => {
    gEl.classList.remove('drag');
    if (gdrag && !gdrag.moved) {
      const r = cv.getBoundingClientRect(), n = hit(e.clientX - r.left, e.clientY - r.top);
      select(n, { fly: !!n && n.kind === 'cluster' && vw.k < .25 });
    }
    gdrag = null;
  });
  on(cv, 'dblclick', (e) => { const r = cv.getBoundingClientRect(), n = hit(e.clientX - r.left, e.clientY - r.top); if (n) select(n, { fly: true }); else zoomAt(e.clientX - r.left, e.clientY - r.top, 2); });
  on(cv, 'pointerleave', () => hideTip());
  gEl.querySelector('[data-z=in]').onclick = () => zoomAt(W / 2, H / 2, 1.6);
  gEl.querySelector('[data-z=out]').onclick = () => zoomAt(W / 2, H / 2, 1 / 1.6);
  gEl.querySelector('[data-z=fit]').onclick = () => animateTo(fitAllView());
  $('crumb').onclick = (e) => {
    const c = e.target.dataset?.c;
    if (c === 'all') { select(null); animateTo(fitAllView()); } else if (c != null) select(S.clusters[+c].root, { fly: true });
  };

  // ---------- tooltip ----------
  function summary(n) {
    switch (n.kind) {
      case 'cluster': {
        const s = S.clusters[n.ci].stats;
        if (n.status !== 'ok') return n.status === 'error' ? `unreachable${n.error ? ': ' + esc(n.error.slice(0, 120)) : ''}` : 'still syncing';
        return `${s.ctrl} controller · ${s.lb} LB · ${s.np} NodePort<br>${s.ing} ingresses · ${s.hosts} hosts · ${s.svc} services<br>${s.wl} workloads · ${s.pods} exposed pods`;
      }
      case 'controller': return n.pseudo ? `${esc(n.ctrl || 'unknown controller')}<br>${n.children.length} ingresses` : `${esc(n.ctrl)} · ${esc(n.ip.join(', ') || 'no address')}<br>${n.children.filter((k) => k.kind === 'ingress').length} ingresses · pods ${n.ready}/${n.total}`;
      case 'lb': case 'nodeport': return `${esc(n.ns)} · ${esc(n.ip.join(', ') || n.type)}${n.internal ? ' (internal)' : ''}<br>${esc(n.ports.join(', '))}`;
      case 'ingress': { const rs = routesOf(n); return `${esc(n.ns)} · class ${esc(n.cls || 'default')}${n.tls ? ' · TLS' : ''}<br>${rs.slice(0, 8).map((r) => `${esc(r.r)} → ${esc(r.svc)}`).join('<br>')}${rs.length > 8 ? `<br>+${rs.length - 8} more` : ''}`; }
      case 'service': return `${esc(n.ns)} · ${esc(n.type)} · ${esc(n.ports.join(', '))}`;
      case 'workload': return `${esc(n.wkind)} · ${esc(n.ns)}<br>pods ${n.ready}/${n.total} ready`;
      case 'pod': return `${esc(n.status)}${n.restarts ? ` · ${n.restarts} restarts` : ''}<br>${esc(n.node || '')} ${esc(n.ip || '')}`;
      case 'source': return esc(n.sub);
    }
    return '';
  }
  function showTip(n, x, y) {
    tip.innerHTML = `<b>${esc(n.name)}</b><span class="m">${KLABEL[n.kind]}</span><br>${summary(n)}${n.issue ? `<br><span style="color:#f87171">⚠ ${esc(n.issue)}</span>` : ''}`;
    tip.style.display = 'block';
    const r = tip.getBoundingClientRect();
    tip.style.left = Math.min(x + 14, innerWidth - r.width - 8) + 'px';
    tip.style.top = Math.min(y + 14, innerHeight - r.height - 8) + 'px';
  }
  const hideTip = () => { tip.style.display = 'none'; };

  // ---------- side panel ----------
  const dot = (n) => `<i style="background:${cssColor(n)}"></i>`;
  const li = (n, extra = '') => `<div class="xp-li" data-ci="${n.ci}" data-key="${esc(n.key)}">${dot(n)}<span>${esc(label(n))}</span><em>${extra}</em></div>`;
  const healthPill = (h) => `<span class="pill ${h}">${({ ok: 'healthy', warn: 'degraded', bad: 'down', neutral: 'done' })[h]}</span>`;
  function pathsTo(n) {
    const out = [];
    const walk = (x, chain) => { if (out.length > 12) return; if (x.kind === 'cluster') { out.push(chain); return; } x.parents.forEach((p) => walk(p, [x, ...chain])); };
    walk(n, []);
    return out;
  }
  function resourceTarget(n) {
    const c = S.clusters[n.ci].name;
    if (n.kind === 'pod') return ['pods', { cluster: c, namespace: n.ns, name: n.name }];
    if (n.kind === 'workload') return WORKLOAD_IDS[n.wkind] ? [WORKLOAD_IDS[n.wkind], { cluster: c, namespace: n.ns, name: n.name }] : null;
    if (n.kind === 'ingress') return ['ingresses', { cluster: c, namespace: n.ns, name: n.name }];
    if ((n.kind === 'service' && !n.issue?.includes('not found')) || n.kind === 'lb' || n.kind === 'nodeport' || (n.kind === 'controller' && !n.pseudo)) return ['services', { cluster: c, namespace: n.ns, name: n.name }];
    return null;
  }
  function renderPanel() {
    const p = $('panel'), n = S.sel;
    const was = p.classList.contains('on');
    if (!n) { p.classList.remove('on'); if (was) requestAnimationFrame(resize); return; }
    const c = S.clusters[n.ci];
    let dl = `<dt>Cluster</dt><dd>${esc(c.name)}</dd>`;
    if (n.ns) dl += `<dt>Namespace</dt><dd>${esc(n.ns)}</dd>`;
    if (n.type && !['workload', 'pod'].includes(n.kind)) dl += `<dt>Type</dt><dd>${esc(n.type)}${n.internal ? ' · internal' : ''}</dd>`;
    if (n.ip?.length && n.kind !== 'pod') dl += `<dt>Address</dt><dd class="mono">${n.ip.map(esc).join('<br>')}</dd>`;
    if (n.ports?.length) dl += `<dt>Ports</dt><dd class="mono">${n.ports.map(esc).join('<br>')}</dd>`;
    if (n.ctrl) dl += `<dt>Controller</dt><dd>${esc(n.ctrl)}</dd>`;
    if (n.kind === 'ingress') dl += `<dt>Class</dt><dd>${esc(n.cls || 'default')}</dd><dt>TLS</dt><dd>${n.tls ? 'yes' : 'no'}</dd>`;
    if (n.total != null && !n.pseudo) dl += `<dt>Pods</dt><dd>${n.ready}/${n.total} ready ${healthPill(n.total === 0 ? 'bad' : n.ready < n.total ? 'warn' : 'ok')}</dd>`;
    if (n.wkind) dl += `<dt>Kind</dt><dd>${esc(n.wkind)}</dd>`;
    if (n.kind === 'pod') dl += `<dt>Status</dt><dd>${esc(n.status)} ${healthPill(health(n))}</dd><dt>Restarts</dt><dd>${n.restarts}</dd>${n.node ? `<dt>Node</dt><dd>${esc(n.node)}</dd>` : ''}${n.ip ? `<dt>Pod IP</dt><dd class="mono">${esc(n.ip)}</dd>` : ''}`;
    if (n.kind === 'cluster') {
      const s = c.stats;
      dl += c.status === 'ok'
        ? `<dt>Entry pts</dt><dd>${s.ctrl} controller · ${s.lb} LB · ${s.np} NodePort</dd><dt>Ingresses</dt><dd>${s.ing} (${s.hosts} hosts)</dd><dt>Workloads</dt><dd>${s.wl} · ${s.pods} exposed pods of ${n.pods}</dd>`
        : `<dt>Status</dt><dd>${n.status === 'error' ? 'unreachable' : 'syncing'}${n.error ? `<br><small>${esc(n.error)}</small>` : ''}</dd>`;
    }
    const target = resourceTarget(n);
    let html = `<button class="close" data-act="close" aria-label="Close">✕</button><div class="kind" style="color:${cssColor(n)}">${n.pseudo ? 'Ingress class' : KLABEL[n.kind]}</div><h2>${esc(n.name)}</h2>`;
    if (n.issue) html += `<div class="pill bad" style="margin-top:6px">⚠ ${esc(n.issue)}</div>`;
    if (n.note) html += `<div class="muted" style="margin-top:6px;font-size:12px">${esc(n.note)}</div>`;
    html += `<dl>${dl}</dl><div class="actions">
      ${target ? '<button class="btn primary" data-act="open">Open details</button>' : ''}
      ${S.view === 'galaxy' ? '<button class="btn" data-act="flow">Flow view</button><button class="btn" data-act="fly">Zoom to</button>' : '<button class="btn" data-act="galaxy">Show in galaxy</button>'}
      ${n.kind === 'workload' && S.depth < 5 ? `<button class="btn" data-act="pods">${S.expanded.has(nk(n)) ? 'Hide pods' : 'Pin pods'}</button>` : ''}
    </div>`;
    if (n.kind === 'ingress') html += `<h3>Routes (${routeCount(n)})</h3>` + routesOf(n).map((r) => `<div class="xp-route">${n.tls ? 'https' : 'http'}://${esc(r.r)} <span style="opacity:.6">→ ${esc(r.svc)}</span></div>`).join('');
    if (n.routes?.length) html += '<h3>Routes into this service</h3>' + n.routes.slice(0, 30).map((r) => `<div class="xp-route">${esc(r.r)} <span style="opacity:.6">← ${esc(r.ing)}</span></div>`).join('');
    if (LAYER[n.kind] >= 2) {
      const ps = pathsTo(n);
      html += `<h3>How traffic reaches it (${ps.length})</h3>` + ps.map((ch) => `<div class="xp-route">${ch.map((x) => esc(label(x))).join(' → ')}</div>`).join('');
    }
    if (n.children.length) {
      const head = { cluster: 'Entry points', controller: 'Ingresses', ingress: 'Backend services', workload: 'Pods' }[n.kind] || 'Workloads';
      const extra = (k) => k.kind === 'workload' ? `${k.ready}/${k.total}` : k.kind === 'pod' ? esc(k.status) : k.kind === 'ingress' ? `${k.children.length} svc` : LAYER[k.kind] === 1 ? (k.pseudo ? 'class' : k.kind === 'controller' ? 'ingress' : k.kind === 'lb' ? 'LB' : 'NodePort') : '';
      html += `<h3>${head} (${n.children.length})</h3>` + n.children.slice(0, 100).map((k) => li(k, extra(k))).join('');
    }
    if (n.parents.length && n.parents[0].kind !== 'cluster') html += `<h3>${n.kind === 'pod' ? 'Workload' : 'Exposed by'} (${n.parents.length})</h3>` + n.parents.map((k) => li(k)).join('');
    p.innerHTML = html;
    p.classList.add('on');
    p.onclick = (e) => {
      const row = e.target.closest('.xp-li');
      if (row) { const m = S.clusters[+row.dataset.ci].nodes.find((x) => x.key === row.dataset.key); if (m) select(m, { fly: true, reveal: true }); return; }
      const act = e.target.closest('[data-act]')?.dataset.act;
      if (act === 'close') select(null);
      if (act === 'open' && target) openResource(target[0], target[1]);
      if (act === 'fly') flyTo(n);
      if (act === 'flow') { S.lanes.add(c.name); setView('flow'); select(n, { reveal: true }); }
      if (act === 'galaxy') { setView('galaxy'); select(n, { fly: true }); }
      if (act === 'pods') { S.expanded.has(nk(n)) ? S.expanded.delete(nk(n)) : S.expanded.add(nk(n)); renderPanel(); S.view === 'flow' ? buildFlow(false) : gDirty(); }
    };
    if (!was) requestAnimationFrame(resize);
  }

  // ---------- flow ----------
  const fEl = $('flow'), fsvg = fEl.querySelector('svg'), fg = fsvg.querySelector('g');
  const NW = 236, NH = 56, GAP = 12, COLX = [0, 300, 600, 1040, 1340, 1640];
  let fview = { k: 1, x: 30, y: 10 }, fnodes = [], fsize = { w: 0, h: 0 };
  const applyF = () => fg.setAttribute('transform', `translate(${fview.x},${fview.y}) scale(${fview.k})`);
  // Flow lays out every selected cluster as a lane, stacked top to bottom; lanes collapse to a header.
  function buildFlow(fit) {
    fg.innerHTML = ''; fnodes = [];
    const note = $('fnote');
    if (!S.clusters.length) { note.hidden = false; note.textContent = 'No clusters selected.'; renderLegends(); return; }
    note.hidden = true;
    S.flowBuilt = true;
    const NS = 'http://www.w3.org/2000/svg';
    const el = (tag, attrs, parent) => { const e = document.createElementNS(NS, tag); for (const k in attrs) e.setAttribute(k, attrs[k]); parent?.appendChild(e); return e; };
    const laneG = el('g', {}, fg), eg = el('g', {}, fg), ng = el('g', {}, fg), rg = el('g', {}, fg);
    const Y = new Map();
    const xOf = (n) => COLX[n.kind === 'source' ? 0 : LAYER[n.kind]];
    const open = (n) => n.kind === 'workload' ? (S.depth >= 5 ? !S.collapsed.has(nk(n)) : S.expanded.has(nk(n)) || S.sel === n) : !S.collapsed.has(nk(n));
    const LANE_H = 44;
    let top = 0, deepest = 1, maxRight = 0;
    const lanes = [];

    for (const c of S.clusters) {
      const laneTop = top, ci = S.clusters.indexOf(c);
      const usable = c.status === 'ok' && c.stats.entries;
      const isOpen = usable && S.lanes.has(c.name);
      lanes.push({ c, top: laneTop, isOpen, usable });
      top += LANE_H;
      if (!isOpen) { top += 10; continue; }
      // Visible tree: DFS from the cluster; a shared node hangs off the first parent that reaches it.
      const vis = new Set(), tkids = new Map();
      const walk = (n) => {
        const kids = [];
        if (n.kind === 'cluster' || open(n)) for (const k of n.children) {
          if ((k.kind !== 'pod' && LAYER[k.kind] > S.depth) || vis.has(k)) continue;
          vis.add(k); kids.push(k);
        }
        tkids.set(n, kids);
        kids.forEach(walk);
      };
      walk(c.root);
      let slot = 0;
      const lay = (n) => {
        const kids = tkids.get(n) || [];
        if (!kids.length) { const y = slot++ * (NH + GAP); Y.set(n, y); return y; }
        const ys = kids.map(lay), y = (Math.min(...ys) + Math.max(...ys)) / 2;
        Y.set(n, y);
        return y;
      };
      const entries = tkids.get(c.root) || [];
      entries.forEach(lay);
      const src = [];
      const mk = (key, name, sub, test) => { const kids = entries.filter(test); if (kids.length) src.push({ key, ci, kind: 'source', name, sub, kids, children: [], parents: [] }); };
      mk('src:pub', 'Internet', 'public LoadBalancer addresses', (n) => n.type === 'LoadBalancer' && !n.internal && !n.pseudo);
      mk('src:int', 'Private network', 'internal LoadBalancer', (n) => n.type === 'LoadBalancer' && n.internal);
      mk('src:np', 'Node IPs', 'NodePort on every node', (n) => n.type === 'NodePort');
      for (const s of src) Y.set(s, s.kids.reduce((a, k) => a + Y.get(k), 0) / s.kids.length);
      const laneNodes = [...src, ...vis];
      for (const n of laneNodes) Y.set(n, Y.get(n) + top + 4);
      top += slot * (NH + GAP) + 22;
      for (const n of vis) deepest = Math.max(deepest, LAYER[n.kind]);
      lanes.at(-1).src = src; lanes.at(-1).vis = vis;
    }
    for (const l of lanes) { // edges and nodes, once every lane has its rows
      if (!l.isOpen) continue;
      for (const s of l.src) for (const k of s.kids) edge(s, k);
      for (const n of l.vis) if (open(n)) for (const k of n.children) if (l.vis.has(k)) edge(n, k);
      l.src.forEach(draw);
      l.vis.forEach(draw);
    }
    maxRight = COLX[deepest] + NW;
    const heads = ['Traffic source', 'Entry point', 'Ingress', 'Service', 'Workload', 'Pod'];
    for (const l of lanes) { // lane header: name, counts, collapse chevron
      const c = l.c, s = c.stats;
      const hg = el('g', { class: 'flane' + (l.usable ? '' : ' off'), transform: `translate(-12,${l.top})` }, laneG);
      el('rect', { width: maxRight + 24, height: LANE_H - 8, rx: 9 }, hg);
      el('text', { x: 14, y: 23, class: 'chev' }, hg).textContent = l.usable ? (l.isOpen ? '▾' : '▸') : '·';
      el('text', { x: 30, y: 23, class: 'name' }, hg).textContent = c.name;
      el('text', { x: 30 + c.name.length * 7.4 + 14, y: 23, class: 'meta' }, hg).textContent = c.status !== 'ok' ? (c.status === 'error' ? 'unreachable' : 'syncing…')
        : !s.entries ? 'exposes nothing' : `${s.entries} entry · ${s.ing} ingress · ${s.hosts} hosts · ${s.pods} pods${s.issues ? ` · ⚠ ${s.issues}` : ''}`;
      if (l.isOpen) for (let i = 1; i <= deepest; i++) el('text', { x: COLX[i] + 12, y: 23, class: 'xp-colhead', 'text-anchor': 'start' }, hg).textContent = i === 1 ? '' : heads[i];
      if (l.usable) hg.addEventListener('click', (ev) => { ev.stopPropagation(); S.lanes.has(c.name) ? S.lanes.delete(c.name) : S.lanes.add(c.name); buildFlow(false); });
    }

    function edge(a, b) {
      const x1 = xOf(a) + NW, y1 = Y.get(a) + NH / 2, x2 = xOf(b), y2 = Y.get(b) + NH / 2, mx = (x1 + x2) / 2;
      const p = el('path', { d: `M${x1},${y1} C${mx},${y1} ${mx},${y2} ${x2},${y2}`, class: 'fedge' + (b.dangling ? ' dangling' : '') }, eg);
      p._a = a; p._b = b;
      if (a.kind === 'ingress' && b.kind === 'service') { // label the connector with the route(s) it carries
        const rs = b.routes.filter((r) => r.ing === a.name).map((r) => r.r);
        if (!rs.length) return;
        // The card already names a single-host ingress, so show just the path; keep the path end when trimming.
        const short = (r) => a.hosts.length === 1 && r.startsWith(a.hosts[0]) && r.length > a.hosts[0].length ? r.slice(a.hosts[0].length) : r;
        let t = short(rs[0]);
        if (t.length > 30) t = '…' + t.slice(-29);
        if (rs.length > 1) t += `  +${rs.length - 1}`;
        const lg = el('g', { class: 'froute', transform: `translate(${x2 - 6},${y2 - 7})` }, rg);
        lg._a = a; lg._b = b;
        el('text', { x: 0, y: 0, 'text-anchor': 'end' }, lg).textContent = t;
        el('title', {}, lg).textContent = rs.join('\n');
      }
    }
    function draw(n) {
      const x = xOf(n), y = Y.get(n);
      const g = el('g', { class: 'fnode', transform: `translate(${x},${y})` }, ng);
      g._n = n;
      el('rect', { class: 'card', width: NW, height: NH, rx: 10 }, g);
      const col = n.kind === 'source' ? 'var(--k-source)' : cssColor(n);
      el('rect', { x: 0, y: 0, width: 4, height: NH, rx: 2, fill: col }, g);
      el('circle', { cx: 22, cy: NH / 2, r: 11, fill: col, 'fill-opacity': .16 }, g);
      el('text', { x: 22, y: NH / 2 + 4, 'text-anchor': 'middle', style: `fill:${col};font-size:10.5px;font-weight:700` }, g).textContent =
        ({ source: '◎', controller: n.pseudo ? 'IC' : '⇄', lb: 'LB', nodeport: 'NP', ingress: '⌘', service: 'S', workload: '▣', pod: '●' })[n.kind];
      const title = label(n);
      const lines = wrap(title, n.kind === 'workload' ? 22 : 26, 2);
      lines.forEach((l, i) => { el('text', { x: 40, y: (lines.length > 1 ? 17 : 22) + i * 15 }, g).textContent = l; });
      if (lines.join('') !== title) el('title', {}, g).textContent = title;
      const sub = n.kind === 'source' ? n.sub
        : n.pseudo ? `${n.ctrl || 'controller not exposed'}`
          : n.kind === 'controller' ? `${n.ctrl} · ${n.ip[0] || 'no address'}`
            : LAYER[n.kind] === 1 ? `${n.ip[0] || (n.type === 'NodePort' ? 'node ports' : 'pending')} ${n.ports.map((p) => p.split('/')[0].split(':')[n.type === 'NodePort' ? 1 : 0] || p).join(',')}`
              : n.kind === 'ingress' ? `${n.ns} · ${routeCount(n) ? `${routeCount(n)} route${routeCount(n) === 1 ? '' : 's'}` : 'no backends'}${n.tls ? ' · TLS' : ''}`
                : n.kind === 'service' ? `${n.ns} · ${n.ports.map((p) => p.split('/')[0]).join(',')}`
                  : n.kind === 'pod' ? `${n.status}${n.restarts ? ` · ${n.restarts}↻` : ''} · ${n.node || ''}`
                    : `${n.wkind} · ${n.ns}`;
      el('text', { x: 40, y: lines.length > 1 ? 47 : 40, class: 's' }, g).textContent = sub.length > 31 ? sub.slice(0, 30) + '…' : sub;
      if (n.dangling && S.depth >= 4 && !n.children.length) { // red stub: this service points at nothing
        const sg = el('g', { class: 'fdangle', transform: `translate(${NW},${NH / 2})` }, g);
        el('path', { d: 'M0,0 H44' }, sg);
        el('path', { d: 'M40,-4 L48,4 M48,-4 L40,4', class: 'x' }, sg);
        el('title', {}, sg).textContent = n.issue;
      }
      if (n.issue) el('text', { x: NW - 14, y: 16, 'text-anchor': 'middle', style: 'fill:var(--bad);font-size:12px' }, g).textContent = '⚠';
      if (n.kind === 'workload') { // pod counter doubles as the expand toggle
        const expanded = open(n);
        const pc = el('g', { class: 'fpods', transform: `translate(${NW - 26},${NH / 2})`, style: `color:${col}` }, g);
        el('circle', { r: 15, fill: col, 'fill-opacity': expanded ? .3 : .15 }, pc);
        el('text', { y: 4, 'text-anchor': 'middle', style: `fill:${col};font-size:11px;font-weight:700` }, pc).textContent = n.total === n.ready ? `×${n.total}` : `${n.ready}/${n.total}`;
        el('title', {}, pc).textContent = expanded ? 'Hide pods' : 'Show pods';
        pc.addEventListener('click', (ev) => {
          ev.stopPropagation();
          const key = nk(n);
          if (S.depth >= 5) S.collapsed.has(key) ? S.collapsed.delete(key) : S.collapsed.add(key);
          else if (S.expanded.has(key) || S.sel === n) { S.expanded.delete(key); if (S.sel === n) { select(null); return; } }
          else S.expanded.add(key);
          buildFlow(false);
        });
      } else if (n.kind !== 'source' && n.kind !== 'pod') {
        const hidden = n.children.filter((k) => LAYER[k.kind] <= S.depth).length;
        if (hidden) {
          const tg = el('g', { class: 'ftog', transform: `translate(${NW},${NH / 2})` }, g);
          el('circle', { r: 9 }, tg);
          el('text', { y: 3.5, 'text-anchor': 'middle' }, tg).textContent = S.collapsed.has(nk(n)) ? '+' + hidden : '−';
          tg.addEventListener('click', (ev) => { ev.stopPropagation(); const key = nk(n); S.collapsed.has(key) ? S.collapsed.delete(key) : S.collapsed.add(key); buildFlow(false); });
        }
      }
      g.addEventListener('click', (ev) => { ev.stopPropagation(); if (fdrag?.moved || n.kind === 'source') return; select(n); });
      g.addEventListener('mouseenter', (ev) => showTip(n, ev.clientX, ev.clientY));
      g.addEventListener('mouseleave', hideTip);
      fnodes.push(g);
    }
    fsize = { w: maxRight + 30, h: top + 20 };
    if (fit) fitFlow();
    flowHighlight();
    renderLegends();
  }
  function fitFlow() {
    const r = fsvg.getBoundingClientRect();
    if (!r.width) return;
    // Fit the width and scroll the lanes vertically; only shrink for height when it all nearly fits.
    const kw = (r.width - 60) / fsize.w, kh = (r.height - 30) / fsize.h;
    const k = clamp(kh > kw * .7 ? Math.min(kw, kh) : kw, .5, 1.1);
    fview = { k, x: Math.max(24, (r.width - fsize.w * k) / 2), y: 10 };
    applyF();
  }
  function flowHighlight() {
    const rel = S.related;
    for (const g of fnodes) {
      const n = g._n;
      g.classList.toggle('sel', S.sel === n);
      g.classList.toggle('dim', !!rel && (n.kind === 'source' ? !n.kids.some((k) => rel.has(k)) : !rel.has(n)));
    }
    fg.querySelectorAll('.fedge, .froute').forEach((p) => {
      const lit = !rel || ((p._a.kind === 'source' || rel.has(p._a)) && rel.has(p._b));
      p.classList.toggle('dim', !lit); p.classList.toggle('hot', !!rel && lit);
    });
  }
  function flowReveal(n) {
    const g = fnodes.find((x) => x._n === n);
    if (!g) return;
    const r = fsvg.getBoundingClientRect();
    const [, x, y] = g.getAttribute('transform').match(/translate\(([-\d.]+),([-\d.]+)\)/).map(Number);
    fview.x = r.width / 2 - (x + NW / 2) * fview.k; fview.y = r.height / 2 - (y + NH / 2) * fview.k;
    applyF();
  }
  let fdrag = null;
  const fzoom = (mx, my, f) => { const k = clamp(fview.k * f, .2, 2.5); const wx = (mx - fview.x) / fview.k, wy = (my - fview.y) / fview.k; fview = { k, x: mx - wx * k, y: my - wy * k }; applyF(); };
  on(fEl, 'wheel', (e) => wheel(e, fzoom, (dx, dy) => { fview.x += dx; fview.y += dy; applyF(); }), { passive: false });
  on(fsvg, 'pointerdown', (e) => { fdrag = { x: e.clientX, y: e.clientY, vx: fview.x, vy: fview.y, moved: false }; });
  on(window, 'pointermove', (e) => {
    if (!fdrag) return;
    const dx = e.clientX - fdrag.x, dy = e.clientY - fdrag.y;
    if (Math.abs(dx) + Math.abs(dy) > 4) { fdrag.moved = true; fEl.classList.add('drag'); }
    if (fdrag.moved) { fview.x = fdrag.vx + dx; fview.y = fdrag.vy + dy; applyF(); }
  });
  on(window, 'pointerup', () => { fEl.classList.remove('drag'); setTimeout(() => { fdrag = null; }, 0); });
  on(fsvg, 'click', (e) => { if (e.target === fsvg && !fdrag?.moved) select(null); });
  const fcenter = (f) => { const r = fsvg.getBoundingClientRect(); fzoom(r.width / 2, r.height / 2, f); };
  fEl.querySelector('[data-fz=in]').onclick = () => fcenter(1.3);
  fEl.querySelector('[data-fz=out]').onclick = () => fcenter(1 / 1.3);
  fEl.querySelector('[data-fz=fit]').onclick = fitFlow;

  // ---------- controls ----------
  function renderLegends() {
    const items = [['controller', 'Ingress controller'], ['lb', 'LoadBalancer'], ['nodeport', 'NodePort'], ['ingress', 'Ingress'], ['service', 'Service'], ['workload', 'Workload / pod']];
    $('glegend').innerHTML = items.map(([k, l]) => `<span><i style="background:${GCOL[k]};box-shadow:0 0 6px ${GCOL[k]}"></i>${l}</span>`).join('') +
      `<span><i style="background:${HCOL.warn}"></i>degraded</span><span><i style="background:${HCOL.bad}"></i>down / issue</span><span><b style="color:${HCOL.bad}">- - ✕</b> dangling service</span><span style="opacity:.7">dashed = shared backend · click a workload for its pods</span>`;
    $('flegend').innerHTML = items.map(([k, l]) => `<span><i style="background:var(--k-${k})"></i>${l}</span>`).join('') +
      `<span><i style="background:${HCOL.warn}"></i>degraded</span><span><i style="background:${HCOL.bad}"></i>down</span><span><b style="color:var(--bad)">- - ✕</b> dangling service</span><span>×N = pods, click to expand · − / + collapse</span>`;
  }
  function setDepth(d, rebuild = true) {
    S.depth = d;
    root.querySelectorAll('[data-role=depth] button').forEach((b) => b.classList.toggle('on', +b.dataset.d === d));
    saveState();
    if (rebuild) { if (S.view === 'flow') buildFlow(true); renderPanel(); }
    gDirty();
  }
  function setView(v) {
    S.view = v;
    root.querySelectorAll('[data-role=views] button').forEach((b) => b.classList.toggle('on', b.dataset.v === v));
    gEl.classList.toggle('on', v === 'galaxy'); fEl.classList.toggle('on', v === 'flow');
    hideTip();
    saveState();
    if (v === 'flow') {
      if (S.sel) S.lanes.add(S.clusters[S.sel.ci].name);
      if (S.loaded) buildFlow(true);
    } else resize();
    renderPanel();
  }
  root.querySelectorAll('[data-role=depth] button').forEach((b) => b.onclick = () => setDepth(+b.dataset.d));
  root.querySelectorAll('[data-role=views] button').forEach((b) => b.onclick = () => setView(b.dataset.v));
  $('search').addEventListener('keydown', (e) => {
    if (e.key !== 'Enter') return;
    const q = e.target.value.trim().toLowerCase();
    if (!q) return;
    const pool = allNodes();
    const m = pool.find((n) => n.kind !== 'cluster' && (n.name.toLowerCase().includes(q) || (n.hosts || []).some((h) => h.toLowerCase().includes(q)) || (n.ip && [].concat(n.ip).some((ip) => String(ip).includes(q)))))
      || pool.find((n) => n.name.toLowerCase().includes(q));
    if (m) select(m, { fly: true, reveal: true });
    else { e.target.style.borderColor = 'var(--bad)'; setTimeout(() => { e.target.style.borderColor = ''; }, 900); }
  });
  on(window, 'keydown', (e) => {
    if (e.target.closest?.('input,select,textarea') || document.getElementById('drawer')?.hidden === false) return;
    if (e.key === 'Escape') select(null);
    if (e.key === 'f' && S.view === 'galaxy') animateTo(fitAllView());
    if (e.key >= '1' && e.key <= '5') setDepth(+e.key);
  });
  on(window, 'resize', () => { resize(); if (S.view === 'flow') fitFlow(); });
  const ro = new ResizeObserver(() => resize());
  ro.observe(gEl);
  cleanups.push(() => ro.disconnect());

  renderLegends();
  setDepth(S.depth, false);
  setView(S.view);
  requestAnimationFrame(loop);
  load();
  timer = setInterval(() => { if (!document.hidden) load(); }, REFRESH_MS);

  return {
    destroy() {
      destroyed = true;
      clearInterval(timer);
      cleanups.forEach((f) => f());
    },
  };
}
