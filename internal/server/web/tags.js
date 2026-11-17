// Tag editor: your own labels per cluster, stored only in k8s-buddy's settings (never in the
// kubeconfig). Opened from the Tags filter, the Overview cards and Settings.
import { api, esc } from '/static/common.js';

const clean = (t) => t.toLowerCase().trim().replace(/[^a-z0-9._:-]+/g, '-').replace(/^-+|-+$/g, '').slice(0, 40);
const parse = (s) => [...new Set(s.split(/[,\s]+/).map(clean).filter(Boolean))];

// openTagEditor shows every cluster with a tag field. `focus` scrolls to and focuses one cluster.
export async function openTagEditor({ clusters, focus } = {}) {
  const settings = await api('/api/settings');
  const current = settings.clusterTags || {};
  const names = clusters.map((c) => c.name).sort();
  const allTags = () => [...new Set(Object.values(draft).flat())].sort();
  const draft = Object.fromEntries(names.map((n) => [n, [...(current[n] || [])]]));

  document.querySelector('.tag-modal')?.remove();
  const m = document.createElement('div');
  m.className = 'tag-modal';
  m.innerHTML = `<div class="tag-scrim" data-x></div>
    <div class="tag-box" role="dialog" aria-label="Cluster tags">
      <div class="tag-head"><b>Cluster tags</b><span class="muted">Your own labels, kept only in k8s-buddy on this computer (never in your kubeconfig). Use them to filter every screen and to group clusters in Cost.</span><button class="btn sm" data-x aria-label="Close">✕</button></div>
      <input class="input tag-find" placeholder="Find a cluster…" spellcheck="false">
      <div class="tag-rows"></div>
      <div class="tag-foot"><span class="muted tag-hint">Type a tag and press Enter or comma. Click a suggestion to add it.</span><span class="spacer"></span><button class="btn sm" data-x>Cancel</button><button class="btn sm primary" data-save>Save tags</button></div>
    </div>`;
  document.body.appendChild(m);
  const rows = m.querySelector('.tag-rows'), find = m.querySelector('.tag-find');

  const draw = () => {
    const q = find.value.toLowerCase().trim();
    const tags = allTags();
    rows.innerHTML = names.filter((n) => !q || n.toLowerCase().includes(q) || draft[n].some((t) => t.includes(q))).map((n) => `
      <div class="tag-row" data-c="${esc(n)}">
        <div class="tag-name" title="${esc(n)}">${esc(n)}</div>
        <div class="tag-edit">
          ${draft[n].map((t) => `<span class="tag-chip">#${esc(t)}<button data-rm="${esc(t)}" title="Remove">×</button></span>`).join('')}
          <input class="tag-in2" placeholder="${draft[n].length ? '' : 'add tag…'}" spellcheck="false" autocomplete="off">
          ${tags.filter((t) => !draft[n].includes(t)).slice(0, 6).map((t) => `<button class="tag-sug" data-add="${esc(t)}">+${esc(t)}</button>`).join('')}
        </div>
      </div>`).join('') || '<div class="muted" style="padding:10px">No clusters match.</div>';
    rows.querySelectorAll('.tag-row').forEach((row) => {
      const n = row.dataset.c, input = row.querySelector('.tag-in2');
      const add = (vals) => { for (const v of vals) if (!draft[n].includes(v)) draft[n].push(v); draft[n].sort(); draw(); rows.querySelector(`.tag-row[data-c="${CSS.escape(n)}"] .tag-in2`)?.focus(); };
      input.onkeydown = (e) => {
        if ((e.key === 'Enter' || e.key === ',') && input.value.trim()) { e.preventDefault(); add(parse(input.value)); }
        if (e.key === 'Backspace' && !input.value && draft[n].length) { draft[n].pop(); draw(); rows.querySelector(`.tag-row[data-c="${CSS.escape(n)}"] .tag-in2`)?.focus(); }
      };
      input.onblur = () => { if (input.value.trim()) add(parse(input.value)); };
      row.querySelectorAll('[data-rm]').forEach((b) => b.onclick = () => { draft[n] = draft[n].filter((t) => t !== b.dataset.rm); draw(); });
      row.querySelectorAll('[data-add]').forEach((b) => b.onclick = () => add([b.dataset.add]));
    });
  };
  const close = () => { m.remove(); document.removeEventListener('keydown', esc_); };
  const esc_ = (e) => { if (e.key === 'Escape') close(); };
  document.addEventListener('keydown', esc_);
  m.querySelectorAll('[data-x]').forEach((b) => b.onclick = close);
  find.oninput = draw;
  m.querySelector('[data-save]').onclick = async () => {
    // Commit anything still typed in a field.
    m.querySelectorAll('.tag-row').forEach((row) => { const v = row.querySelector('.tag-in2').value; if (v.trim()) draft[row.dataset.c].push(...parse(v)); });
    const latest = await api('/api/settings');
    const clusterTags = { ...(latest.clusterTags || {}) };
    for (const n of names) { if (draft[n].length) clusterTags[n] = [...new Set(draft[n])]; else delete clusterTags[n]; }
    await api('/api/settings', { method: 'PUT', body: JSON.stringify({ ...latest, clusterTags }) });
    close();
    document.dispatchEvent(new CustomEvent('buddy:tags'));
  };
  draw();
  if (focus) {
    const row = rows.querySelector(`.tag-row[data-c="${CSS.escape(focus)}"]`);
    row?.scrollIntoView({ block: 'center' });
    row?.querySelector('.tag-in2')?.focus();
    row?.classList.add('focus');
  } else find.focus();
}
