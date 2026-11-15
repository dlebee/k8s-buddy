// Cron schedules as Kubernetes CronJobs read them (5 fields, names, steps, ranges, @macros), turned
// into plain English plus upcoming run times. No imports so it can be tested on its own.

const MACROS = { '@yearly': '0 0 1 1 *', '@annually': '0 0 1 1 *', '@monthly': '0 0 1 * *', '@weekly': '0 0 * * 0', '@daily': '0 0 * * *', '@midnight': '0 0 * * *', '@hourly': '0 * * * *' };
const MONTHS = ['January', 'February', 'March', 'April', 'May', 'June', 'July', 'August', 'September', 'October', 'November', 'December'];
const DAYS = ['Sunday', 'Monday', 'Tuesday', 'Wednesday', 'Thursday', 'Friday', 'Saturday'];
const MON_ABBR = ['jan', 'feb', 'mar', 'apr', 'may', 'jun', 'jul', 'aug', 'sep', 'oct', 'nov', 'dec'];
const DAY_ABBR = ['sun', 'mon', 'tue', 'wed', 'thu', 'fri', 'sat'];
const SPECS = [
  { name: 'minute', min: 0, max: 59 },
  { name: 'hour', min: 0, max: 23 },
  { name: 'day of month', min: 1, max: 31 },
  { name: 'month', min: 1, max: 12, names: MON_ABBR, base: 1 },
  { name: 'day of week', min: 0, max: 7, names: DAY_ABBR, base: 0 },
];

function parseField(text, spec) {
  const val = (s) => {
    const i = spec.names ? spec.names.indexOf(s.toLowerCase()) : -1;
    const n = i >= 0 ? i + spec.base : /^\d+$/.test(s) ? +s : NaN;
    if (!(n >= spec.min && n <= spec.max)) throw new Error(`bad ${spec.name} value "${s}"`);
    return n;
  };
  const set = new Set();
  const parts = [];
  for (const part of text.split(',')) {
    const [range, stepS] = part.split('/');
    const step = stepS == null ? 1 : +stepS;
    if (!(step >= 1) || (stepS != null && !/^\d+$/.test(stepS))) throw new Error(`bad step in ${spec.name} "${part}"`);
    let lo, hi;
    if (range === '*' || range === '?') { lo = spec.min; hi = spec.name === 'day of week' ? 6 : spec.max; }
    else if (range.includes('-')) { [lo, hi] = range.split('-').map(val); if (lo > hi) throw new Error(`bad range in ${spec.name} "${range}"`); }
    else { lo = val(range); hi = stepS != null ? spec.max : lo; }
    for (let v = lo; v <= hi; v += step) set.add(spec.name === 'day of week' && v === 7 ? 0 : v);
    parts.push({ star: range === '*' || range === '?', lo, hi, step });
  }
  return { set, parts, star: parts.length === 1 && parts[0].star && parts[0].step === 1, text };
}

export function parseCron(expr) {
  let e = String(expr || '').trim();
  let tz = null;
  const m = /^(?:CRON_TZ|TZ)=(\S+)\s+(.*)$/.exec(e);
  if (m) { tz = m[1]; e = m[2]; }
  if (/^@every\s/.test(e)) throw new Error('@every intervals are not supported by CronJobs');
  e = MACROS[e.toLowerCase()] || e;
  const f = e.split(/\s+/);
  if (f.length !== 5) throw new Error(`expected 5 fields, got ${f.length}`);
  const [minute, hour, dom, month, dow] = f.map((t, i) => parseField(t, SPECS[i]));
  return { minute, hour, dom, month, dow, tz };
}

// ---------- plain English ----------
const pad = (n) => String(n).padStart(2, '0');
const list = (xs) => xs.length <= 1 ? xs.join('') : `${xs.slice(0, -1).join(', ')} and ${xs.at(-1)}`;
const ord = (n) => n + (n % 100 >= 11 && n % 100 <= 13 ? 'th' : ['th', 'st', 'nd', 'rd'][n % 10] || 'th');
const single = (f) => f.parts.length === 1 && f.parts[0].lo === f.parts[0].hi && !f.star;
const everyN = (f) => f.parts.length === 1 && f.parts[0].star && f.parts[0].step > 1 ? f.parts[0].step : 0;
const values = (f) => [...f.set].sort((a, b) => a - b);

function namedRange(f, names) {
  // "Monday through Friday", "Monday, Wednesday and Friday", "every 2nd month"
  return list(f.parts.map((p) => {
    if (p.star && p.step > 1) return `every ${ord(p.step)} ${names === DAYS ? 'day of the week' : 'month'}`;
    const at = (v) => names === MONTHS ? names[v - 1] : names[v % 7];
    const lo = at(p.lo), hi = at(p.hi);
    if (p.lo === p.hi) return lo;
    if (p.step > 1) return `every ${ord(p.step)} from ${lo} through ${hi}`;
    return `${lo} through ${hi}`;
  }));
}

