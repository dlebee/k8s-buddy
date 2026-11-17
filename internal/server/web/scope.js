// The cluster/namespace scope shared by every screen, remembered across reloads, plus its picker.
import { esc } from '/static/common.js';

const KEY = 'buddyScope';
// tags: the tag filter. Picking tags selects the clusters carrying them (so every screen, which
// filters by clusters, follows); picking clusters by hand clears the tag filter.
export const scope = { clusters: new Set(), namespaces: new Set(), tags: new Set() };

try {
  const saved = JSON.parse(localStorage.getItem(KEY) || '{}');
  scope.clusters = new Set(saved.clusters || []);
  scope.namespaces = new Set(saved.namespaces || []);
  scope.tags = new Set(saved.tags || []);
} catch { /* storage unavailable: start unscoped */ }

function save(notify = true) {
  try { localStorage.setItem(KEY, JSON.stringify({ clusters: [...scope.clusters], namespaces: [...scope.namespaces], tags: [...scope.tags] })); } catch { /* ignore */ }
  if (notify) document.dispatchEvent(new CustomEvent('buddy:scope'));
}

export function setScope(kind, values, { notify = true } = {}) {
  scope[kind] = new Set(values);
  if (kind === 'clusters') scope.tags = new Set();
  save(notify);
}

// setTags filters to the clusters carrying any of `tags`. tagMap: tag → cluster names.
export function setTags(tags, tagMap, { notify = true } = {}) {
  scope.tags = new Set(tags);
  scope.clusters = new Set([...scope.tags].flatMap((t) => tagMap[t] || []));
  save(notify);
}

// refreshTags re-derives the cluster selection after tags were edited (a cluster gained or lost one).
export function refreshTags(tagMap) {
  if (!scope.tags.size) return;
  const keep = [...scope.tags].filter((t) => tagMap[t]);
  setTags(keep, tagMap, { notify: false });
}

