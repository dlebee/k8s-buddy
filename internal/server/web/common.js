// The API token arrives once in the page URL: as the token itself in the app's own web views, or
// as a one-time code (?c=) in a regular browser, swapped here for the token so the token never
// lands in browser history. Keep it in this tab's session storage, take it out of the address bar,
// then send it as a header.
async function redeem(code) {
  try {
    const r = await fetch('/api/session', { method: 'POST', body: JSON.stringify({ code }) });
    return r.ok ? (await r.json()).token || '' : '';
  } catch { return ''; }
}

async function takeToken() {
  const params = new URLSearchParams(location.search);
  let token = params.get('t') || '';
  if (!token && params.get('c')) token = await redeem(params.get('c'));
  try {
    if (token) sessionStorage.setItem('buddyToken', token);
    else token = sessionStorage.getItem('buddyToken') || '';
  } catch { /* storage unavailable: keep it in memory only */ }
  if (params.has('t') || params.has('c')) {
    params.delete('t');
    params.delete('c');
    const qs = params.toString();
    history.replaceState(null, '', location.pathname + (qs ? '?' + qs : '') + location.hash);
  }
  return token;
}
export const TOKEN = await takeToken();

export async function api(path, opts = {}) {
  const r = await fetch(new URL(path, location.origin), { ...opts, headers: { ...(opts.headers || {}), 'X-Buddy-Token': TOKEN } });
  if (!r.ok) throw new Error(`${r.status} ${await r.text()}`);
  return r.status === 204 ? null : r.json();
}

// The build's version for display: "v1.2.3" for releases, "dev build" (full string on hover) otherwise.
export function showVersion(el, v) {
  if (!el) return;
  const release = /^\d+\.\d+\.\d+$/.test(v || '');
  el.textContent = !v ? '' : release ? `v${v}` : 'dev build';
  el.title = v ? `k8s-buddy ${v}` : '';
}

export const post = (path) => api(path, { method: 'POST' });

export function stream(handlers) {
  const es = new EventSource(`/api/stream?t=${encodeURIComponent(TOKEN)}`);
  for (const [name, fn] of Object.entries(handlers)) {
    es.addEventListener(name, (e) => fn(JSON.parse(e.data)));
  }
  return es;
}

export const RANK = { info: 0, notice: 1, warning: 2, critical: 3 };

export function esc(s) {
  return String(s ?? '').replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' })[c]);
}

export function ago(ts) {
  if (!ts) return '';
  const s = Math.max(0, (Date.now() - new Date(ts).getTime()) / 1000);
  if (s < 60) return `${Math.floor(s)}s`;
  if (s < 3600) return `${Math.floor(s / 60)}m`;
  if (s < 172800) return `${Math.floor(s / 3600)}h`;
  return `${Math.floor(s / 86400)}d`;
}

export async function copy(text) {
  try {
    await navigator.clipboard.writeText(text);
  } catch {
    const ta = document.createElement('textarea');
    ta.value = text;
    document.body.appendChild(ta);
    ta.select();
    document.execCommand('copy');
    ta.remove();
  }
}

export function debounce(fn, ms) {
  let t;
  return (...a) => { clearTimeout(t); t = setTimeout(() => fn(...a), ms); };
}
