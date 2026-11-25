// Usage-over-time charts from the local history: median line, min–max band, request (dashed) and
// limit/allocatable (solid) lines, and optionally a spike line with everything above it in red.
// Drag across a chart to zoom into that stretch. Plain SVG, no library.
import { api, esc } from '/static/common.js';

const W = 600, H = 150, PAD = { l: 46, r: 8, t: 10, b: 20 };
const fmtCPU = (m) => (m >= 1000 ? `${+(m / 1000).toFixed(m >= 10000 ? 0 : 1)} core${m === 1000 ? '' : 's'}` : `${Math.round(m)}m`);
const fmtMem = (b) => { const u = ['B', 'Ki', 'Mi', 'Gi', 'Ti']; let i = 0; while (b >= 1024 && i < u.length - 1) { b /= 1024; i++; } return `${b >= 100 || i === 0 ? Math.round(b) : b.toFixed(1)}${u[i]}`; };

let uid = 0;

function chart(points, key, ref, fmt, label, bucket) {
  if (!points.length) return '';
  const avg = points.map((p) => p[key]), peak = points.map((p) => p[key + 'Max']), low = points.map((p) => p[key + 'Min'] ?? p[key]);
  const gapAt = (i) => i > 0 && points[i].t - points[i - 1].t > (points[i - 1].step || bucket) * 2.5;
  const t0 = points[0].t, t1 = Math.max(points.at(-1).t, t0 + bucket);
  const top = Math.max(...peak, ref.req > 0 ? ref.req : 0, ref.lim > 0 ? ref.lim : 0, ref.over > 0 ? ref.over : 0, 1) * 1.1;
  const x = (t) => PAD.l + ((t - t0) / (t1 - t0 || 1)) * (W - PAD.l - PAD.r);
  const y = (v) => PAD.t + (1 - v / top) * (H - PAD.t - PAD.b);
  // Break lines where buckets are missing (history off, cluster unreachable) instead of bridging.
  const segs = (vals) => { let d = ''; points.forEach((p, i) => { d += `${!i || gapAt(i) ? 'M' : 'L'}${x(p.t).toFixed(1)},${y(vals[i]).toFixed(1)}`; }); return d; };
  let band = '';
  { let run = []; const flush = () => { if (run.length) { band += `<path class="band" d="M${run.map((i) => `${x(points[i].t).toFixed(1)},${y(peak[i]).toFixed(1)}`).join('L')}L${run.slice().reverse().map((i) => `${x(points[i].t).toFixed(1)},${y(low[i]).toFixed(1)}`).join('L')}Z"/>`; } run = []; };
    points.forEach((p, i) => { if (gapAt(i)) flush(); run.push(i); }); flush(); }
  const hline = (v, cls, txt) => v > 0 ? `<line class="${cls}" x1="${PAD.l}" x2="${W - PAD.r}" y1="${y(v)}" y2="${y(v)}"/><text class="lbl ${cls}" x="${W - PAD.r - 2}" y="${y(v) - 3}" text-anchor="end">${esc(txt)} ${esc(fmt(v))}</text>` : '';
  const ticks = [0, top / 2, top / 1.1].map((v) => `<text class="ax" x="${PAD.l - 6}" y="${y(v) + 3}" text-anchor="end">${esc(fmt(v))}</text><line class="grid" x1="${PAD.l}" x2="${W - PAD.r}" y1="${y(v)}" y2="${y(v)}"/>`).join('');
  const span = t1 - t0, tf = (t) => new Date(t * 1000).toLocaleString([], span > 2 * 86400 ? { month: 'short', day: 'numeric' } : { hour: '2-digit', minute: '2-digit', hour12: false });
  const xt = [t0, (t0 + t1) / 2, t1].map((t, i) => `<text class="ax" x="${x(t)}" y="${H - 5}" text-anchor="${['start', 'middle', 'end'][i]}">${esc(tf(t))}</text>`).join('');
  const peakAll = Math.max(...peak), avgAll = avg.reduce((a, b) => a + b, 0) / avg.length;
  // Above the spike line: the same band and line again, clipped to the area over it, in red.
  const id = `uc-over-${++uid}`;
  const hot = ref.over > 0 ? `<clipPath id="${id}"><rect x="0" y="0" width="${W}" height="${Math.max(0, y(ref.over)).toFixed(1)}"/></clipPath>
    <g clip-path="url(#${id})" class="hot">${band}<path class="avg" d="${segs(avg)}"/></g>${hline(ref.over, 'over', ref.overLabel || 'spike line')}` : '';
  // Each run of points over the spike line: a red column behind the chart (never thinner than a
  // few pixels, so a 5-minute spike still shows in 14 days; click it to zoom in) and a dot on its peak.
  const runs = [];
  if (ref.over > 0) {
    points.forEach((p, i) => {
      if (!(peak[i] > ref.over)) return;
      const end = p.t + (p.step || bucket), last = runs.at(-1);
      if (last && !gapAt(i) && last.i === i - 1) { last.to = end; last.i = i; if (peak[i] > last.peak) { last.peak = peak[i]; last.at = p.t; } return; }
      runs.push({ from: p.t, to: end, i, peak: peak[i], at: p.t });
    });
  }
  const cols = runs.map((r) => {
    const a = x(r.from), b = x(r.to), w = Math.max(b - a, 4), left = (a + b) / 2 - w / 2;
    const when = new Date(r.from * 1000).toLocaleString([], { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit', hour12: false });
    return `<rect class="spike-col" data-from="${r.from}" data-to="${r.to}" x="${left.toFixed(1)}" y="${PAD.t}" width="${w.toFixed(1)}" height="${H - PAD.t - PAD.b}"><title>Over the line from ${esc(when)}, peak ${esc(fmt(r.peak))}: click to zoom in</title></rect>`;
  }).join('');
  const dots = runs.map((r) => `<i class="spike-dot" style="left:${(x(r.at) / W * 100).toFixed(2)}%;top:${(y(r.peak) / H * 100).toFixed(2)}%"></i>`).join('');
  return `<div class="uc-chart" data-key="${key}" data-t0="${t0}" data-t1="${t1}">
    <div class="uc-head"><b>${label}${runs.length ? ` <span class="spike-n">${runs.length} spike${runs.length === 1 ? '' : 's'}</span>` : ''}</b><span class="muted">avg ${esc(fmt(avgAll))} · peak ${esc(fmt(peakAll))}</span></div>
    <div class="uc-plot"><svg viewBox="0 0 ${W} ${H}" preserveAspectRatio="none">${ticks}${cols}${band}<path class="avg" d="${segs(avg)}"/>${points.length < 4 ? points.map((p, i) => `<circle class="dot" cx="${x(p.t).toFixed(1)}" cy="${y(avg[i]).toFixed(1)}" r="3"/>`).join('') : ''}${hline(ref.req, 'req', 'request')}${hline(ref.lim, 'lim', ref.limLabel || 'limit')}${hot}
      <rect class="sel" x="0" y="${PAD.t}" width="0" height="${H - PAD.t - PAD.b}" visibility="hidden"/>
      <line class="cross" x1="0" x2="0" y1="${PAD.t}" y2="${H - PAD.b}" visibility="hidden"/>${xt}</svg>${dots}</div>
    <div class="uc-tip" hidden></div></div>`;
}

const WINDOWS = [[6, '6h'], [24, '24h'], [168, '7d'], [336, '14d']];

// mountHistory fills `el` with CPU and memory charts for a node, pod or workload. Options:
// overPct: {cpu, mem}, a spike line at that share of the limit (or allocatable), what went above
// it in red;
// focus: {from, to} (unix seconds), offered as "Zoom to the spike".
export async function mountHistory(el, { cluster, kind, namespace = '', name, allocatable = false, overPct = null, focus = null }) {
  let hours = +(el.dataset.hours || 24);
  let range = null; // [from, to] while zoomed
  let r;
  const render = () => {
    const maxH = (r.retentionDays || 7) * 24;
    const wins = [...WINDOWS.filter(([h]) => h < maxH), [maxH, `all (${r.retentionDays}d)`]];
    const seg = `<div class="seg uc-win">${wins.map(([h, l]) => `<button data-h="${h}" class="${h === hours && !range ? 'on' : ''}">${l}</button>`).join('')}</div>`;
    const zoom = `<div class="uc-zoom">${focus ? '<button class="btn sm" data-zoom="focus">🔍 Zoom to the spike</button>' : ''}${range ? '<button class="btn sm" data-zoom="out">Zoom out</button>' : ''}<span class="muted">${range ? `${esc(fmtRange(range))} · ` : ''}drag across a chart to zoom</span></div>`;
    const pts = range ? r.points.filter((p) => p.t + (p.step || 60) > range[0] && p.t < range[1]) : r.points;
    const i = r.info || {};
    const line = (lim, pct) => (overPct && lim > 0 ? lim * pct / 100 : 0);
    const over = overPct && { cpu: line(i.cpuLim, overPct.cpu), mem: line(i.memLim, overPct.mem) };
    const body = pts.length
      ? chart(pts, 'cpu', { req: i.cpuReq, lim: i.cpuLim, limLabel: allocatable ? 'allocatable' : 'limit', over: over?.cpu, overLabel: overPct && `${overPct.cpu}% line` }, fmtCPU, 'CPU', r.bucketSeconds)
        + chart(pts, 'mem', { req: i.memReq, lim: i.memLim, limLabel: allocatable ? 'allocatable' : 'limit', over: over?.mem, overLabel: overPct && `${overPct.mem}% line` }, fmtMem, 'Memory', r.bucketSeconds)
      : `<p class="muted">${range ? 'No samples in this stretch.' : 'No samples yet for this window. History is recorded once a minute while the app runs; graphs fill in as it goes.'}</p>`;
    el.innerHTML = `<div class="uc-bar">${seg}${zoom}</div>${body}<p class="muted rv-small">Recorded locally every minute, kept ${r.retentionDays} days · 1-minute detail for 6 h, then 5-minute to 2 days, then 15-minute · line = median, band = min to max${over ? ' · red = above the spike line' : ''}${i.pods > 1 ? ` · summed over ${i.pods} pods` : ''}</p>`;
    el.querySelectorAll('[data-h]').forEach((b) => b.onclick = () => { hours = +b.dataset.h; el.dataset.hours = hours; range = null; load(); });
    el.querySelectorAll('[data-zoom]').forEach((b) => b.onclick = () => {
      if (b.dataset.zoom === 'out') { range = null; render(); return; }
      const need = (Date.now() / 1000 - focus.from) / 3600;
      range = [focus.from, focus.to];
      if (need > hours) { hours = [...wins.map(([h]) => h)].find((h) => h >= need) || maxH; load(); } else render();
    });
    el.querySelectorAll('.uc-chart').forEach((c) => {
      hover(c, pts, c.dataset.key === 'cpu' ? fmtCPU : fmtMem);
      dragZoom(c, (a, b) => { range = [a, b]; render(); });
    });
  };
  const load = async () => {
    try { r = await api('/api/usage/history?' + new URLSearchParams({ cluster, kind, namespace, name, hours })); } catch (e) { el.innerHTML = `<p class="muted">${esc(e.message.replace(/^\d+ /, ''))}</p>`; return; }
    if (!document.contains(el)) return;
    if (!r.enabled) { el.innerHTML = '<p class="muted">Usage history is off. Turn it on in Settings to graph usage over time.</p>'; return; }
    render();
  };
  await load();
}

const fmtRange = ([a, b]) => { const f = (t) => new Date(t * 1000).toLocaleString([], { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit', hour12: false }); return `${f(a)} – ${f(b)}`; };

// dragZoom: press, drag across, release to zoom into that stretch (a short click does nothing).
function dragZoom(c, onZoom) {
  const svg = c.querySelector('svg'), sel = svg.querySelector('.sel');
  const t0 = +c.dataset.t0, t1 = +c.dataset.t1;
  const fx = (e) => { const r = svg.getBoundingClientRect(); return Math.min(W - PAD.r, Math.max(PAD.l, (e.clientX - r.left) / r.width * W)); };
  const tAt = (x) => t0 + ((x - PAD.l) / (W - PAD.l - PAD.r)) * (t1 - t0);
  svg.addEventListener('mousedown', (e) => {
    if (e.button !== 0) return;
    e.preventDefault();
    const start = fx(e);
    const move = (ev) => { const x = fx(ev); sel.setAttribute('x', Math.min(start, x)); sel.setAttribute('width', Math.abs(x - start)); sel.setAttribute('visibility', 'visible'); };
    const up = (ev) => {
      window.removeEventListener('mousemove', move);
      window.removeEventListener('mouseup', up);
      sel.setAttribute('visibility', 'hidden');
      const x = fx(ev);
      if (Math.abs(x - start) < 6) {
        // A click on a red column zooms into that spike, with some time around it.
        const col = e.target.closest?.('.spike-col');
        if (col) { const a = +col.dataset.from, b = +col.dataset.to, pad = Math.max(1800, (b - a) * 2); onZoom(a - pad, b + pad); }
        return;
      }
      const a = tAt(Math.min(start, x)), b = tAt(Math.max(start, x));
      if (b - a >= 120) onZoom(a, b);
    };
    window.addEventListener('mousemove', move);
    window.addEventListener('mouseup', up);
  });
}

function hover(c, points, fmt) {
  const svg = c.querySelector('svg'), tip = c.querySelector('.uc-tip'), cross = svg.querySelector('.cross');
  const key = c.dataset.key, t0 = points[0].t, t1 = Math.max(points.at(-1).t, t0 + 1);
  svg.addEventListener('mousemove', (e) => {
    const r = svg.getBoundingClientRect();
    const fx = (e.clientX - r.left) / r.width * W;
    const t = t0 + ((fx - PAD.l) / (W - PAD.l - PAD.r)) * (t1 - t0);
    let best = points[0];
    for (const p of points) if (Math.abs(p.t - t) < Math.abs(best.t - t)) best = p;
    const px = PAD.l + ((best.t - t0) / (t1 - t0)) * (W - PAD.l - PAD.r);
    cross.setAttribute('x1', px); cross.setAttribute('x2', px); cross.setAttribute('visibility', 'visible');
    tip.hidden = false;
    tip.innerHTML = `${esc(new Date(best.t * 1000).toLocaleString([], { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit', hour12: false }))} · avg <b>${esc(fmt(best[key]))}</b> · peak <b>${esc(fmt(best[key + 'Max']))}</b>`;
    tip.style.left = `${Math.min(Math.max(0, (px / W) * r.width - 80), r.width - 230)}px`;
  });
  svg.addEventListener('mouseleave', () => { tip.hidden = true; cross.setAttribute('visibility', 'hidden'); });
}
