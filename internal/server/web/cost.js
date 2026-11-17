// Cost: pick clusters by name, or tags (the clusters carrying all of them); those are studied for worker nodes that could be
// removed by moving their replicas onto other worker nodes. Up to 10 scenarios, compared side by
// side, each showing exactly what moves. A simulation only: nothing changes in any cluster.
import { api, esc } from '/static/common.js';

const usd = (v) => `$${Math.round(v).toLocaleString()}`;
const fmtCPU = (m) => (m >= 1000 ? `${+(m / 1000).toFixed(m >= 10000 ? 0 : 1)} core${m === 1000 ? '' : 's'}` : `${Math.round(m)}m`);
const fmtMem = (b) => { const u = ['B', 'Ki', 'Mi', 'Gi', 'Ti']; let i = 0; while (b >= 1024 && i < u.length - 1) { b /= 1024; i++; } return `${b >= 100 || i === 0 ? Math.round(b) : b.toFixed(1)}${u[i]}`; };
const pct = (a, b) => (b > 0 ? Math.round((a / b) * 100) : 0);
const EST = '<span class="cost-est" title="Estimated from public pay-as-you-go Linux list prices (Azure Retail Prices API, Linode public API) for the node\'s VM size and region. Reservations, savings plans, spot, discounts, disks and networking are not included.">est.</span>';

const KEY = 'buddy.cost.tags';
const savedTags = () => { try { return JSON.parse(localStorage.getItem(KEY) || '[]'); } catch { return []; } };
const saveTags = (t) => { try { localStorage.setItem(KEY, JSON.stringify(t)); } catch { /* ignore */ } };
const load = (k, d) => { try { return JSON.parse(localStorage.getItem(k)) ?? d; } catch { return d; } };
const store = (k, v) => { try { localStorage.setItem(k, JSON.stringify(v)); } catch { /* ignore */ } };
const NSKEY = 'buddy.cost.ns'; // [] = every app namespace (the default)
const savedNs = () => { try { return JSON.parse(localStorage.getItem(NSKEY) || '[]'); } catch { return []; } };
const saveNs = (v) => { try { localStorage.setItem(NSKEY, JSON.stringify(v)); } catch { /* ignore */ } };

// nsPicker: which namespaces' pods the scenarios move. Default: every app namespace.
function nsPicker(r, chosen) {
  const label = !chosen.length ? `All app namespaces (${r.studiedNamespaces.length})` : chosen.length === 1 ? chosen[0] : `${chosen.length} namespaces`;
  return `<span class="colpick"><button class="btn sm ${chosen.length ? 'primary' : ''}" id="nsBtn" title="${esc(r.studiedNamespaces.join(', '))}">Namespaces: ${esc(label)} ▾</button>
    <div class="colpick-menu ns-menu" id="nsMenu" hidden>
      <div class="colpick-head">Namespaces to study<button class="btn sm" data-ns-reset>App only (default)</button></div>
      <input class="input" id="nsFind" placeholder="Filter…" style="margin:0 6px 6px;width:calc(100% - 12px)" spellcheck="false">
      <div id="nsList">${r.namespaces.map((n) => `<label data-name="${esc(n.name)}"><input type="checkbox" value="${esc(n.name)}" ${r.studiedNamespaces.includes(n.name) ? 'checked' : ''}> ${esc(n.name)}${n.system ? ' <span class="muted">system</span>' : ''}<span class="spacer"></span><small class="muted">${n.pods} pod${n.pods === 1 ? '' : 's'}</small></label>`).join('')}</div>
      <div class="colpick-head"><span></span><button class="btn sm primary" data-ns-apply>Apply</button></div>
    </div></span>`;
}

// What makes a scenario different, in plain words, for the comparison table.
const CONF = { high: 'High', medium: 'Medium', low: 'Low' };
const confBadge = (s) => `<span class="conf conf-${s.confidence}" title="${esc(s.why.length ? s.why.map((w) => w.text).join('\n') : (s.basis || []).join(' · '))}">${CONF[s.confidence] || s.confidence} confidence</span>`;

