export const TOKEN = new URLSearchParams(location.search).get('t') || '';

export async function api(path, opts = {}) {
  const u = new URL(path, location.origin);
  u.searchParams.set('t', TOKEN);
  const r = await fetch(u, opts);
  if (!r.ok) throw new Error(`${r.status} ${await r.text()}`);
  return r.status === 204 ? null : r.json();
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
