// Kubernetes version context: how far a version is from the newest minor, the "newest − 1" line
// managed platforms default to, and its own patch / end-of-life status. Shown in an instant hover
// card (not the browser's slow native tooltip) and inline in the node drawer.
import { api, esc } from '/static/common.js';

let releases = [];
let loading = null;
export function loadReleases() {
  loading ??= api('/api/k8s-releases').then((r) => { releases = r.releases || []; }).catch(() => {});
  return loading;
}

const parse = (v) => { const m = /v?(\d+)\.(\d+)(?:\.(\d+))?/.exec(v || ''); return m ? { minor: `${m[1]}.${m[2]}`, n: +m[1] * 1000 + +m[2], patch: m[3] != null ? +m[3] : null } : null; };
const fmtDate = (d) => new Date(d).toLocaleDateString([], { year: 'numeric', month: 'short', day: 'numeric', timeZone: 'UTC' }); // dates, not instants
const monthsUntil = (d) => Math.round((new Date(d) - Date.now()) / (30.44 * 86400000));

// versionInfo describes a version like "v1.33.5-aks", or returns null without release data.
export function versionInfo(ver) {
  const v = parse(ver);
  if (!v || !releases.length) return null;
  const latest = releases[0], prev = releases[1];
  const idx = releases.findIndex((r) => r.minor === v.minor);
  const mine = idx >= 0 ? releases[idx] : null;
  const behind = idx >= 0 ? idx : Math.max(0, parse(latest.minor).n - v.n);
  let tone = behind <= 1 ? 'ok' : behind === 2 ? 'warn' : 'bad';
  let patch = null, eol = null;
  if (mine) {
    const lp = parse(mine.latestPatch)?.patch;
    patch = v.patch != null && lp != null && lp > v.patch ? { latest: mine.latestPatch, behind: lp - v.patch } : { latest: mine.latestPatch, behind: 0 };
    const left = monthsUntil(mine.eol);
    const end = new Date(mine.eol), now = new Date();
    const within = (m) => { const d = new Date(now); d.setMonth(d.getMonth() + m); return d > end; };
    eol = { date: fmtDate(mine.eol), months: left, past: end < now, soon: within(2), urgent: within(1) };
    // Same thresholds as the alert: warning within 2 months, critical within 1 or past.
    if (eol.past || eol.urgent) tone = 'bad'; else if (eol.soon && tone === 'ok') tone = 'warn';
  }
  return { ver, minor: v.minor, behind, tone, latest, prev, patch, eol };
}

// versionCardHTML is the card body: used by the hover card and inline in the node drawer.
export function versionCardHTML(ver) {
  const i = versionInfo(ver);
  if (!i) return `<div class="kv-card"><b>${esc(ver || '–')}</b><div class="muted">No Kubernetes release data yet.</div></div>`;
  const row = (label, value, tone = '') => `<div class="kv-row ${tone}"><span>${label}</span><span>${value}</span></div>`;
  const behindTxt = i.behind === 0 ? 'on the newest minor' : `${i.behind} minor version${i.behind === 1 ? '' : 's'} behind`;
  return `<div class="kv-card">
    <div class="kv-head"><b>${esc(i.ver)}</b><span class="kv-pill ${i.tone}">${esc(behindTxt)}</span></div>
    ${row('Newest', `<b>${esc(i.latest.minor)}</b> <span class="muted">patch ${esc(i.latest.latestPatch)}${i.latest.released ? ` · ${fmtDate(i.latest.released)}` : ''}</span>`)}
    ${i.prev ? row('Newest − 1', `<b>${esc(i.prev.minor)}</b> <span class="muted">patch ${esc(i.prev.latestPatch)} · generally supported by providers</span>`) : ''}
    ${i.patch ? row(`Your ${esc(i.minor)}`, i.patch.behind ? `patch <b>${esc(i.patch.latest)}</b> available <span class="muted">(${i.patch.behind} behind)</span>` : 'on the latest patch', i.patch.behind ? 'warn' : '') : row(`Your ${esc(i.minor)}`, '<span class="muted">no upstream data</span>')}
    ${i.eol ? row('End of life', i.eol.past ? `<b>passed</b> on ${esc(i.eol.date)}` : `${esc(i.eol.date)} <span class="muted">(${i.eol.months <= 1 ? 'within a month' : `in ${i.eol.months} months`})</span>`, i.eol.past || i.eol.urgent ? 'bad' : i.eol.soon ? 'warn' : '') : ''}
    <div class="kv-note">Kubernetes has no LTS: each minor gets ~14 months of upstream patches; managed platforms may support longer.</div>
  </div>`;
}

// verSpan renders a version that shows the card on hover.
export function verSpan(v, extra = '') {
  if (!v) return `<span class="ver ${extra}"></span>`;
  const i = versionInfo(v);
  if (!i) return `<span class="ver ${extra}">${esc(v)}</span>`;
  return `<span class="ver k8sv ${i.tone} ${extra}" data-k8sv="${esc(v)}" tabindex="0">${esc(v)}${i.behind >= 2 ? ` <small>−${i.behind}</small>` : ''}</span>`;
}

// One floating card for the whole page, shown immediately on hover or focus.
let card = null;
function show(el) {
  if (!card) { card = document.createElement('div'); card.className = 'kv-pop'; document.body.appendChild(card); }
  card.innerHTML = versionCardHTML(el.dataset.k8sv);
  card.style.display = 'block';
  const r = el.getBoundingClientRect(), c = card.getBoundingClientRect();
  let x = Math.min(r.left, innerWidth - c.width - 8), y = r.bottom + 6;
  if (y + c.height > innerHeight - 8) y = r.top - c.height - 6;
  card.style.left = `${Math.max(8, x)}px`; card.style.top = `${Math.max(8, y)}px`;
}
const hide = () => { if (card) card.style.display = 'none'; };
const target = (e) => e.target.closest?.('[data-k8sv]');
document.addEventListener('mouseover', (e) => { const t = target(e); if (t) show(t); });
document.addEventListener('mouseout', (e) => { const t = target(e); if (t && !t.contains(e.relatedTarget)) hide(); });
document.addEventListener('focusin', (e) => { const t = target(e); if (t) show(t); });
document.addEventListener('focusout', (e) => { if (target(e)) hide(); });
document.addEventListener('scroll', hide, true);
