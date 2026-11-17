// Usage-over-time charts from the local history: median line, min–max band, request (dashed) and
// limit/allocatable (solid) lines. Plain SVG, no library.
import { api, esc } from '/static/common.js';

const W = 600, H = 150, PAD = { l: 46, r: 8, t: 10, b: 20 };
const fmtCPU = (m) => (m >= 1000 ? `${+(m / 1000).toFixed(m >= 10000 ? 0 : 1)} core${m === 1000 ? '' : 's'}` : `${Math.round(m)}m`);
const fmtMem = (b) => { const u = ['B', 'Ki', 'Mi', 'Gi', 'Ti']; let i = 0; while (b >= 1024 && i < u.length - 1) { b /= 1024; i++; } return `${b >= 100 || i === 0 ? Math.round(b) : b.toFixed(1)}${u[i]}`; };

function chart(points, key, ref, fmt, label, bucket) {
  if (!points.length) return '';
  const avg = points.map((p) => p[key]), peak = points.map((p) => p[key + 'Max']), low = points.map((p) => p[key + 'Min'] ?? p[key]);
  const gapAt = (i) => i > 0 && points[i].t - points[i - 1].t > (points[i - 1].step || bucket) * 2.5;
  const t0 = points[0].t, t1 = Math.max(points.at(-1).t, t0 + bucket);
  const top = Math.max(...peak, ref.req > 0 ? ref.req : 0, ref.lim > 0 ? ref.lim : 0, 1) * 1.1;
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
  return `<div class="uc-chart" data-key="${key}">
    <div class="uc-head"><b>${label}</b><span class="muted">avg ${esc(fmt(avgAll))} · peak ${esc(fmt(peakAll))}</span></div>
    <svg viewBox="0 0 ${W} ${H}" preserveAspectRatio="none">${ticks}${band}<path class="avg" d="${segs(avg)}"/>${points.length < 4 ? points.map((p, i) => `<circle class="dot" cx="${x(p.t).toFixed(1)}" cy="${y(avg[i]).toFixed(1)}" r="3"/>`).join('') : ''}${hline(ref.req, 'req', 'request')}${hline(ref.lim, 'lim', ref.limLabel || 'limit')}
      <line class="cross" x1="0" x2="0" y1="${PAD.t}" y2="${H - PAD.b}" visibility="hidden"/>${xt}</svg>
    <div class="uc-tip" hidden></div></div>`;
}

const WINDOWS = [[6, '6h'], [24, '24h'], [168, '7d'], [336, '14d']];

// mountHistory fills `el` with CPU and memory charts for a node, pod or workload.
export async function mountHistory(el, { cluster, kind, namespace = '', name, allocatable = false }) {
  let hours = +(el.dataset.hours || 24);
  const draw = async () => {
    let r;
    try { r = await api('/api/usage/history?' + new URLSearchParams({ cluster, kind, namespace, name, hours })); } catch (e) { el.innerHTML = `<p class="muted">${esc(e.message.replace(/^\d+ /, ''))}</p>`; return; }
    if (!document.contains(el)) return;
    if (!r.enabled) { el.innerHTML = '<p class="muted">Usage history is off. Turn it on in Settings to graph usage over time.</p>'; return; }
    const maxH = (r.retentionDays || 7) * 24;
    const wins = [...WINDOWS.filter(([h]) => h < maxH), [maxH, `all (${r.retentionDays}d)`]];
    const seg = `<div class="seg uc-win">${wins.map(([h, l]) => `<button data-h="${h}" class="${h === hours ? 'on' : ''}">${l}</button>`).join('')}</div>`;
    const i = r.info || {};
    const body = r.points.length
      ? chart(r.points, 'cpu', { req: i.cpuReq, lim: i.cpuLim, limLabel: allocatable ? 'allocatable' : 'limit' }, fmtCPU, 'CPU', r.bucketSeconds)
        + chart(r.points, 'mem', { req: i.memReq, lim: i.memLim, limLabel: allocatable ? 'allocatable' : 'limit' }, fmtMem, 'Memory', r.bucketSeconds)
      : '<p class="muted">No samples yet for this window. History is recorded once a minute while the app runs; graphs fill in as it goes.</p>';
    el.innerHTML = `${seg}${body}<p class="muted rv-small">Recorded locally every minute, kept ${r.retentionDays} days · 1-minute detail for 6 h, then 5-minute to 2 days, then 15-minute · line = median, band = min to max${i.pods > 1 ? ` · summed over ${i.pods} pods` : ''}</p>`;
    el.querySelectorAll('[data-h]').forEach((b) => b.onclick = () => { hours = +b.dataset.h; el.dataset.hours = hours; draw(); });
    el.querySelectorAll('.uc-chart').forEach((c) => hover(c, r.points, c.dataset.key === 'cpu' ? fmtCPU : fmtMem));
  };
  await draw();
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
