// wirefan demo client.
//
// Dogfoods the packaged client library (clients/js). The built ESM output is
// vendored into web/wirefan-client.js (single dependency-free file) so the
// Go binary can embed it with no bundler or build step; regenerate it with
// `cd clients/js && npm run vendor:web`. This file stays demo-only glue:
// DOM wiring, peer identity colors, the stats panel, and the stress button.
import { WirefanClient, WirefanError } from './wirefan-client.js';

const STATS_CHANNEL = '_wirefan-stats';
const STRESS_CONNS = 50;
const STRESS_HOLD_MS = 10_000;

// Peer identity is DECORATIVE ONLY. It derives a short label and a color
// from the real server-issued socket_id so that two browser windows in the
// same screenshot are visually distinguishable. It adds no server semantics
// and is not a presence or membership feature (see CLAUDE.md, "Deferred").
// The label is the last 4 chars of the socket_id the server already sent;
// the color is a local hash of it. Nothing here is invented data.
const PEER_COLORS = [
  '#9e470f', '#12615f', '#33357f', '#6e2050',
  '#27526e', '#6f520e', '#276030', '#9b2033',
];
function deriveIdentity(sid) {
  if (!sid) return { short: '', color: 'var(--accent)' };
  let h = 2166136261;
  for (let i = 0; i < sid.length; i++) {
    h ^= sid.charCodeAt(i);
    h = Math.imul(h, 16777619);
  }
  return { short: sid.slice(-4), color: PEER_COLORS[(h >>> 0) % PEER_COLORS.length] };
}

// ---------------- DOM ----------------
const $ = (id) => document.getElementById(id);
const els = {
  apiKey: $('apiKey'),
  btnConnect: $('btnConnect'),
  btnDisconnect: $('btnDisconnect'),
  btnSubscribe: $('btnSubscribe'),
  btnPublish: $('btnPublish'),
  btnStress: $('btnStress'),
  statusPill: $('status-pill'),
  socketId: $('socketId'),
  wsEndpoint: $('wsEndpoint'),
  subList: $('subList'),
  channelName: $('channelName'),
  msgBody: $('msgBody'),
  log: $('log'),
  msgCount: $('msgCount'),
  stressStatus: $('stressStatus'),
  statsGrid: $('statsGrid'),
  statsCaption: $('statsCaption'),
  statsRaw: $('statsRaw'),
  statsAge: $('statsAge'),
  peerBadge: $('peerBadge'),
  peerBadgeId: $('peerBadgeId'),
};

// ---------------- state ----------------
let client = null;          // WirefanClient while connected/reconnecting
const subscribed = new Set();
let messageCount = 0;
let stressActive = false;
let lastStatsAt = 0;
let logCleared = false;
const BASE_TITLE = document.title;

// ---------------- helpers ----------------
function setStatus(state, label) {
  els.statusPill.dataset.state = state;
  els.statusPill.textContent = label;
}

function refreshSubList() {
  if (subscribed.size === 0) {
    els.subList.textContent = '—';
  } else {
    els.subList.textContent = [...subscribed].join(', ');
  }
}

function setUiConnected(connected) {
  els.btnConnect.disabled = connected;
  els.btnDisconnect.disabled = !connected;
  els.btnSubscribe.disabled = !connected;
  els.btnPublish.disabled = !connected;
  els.btnStress.disabled = !connected || stressActive;
  if (!connected) {
    els.socketId.textContent = '—';
    els.wsEndpoint.textContent = '—';
    subscribed.clear();
    refreshSubList();
    document.body.classList.remove('is-connected');
    document.documentElement.style.removeProperty('--peer-color');
    els.peerBadgeId.textContent = '';
    document.title = BASE_TITLE;
  }
}

// Build a log row using DOM APIs (no innerHTML &mdash; avoids XSS via frame data).
function logFrame(kind, channel, body, fromSid) {
  if (!logCleared) {
    els.log.replaceChildren();
    logCleared = true;
  }
  const row = document.createElement('div');
  row.className = 'log-row';

  const tsEl = document.createElement('span');
  tsEl.className = 'ts';
  tsEl.textContent = new Date().toISOString().slice(11, 19);

  const bodyEl = document.createElement('span');
  bodyEl.className = 'body';

  const tagEl = document.createElement('span');
  let tagClass = 'evt';
  if (kind === 'system') tagClass = 'sys';
  else if (kind === 'error') tagClass = 'err';
  tagEl.className = `tag ${tagClass}`;
  tagEl.textContent = kind.toUpperCase();
  bodyEl.appendChild(tagEl);

  if (channel) {
    const chEl = document.createElement('span');
    chEl.className = 'ch';
    chEl.textContent = channel;
    bodyEl.appendChild(chEl);
  }

  if (fromSid) {
    const from = deriveIdentity(fromSid);
    const chip = document.createElement('span');
    chip.className = 'peer-chip';
    chip.style.background = from.color;
    chip.textContent = `from ·${from.short}`;
    chip.title = `socket_id ${fromSid}`;
    bodyEl.appendChild(chip);
  }

  bodyEl.appendChild(document.createTextNode(body));

  row.appendChild(tsEl);
  row.appendChild(bodyEl);

  // Newest at top.
  els.log.prepend(row);
  if (kind === 'event') {
    row.classList.add('log-row--new');
    row.addEventListener('animationend', () => row.classList.remove('log-row--new'), { once: true });
  }
  while (els.log.children.length > 200) {
    els.log.removeChild(els.log.lastChild);
  }
  if (kind === 'event') {
    messageCount++;
    els.msgCount.textContent = `${messageCount} received`;
  }
}