function traits(s) {
  const t = [];
  for (const w of s.why.filter((x) => x.short)) t.push(`<span class="${w.level === 'low' ? 'sc-bad' : 'sc-warn'}">${esc(w.short)}</span>`);
  t.push(s.strategy === 'in-cluster' ? '<span class="sc-ok">stays in cluster</span>' : `<span class="sc-warn">${s.crossCluster} replica${s.crossCluster === 1 ? '' : 's'} → ${s.targetsUsed.map(esc).join(', ')}</span>`);
  if (s.disksMoved) t.push(`${s.disksMoved} disk${s.disksMoved === 1 ? '' : 's'} re-attach`);
  if (s.dataMigration) t.push(`<span class="sc-warn">${s.dataMigration} volume${s.dataMigration === 1 ? '' : 's'} to migrate</span>`);
  if (s.outages) t.push(`<span class="sc-warn">${s.outages} brief outage${s.outages === 1 ? '' : 's'}</span>`);
  return t.join(' · ');
}

const bar = (v, cap) => `<span class="pbar"><i style="width:${Math.min(100, pct(v, cap))}%"></i></span>`;

function detail(s) {
  const vols = s.moves.flatMap((m) => (m.volumes || []).map((v) => ({ ...v, pod: m.pod, cross: m.crossCluster })));
  const byWl = {};
  for (const m of s.moves) (byWl[`${m.namespace}/${m.kind}/${m.workload}`] ||= []).push(m);
  return `<div class="sc-detail">
    <div class="conf-box conf-box-${s.confidence}"><div>${confBadge(s)}${s.priced ? ` <span class="muted">· saves ≈ ${usd(s.savings)}/mo ${EST} · ranked by ≈ ${usd(s.weight)}/mo after confidence</span>` : ''}</div>
      ${s.why.length ? `<ul>${s.why.map((w) => `<li class="${w.level === 'low' ? 'sc-bad' : 'sc-warn'}">${esc(w.text)}</li>`).join('')}</ul>` : ''}
      ${(s.basis || []).length ? `<div class="muted small">Backed by: ${s.basis.map(esc).join(' · ')}</div>` : ''}</div>
    <h4>1 · Drain ${s.drain.length === 1 ? 'this worker node' : `these ${s.drain.length} worker nodes`} <span class="muted">from ${esc(s.cluster)} / ${esc(s.pool)} (${s.nodesFrom} → ${s.nodesTo} nodes)</span></h4>
    <table class="ptable"><thead><tr><th>Node</th><th>Replicas on it</th><th>Disks attached</th></tr></thead><tbody>
      ${s.drain.map((d) => `<tr><td class="mono">${esc(d.name)}</td><td>${d.pods}</td><td>${d.disks}</td></tr>`).join('')}</tbody></table>
    <h4>2 · Exactly what moves <span class="muted">${s.replicas} replica${s.replicas === 1 ? '' : 's'} of ${s.workloads} workload${s.workloads === 1 ? '' : 's'}</span></h4>
    <table class="ptable"><thead><tr><th>Workload · pod</th><th>Requests</th><th>Mem limit</th><th>Disks</th><th>From → to</th></tr></thead><tbody>
      ${Object.values(byWl).map((ms) => ms.map((m, i) => `<tr class="${i ? 'cont' : ''}">
        <td>${i ? '' : `<b>${esc(m.workload)}</b> <span class="muted">${esc(m.kind)} · ${esc(m.namespace)} · ${ms.length} of ${m.replicas} replica${m.replicas === 1 ? '' : 's'}</span><br>`}<span class="mono small">${esc(m.pod)}</span></td>
        <td>${fmtCPU(m.cpu)} · ${fmtMem(m.mem)}</td><td>${fmtMem(m.memLim)}</td><td>${m.disks || '–'}</td>
        <td class="mono small">${esc(m.fromNode)} → ${m.crossCluster ? `<b class="sc-warn">${esc(m.toCluster)}</b> / ` : ''}${esc(m.toNode)}</td></tr>`).join('')).join('')}
    </tbody></table>
    ${vols.length ? `<h4>Volumes <span class="muted">disks follow their pod within the cluster; across clusters the data must be copied</span></h4>
      <table class="ptable"><thead><tr><th>Claim</th><th>Pod</th><th>Size</th><th>Storage class</th><th></th></tr></thead><tbody>
      ${vols.map((v) => `<tr><td class="mono">${esc(v.claim)}</td><td class="mono small">${esc(v.pod)}</td><td>${esc(v.size || '?')}</td><td>${esc(v.storageClass || '–')}</td><td>${v.cross ? '<span class="sc-warn">migrate data</span>' : v.disk ? 're-attaches' : ''}</td></tr>`).join('')}</tbody></table>` : ''}
    <h4>3 · Receiving nodes, before → after</h4>
    <table class="ptable"><thead><tr><th>Node</th><th>Gets</th><th>CPU requested</th><th>Memory requested</th><th>Mem limits</th><th>Disks</th></tr></thead><tbody>
      ${s.after.map((a) => `<tr><td class="mono small">${a.cluster !== s.cluster ? `<b>${esc(a.cluster)}</b> / ` : ''}${esc(a.name)}</td><td>+${a.gains}</td>
        <td>${bar(a.cpu, a.allocCpu)} ${pct(a.cpuBefore, a.allocCpu)}% → <b>${pct(a.cpu, a.allocCpu)}%</b> <span class="muted">of ${fmtCPU(a.allocCpu)}</span></td>
        <td>${bar(a.mem, a.allocMem)} ${pct(a.memBefore, a.allocMem)}% → <b>${pct(a.mem, a.allocMem)}%</b> <span class="muted">of ${fmtMem(a.allocMem)}</span></td>
        <td class="${a.memLim > a.allocMem ? 'warn' : ''}" title="Sum of memory limits on the node vs its allocatable memory">${pct(a.memLim, a.allocMem)}%</td>
        <td class="${a.diskLimit > 0 && a.disks >= a.diskLimit ? 'bad' : a.diskLimit > 0 && a.disks >= a.diskLimit * 0.8 ? 'warn' : ''}">${a.disksBefore} → <b>${a.disks}</b>${a.diskLimit >= 0 ? ` / ${a.diskLimit}` : ''}</td></tr>`).join('')}
    </tbody></table>
    <h4>Notes</h4><ul class="plan-notes">${s.notes.map((n) => `<li>${esc(n)}</li>`).join('')}</ul>
    <h4>Commands <span class="muted">(suggestions to run yourself; nothing runs from here)</span></h4>
    <div class="plan-cmds">${s.commands.map((c) => `<div class="cmd"><pre class="code">${esc(c)}</pre><button class="btn sm" data-copy="${esc(c)}">Copy</button></div>`).join('')}</div>
  </div>`;
}

