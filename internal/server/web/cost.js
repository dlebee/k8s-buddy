// Cost optimization: capacity vs requests vs real usage per node pool, how many nodes each pool
// needs, workloads to right-size, and tagged clusters that could fold into another. Dollar figures
// are estimates from public list prices.
import { api, esc } from '/static/common.js';
import { inScope } from '/static/scope.js';

const usd = (v) => `$${Math.round(v).toLocaleString()}`;
const cores = (m) => (m >= 1000 ? `${+(m / 1000).toFixed(m >= 10000 ? 0 : 1)}` : `${(m / 1000).toFixed(2)}`);
const gib = (b) => `${+(b / 1073741824).toFixed(b >= 10737418240 ? 0 : 1)}`;
const fmtCPU = (m) => (m >= 1000 ? `${+(m / 1000).toFixed(m >= 10000 ? 0 : 1)} cores` : `${Math.round(m)}m`);
const fmtMem = (b) => { const u = ['B', 'Ki', 'Mi', 'Gi', 'Ti']; let i = 0; while (b >= 1024 && i < u.length - 1) { b /= 1024; i++; } return `${b >= 100 || i === 0 ? Math.round(b) : b.toFixed(1)}${u[i]}`; };
const pct = (a, b) => (b > 0 ? Math.round((a / b) * 100) : 0);

const EST = '<span class="cost-est" title="Estimated from public pay-as-you-go Linux list prices (Azure Retail Prices API, Linode public API) for each node\'s VM size and region. Reservations, savings plans, spot, discounts, disks, networking and control-plane fees are not included.">est.</span>';

function bar(label, req, use, cap) {
  const r = pct(req, cap), u = pct(use, cap);
  return `<div class="cbar"><div class="cbar-top"><span>${label}</span><span class="muted">requested ${r}% · used ${u}%</span></div>
    <div class="cbar-track"><span class="req" style="width:${Math.min(100, r)}%"></span><span class="use" style="width:${Math.min(100, u)}%"></span></div></div>`;
}

function poolRow(p, hist) {
  const capCPU = p.nodeCpu * p.nodes, capMem = p.nodeMem * p.nodes;
  const dsCPU = p.dsCpu * p.nodes, dsMem = p.dsMem * p.nodes;
  const fewer = p.nodes - p.needByRequests, fewerU = p.nodes - p.needByUsage;
  const why = { cpu: 'CPU requests', memory: 'memory requests', disks: 'attached disks', minimum: 'keeping 2 nodes for redundancy' }[p.binding] || p.binding;
  let advice;
  if (fewer > 0) advice = `<b class="ok">Could run on ${p.needByRequests} node${p.needByRequests === 1 ? '' : 's'} instead of ${p.nodes}</b>${p.priced ? ` · saves ≈ ${usd(p.savings)}/mo ${EST}` : ''}<div class="muted">Pods' requests fit on ${p.needByRequests} after packing (limited by ${why}), keeping 15% headroom per node.</div>`;
  else if (hist && fewerU > 0) advice = `<b>Right-size first:</b> requests need all ${p.nodes} nodes, but real usage would fit on ${p.needByUsage}${p.priced ? ` (≈ ${usd(p.savingsByUsage)}/mo ${EST})` : ''}. Lower the over-provisioned requests below.`;
  else advice = `<span class="muted">Sized about right: requests need ${p.needByRequests} of ${p.nodes} node${p.nodes === 1 ? '' : 's'} (limited by ${why}).</span>`;
  return `<div class="cpool">
    <div class="cpool-head"><b>${esc(p.name)}</b><span class="muted">${p.nodes} × ${esc(p.sku || 'unknown size')}${p.mode ? ` · ${esc(p.mode)}` : ''}${p.region ? ` · ${esc(p.region)}` : ''}</span>
      <span class="spacer"></span>${p.priced ? `<span title="${esc(`$${p.hourlyPerNode}/h per node · ${p.priceSource}`)}">≈ ${usd(p.monthly)}/mo ${EST}</span>` : '<span class="muted" title="No public price list for this provider or size yet">no price</span>'}</div>
    <div class="cpool-bars">
      ${bar(`CPU <span class="muted">${cores(capCPU)} cores${dsCPU ? ` · ${cores(dsCPU)} in DaemonSets` : ''}</span>`, p.reqCpu + dsCPU, p.useCpu + dsCPU, capCPU)}
      ${bar(`Memory <span class="muted">${gib(capMem)} GiB${dsMem ? ` · ${gib(dsMem)} GiB in DaemonSets` : ''}</span>`, p.reqMem + dsMem, p.useMem + dsMem, capMem)}
      ${p.diskLimit > 0 ? `<div class="cbar"><div class="cbar-top"><span>Disks</span><span class="muted">${p.disks} of ${p.diskLimit * p.nodes} attachable</span></div><div class="cbar-track"><span class="req" style="width:${pct(p.disks, p.diskLimit * p.nodes)}%"></span></div></div>` : ''}
    </div>
    <div class="cpool-advice">${advice}</div></div>`;
}