function timePart(c) {
  const { minute: mi, hour: h } = c;
  if (mi.star && h.star) return 'every minute';
  const nm = everyN(mi);
  if (nm && h.star) return `every ${nm} minutes`;
  if (single(mi) && h.star) return mi.parts[0].lo === 0 ? 'every hour, on the hour' : `every hour at ${pad(mi.parts[0].lo)} past`;
  const nh = everyN(h);
  if (single(mi) && nh) return `every ${nh} hours at :${pad(mi.parts[0].lo)} (${values(h).slice(0, 4).map((x) => pad(x) + ':' + pad(mi.parts[0].lo)).join(', ')}${values(h).length > 4 ? ', …' : ''})`;
  if (mi.set.size * h.set.size <= 6 && !h.star && !mi.star) {
    const times = values(h).flatMap((x) => values(mi).map((y) => `${pad(x)}:${pad(y)}`));
    return `at ${list(times)}`;
  }
  const range = h.parts.length === 1 && h.parts[0].lo !== h.parts[0].hi && h.parts[0].step === 1 && !h.star ? h.parts[0] : null;
  if (range && (nm || mi.star)) return `every ${nm ? nm + ' minutes' : 'minute'} between ${pad(range.lo)}:00 and ${pad(range.hi)}:59`;
  if (range && single(mi)) return `every hour from ${pad(range.lo)}:${pad(mi.parts[0].lo)} to ${pad(range.hi)}:${pad(mi.parts[0].lo)}`;
  const mins = mi.star ? 'every minute' : nm ? `every ${nm} minutes` : `at minute ${list(values(mi).map(String))}`;
  const hrs = h.star ? '' : nh ? ` of every ${ord(nh)} hour` : ` past hour ${list(values(h).map(String))}`;
  return mins + hrs;
}

function dayPart(c) {
  const { dom, month, dow } = c;
  let s;
  if (single(dom) && single(month) && dow.star) return `on ${MONTHS[month.parts[0].lo - 1]} ${ord(dom.parts[0].lo)}`;
  if (dom.star && dow.star) s = 'every day';
  else if (dom.star) s = `on ${namedRange(dow, DAYS)}`;
  else {
    const n = everyN(dom);
    const days = n ? `every ${ord(n)} day of the month` : `on the ${list(values(dom).map(ord))} of the month`;
    s = dow.star ? days : `${days}, and on ${namedRange(dow, DAYS)}`;
  }
  if (!month.star) s += ` in ${namedRange(month, MONTHS)}`;
  return s;
}

export function describeCron(expr) {
  const c = parseCron(expr);
  const t = timePart(c), d = dayPart(c);
  const text = d === 'every day' ? (/^every (minute|\d+ minutes|hour)/.test(t) ? t : `${t}, every day`) : `${t}, ${d}`;
  return text.charAt(0).toUpperCase() + text.slice(1);
}

// ---------- next runs ----------
const fmts = new Map();
function wall(date, tz) {
  let f = fmts.get(tz);
  if (!f) {
    f = new Intl.DateTimeFormat('en-US', { timeZone: tz, hourCycle: 'h23', year: 'numeric', month: 'numeric', day: 'numeric', hour: 'numeric', minute: 'numeric', weekday: 'short' });
    fmts.set(tz, f);
  }
  const p = Object.fromEntries(f.formatToParts(date).map((x) => [x.type, x.value]));
  return { month: +p.month, day: +p.day, hour: +p.hour % 24, minute: +p.minute, dow: DAY_ABBR.indexOf(p.weekday.toLowerCase().slice(0, 3)) };
}

// nextRuns returns the next `count` run times after `from`, evaluating the schedule in `tz`.
export function nextRuns(expr, tz = 'UTC', count = 5, from = new Date()) {
  const c = parseCron(expr);
  tz = c.tz || tz;
  const domR = !c.dom.star, dowR = !c.dow.star;
  const dayOK = (w) => {
    if (domR && dowR) return c.dom.set.has(w.day) || c.dow.set.has(w.dow); // cron's OR rule
    return (!domR || c.dom.set.has(w.day)) && (!dowR || c.dow.set.has(w.dow));
  };
  const out = [];
  let t = Math.floor(from.getTime() / 60000) * 60000 + 60000;
  const limit = t + 30 * 366 * 86400000; // Feb 29 needs decades for five runs; Feb 30 never runs
  for (let guard = 0; out.length < count && t < limit && guard < 200000; guard++) {
    const w = wall(new Date(t), tz);
    if (!c.month.set.has(w.month) || !dayOK(w)) { t += ((23 - w.hour) * 60 + (60 - w.minute)) * 60000; continue; }
    if (!c.hour.set.has(w.hour)) { t += (60 - w.minute) * 60000; continue; }
    if (!c.minute.set.has(w.minute)) { t += 60000; continue; }
    out.push(new Date(t));
    t += 60000;
  }
  return out;
}
