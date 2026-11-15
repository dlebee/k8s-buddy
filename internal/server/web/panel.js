import { api, post, stream, esc, ago, copy, debounce } from '/static/common.js';

let scope = 'pending';
const open = new Set();
const list = document.getElementById('list');

async function renderAzure() {
  const az = await api('/api/azure').catch(() => null);
  const el = document.getElementById('azure');
  el.hidden = !az || (!az.needed && !az.login.running);
  if (el.hidden) return;
  el.innerHTML = `<span>🔑</span><div><b>Azure sign-in expired</b>${az.clusters.length} cluster${az.clusters.length === 1 ? '' : 's'} waiting${az.login.error ? ` · <span style="color:#ff453a">${esc(az.login.error)}</span>` : ''}</div>
    <button ${az.login.running ? 'disabled' : ''}>${az.login.running ? 'Check your browser…' : 'Sign in'}</button>`;
  el.querySelector('button').onclick = async () => { await post('/api/azure/login'); renderAzure(); };
  if (az.login.running) setTimeout(renderAzure, 2000);
}

async function render() {
  renderAzure();
  const [alerts, sum] = await Promise.all([api(`/api/alerts?scope=${scope}&limit=300`), api('/api/summary')]);
  document.getElementById('sub').textContent = sum.pending ? `· ${sum.pending} open on ${sum.clusters} cluster${sum.clusters > 1 ? 's' : ''}` : '· all quiet';
  if (!alerts.length) {
    list.innerHTML = `<div class="empty"><span class="big">${scope === 'pending' ? '😌' : '🗂️'}</span>${scope === 'pending' ? 'Nothing needs your attention.' : 'No alerts yet.'}</div>`;
    return;
  }
  list.innerHTML = alerts.map((a) => `
    <div class="item sev-${a.severity} ${a.resolved ? 'resolved' : ''} ${open.has(a.id) ? 'open' : ''}" data-id="${a.id}">
      <div class="bar"></div>
      <div class="meta"><span class="chip" title="${esc(a.cluster)}">${esc(a.cluster)}</span>
        ${a.namespace ? `<span>${esc(a.namespace)}</span>` : ''}
        <span>${a.resolved ? 'resolved ' + ago(a.resolvedAt) + ' ago' : ago(a.lastSeen) + ' ago'}</span>
        ${a.count > 1 ? `<span>×${a.count}</span>` : ''}</div>
      <div class="title">${esc(a.title)}</div>
      <div>
        ${a.detail ? `<div class="detail">${esc(a.detail)}</div>` : ''}
        <div class="more">
          ${a.hint ? `<pre>${esc(a.hint)}</pre>` : ''}
          <div class="actions">
            ${a.hint ? `<button data-act="copy">Copy commands</button>` : ''}
            ${!a.resolved ? `<button data-act="ack">Snooze</button>` : ''}
            <button data-act="mute" title="Never show this exact alert again">Mute</button>
          </div>
        </div>
      </div>
    </div>`).join('');
  list.querySelectorAll('.item').forEach((el) => {
    const a = alerts.find((x) => x.id === el.dataset.id);
    el.addEventListener('click', async (e) => {
      const act = e.target.dataset?.act;
      if (act === 'copy') { await copy(a.hint.split('\n').filter((l) => l.startsWith('kubectl')).join('\n') || a.hint); e.target.textContent = 'Copied!'; return; }
      if (act === 'ack') { await post(`/api/alerts/${a.id}/ack`); return render(); }
      if (act === 'mute') { await post(`/api/alerts/${a.id}/mute`); return render(); }
      open.has(a.id) ? open.delete(a.id) : open.add(a.id);
      el.classList.toggle('open');
    });
  });
}

document.querySelectorAll('.tabs button').forEach((b) => b.addEventListener('click', () => {
  document.querySelectorAll('.tabs button').forEach((x) => x.classList.toggle('on', x === b));
  scope = b.dataset.scope;
  render();
}));
document.getElementById('close').onclick = () => post('/api/action/hide-panel');
document.getElementById('openOverview').onclick = () => post('/api/action/overview');
document.getElementById('settings').onclick = () => post('/api/action/overview?tab=settings');
document.getElementById('ackAll').onclick = async () => { await post('/api/alerts/ack-all'); render(); };
document.addEventListener('keydown', (e) => { if (e.key === 'Escape') post('/api/action/hide-panel'); });

const refresh = debounce(render, 300);
stream({ summary: refresh });
document.addEventListener('visibilitychange', () => { if (!document.hidden) render(); });
render();