function workloadRows(ws) {
  if (!ws.length) return '';
  return `<details class="cwl" ${ws.some((w) => w.direction === 'under') ? 'open' : ''}><summary>Workloads to right-size <span class="muted">${ws.length}</span></summary>
    <table><thead><tr><th>Workload</th><th>Pods</th><th>CPU request → suggested</th><th>Memory request → suggested</th><th>Seen (p95 / peak)</th></tr></thead><tbody>
    ${ws.map((w) => `<tr class="${w.direction}"><td><b>${esc(w.name)}</b><div class="muted">${esc(w.kind)} · ${esc(w.namespace)}</div>${w.direction === 'under' ? '<span class="pill warn">asks for less than it uses</span>' : ''}</td>
      <td>${w.pods}</td>
      <td>${fmtCPU(w.reqCpu)} → <b>${fmtCPU(w.suggestCpu)}</b>${w.freedCpu > 0 ? `<div class="muted">frees ${fmtCPU(w.freedCpu)}</div>` : ''}</td>
      <td>${fmtMem(w.reqMem)} → <b>${fmtMem(w.suggestMem)}</b>${w.freedMem > 0 ? `<div class="muted">frees ${fmtMem(w.freedMem)}</div>` : ''}</td>
      <td class="muted">CPU ${fmtCPU(w.p95Cpu)} / ${fmtCPU(w.maxCpu)}<br>Mem ${fmtMem(w.p95Mem)} / ${fmtMem(w.maxMem)}</td></tr>`).join('')}
    </tbody></table>
    <p class="muted rv-small">Suggested = 95th percentile of recorded usage + 30% (memory never below the recorded peak + 15%). Per pod.</p></details>`;
}

// ---------- plans ----------
const bar2 = (used, cap, cls = '') => `<span class="pbar ${cls}"><i style="width:${Math.min(100, pct(used, cap))}%"></i></span>`;
const toneDisks = (d, l) => (l > 0 ? (d >= l ? 'bad' : d >= l * 0.8 ? 'warn' : '') : '');

