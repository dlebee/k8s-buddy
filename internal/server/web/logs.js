// Logs tab: streams a pod's containers, or every pod of a workload merged, over server-sent events.
import { esc, TOKEN } from '/static/common.js';

const MAX_LINES = 5000;
const PALETTE = ['#60a5fa', '#f472b6', '#34d399', '#fbbf24', '#a78bfa', '#f87171', '#2dd4bf', '#fb923c', '#93c5fd', '#c084fc'];
// ---------- ANSI colour codes ----------
// Programs that log with terminal colours (tracing, zap, logrus…) emit SGR escape sequences. We
// render them as styles and keep a plain copy for filtering, copying and error detection.
const ANSI = /\x1b\[[0-9;?]*[ -\/]*[@-~]|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)|\x1b[@-Z\\-_]/g;
const BASE = ['#4b5563', '#f87171', '#4ade80', '#facc15', '#60a5fa', '#e879f9', '#22d3ee', '#d1d5db'];
const BRIGHT = ['#9ca3af', '#fca5a5', '#86efac', '#fde047', '#93c5fd', '#f0abfc', '#67e8f9', '#ffffff'];
function color256(n) {
  if (n < 8) return BASE[n];
  if (n < 16) return BRIGHT[n - 8];
  if (n >= 232) { const v = 8 + (n - 232) * 10; return `rgb(${v},${v},${v})`; }
  n -= 16;
  const c = (x) => (x ? 55 + x * 40 : 0);
  return `rgb(${c(Math.floor(n / 36))},${c(Math.floor(n / 6) % 6)},${c(n % 6)})`;
}
export const stripAnsi = (t) => t.replace(ANSI, '');
export function ansiHTML(t) {
  const st = {};
  let out = '';
  const span = (text) => {
    if (!text) return;
    const css = [];
    if (st.fg) css.push(`color:${st.fg}`);
    if (st.bg) css.push(`background:${st.bg}`);
    if (st.bold) css.push('font-weight:700');
    if (st.dim) css.push('opacity:.6');
    if (st.italic) css.push('font-style:italic');
    if (st.under) css.push('text-decoration:underline');
    out += css.length ? `<span style="${css.join(';')}">${esc(text)}</span>` : esc(text);
  };
  let last = 0;
  for (const m of t.matchAll(ANSI)) {
    span(t.slice(last, m.index));
    last = m.index + m[0].length;
    if (!m[0].endsWith('m') || m[0][1] !== '[') continue; // only colours/styles; drop cursor moves etc.
    const codes = m[0].slice(2, -1).split(';').map((x) => (x === '' ? 0 : +x));
    for (let i = 0; i < codes.length; i++) {
      const c = codes[i];
      if (c === 0) Object.keys(st).forEach((k) => delete st[k]);
      else if (c === 1) st.bold = true;
      else if (c === 2) st.dim = true;
      else if (c === 3) st.italic = true;
      else if (c === 4) st.under = true;
      else if (c === 22) { st.bold = false; st.dim = false; }
      else if (c === 23) st.italic = false;
      else if (c === 24) st.under = false;
      else if (c >= 30 && c <= 37) st.fg = BASE[c - 30];
      else if (c >= 90 && c <= 97) st.fg = BRIGHT[c - 90];
      else if (c >= 40 && c <= 47) st.bg = BASE[c - 40];
      else if (c >= 100 && c <= 107) st.bg = BRIGHT[c - 100];
      else if (c === 39) delete st.fg;
      else if (c === 49) delete st.bg;
      else if ((c === 38 || c === 48) && codes[i + 1] === 5) { st[c === 38 ? 'fg' : 'bg'] = color256(codes[i + 2] || 0); i += 2; }
      else if ((c === 38 || c === 48) && codes[i + 1] === 2) { st[c === 38 ? 'fg' : 'bg'] = `rgb(${codes[i + 2] || 0},${codes[i + 3] || 0},${codes[i + 4] || 0})`; i += 4; }
    }
  }
  span(t.slice(last));
  return out;
}

