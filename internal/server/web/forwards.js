// Port forwards and shells: the floating "Port forwards" dock plus the pickers used by drawer summaries.
import { api, esc, ago, copy } from '/static/common.js';

let dock;
let list = [];
let pollTimer;
let open = false;

const say = (msg) => window.buddyToast?.(msg);
const label = (f) => `${{ services: 'svc', pods: 'pod', deployments: 'deploy', statefulsets: 'sts' }[f.kind] || f.kind}/${f.name}`;

function renderDock() {
  if (!dock) return;
  document.body.classList.toggle('has-forwards', list.length > 0);
  dock.hidden = !list.length;
  if (!list.length) { open = false; return; }
  const failed = list.filter((f) => f.state === 'failed').length;
  dock.innerHTML = `
    <button class="pf-pill ${failed ? 'bad' : ''}" data-toggle>
      <span class="pf-live"></span>${list.length} port forward${list.length === 1 ? '' : 's'}${failed ? ` · ${failed} stopped` : ''}<span class="pf-chev">${open ? '▾' : '▴'}</span>
    </button>
    ${open ? `<div class="pf-panel">
      <div class="pf-head"><b>Port forwards</b><span class="muted">stop when k8s-buddy quits</span><button class="btn sm" data-stop-all>Stop all</button></div>
      ${list.map((f) => `<div class="pf-row ${f.state}">
        <span class="dot ${f.state === 'active' ? (f.error ? 'warn' : 'ok') : f.state === 'failed' ? 'bad' : 'connecting'}"></span>
        <div class="pf-main">
          <div class="pf-target" title="${esc(f.cluster)} · ${esc(f.namespace)}">${esc(label(f))}<span class="muted"> :${f.remotePort} · ${esc(f.cluster)}/${esc(f.namespace)}</span></div>
          ${f.state === 'failed' ? `<div class="pf-err">${esc(f.error || 'stopped')}</div>`
            : `<a class="pf-link" href="${esc(f.url)}" data-open="${f.id}">${esc(f.url.replace('http://', ''))}</a><span class="muted pf-age">${ago(f.startedAt)}</span>
               ${f.error ? `<div class="pf-err warn">${esc(f.error)}</div>` : ''}`}
        </div>
        ${f.state === 'failed'
          ? `<button class="btn sm" data-retry="${f.id}">Retry</button><button class="icon-btn" data-stop="${f.id}" title="Dismiss">✕</button>`
          : `<button class="btn sm" data-copy-url="${esc(f.url)}">Copy</button><button class="icon-btn" data-stop="${f.id}" title="Stop">■</button>`}
      </div>`).join('')}
    </div>` : ''}`;
  dock.querySelector('[data-toggle]').onclick = () => { open = !open; renderDock(); };
  dock.querySelector('[data-stop-all]')?.addEventListener('click', async () => {
    await Promise.all(list.map((f) => api(`/api/portforwards/${f.id}`, { method: 'DELETE' }).catch(() => {})));
    refreshForwards();
  });
  dock.querySelectorAll('[data-stop]').forEach((b) => b.onclick = async () => { await api(`/api/portforwards/${b.dataset.stop}`, { method: 'DELETE' }).catch(() => {}); refreshForwards(); });
  dock.querySelectorAll('[data-copy-url]').forEach((b) => b.onclick = async () => { await copy(b.dataset.copyUrl); say('Copied link'); });
  dock.querySelectorAll('[data-open]').forEach((a) => a.onclick = (e) => { e.preventDefault(); openForward(a.dataset.open, a.href); });
  dock.querySelectorAll('[data-retry]').forEach((b) => b.onclick = async () => {
    const f = list.find((x) => x.id === b.dataset.retry);
    await api(`/api/portforwards/${f.id}`, { method: 'DELETE' }).catch(() => {});
    startForward({ cluster: f.cluster, namespace: f.namespace, kind: f.kind, name: f.name, port: f.remotePort, localPort: f.localPort });
  });
}

export async function openForward(id, url) {
  try { await api(`/api/portforwards/${id}/open`, { method: 'POST' }); } catch { window.open(url, '_blank'); }
}

export async function refreshForwards() {
  try { list = await api('/api/portforwards'); } catch { return list; }
  renderDock();
  document.dispatchEvent(new CustomEvent('buddy:forwards', { detail: list }));
  return list;
}

export function forwardsFor(t) {
  return list.filter((f) => f.cluster === t.cluster && f.namespace === t.namespace && f.kind === t.kind && f.name === t.name);
}