function planCard(p, open) {
  const shrink = p.type === 'shrink-pool';
  const title = shrink
    ? `Shrink <b>${esc(p.pool)}</b> in <b>${esc(p.cluster)}</b> from ${p.nodesFrom} to ${p.nodesTo} node${p.nodesTo === 1 ? '' : 's'}`
    : p.retire?.length
      ? `Keep <b>${esc(p.target)}</b>, retire ${p.retire.map((r) => `<b>${esc(r)}</b>`).join(', ')}`
      : `Move <b>${esc(p.cluster)}</b>'s workloads into <b>${esc(p.target)}</b> and retire ${esc(p.cluster)}`;
  const replicas = p.moves.reduce((a, m) => a + m.replicas, 0), disks = p.moves.reduce((a, m) => a + m.disks, 0);
  const vols = p.moves.flatMap((m) => (m.volumes || []).map((v) => ({ ...v, ns: m.namespace, wl: `${m.kind}/${m.name}` })));
  return `<details class="card plan" ${open ? 'open' : ''}>
    <summary><div class="plan-title">${shrink ? '🔻' : '🔀'} ${title}${p.tag ? p.tag.split(', ').map((t) => ` <span class="ms-chip">#${esc(t)}</span>`).join('') : ''}</div>
      <div class="plan-sub muted">${replicas} replica${replicas === 1 ? '' : 's'} of ${p.moves.length} workload${p.moves.length === 1 ? '' : 's'} move${disks ? ` · ${disks} disk${disks === 1 ? '' : 's'} re-attach` : ''}${!shrink && vols.length ? ` · ${vols.length} volume${vols.length === 1 ? '' : 's'} to migrate` : ''}${shrink && p.sku ? ` · ${esc(p.sku)}` : ''}</div>
      <div class="plan-save">${p.priced ? `<b class="ok">≈ ${usd(p.savings)}/mo</b> ${EST}` : '<span class="muted">no price</span>'}</div></summary>
    ${p.notes.filter((n) => n.startsWith('⚠')).map((n) => `<div class="plan-warn">${esc(n)}</div>`).join('')}
    ${shrink ? `<h4>1 · Drain ${p.drain.length === 1 ? 'this node' : `these ${p.drain.length} nodes`}</h4>
      <table class="ptable"><thead><tr><th>Node</th><th>Pods to move</th><th>Disks attached</th></tr></thead><tbody>
      ${p.drain.map((d) => `<tr><td class="mono">${esc(d.name)}</td><td>${d.pods}</td><td>${d.disks}</td></tr>`).join('')}</tbody></table>` : ''}
    <h4>${shrink ? '2 · ' : ''}What moves</h4>
    <table class="ptable"><thead><tr><th>Workload</th><th>Replicas</th><th>Per replica</th><th>Disks</th><th>From → to</th></tr></thead><tbody>
    ${p.moves.map((m) => `<tr><td><b>${esc(m.name)}</b><div class="muted">${esc(m.kind)} · ${esc(m.namespace)}${m.source ? ` · from <b>${esc(m.source)}</b>` : ''}</div></td>
      <td><b>${m.replicas}</b> of ${m.totalReplicas}${m.targetReplicas ? `<div class="merge">${esc(p.target)} runs ${m.targetReplicas}: scale it to <b>${m.targetReplicas + m.replicas}</b></div>` : ''}${shrink && m.replicas === m.totalReplicas && m.totalReplicas === 1 ? '<div class="muted">brief outage</div>' : ''}</td>
      <td>${fmtCPU(m.cpu)} · ${fmtMem(m.mem)}</td>
      <td>${m.disks || '–'}</td>
      <td class="mono small">${m.from.map(esc).join('<br>')}<div class="arrow">↓</div>${m.to.map(esc).join('<br>')}</td></tr>`).join('')}
    </tbody></table>
    ${vols.length ? `<h4>${shrink ? 'Disks that follow their pods' : 'Volumes with data to migrate'}</h4>
      <table class="ptable"><thead><tr><th>Claim</th><th>Workload</th><th>Size</th><th>Storage class</th></tr></thead><tbody>
      ${vols.map((v) => `<tr><td class="mono">${esc(v.claim)}</td><td class="muted">${esc(v.ns)} · ${esc(v.wl)}</td><td>${esc(v.size || '?')}</td><td>${esc(v.storageClass || '–')}${v.disk ? ' <span class="muted">disk</span>' : ''}</td></tr>`).join('')}</tbody></table>` : ''}
    <h4>${shrink ? '3 · Remaining nodes afterwards' : `${esc(p.target)} nodes afterwards`}</h4>
    <table class="ptable"><thead><tr><th>Node</th><th>Receives</th><th>CPU requested</th><th>Memory requested</th><th>Disks</th></tr></thead><tbody>
    ${p.after.map((a) => `<tr><td class="mono">${esc(a.name)}</td><td>${a.gains ? `+${a.gains} pod${a.gains === 1 ? '' : 's'}` : '–'}</td>
      <td>${bar2(a.cpu, a.allocCpu)} ${pct(a.cpu, a.allocCpu)}% <span class="muted">${fmtCPU(a.cpu)} / ${fmtCPU(a.allocCpu)}</span></td>
      <td>${bar2(a.mem, a.allocMem)} ${pct(a.mem, a.allocMem)}% <span class="muted">${fmtMem(a.mem)} / ${fmtMem(a.allocMem)}</span></td>
      <td class="${toneDisks(a.disks, a.diskLimit)}">${a.disks}${a.diskLimit >= 0 ? ` / ${a.diskLimit}` : ''}</td></tr>`).join('')}
    </tbody></table>
    ${p.merged?.length ? `<details class="plan-more"><summary>${p.merged.length} workload${p.merged.length === 1 ? '' : 's'} also run in ${esc(p.target)}: kept at full size and merged (target scales up)</summary><div class="mono small">${p.merged.map(esc).join('<br>')}</div></details>` : ''}
    ${p.skipped?.length ? `<details class="plan-more"><summary>${p.skipped.length} system workload${p.skipped.length === 1 ? '' : 's'} and cluster add-ons not moved: ${esc(p.target)} runs its own (DaemonSets, kube-*, cert-manager, ingress, monitoring…)</summary><div class="mono small">${p.skipped.map(esc).join('<br>')}</div></details>` : ''}
    <h4>Notes</h4><ul class="plan-notes">${p.notes.filter((n) => !n.startsWith('⚠')).map((n) => `<li>${esc(n)}</li>`).join('')}</ul>
    <h4>Commands <span class="muted">(suggested; nothing runs from here)</span></h4>
    <div class="plan-cmds">${p.commands.map((c) => `<div class="cmd"><pre class="code">${esc(c)}</pre><button class="btn sm" data-copy="${esc(c)}">Copy</button></div>`).join('')}</div>
  </details>`;
}

function plansHTML(rep, fold) {
  const shrink = rep.plans.filter((p) => p.type === 'shrink-pool' && inScope('clusters', p.cluster));
  const tags = Object.entries(fold.tags || {}).sort(([a], [b]) => a.localeCompare(b));
  const foldPlans = fold.plans || [];
  return `<p class="muted plans-intro">Every plan is simulated on the pods running now: each replica and attached disk gets a place, within 15% headroom and each VM's disk-attach limit. Plans are independent; after doing one, re-check the others.</p>
    <div class="section-title">Remove spare nodes <span class="muted">${shrink.length}</span></div>
    ${shrink.length ? shrink.map((p, i) => planCard(p, i === 0 && !fold.tag)).join('') : '<div class="card empty">No pool has a spare node: every one needs its nodes once pods are packed with headroom and disk limits.</div>'}
    <div class="section-title">Fold clusters together</div>
    <div class="card fold-pick">
      ${tags.length ? `<div class="fold-q">Which clusters could share? Pick a tag to see the alternatives:</div>
        <div class="fold-tags">${tags.map(([t, cs]) => `<button class="ms-tag ${fold.tag === t ? 'on' : ''}" data-fold="${esc(t)}" title="${esc(cs.join(', '))}">#${esc(t)} <small>${cs.length}</small></button>`).join('')}</div>`
        : `<div class="fold-q">Tag clusters that could share capacity (same environment, team or region) to compare folding them together.</div><button class="btn sm" data-tag-edit="">Tag clusters…</button>`}
    </div>
    ${fold.tag ? (foldPlans.length
      ? `<p class="muted plans-intro">${foldPlans.length} alternative${foldPlans.length === 1 ? '' : 's'} for <b>#${esc(fold.tag)}</b>, one per cluster you could keep. Each keeps every app replica (shared workloads scale up) and adds the retired clusters one by one while they still fit. Best saving first.</p>${foldPlans.map((p, i) => planCard(p, i === 0)).join('')}`
      : `<div class="card empty">None of the <b>#${esc(fold.tag)}</b> clusters can absorb another: there isn't enough room once every replica and disk is placed.</div>`) : ''}`;
}

export async function costView(view, head, current, params) {
  const tab = params?.get('tab') === 'plans' ? 'plans' : 'capacity';
  view.innerHTML = `${head('Cost optimization', 'Capacity vs what pods request and really use, per node pool. Advice only: nothing is changed.')}<div class="card empty">Working it out…</div>`;
  const rep = await api('/api/cost');
  if (current() !== 'cost') return;
  const nPlans = rep.plans.filter((p) => p.type === 'shrink-pool' && inScope('clusters', p.cluster)).length;
  const planSave = rep.plans.filter((p) => inScope('clusters', p.cluster) && p.type === 'shrink-pool').reduce((a, p) => a + p.savings, 0);
  const tabs = `<div class="seg cost-tabs"><button data-tab="capacity" class="${tab === 'capacity' ? 'on' : ''}">Capacity</button><button data-tab="plans" class="${tab === 'plans' ? 'on' : ''}">Plans${nPlans ? ` <span class="count">${nPlans}</span>` : ''}</button></div>`;
  const foldTag = params?.get('fold') || '';
  const fold = tab === 'plans' ? await api('/api/cost/fold?' + new URLSearchParams({ tag: foldTag })).catch(() => ({ tags: {}, plans: [] })) : null;
  if (current() !== 'cost') return;
  const body = tab === 'plans' ? plansHTML(rep, fold) : capacityHTML(rep);
  view.innerHTML = `
    ${head('Cost optimization', 'Capacity vs what pods request and really use, per node pool. Advice only: nothing is changed.', '<span data-pick="clusters"></span>')}
    ${tabs}
    <div class="cost-note">💡 Dollar amounts are <b>estimates</b> from public pay-as-you-go list prices for each node's VM size and region (Azure and Linode today). Reservations, savings plans, spot, discounts, disks and networking aren't included, so your bill will differ.${tab === 'capacity' && planSave > 0 ? ` <a href="#cost?tab=plans">See ${nPlans} plan${nPlans === 1 ? '' : 's'} →</a>` : ''}</div>
    ${body}`;
  view.querySelectorAll('[data-tab]').forEach((b) => b.onclick = () => { location.hash = b.dataset.tab === 'plans' ? '#cost?tab=plans' : '#cost'; });
  if (foldTag) view.querySelector('.fold-pick')?.scrollIntoView({ block: 'start' });
  view.querySelectorAll('[data-fold]').forEach((b) => b.onclick = () => {
    const t = b.dataset.fold === foldTag ? '' : b.dataset.fold;
    location.hash = '#cost?' + new URLSearchParams({ tab: 'plans', ...(t ? { fold: t } : {}) });
  });
  return rep;
}

function capacityHTML(rep) {
  const clusters = rep.clusters.filter((c) => inScope('clusters', c.cluster));
  const tot = clusters.reduce((a, c) => ({ monthly: a.monthly + c.monthly, savings: a.savings + c.savings, savingsU: a.savingsU + c.savingsByUsage, nodes: a.nodes + c.nodes, priced: a.priced && c.priced }), { monthly: 0, savings: 0, savingsU: 0, nodes: 0, priced: true });
  const hours = Math.max(0, ...clusters.map((c) => c.historyHours));
  const order = [...clusters].sort((a, b) => b.savings - a.savings || b.savingsByUsage - a.savingsByUsage || a.cluster.localeCompare(b.cluster));
  return `
    <p class="muted plans-intro">${!rep.history ? 'Usage history is off, so this uses requests only. Turn it on in Settings to compare against real usage.' : hours < 24 ? `Based on ${hours < 1 ? 'less than an hour' : `${Math.round(hours)} hours`} of usage history; usage-based advice gets reliable after a day or more of recording.` : `Usage is the 95th percentile over the last ${Math.round(hours / 24)} days of recorded history.`}</p>
    <div class="stats">
      <div class="card stat"><div class="label">Nodes</div><div class="value">${tot.nodes}</div><div class="foot">in ${clusters.length} clusters</div></div>
      <div class="card stat"><div class="label">Est. monthly ${EST}</div><div class="value">${usd(tot.monthly)}</div><div class="foot">${tot.priced ? 'node list prices' : 'some nodes unpriced'}</div></div>
      <div class="card stat"><div class="label">Could save now ${EST}</div><div class="value ${tot.savings > 0 ? 'ok' : ''}">${usd(tot.savings)}/mo</div><div class="foot">by removing spare nodes</div></div>
      <div class="card stat"><div class="label">After right-sizing ${EST}</div><div class="value">${usd(tot.savingsU)}/mo</div><div class="foot">if requests matched usage</div></div>
    </div>
    ${order.map((c) => `<div class="card ccluster">
      <div class="ccluster-head"><b>${esc(c.cluster)}</b>${(c.tags || []).map((t) => `<span class="ms-chip">#${esc(t)}</span>`).join('')}<span class="muted">${esc(c.provider || 'unknown provider')} · ${c.nodes} node${c.nodes === 1 ? '' : 's'}</span><span class="spacer"></span>
        ${c.monthly ? `≈ ${usd(c.monthly)}/mo ${EST}` : ''}${c.savings > 0 ? ` <b class="ok">· save ≈ ${usd(c.savings)}</b>` : ''}</div>
      ${c.pools.map((p) => poolRow(p, rep.history && c.historyHours >= 1)).join('')}
      ${workloadRows(c.workloads)}
    </div>`).join('')}
    ${!rep.clusters.length ? '<div class="card empty">No connected clusters yet.</div>' : ''}`;
}