// Shared per-channel event renderer (peer chip + payload).
function renderChannelEvent(ev) {
  let payload = ev.data;
  let fromSid = null;
  if (payload !== null && typeof payload === 'object' && !Array.isArray(payload)
      && typeof payload._from === 'string') {
    fromSid = payload._from;
    payload = { ...payload };
    delete payload._from;
  }
  const data = typeof payload === 'string' ? payload : JSON.stringify(payload);
  const id = ev.id ? ` id=${ev.id}` : '';
  logFrame('event', ev.channel, `${data}${id}`, fromSid);
}

// ---------------- connect ----------------
function readKey() {
  const fromInput = els.apiKey.value.trim();
  if (fromInput) return fromInput;
  const params = new URLSearchParams(location.search);
  const fromUrl = params.get('key');
  if (fromUrl) {
    els.apiKey.value = fromUrl;
    return fromUrl;
  }
  return '';
}

async function connect() {
  const key = readKey();
  if (!key) {
    logFrame('error', '', 'no api key. paste one or use ?key=...');
    return;
  }
  if (client) return;

  const c = new WirefanClient({ url: location.origin, key });
  client = c;
  els.wsEndpoint.textContent =
    `${location.protocol === 'https:' ? 'wss' : 'ws'}://${location.host}/v1/connect`;
  setStatus('connecting', 'connecting');

  c.on('connected', ({ socketId, reconnected }) => {
    setStatus('connected', 'connected');
    setUiConnected(true);
    els.socketId.textContent = socketId;
    const me = deriveIdentity(socketId);
    document.documentElement.style.setProperty('--peer-color', me.color);
    els.peerBadgeId.textContent = `·${me.short}`;
    document.body.classList.add('is-connected');
    document.title = `wirefan · ${me.short}`;
    logFrame('system', '', `${reconnected ? 'reconnected' : 'connected'} sid=${socketId}`);
  });
  c.on('reconnecting', ({ attempt, delayMs }) => {
    setStatus('connecting', `reconnecting #${attempt}`);
    logFrame('system', '', `reconnecting: attempt ${attempt} in ${delayMs}ms`);
  });
  c.on('resubscribed', ({ channels }) => {
    logFrame('system', '', `resubscribed: ${channels.join(', ')}`);
  });
  c.on('disconnected', ({ code, reason, willReconnect }) => {
    if (!willReconnect) return;
    logFrame('system', '', `connection lost (${reason || code || 'network'})`);
  });
  c.on('error', (err) => {
    logFrame('error', '', err instanceof WirefanError ? err.message : String(err.message || err));
  });
  c.on('closed', ({ reason }) => {
    if (client === c) client = null;
    setStatus('idle', 'idle');
    setUiConnected(false);
    if (reason === 'exhausted') logFrame('system', '', 'closed: reconnect attempts exhausted');
  });

  try {
    await c.connect();
  } catch (e) {
    if (client === c) client = null;
    setStatus('error', 'error');
    logFrame('error', '', String(e.message || e));
    return;
  }

  // Auto-subscribe to the stats system channel. Automatic resubscription
  // restores it after reconnects; only the first subscribe happens here.
  try {
    await c.subscribe(STATS_CHANNEL, (ev) => renderStats(ev.data));
    logFrame('system', STATS_CHANNEL, 'subscribed');
  } catch (e) {
    if (e instanceof WirefanError && e.code === 'RESERVED_CHANNEL') {
      // Degrade the stats panel honestly on server builds that keep the
      // reserved channel closed to clients.
      els.statsCaption.textContent = 'client subscription to the _wirefan-stats reserved channel is not enabled on this server build.';
      els.statsGrid.style.display = 'none';
      els.statsRaw.textContent = 'stats channel not open to clients on this server build';
    } else {
      logFrame('error', '', String(e.message || e));
    }
  }
}

function disconnect() {
  if (!client) return;
  const c = client;
  client = null;
  c.close(); // explicit close: the library never reconnects after this
}