export function mountForwardsDock() {
  dock = document.createElement('div');
  dock.className = 'pf-dock';
  dock.hidden = true;
  document.body.appendChild(dock);
  document.addEventListener('click', (e) => { if (open && !dock.contains(e.target)) { open = false; renderDock(); } });
  const tick = () => { if (!document.hidden) refreshForwards(); };
  tick();
  clearInterval(pollTimer);
  pollTimer = setInterval(tick, 4000);
}

export async function startForward(target) {
  say(`Forwarding ${label(target)}:${target.port}…`);
  try {
    const f = await api('/api/portforwards', { method: 'POST', body: JSON.stringify(target) });
    await refreshForwards();
    open = true; renderDock();
    say(`Forwarding on ${f.url.replace('http://', '')}`);
    return f;
  } catch (e) {
    say(e.message.replace(/^\d+ /, '').trim().split('\n').pop().slice(0, 160));
    return null;
  }
}

// ---------- pickers ----------
let menuEl;
function closeMenu() { menuEl?.remove(); menuEl = null; }
document.addEventListener('keydown', (e) => { if (e.key === 'Escape' && menuEl) { e.stopPropagation(); closeMenu(); } }, true);
document.addEventListener('mousedown', (e) => { if (menuEl && !menuEl.contains(e.target)) closeMenu(); });

// menu shows a small popover under anchor. items: [{label, sub, onPick, disabled}]; custom: optional {placeholder, onSubmit}.
export function menu(anchor, { title, items, custom }) {
  closeMenu();
  menuEl = document.createElement('div');
  menuEl.className = 'rv-menu';
  menuEl.innerHTML = `${title ? `<div class="rv-menu-title">${esc(title)}</div>` : ''}
    ${items.map((it, i) => `<button data-i="${i}" ${it.disabled ? 'disabled' : ''}><span>${esc(it.label)}</span>${it.sub ? `<small>${esc(it.sub)}</small>` : ''}</button>`).join('')}
    ${custom ? `<form class="rv-menu-custom"><input type="number" min="1" max="65535" placeholder="${esc(custom.placeholder)}"><button class="btn sm primary">Go</button></form>` : ''}`;
  document.body.appendChild(menuEl);
  const r = anchor.getBoundingClientRect();
  const w = menuEl.offsetWidth;
  menuEl.style.top = `${Math.min(r.bottom + 6, innerHeight - menuEl.offsetHeight - 8)}px`;
  menuEl.style.left = `${Math.max(8, Math.min(r.left, innerWidth - w - 8))}px`;
  menuEl.querySelectorAll('button[data-i]').forEach((b) => b.onclick = () => { const it = items[+b.dataset.i]; closeMenu(); it.onPick(); });
  const form = menuEl.querySelector('form');
  if (form) {
    form.onsubmit = (e) => { e.preventDefault(); const v = +form.querySelector('input').value; if (v > 0 && v < 65536) { closeMenu(); custom.onSubmit(v); } };
    if (!items.length) form.querySelector('input').focus();
  }
}

// pickForward starts a forward right away for a single port, otherwise asks which one.
export function pickForward(anchor, target, ports) {
  const uniq = [...new Map(ports.map((p) => [p.port, p])).values()];
  if (uniq.length === 1 && !anchor.dataset.always) return startForward({ ...target, port: uniq[0].port });
  menu(anchor, {
    title: uniq.length ? 'Forward which port?' : 'No ports declared — which port?',
    items: uniq.map((p) => ({ label: `:${p.port}`, sub: p.label || '', onPick: () => startForward({ ...target, port: p.port }) })),
    custom: { placeholder: 'Other port…', onSubmit: (port) => startForward({ ...target, port }) },
  });
}

export async function openShell(t) {
  try {
    const res = await api('/api/shell', { method: 'POST', body: JSON.stringify(t) });
    if (res.opened) say(`Opening a shell in ${t.container} on Terminal`);
    else { await copy(res.command); say(`${res.error} — command copied`); }
  } catch (e) {
    say(e.message.replace(/^\d+ /, '').trim().slice(0, 160));
  }
}

// pickShell offers containers (and pods, for workloads). choices: [{pod, container, status}]
export function pickShell(anchor, base, choices) {
  const runnable = choices.filter((c) => c.running);
  if (choices.length === 1 && runnable.length === 1) return openShell({ ...base, pod: runnable[0].pod, container: runnable[0].container });
  const multiPod = new Set(choices.map((c) => c.pod)).size > 1;
  menu(anchor, {
    title: multiPod ? 'Shell into…' : 'Which container?',
    items: choices.slice(0, 12).map((c) => ({
      label: multiPod ? `${c.pod} › ${c.container}` : c.container,
      sub: c.running ? '' : c.status || 'not running',
      disabled: !c.running,
      onPick: () => openShell({ ...base, pod: c.pod, container: c.container }),
    })),
  });
}
