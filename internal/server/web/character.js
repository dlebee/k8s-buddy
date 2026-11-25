import { api, stream, RANK } from '/static/common.js';

const buddy = document.getElementById('buddy');
const badge = document.getElementById('badge');
const bubble = document.getElementById('bubble');
const startedAt = Date.now();
const WARMUP_MS = 45000;
let cfg = { nudgeLevel: 'notice', showBubble: true };
let queue = [];
let warmupAnnounced = false;
let bubbleTimer, queueTimer;
let last = null;

function mood(s) {
  buddy.classList.remove('idle', 'happy', 'alert', 'critical');
  const sev = s.bySeverity || {};
  if (sev.critical) buddy.classList.add('critical');
  else if (s.nudgeable > 0) buddy.classList.add('alert');
  else buddy.classList.add('happy');
}

function setBadge(s) {
  const n = s.nudgeable;
  badge.textContent = n > 99 ? '99+' : String(n);
  badge.classList.toggle('on', n > 0);
  const sev = s.bySeverity || {};
  badge.classList.toggle('warn', !sev.critical && !!sev.warning);
  badge.classList.toggle('calm', !sev.critical && !sev.warning);
}

function bounce() {
  buddy.classList.remove('bounce');
  void buddy.offsetWidth;
  buddy.classList.add('bounce');
}

function say(title, text, ms = 7000) {
  if (!cfg.showBubble) return;
  document.getElementById('bubbleTitle').textContent = title;
  document.getElementById('bubbleText').textContent = text;
  bubble.classList.add('on');
  clearTimeout(bubbleTimer);
  bubbleTimer = setTimeout(() => bubble.classList.remove('on'), ms);
}

function flush() {
  const items = queue; queue = [];
  if (!items.length) return;
  bounce();
  if (items.length === 1) {
    say(items[0].cluster, items[0].title);
  } else {
    const clusters = new Set(items.map((a) => a.cluster));
    say(`${items.length} new things to look at`, `across ${clusters.size} cluster${clusters.size > 1 ? 's' : ''} · click me`);
  }
}

// The Doctor's latest checkup: a badge with its count, and a "psst" when it found more than the
// buddy last mentioned (remembered across launches, so the same findings aren't announced again).
const DOC_KEY = 'buddy.doctorAnnounced';
let docAt = '';
function setDoctor(d) {
  const el = document.getElementById('doc');
  if (!d) return;
  const n = d.ran ? d.findings : 0;
  document.getElementById('docN').textContent = n > 99 ? '99+' : String(n);
  el.classList.toggle('on', n > 0);
  el.title = n ? `Doctor: ${n} thing${n === 1 ? '' : 's'} need attention (click me)` : 'Doctor';
  if (!d.ran || d.at === docAt) return;
  docAt = d.at;
  let announced = -1;
  try { announced = Number(localStorage.getItem(DOC_KEY) ?? -1); } catch { /* no storage */ }
  if (n > 0 && n > announced) {
    setTimeout(() => {
      bounce();
      say('Psst… the doctor has a few notes', `${n} thing${n === 1 ? '' : 's'} need${n === 1 ? 's' : ''} attention${d.clusters ? ` on ${d.clusters} cluster${d.clusters === 1 ? '' : 's'}` : ''} · click me`, 9000);
    }, bubble.classList.contains('on') ? 8000 : 0);
  }
  try { localStorage.setItem(DOC_KEY, String(n)); } catch { /* no storage */ }
}

// A newer release: a small arrow badge, and one "psst" per version (remembered across launches).
const UPD_KEY = 'buddy.updateAnnounced';
function setUpdate(u) {
  const el = document.getElementById('upd');
  const on = !!u?.available;
  el.classList.toggle('on', on);
  if (!on) return;
  el.title = `k8s-buddy ${u.latest} is out (click me)`;
  let announced = '';
  try { announced = localStorage.getItem(UPD_KEY) || ''; } catch { /* no storage */ }
  if (announced === u.latest) return;
  try { localStorage.setItem(UPD_KEY, u.latest); } catch { /* no storage */ }
  setTimeout(() => say('Psst… there\'s a new me', `k8s-buddy ${u.latest} is out · click me for the download`, 9000), bubble.classList.contains('on') ? 9000 : 0);
}

function onSummary(s) {
  cfg = { nudgeLevel: s.nudgeLevel, showBubble: s.showBubble };
  last = s;
  setBadge(s);
  mood(s);
  setDoctor(s.doctor);
  setUpdate(s.update);
}

function onAlert(a) {
  if (RANK[a.severity] < RANK[cfg.nudgeLevel]) return;
  if (Date.now() - startedAt < WARMUP_MS) return;
  queue.push(a);
  clearTimeout(queueTimer);
  queueTimer = setTimeout(flush, 1500);
}

setTimeout(() => {
  if (warmupAnnounced || !last) return;
  warmupAnnounced = true;
  if (last.nudgeable > 0) {
    bounce();
    say('Hi! I checked your clusters', `${last.nudgeable} thing${last.nudgeable > 1 ? 's' : ''} across ${last.clusters} cluster${last.clusters > 1 ? 's' : ''} could use a look · click me`, 10000);
  } else {
    say('Hi! All quiet so far', "I'll nudge you if something breaks.", 5000);
  }
}, WARMUP_MS);

api('/api/summary').then(onSummary).catch(() => {});
stream({ summary: onSummary, alert: onAlert });