// tagPicker: a "# Tags" button that filters every screen by your cluster tags. With no tags yet it
// opens the tag editor instead.
export function tagPicker(tagMap, onManage) {
  const el = document.createElement('div');
  el.className = 'ms';
  const tags = Object.keys(tagMap).sort();
  const sel = [...scope.tags].filter((t) => tagMap[t]);
  const text = !tags.length ? 'Tags' : !sel.length ? 'All tags' : sel.length === 1 ? '#' + sel[0] : `${sel.length} tags`;
  el.innerHTML = `<button type="button" class="input ms-btn ${sel.length ? 'active' : ''}" title="${esc(tags.length ? 'Filter by your cluster tags' : 'Tag your clusters to filter by them')}">
    <span class="ms-ico">#</span><span class="ms-text">${esc(text)}</span><span class="ms-chev">${tags.length ? '▾' : '+'}</span></button>`;
  el.querySelector('button').onclick = (e) => {
    e.stopPropagation();
    if (!tags.length) return onManage();
    popClose?.();
    const draft = new Set(sel);
    pop = document.createElement('div');
    pop.className = 'ms-pop';
    const draw = () => {
      pop.innerHTML = `
        <div class="ms-actions"><button type="button" data-all>All tags</button><span class="ms-count">${draft.size ? `${draft.size} selected` : 'showing all clusters'}</span></div>
        <div class="ms-list">${tags.map((t) => `<label class="ms-item"><input type="checkbox" value="${esc(t)}" ${draft.has(t) ? 'checked' : ''}><span class="ms-name">#${esc(t)}</span><small>${tagMap[t].length} cluster${tagMap[t].length === 1 ? '' : 's'}</small><button type="button" class="ms-only" data-only="${esc(t)}">only</button></label>`).join('')}</div>
        <div class="ms-foot"><button type="button" class="btn sm" data-manage>Manage tags…</button><span class="spacer"></span><button type="button" class="btn sm primary" data-apply>Apply</button></div>`;
      pop.querySelectorAll('input').forEach((cb) => cb.onchange = () => { cb.checked ? draft.add(cb.value) : draft.delete(cb.value); draw(); });
      pop.querySelectorAll('[data-only]').forEach((b) => b.onclick = (ev) => { ev.preventDefault(); draft.clear(); draft.add(b.dataset.only); apply(); });
      pop.querySelector('[data-all]').onclick = () => { draft.clear(); draw(); };
      pop.querySelector('[data-apply]').onclick = apply;
      pop.querySelector('[data-manage]').onclick = () => { close(); onManage(); };
    };
    const changed = () => draft.size !== sel.length || [...draft].some((t) => !sel.includes(t));
    const close = () => { pop?.remove(); pop = null; popClose = null; document.removeEventListener('mousedown', outside, true); };
    const apply = () => { const c = changed(); close(); if (c) setTags([...draft], tagMap); };
    const outside = (ev) => { if (pop && !pop.contains(ev.target) && !el.contains(ev.target)) apply(); };
    popClose = close;
    document.body.appendChild(pop);
    draw();
    const r = el.getBoundingClientRect();
    pop.style.top = `${r.bottom + 6}px`;
    pop.style.left = `${Math.max(8, Math.min(r.left, innerWidth - pop.offsetWidth - 8))}px`;
    document.addEventListener('mousedown', outside, true);
  };
  return el;
}

export const scopeParam = (kind) => [...scope[kind]].join(',');
export const inScope = (kind, value) => !scope[kind].size || !value || scope[kind].has(value);

// pruneClusters drops selected clusters that no longer exist (removed from kubeconfig or excluded).
export function pruneClusters(names) {
  const valid = new Set(names);
  const kept = [...scope.clusters].filter((c) => valid.has(c));
  if (kept.length !== scope.clusters.size) setScope('clusters', kept, { notify: false });
}

const NOUN = { clusters: 'cluster', namespaces: 'namespace' };

// picker returns a button that opens a searchable multi-select. Changes apply when it closes, so
// ticking several boxes reloads the view once. options: [{ value, sub }]
export function picker(kind, options, { disabled = false, note = '' } = {}) {
  const el = document.createElement('div');
  el.className = 'ms';
  const sel = scope[kind];
  const text = !sel.size ? `All ${NOUN[kind]}s` : sel.size === 1 ? [...sel][0] : `${sel.size} ${NOUN[kind]}s`;
  el.innerHTML = `<button type="button" class="input ms-btn ${sel.size ? 'active' : ''}" ${disabled ? 'disabled' : ''}
      title="${esc(disabled ? note : [...sel].join(', ') || `All ${NOUN[kind]}s`)}">
      <span class="ms-ico">${kind === 'clusters' ? '⎈' : '▤'}</span><span class="ms-text">${esc(text)}</span><span class="ms-chev">▾</span></button>`;
  if (!disabled) el.querySelector('button').onclick = (e) => { e.stopPropagation(); open(el, kind, options); };
  return el;
}

let pop, popClose;

function open(anchor, kind, options) {
  popClose?.();
  const draft = new Set(scope[kind]);
  // Keep selections that this screen has no data for, so they aren't lost on the next screen.
  const extra = [...draft].filter((v) => !options.some((o) => o.value === v)).map((value) => ({ value, sub: 'not in this view' }));
  const all = [...extra, ...options];
  pop = document.createElement('div');
  pop.className = `ms-pop ms-pop-${kind}`;
  pop.innerHTML = `
    <input class="ms-search" placeholder="Filter ${NOUN[kind]}s…" spellcheck="false">
    <div class="ms-actions"><button type="button" data-all>All ${NOUN[kind]}s</button><span class="ms-count"></span></div>
    <div class="ms-tags"></div>
    <div class="ms-list"></div>
    <div class="ms-foot"><button type="button" class="btn sm" data-cancel>Cancel</button><button type="button" class="btn sm primary" data-apply>Apply</button></div>`;
  document.body.appendChild(pop);
  const r = anchor.getBoundingClientRect();
  pop.style.top = `${r.bottom + 6}px`;
  pop.style.left = `${Math.max(8, Math.min(r.left, innerWidth - pop.offsetWidth - 8))}px`;

  const listEl = pop.querySelector('.ms-list');
  // Tags (your own labels per cluster, from Settings) select every cluster that carries them.
  const tagsEl = pop.querySelector('.ms-tags');
  const tagMap = new Map();
  for (const o of all) for (const t of o.tags || []) { if (!tagMap.has(t)) tagMap.set(t, []); tagMap.get(t).push(o.value); }
  const drawTags = () => {
    tagsEl.hidden = !tagMap.size;
    tagsEl.innerHTML = [...tagMap].sort().map(([t, vs]) => `<button type="button" class="ms-tag ${vs.every((v) => draft.has(v)) ? 'on' : ''}" data-tag="${esc(t)}" title="${esc(vs.join(', '))}">#${esc(t)} <small>${vs.length}</small></button>`).join('');
    tagsEl.querySelectorAll('[data-tag]').forEach((b) => b.onclick = () => {
      const vs = tagMap.get(b.dataset.tag);
      if (vs.every((v) => draft.has(v))) vs.forEach((v) => draft.delete(v)); else vs.forEach((v) => draft.add(v));
      draw();
    });
  };
  const search = pop.querySelector('.ms-search');
  const count = () => { pop.querySelector('.ms-count').textContent = draft.size ? `${draft.size} selected` : 'showing all'; };
  const draw = () => {
    const q = search.value.toLowerCase();
    const shown = all.filter((o) => !q || o.value.toLowerCase().includes(q) || (o.tags || []).some((t) => t.includes(q.replace(/^#/, ''))));
    listEl.innerHTML = shown.length ? shown.map((o) => `
      <label class="ms-item ${o.sub === 'not in this view' ? 'dim' : ''}">
        <input type="checkbox" value="${esc(o.value)}" ${draft.has(o.value) ? 'checked' : ''}>
        <span class="ms-main"><span class="ms-name" title="${esc(o.value)}">${esc(o.value)}</span>${o.sub ? `<small>${esc(o.sub)}</small>` : ''}
          ${(o.tags || []).length ? `<span class="ms-meta">${o.tags.map((t) => `<span class="ms-chip">#${esc(t)}</span>`).join('')}</span>` : ''}</span>
        <button type="button" class="ms-only" data-only="${esc(o.value)}">only</button>
      </label>`).join('') : `<div class="ms-empty">No ${NOUN[kind]}s match</div>`;
    listEl.querySelectorAll('input').forEach((cb) => cb.onchange = () => { cb.checked ? draft.add(cb.value) : draft.delete(cb.value); count(); drawTags(); });
    drawTags();
    listEl.querySelectorAll('[data-only]').forEach((b) => b.onclick = (e) => { e.preventDefault(); draft.clear(); draft.add(b.dataset.only); apply(); });
    count();
  };
  const changed = () => draft.size !== scope[kind].size || [...draft].some((v) => !scope[kind].has(v));
  const close = () => { pop?.remove(); pop = null; popClose = null; document.removeEventListener('mousedown', outside, true); document.removeEventListener('keydown', keys, true); };
  const apply = () => { const c = changed(); close(); if (c) setScope(kind, draft); };
  const outside = (e) => { if (pop && !pop.contains(e.target) && !anchor.contains(e.target)) apply(); };
  const keys = (e) => {
    if (e.key === 'Escape') { e.stopPropagation(); close(); }
    if (e.key === 'Enter' && document.activeElement === search) { e.preventDefault(); apply(); }
  };
  popClose = close;
  pop.querySelector('[data-all]').onclick = () => { draft.clear(); draw(); };
  pop.querySelector('[data-cancel]').onclick = close;
  pop.querySelector('[data-apply]').onclick = apply;
  search.oninput = draw;
  document.addEventListener('mousedown', outside, true);
  document.addEventListener('keydown', keys, true);
  draw();
  search.focus();
}

// scopeChip summarises an active scope with a one-click reset, for screens that are filtered by it.
export function scopeChip(total) {
  const n = scope.clusters.size;
  if (!n) return '';
  return `<div class="scope-chip">Showing ${n} of ${total} clusters <button type="button" data-scope-clear>Show all</button></div>`;
}