export async function costView(view, head, current) {
  const picked = new Set(savedTags());
  const pickedClusters = new Set(load('buddy.cost.clusters', []));
  let mode = load('buddy.cost.mode', ''); // 'tags' | 'clusters'; '' = tags when any exist
  let chosenNs = savedNs();
  const bindCopy = () => view.querySelectorAll('[data-copy]').forEach((b) => b.onclick = (e) => {
    e.stopPropagation();
    navigator.clipboard?.writeText(b.dataset.copy);
    b.textContent = 'Copied';
    setTimeout(() => { b.textContent = 'Copy'; }, 1200);
  });
  const draw = async () => {
    const qs = new URLSearchParams();
    const byName = mode === 'clusters';
    if (byName) for (const c of pickedClusters) qs.append('cluster', c);
    else for (const t of picked) qs.append('tag', t);
    for (const n of chosenNs) qs.append('ns', n);
    const body = view.querySelector('#scBody');
    if (body) body.innerHTML = '<div class="card empty">Simulating…</div>';
    const r = await api('/api/cost/scenarios?' + qs);
    if (current() !== 'cost') return;
    for (const t of [...picked]) if (!r.tags[t]) picked.delete(t); // tags that no longer exist
    saveTags([...picked]);
    for (const c of [...pickedClusters]) if (!r.available.includes(c)) pickedClusters.delete(c);
    store('buddy.cost.clusters', [...pickedClusters]);
    const tags = Object.entries(r.tags).sort(([a], [b]) => a.localeCompare(b));
    if (!mode) { mode = tags.length ? 'tags' : 'clusters'; if (mode === 'clusters') return draw(); }
    const any = mode === 'clusters' ? pickedClusters.size : picked.size;
    const studied = any ? `<div class="sc-studied">Studying <b>${r.clusters.length}</b> cluster${r.clusters.length === 1 ? '' : 's'}${r.clusters.length ? `: ${r.clusters.map(esc).join(', ')}` : mode === 'tags' ? ' (no cluster has all of these tags)' : ' (not connected yet)'}</div>
      ${r.clusters.length ? `<div class="sc-ns">${nsPicker(r, chosenNs)} <span class="muted">Only these namespaces' replicas are moved and listed; system and add-on namespaces are left out by default (their pods are still checked to fit).</span></div>` : ''}` : '';
    const tagPick = tags.length ? `<div class="fold-q">Pick tags. Studied clusters must have <b>all</b> the tags you pick (they can have others too).</div>
          <div class="fold-tags">${tags.map(([t, cs]) => `<button class="ms-tag ${picked.has(t) ? 'on' : ''}" data-t="${esc(t)}" title="${esc(cs.join(', '))}">#${esc(t)} <small>${cs.length}</small></button>`).join('')}</div>`
      : '<div class="fold-q">No tags yet. Tag your clusters (e.g. by environment, team or region) to study groups of them, or pick clusters by name.</div><button class="btn sm" data-tag-edit="">Tag clusters…</button>';
    const clusterPick = `<div class="fold-q">Pick the clusters to study. Replicas can move between nodes of a cluster, or onto the other clusters you pick.
        <span class="sc-quick"><button class="linkish" data-call>All</button> · <button class="linkish" data-cnone>None</button></span></div>
      <div class="fold-tags">${r.available.map((c) => `<button class="ms-tag ${pickedClusters.has(c) ? 'on' : ''}" data-c="${esc(c)}">${esc(c)}</button>`).join('')}</div>`;
    view.innerHTML = `
      ${head('Cost', 'Find worker nodes you could remove by moving their replicas onto other worker nodes. A simulation on the pods running now: nothing is changed.')}
      <div class="card fold-pick">
        <div class="seg sc-mode"><button class="${mode === 'tags' ? 'on' : ''}" data-mode="tags">By tags</button><button class="${mode === 'clusters' ? 'on' : ''}" data-mode="clusters">By cluster</button></div>
        ${mode === 'clusters' ? clusterPick : tagPick}
        ${studied}
      </div>
      <div id="scBody">${!any ? '' : !r.scenarios.length
        ? `<div class="card empty"><span class="big">✨</span>No worker node can be removed from ${r.clusters.length === 1 ? 'this cluster' : 'these clusters'}: in every case some replica, disk or memory limit doesn't fit elsewhere.</div>`
        : `<div class="cost-note">💡 Savings are <b>estimates</b> from public list prices for the node's VM size and region. To look a price up, k8s-buddy sends that VM size and region (and nothing else about your clusters) to the Azure Retail Prices API; Linode's full price list is downloaded instead. A simulation only: run the commands yourself if you agree. Each scenario is independent: after doing one, re-check the others.</div>
          <p class="muted plans-intro">${r.scenarios.length} scenario${r.scenarios.length === 1 ? '' : 's'}${r.considered > r.scenarios.length ? ` (best of ${r.considered} that fit)` : ''}, ranked by weight: the saving scaled by confidence (high ×1, medium ×0.6, low ×0.25), so a big saving that loses high availability ranks below a safer one. Confidence drops for losing high availability (pools under 2 nodes, system pools under 3), thin usage history, recorded peaks near capacity, single replicas, PodDisruptionBudgets that block the drain, and replicas or zones losing their spread. Every replica lands where its requests fit with 15% headroom, memory limits stay within the overcommit its cluster already runs with, its node selector, affinity and taints allow it, and each disk gets an attach slot under the VM's limit.</p>
          <div class="card sc-table"><table><thead><tr><th>#</th><th>Remove</th><th>From</th><th>Saves ${EST}</th><th>Replicas move</th><th>What's different</th></tr></thead><tbody>
          ${r.scenarios.map((s, i) => `<tr data-sc="${esc(s.id)}" class="${i === 0 ? 'on' : ''}"><td>${i + 1}</td>
            <td><b>${s.drain.length} node${s.drain.length === 1 ? '' : 's'}</b><div class="muted mono small">${s.drain.map((d) => esc(d.name.replace(/^aks-/, ''))).join(', ')}</div></td>
            <td>${esc(s.cluster)}<div class="muted">${esc(s.pool)} · ${s.nodesFrom} → ${s.nodesTo}${s.sku ? ` · ${esc(s.sku)}` : ''}</div></td>
            <td>${s.priced ? `<b class="ok">≈ ${usd(s.savings)}/mo</b>` : '<span class="muted">no price</span>'}<div>${confBadge(s)}</div></td>
            <td><b>${s.replicas}</b> replica${s.replicas === 1 ? '' : 's'}<div class="muted">${s.workloads} workload${s.workloads === 1 ? '' : 's'}</div></td>
            <td class="sc-traits">${traits(s)}</td></tr>`).join('')}
          </tbody></table></div>
          <div id="scDetail">${detail(r.scenarios[0])}</div>`}</div>`;
    const nsBtn = view.querySelector('#nsBtn'), nsMenu = view.querySelector('#nsMenu');
    if (nsBtn) {
      nsBtn.onclick = (e) => { e.stopPropagation(); nsMenu.hidden = !nsMenu.hidden; nsMenu.querySelector('#nsFind').focus(); };
      nsMenu.onclick = (e) => e.stopPropagation();
      document.addEventListener('click', () => { nsMenu.hidden = true; }, { once: true });
      nsMenu.querySelector('#nsFind').oninput = (e) => { const q = e.target.value.toLowerCase(); nsMenu.querySelectorAll('#nsList label').forEach((l) => { l.hidden = !l.dataset.name.toLowerCase().includes(q); }); };
      nsMenu.querySelector('[data-ns-reset]').onclick = () => { chosenNs = []; saveNs(chosenNs); draw(); };
      nsMenu.querySelector('[data-ns-apply]').onclick = () => {
        const sel = [...nsMenu.querySelectorAll('#nsList input:checked')].map((c) => c.value);
        const defaults = r.namespaces.filter((n) => !n.system).map((n) => n.name);
        chosenNs = sel.length === defaults.length && sel.every((n) => defaults.includes(n)) ? [] : sel;
        saveNs(chosenNs);
        draw();
      };
    }
    view.querySelectorAll('[data-mode]').forEach((b) => b.onclick = () => { mode = b.dataset.mode; store('buddy.cost.mode', mode); draw(); });
    view.querySelectorAll('[data-c]').forEach((b) => b.onclick = () => { pickedClusters.has(b.dataset.c) ? pickedClusters.delete(b.dataset.c) : pickedClusters.add(b.dataset.c); store('buddy.cost.clusters', [...pickedClusters]); draw(); });
    const setAll = (v) => { pickedClusters.clear(); v.forEach((c) => pickedClusters.add(c)); store('buddy.cost.clusters', v); draw(); };
    view.querySelector('[data-call]')?.addEventListener('click', () => setAll(r.available));
    view.querySelector('[data-cnone]')?.addEventListener('click', () => setAll([]));
    view.querySelectorAll('[data-t]').forEach((b) => b.onclick = () => { picked.has(b.dataset.t) ? picked.delete(b.dataset.t) : picked.add(b.dataset.t); saveTags([...picked]); draw(); });
    view.querySelectorAll('[data-sc]').forEach((tr) => tr.onclick = () => {
      const s = r.scenarios.find((x) => x.id === tr.dataset.sc);
      view.querySelectorAll('[data-sc]').forEach((x) => x.classList.toggle('on', x === tr));
      view.querySelector('#scDetail').innerHTML = detail(s);
      bindCopy();
      view.querySelector('#scDetail').scrollIntoView({ block: 'start', behavior: 'smooth' });
    });
    bindCopy();
  };
  view.innerHTML = `${head('Cost', 'Find worker nodes you could remove by moving their replicas onto other worker nodes.')}<div class="card empty">Loading…</div>`;
  await draw();
}