// Log time in local time: HH:MM:SS.mmm today, with the date in front for older lines.
function stamp(ts) {
  const d = new Date(ts);
  const t = d.toLocaleTimeString([], { hour12: false }) + '.' + String(d.getMilliseconds()).padStart(3, '0');
  return d.toDateString() === new Date().toDateString() ? t : `${d.toLocaleDateString([], { month: 'short', day: 'numeric' })} ${t}`;
}
const LEVEL = [[/\b(panic|fatal|error|err|exception|failed|crit(ical)?)\b|level=error|"level":"error"/i, 'bad'], [/\b(warn(ing)?)\b|level=warn|"level":"warn/i, 'warn']];

export function logsTab(kind, t) {
  const workload = kind !== 'pods';
  const tab = {
    label: 'Logs',
    html: `<div class="lg">
      <div class="lg-bar">
        ${workload ? '<select class="lg-sel" data-l="pod" title="Pod"><option value="">All pods</option></select>' : ''}
        <select class="lg-sel" data-l="container" title="Container"><option value="">All containers</option></select>
        <select class="lg-sel" data-l="tail" title="Lines to load first"><option value="100">last 100</option><option value="500" selected>last 500</option><option value="2000">last 2000</option></select>
        <label class="lg-chk"><input type="checkbox" data-l="follow" checked> Follow</label>
        ${workload ? '' : '<label class="lg-chk" title="Logs of the previous (crashed) container instance"><input type="checkbox" data-l="previous"> Previous</label>'}
        <label class="lg-chk"><input type="checkbox" data-l="ts" checked> Time</label>
        <label class="lg-chk"><input type="checkbox" data-l="wrap" checked> Wrap</label>
        <input class="input lg-q" data-l="q" placeholder="Filter…" spellcheck="false" autocomplete="off">
        <span class="spacer"></span>
        <button class="btn sm" data-l="pause">Pause</button>
        <button class="btn sm" data-l="copy" title="Copy the visible lines">Copy</button>
        <button class="btn sm" data-l="clear">Clear</button>
      </div>
      <div class="lg-status muted" data-l="status">Connecting…</div>
      <div class="lg-legend" data-l="legend" hidden></div>
      <div class="lg-out wrap" data-l="out"></div>
    </div>`,
  };
  let es = null, lines = [], pending = [], paused = false, multiPod = workload, multiC = false, raf = 0, colors = new Map();
  const stop = () => { if (es) { es.close(); es = null; } cancelAnimationFrame(raf); };
  tab.cleanup = stop;
  tab.bind = (root) => {
    const $ = (k) => root.querySelector(`[data-l="${k}"]`);
    const out = $('out'), status = $('status');
    const color = (pod) => { if (!colors.has(pod)) colors.set(pod, PALETTE[colors.size % PALETTE.length]); return colors.get(pod); };
    const shortPod = (p) => { const m = /-([a-z0-9]{4,6}|\d+)$/.exec(p); return m ? m[1] : p; };
    const q = () => $('q').value.trim().toLowerCase();
    const lineHTML = (l) => {
      const lvl = LEVEL.find(([re]) => re.test(l.text))?.[1] || '';
      const pre = (multiPod ? `<b style="color:${color(l.pod)}" title="${esc(l.pod)}">${esc(shortPod(l.pod))}</b>` : '') + (multiC ? `<i>${esc(l.container)}</i>` : '');
      const ts = $('ts').checked && l.ts ? `<s title="${esc(new Date(l.ts).toLocaleString())} · ${esc(l.ts)}">${esc(stamp(l.ts))}</s>` : '';
      return `<div class="lg-l ${l.raw ? '' : lvl}">${ts}${pre}<span>${l.raw ? ansiHTML(l.raw) : esc(l.text)}</span></div>`;
    };
    const match = (l) => { const f = q(); return !f || l.text.toLowerCase().includes(f) || l.pod.toLowerCase().includes(f); };
    const atBottom = () => out.scrollHeight - out.scrollTop - out.clientHeight < 40;
    const renderAll = () => { out.innerHTML = lines.filter(match).map(lineHTML).join(''); out.scrollTop = out.scrollHeight; };
    const flush = () => {
      raf = 0;
      if (paused || !pending.length) return;
      const stick = atBottom();
      const add = pending; pending = [];
      add.sort((a, b) => (a.ts < b.ts ? -1 : a.ts > b.ts ? 1 : 0));
      // Streams start with their own backlog, so keep the merged view in time order.
      const last = lines.at(-1)?.ts || '';
      const inOrder = !last || !add[0].ts || add[0].ts >= last;
      lines.push(...add);
      if (!inOrder) lines.sort((a, b) => (a.ts < b.ts ? -1 : a.ts > b.ts ? 1 : 0));
      if (!inOrder || lines.length > MAX_LINES) {
        lines = lines.slice(-MAX_LINES);
        out.innerHTML = lines.filter(match).map(lineHTML).join('');
      } else {
        out.insertAdjacentHTML('beforeend', add.filter(match).map(lineHTML).join(''));
        while (out.childElementCount > MAX_LINES) out.firstElementChild.remove();
      }
      if (stick) out.scrollTop = out.scrollHeight;
      status.dataset.lines = lines.length;
      setStatus();
    };
    let streams = 0, ended = 0, omitted = 0, failed = [];
    const setStatus = (msg) => {
      if (msg) { status.textContent = msg; return; }
      const live = $('follow').checked && es;
      status.innerHTML = `${live ? '<span class="dot ok"></span> streaming' : 'loaded'} ${streams} container${streams === 1 ? '' : 's'}${omitted ? ` (+${omitted} not shown)` : ''} · ${lines.length.toLocaleString()} lines${paused ? ' · <b>paused</b>' : ''}${failed.length ? ` · <span style="color:var(--bad)" title="${esc(failed.join('\n'))}">${failed.length} failed</span>` : ''}`;
    };
    const start = () => {
      stop();
      lines = []; pending = []; out.innerHTML = ''; streams = 0; ended = 0; omitted = 0; failed = [];
      const p = new URLSearchParams({ cluster: t.cluster, kind, namespace: t.namespace || '', name: t.name, tail: $('tail').value, follow: $('follow').checked ? '1' : '0', t: TOKEN });
      if (workload && $('pod').value) p.set('pod', $('pod').value);
      if ($('container').value) p.set('container', $('container').value);
      if (!workload && $('previous').checked) p.set('previous', '1');
      setStatus('Connecting…');
      let opened = false;
      es = new EventSource('/api/logs?' + p);
      es.addEventListener('meta', (e) => {
        opened = true;
        const m = JSON.parse(e.data);
        streams = m.streams.length; omitted = m.omitted;
        multiPod = workload && !$('pod')?.value && new Set(m.streams.map((s) => s.pod)).size > 1;
        multiC = new Set(m.streams.map((s) => s.container)).size > 1;
        // Colours follow pod order so a pod keeps its colour across reconnects.
        m.pods.forEach(color);
        const legend = $('legend');
        legend.hidden = !multiPod;
        if (multiPod) {
          const shown = [...new Set(m.streams.map((s) => s.pod))];
          legend.innerHTML = shown.map((pod) => `<button class="lg-key" data-pod="${esc(pod)}" title="Show only this pod"><b style="color:${color(pod)}">${esc(shortPod(pod))}</b>${esc(pod)}</button>`).join('');
          legend.querySelectorAll('[data-pod]').forEach((b) => b.onclick = () => { $('pod').value = b.dataset.pod; start(); });
        }
        const fill = (sel, items, all) => { if (!sel || sel.options.length > 1) return; sel.insertAdjacentHTML('beforeend', items.map((x) => `<option>${esc(x)}</option>`).join('')); if (items.length === 1 && all) sel.value = ''; };
        fill(workload ? $('pod') : null, m.pods, true);
        fill($('container'), m.containers, true);
        if (!streams) setStatus('No containers to show.'); else setStatus();
      });
      es.addEventListener('line', (e) => {
        const l = JSON.parse(e.data);
        if (l.text.includes('\x1b')) { l.raw = l.text; l.text = stripAnsi(l.text); } // keep colours, search the plain text
        pending.push(l);
        if (!raf) raf = requestAnimationFrame(flush);
      });
      es.addEventListener('end', (e) => { const x = JSON.parse(e.data); ended++; if (x.error) failed.push(`${x.pod}/${x.container}: ${x.error}`); setStatus(); });
      es.addEventListener('done', () => { es.close(); es = null; flush(); setStatus(); });
      es.onerror = async () => {
        if (!es) return;
        es.close(); es = null;
        if (opened) { setStatus(); status.insertAdjacentHTML('beforeend', ' · <span style="color:var(--warn)">disconnected</span> <a href="#" data-l="re">reconnect</a>'); root.querySelector('[data-l="re"]').onclick = (ev) => { ev.preventDefault(); start(); }; return; }
        // The stream never opened: fetch once to show the server's reason.
        try { const r = await fetch('/api/logs?' + new URLSearchParams({ ...Object.fromEntries(p), follow: '0', tail: '1' }), { headers: { 'X-Buddy-Token': TOKEN } }); setStatus(r.ok ? 'Stream closed.' : (await r.text()).trim()); } catch (err) { setStatus(err.message); }
      };
    };
    ['pod', 'container', 'tail', 'follow', 'previous'].forEach((k) => { const el = $(k); if (el) el.onchange = start; });
    $('wrap').onchange = () => out.classList.toggle('wrap', $('wrap').checked);
    try { if (localStorage.getItem('buddy.logs.ts') === '0') $('ts').checked = false; } catch { /* storage unavailable */ }
    $('ts').onchange = () => { try { localStorage.setItem('buddy.logs.ts', $('ts').checked ? '1' : '0'); } catch { /* ignore */ } renderAll(); };
    let qt; $('q').oninput = () => { clearTimeout(qt); qt = setTimeout(renderAll, 120); };
    $('pause').onclick = () => { paused = !paused; $('pause').textContent = paused ? 'Resume' : 'Pause'; if (!paused && pending.length) flush(); setStatus(); };
    $('clear').onclick = () => { lines = []; out.innerHTML = ''; setStatus(); };
    $('copy').onclick = () => { navigator.clipboard?.writeText(lines.filter(match).map((l) => (multiPod ? l.pod + ' ' : '') + l.text).join('\n')); $('copy').textContent = 'Copied'; setTimeout(() => { $('copy').textContent = 'Copy'; }, 1200); };
    start();
  };
  return tab;
}