// ---------------- stats ----------------
function renderStats(data) {
  if (!data) return;
  let snap = data;
  if (typeof snap === 'string') {
    try { snap = JSON.parse(snap); } catch (_) { /* leave as string */ }
  }
  if (typeof snap === 'object' && snap !== null) {
    els.statsGrid.style.display = '';
    els.statsGrid.querySelectorAll('[data-stat]').forEach((node) => {
      const key = node.dataset.stat;
      if (key in snap) node.textContent = formatStat(snap[key]);
    });
    els.statsRaw.textContent = JSON.stringify(snap, null, 2);
  } else {
    els.statsRaw.textContent = String(snap);
  }
  lastStatsAt = Date.now();
  updateStatsAge();
}

function formatStat(v) {
  if (typeof v === 'number') return v.toLocaleString();
  return String(v);
}

function updateStatsAge() {
  if (!lastStatsAt) {
    els.statsAge.textContent = '—';
    return;
  }
  const dt = Math.floor((Date.now() - lastStatsAt) / 1000);
  els.statsAge.textContent = `${dt}s ago`;
}
setInterval(updateStatsAge, 1000);

// ---------------- subscribe / publish ----------------
async function subscribe() {
  const ch = els.channelName.value.trim();
  if (!ch || !client) return;
  try {
    await client.subscribe(ch, renderChannelEvent);
    subscribed.add(ch);
    refreshSubList();
    logFrame('system', ch, 'subscribed');
  } catch (e) {
    logFrame('error', '', String(e.message || e));
  }
}

function publish() {
  const ch = els.channelName.value.trim();
  if (!ch || !client) return;
  let data;
  const raw = els.msgBody.value.trim();
  try { data = JSON.parse(raw); }
  catch (_) { data = raw; }
  // The protocol's event frame carries no publisher identity
  // (docs/PROTOCOL.md), so provenance has to ride in the client-authored
  // payload. Objects only: never silently promote a user's string or
  // number payload into an object. Any client can put any value here,
  // so the receiving chip is a display convenience, not an identity claim.
  // Skip the stats channel: its snapshots come from the server and the raw
  // pane shows them verbatim.
  const sid = client.socketId;
  if (ch !== STATS_CHANNEL && sid && data !== null
      && typeof data === 'object' && !Array.isArray(data)) {
    data = { ...data, _from: sid };
  }
  try {
    client.publish(ch, data);
  } catch (e) {
    logFrame('error', '', String(e.message || e));
  }
}

// ---------------- stress test ----------------
// Raw WebSockets on purpose: phantom connections should NOT reconnect or
// resubscribe, they exist only to push the live connection counter up.
async function runStress() {
  if (stressActive) return;
  const key = readKey();
  if (!key) { logFrame('error', '', 'stress: need an api key'); return; }
  stressActive = true;
  els.btnStress.disabled = true;
  let opened = 0, closed = 0;
  const updateLabel = () => {
    els.stressStatus.textContent = `phantom: opened ${opened}/${STRESS_CONNS}, closed ${closed}`;
  };
  updateLabel();
  const wsScheme = location.protocol === 'https:' ? 'wss' : 'ws';
  const url = `${wsScheme}://${location.host}/v1/connect?key=${encodeURIComponent(key)}`;
  const sockets = [];
  for (let i = 0; i < STRESS_CONNS; i++) {
    try {
      const s = new WebSocket(url);
      s.onopen = () => { opened++; updateLabel(); };
      s.onclose = () => { closed++; updateLabel(); };
      s.onerror = () => { /* swallow &mdash; counted by close */ };
      sockets.push(s);
    } catch (e) {
      logFrame('error', '', `stress dial: ${e}`);
    }
    // Tiny stagger so we don't slam the loop.
    if (i % 10 === 9) await new Promise((r) => setTimeout(r, 30));
  }
  // Hold for STRESS_HOLD_MS, then close all.
  setTimeout(() => {
    sockets.forEach((s) => {
      try { s.close(1000, 'stress-end'); } catch (_) { /* ignore */ }
    });
    stressActive = false;
    els.btnStress.disabled = !(client && client.state === 'connected');
    els.stressStatus.textContent = `done: opened ${opened}, closed ${closed}`;
  }, STRESS_HOLD_MS);
}

// ---------------- wiring ----------------
els.btnConnect.addEventListener('click', connect);
els.btnDisconnect.addEventListener('click', disconnect);
els.btnSubscribe.addEventListener('click', subscribe);
els.btnPublish.addEventListener('click', publish);
els.btnStress.addEventListener('click', runStress);

// Pre-fill key from URL if present.
const initial = new URLSearchParams(location.search).get('key');
if (initial) els.apiKey.value = initial;

setStatus('idle', 'idle');
refreshSubList();
