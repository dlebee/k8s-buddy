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

export async function costView(view, head, current) {
  view.innerHTML = `${head('Cost optimization', 'Capacity vs what pods request and really use, per node pool. Advice only: nothing is changed.')}<div class="card empty">Working it out…</div>`;
  const rep = await api('/api/cost');
  if (current() !== 'cost') return;
  const clusters = rep.clusters.filter((c) => inScope('clusters', c.cluster));
  const tot = clusters.reduce((a, c) => ({ monthly: a.monthly + c.monthly, savings: a.savings + c.savings, savingsU: a.savingsU + c.savingsByUsage, nodes: a.nodes + c.nodes, priced: a.priced && c.priced }), { monthly: 0, savings: 0, savingsU: 0, nodes: 0, priced: true });
  const hours = Math.max(0, ...clusters.map((c) => c.historyHours));
  const cons = rep.consolidations.filter((c) => inScope('clusters', c.from));
  const order = [...clusters].sort((a, b) => b.savings - a.savings || b.savingsByUsage - a.savingsByUsage || a.cluster.localeCompare(b.cluster));
  view.innerHTML = `
    ${head('Cost optimization', 'Capacity vs what pods request and really use, per node pool. Advice only: nothing is changed.', '<span data-pick="clusters"></span>')}
    <div class="cost-note">💡 Dollar amounts are <b>estimates</b> from public pay-as-you-go list prices for each node's VM size and region (Azure and Linode today). Reservations, savings plans, spot, discounts, disks and networking aren't included, so your bill will differ.
      ${!rep.history ? '<br>Usage history is off, so this uses requests only. Turn it on in Settings to compare against real usage.' : hours < 24 ? `<br>Based on ${hours < 1 ? 'less than an hour' : `${Math.round(hours)} hours`} of usage history; usage-based advice gets reliable after a day or more of recording.` : `<br>Usage is the 95th percentile over the last ${Math.round(hours / 24)} days of recorded history.`}</div>
    <div class="stats">
      <div class="card stat"><div class="label">Nodes</div><div class="value">${tot.nodes}</div><div class="foot">in ${clusters.length} clusters</div></div>
      <div class="card stat"><div class="label">Est. monthly ${EST}</div><div class="value">${usd(tot.monthly)}</div><div class="foot">${tot.priced ? 'node list prices' : 'some nodes unpriced'}</div></div>
      <div class="card stat"><div class="label">Could save now ${EST}</div><div class="value ${tot.savings > 0 ? 'ok' : ''}">${usd(tot.savings)}/mo</div><div class="foot">by removing spare nodes</div></div>
      <div class="card stat"><div class="label">After right-sizing ${EST}</div><div class="value">${usd(tot.savingsU)}/mo</div><div class="foot">if requests matched usage</div></div>
    </div>
    ${cons.length ? `<div class="section-title">Across tagged clusters</div>${cons.map((c) => `<div class="card cons">
      <div><b>${esc(c.from)}</b> → <b>${esc(c.into)}</b> <span class="ms-chip">#${esc(c.tag)}</span>${c.priced ? `<span class="spacer"></span><b class="ok">≈ ${usd(c.savings)}/mo ${EST}</b>` : ''}</div>
      <div class="muted">${c.workloads} workload${c.workloads === 1 ? '' : 's'} (${fmtCPU(c.moveCpu)}, ${fmtMem(c.moveMem)} requested) fit in ${esc(c.into)}'s spare ${fmtCPU(c.spareCpu)} / ${fmtMem(c.spareMem)}${c.alreadyThere ? ` · ${c.alreadyThere} already run there` : ''}. Retiring ${esc(c.from)} would save its nodes.</div>
      ${c.withVolumes.length ? `<div class="cons-flag warn">💾 ${c.withVolumes.length} with volumes: data must be migrated (${c.withVolumes.slice(0, 4).map(esc).join(', ')}${c.withVolumes.length > 4 ? ', …' : ''})</div>` : ''}
      ${c.pinned.length ? `<div class="cons-flag">📌 ${c.pinned.length} pinned to specific nodes (selectors/affinity/hostPath): check ${esc(c.into)} has matching nodes (${c.pinned.slice(0, 4).map(esc).join(', ')}${c.pinned.length > 4 ? ', …' : ''})</div>` : ''}
    </div>`).join('')}` : ''}
    ${order.map((c) => `<div class="card ccluster">
      <div class="ccluster-head"><b>${esc(c.cluster)}</b>${(c.tags || []).map((t) => `<span class="ms-chip">#${esc(t)}</span>`).join('')}<span class="muted">${esc(c.provider || 'unknown provider')} · ${c.nodes} node${c.nodes === 1 ? '' : 's'}</span><span class="spacer"></span>
        ${c.monthly ? `≈ ${usd(c.monthly)}/mo ${EST}` : ''}${c.savings > 0 ? ` <b class="ok">· save ≈ ${usd(c.savings)}</b>` : ''}</div>
      ${c.pools.map((p) => poolRow(p, rep.history && c.historyHours >= 1)).join('')}
      ${workloadRows(c.workloads)}
    </div>`).join('')}
    ${!Object.keys(rep.clusters).length ? '<div class="card empty">No connected clusters yet.</div>' : ''}
    <p class="muted rv-small">Tip: tag clusters in Settings (e.g. <code>prod</code>, <code>eastus</code>) to get suggestions for folding one cluster into another with spare capacity.</p>`;
  return rep;
}
