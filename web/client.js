// wirefan live demo: page logic.
//
// Dogfoods the packaged client library (clients/js). Its built ESM output is
// vendored into web/wirefan-client.js (one dependency-free file) so the Go
// binary can embed it with no bundler or build step; regenerate it with
// `cd clients/js && npm run vendor:web`, never by hand. This file is demo glue
// only: the connection lifecycle, the live diagram (diagram.js), the stats
// strip, the arrivals feed and the "under the hood" developer panel.
//
// Divergence from the design spec (feature 11, "three panels: connection,
// messages, live stats"): all three panels still exist, collapsed under
// "Under the hood", and the page now leads with a live fanout diagram so a
// visitor who is not an engineer sees what fanout means in the first
// seconds. The capped stress button (50 sockets, 10 s) and the two-tab
// invitation above the fold are unchanged.
import { WirefanClient, WirefanError } from './wirefan-client.js';
import { FanoutDiagram } from './diagram.js';

const STATS_CHANNEL = '_wirefan-stats';
const DEFAULT_CHANNEL = 'demo';

// Publish budget for this tab. The demo key is shared by every visitor
// (100 publishes/s across all of them, docs/PROTOCOL.md section 9), so a tab
// never sends more than 4 per second, and nothing is ever sent on a timer:
// every publish on this page starts with a click.
const PULSE_RATE = 4;   // tokens per second
const PULSE_BURST = 4;

const STRESS_CONNS = 50;
const STRESS_HOLD_MS = 10_000;
const FEED_MAX = 5;
const FRAMES_MAX = 200;
const RATE_WINDOW_MS = 5_000;
const ANNOUNCE_GAP_MS = 4_000;
const NAMED_TTL_MS = 10 * 60_000;
const ULID_RE = /^[0-9A-HJKMNP-TV-Z]{26}$/;
const HOOD_KEY = 'wirefan-demo:hood-open';

// ---------------- DOM ----------------
const $ = (id) => document.getElementById(id);
const els = {
  status: $('status'), statusText: $('statusText'),
  stage: $('stage'), wire: $('wire'), stageChannel: $('stageChannel'), stageCount: $('stageCount'),
  stageLast: $('stageLast'),
  overlay: $('stageOverlay'), overlayTitle: $('overlayTitle'), overlayBody: $('overlayBody'),
  overlayAction: $('overlayAction'),
  btnPulse: $('btnPulse'), btnTab: $('btnTab'), hint: $('hint'), notice: $('notice'),
  statConns: $('statConns'), statRate: $('statRate'), statRecv: $('statRecv'), statRtt: $('statRtt'),
  feed: $('feed'), announcer: $('announcer'),
  hood: $('hood'),
  kvState: $('kvState'), kvSocket: $('kvSocket'), kvEndpoint: $('kvEndpoint'), kvProto: $('kvProto'),
  kvSubs: $('kvSubs'), kvReconnects: $('kvReconnects'),
  keyForm: $('keyForm'), apiKey: $('apiKey'), btnConnect: $('btnConnect'), btnDisconnect: $('btnDisconnect'),
  frames: $('frames'), showStats: $('showStats'), btnClearFrames: $('btnClearFrames'),
  pubForm: $('pubForm'), pubBody: $('pubBody'), pubChannel: $('pubChannel'), btnPublish: $('btnPublish'),
  statsGrid: $('statsGrid'), statsCaption: $('statsCaption'), statsRaw: $('statsRaw'), statsAge: $('statsAge'),
  serverRate: $('serverRate'),
  btnStress: $('btnStress'), stressStatus: $('stressStatus'),
};

// ---------------- config from the URL ----------------
const params = new URLSearchParams(location.search);
const CHANNEL = pickChannel(params.get('channel'));
let activeKey = (params.get('key') || '').trim();

// Public channels only: `_` names are reserved for the server, and
// private-/presence- channels need a token minted by an app server, which a
// static demo page does not have.
function pickChannel(raw) {
  if (!raw) return DEFAULT_CHANNEL;
  if (raw.startsWith('_') || raw.startsWith('private-') || raw.startsWith('presence-')) return DEFAULT_CHANNEL;
  if (/[\u0000-\u001f\u007f]/.test(raw)) return DEFAULT_CHANNEL;
  if (new TextEncoder().encode(raw).length > 128) return DEFAULT_CHANNEL;
  return raw;
}

// ---------------- state ----------------
let client = null;            // WirefanClient while connecting, live or reconnecting
let connState = 'idle';       // idle | connecting | live | reconnecting | closed | error
let connDetail = null;
let mySid = null;
let demoSubscribed = false;
let statsSubscribed = false;
let reconnects = 0;
let userClosed = false;
let connections = null;       // from the latest _wirefan-stats snapshot
let lastStatsAt = 0;
let prevPublished = null;     // { v, t } for the server-wide publish rate
let received = 0;
const recvTimes = [];
let pulseSeq = 0;
const ownPending = new Map(); // pulse n -> { t0, hubAt }
const named = new Map();      // sid -> last time (Date.now()) an event carried it in _from
let stressActive = false;
let overlayTimer = null;
const BASE_TITLE = document.title;

const reducedMotion = window.matchMedia('(prefers-reduced-motion: reduce)');
const diagram = new FanoutDiagram(els.wire, { reducedMotion });

// ---------------- helpers ----------------
const short = (sid) => (sid ? sid.slice(-4) : '');
const pad2 = (n) => String(n).padStart(2, '0');
function clock() {
  const d = new Date();
  return `${pad2(d.getHours())}:${pad2(d.getMinutes())}:${pad2(d.getSeconds())}`;
}
function node(tag, cls, text) {
  const n = document.createElement(tag);
  if (cls) n.className = cls;
  if (text !== undefined) n.textContent = text;
  return n;
}
function plural(n, word) { return `${n} ${word}${n === 1 ? '' : 's'}`; }

// Client-side token bucket for publishes (see PULSE_RATE above).
const bucket = { tokens: PULSE_BURST, at: performance.now() };
function refill() {
  const now = performance.now();
  bucket.tokens = Math.min(PULSE_BURST, bucket.tokens + ((now - bucket.at) / 1000) * PULSE_RATE);
  bucket.at = now;
}
function takeToken() {
  refill();
  if (bucket.tokens < 1) return false;
  bucket.tokens -= 1;
  return true;
}

let noticeTimer = null;
function showNotice(text, tone = 'warn', ms = 4000) {
  els.notice.dataset.tone = tone;
  els.notice.textContent = text;
  els.hint.classList.add('is-covered'); // the notice takes the hint's slot, so nothing shifts
  clearTimeout(noticeTimer);
  noticeTimer = setTimeout(() => {
    els.notice.textContent = '';
    els.hint.classList.remove('is-covered');
  }, ms);
}

// Screen readers hear arrivals in batches, never one announcement per event.
let announceQueue = { count: 0, last: '' };
let announceTimer = null;
function announce(text) {
  announceQueue.count += 1;
  announceQueue.last = text;
  if (!announceTimer) announceTimer = setTimeout(flushAnnounce, 1200);
}
function flushAnnounce() {
  const { count, last } = announceQueue;
  announceQueue = { count: 0, last: '' };
  if (!count) { announceTimer = null; return; }
  els.announcer.textContent = count === 1 ? last : `${count} messages arrived. Latest: ${last}`;
  announceTimer = setTimeout(flushAnnounce, ANNOUNCE_GAP_MS);
}

// ---------------- raw frame tap ----------------
// A WebSocket that copies every frame into the raw-frames panel. The client
// library takes an injected implementation (clients/js README, "Node /
// injection"); this one changes nothing about what goes over the wire.
class TapSocket extends WebSocket {
  constructor(url, protocols) {
    super(url, protocols);
    this.addEventListener('message', (ev) => {
      if (typeof ev.data === 'string') tapFrame('in', ev.data);
    });
  }
  send(data) {
    if (typeof data === 'string') tapFrame('out', data);
    super.send(data);
  }
}

let framesEmpty = true;
function tapFrame(dir, text) {
  let msg = null;
  try { msg = JSON.parse(text); } catch (_) { /* shown verbatim below */ }
  const type = msg && typeof msg.type === 'string' ? msg.type : '?';
  if (type === 'connected' && typeof msg.version === 'string') els.kvProto.textContent = msg.version;

  if (framesEmpty) { els.frames.replaceChildren(); framesEmpty = false; }
  const li = node('li');
  li.dataset.kind = type;
  if (msg && msg.channel === STATS_CHANNEL) li.dataset.stats = '';
  li.append(node('span', 'ts', clock()));
  li.append(node('span', `dir dir-${dir}`, dir === 'in' ? 'IN' : 'OUT'));
  const body = node('span', 'body');
  body.append(node('span', 'ty', type), ' ', text);
  li.append(body);
  els.frames.prepend(li);
  while (els.frames.children.length > FRAMES_MAX) els.frames.lastElementChild.remove();
}

// ---------------- connection ----------------
function endpointFor(key) {
  const scheme = location.protocol === 'https:' ? 'wss' : 'ws';
  return `${scheme}://${location.host}/v1/connect?key=${encodeURIComponent(key)}`;
}

async function connect(key) {
  if (!key) { setConnState('idle'); return; }
  if (client) dropClient();
  userClosed = false;
  activeKey = key;
  const c = new WirefanClient({
    url: location.origin,
    key,
    // Bounded retries so a server that stays down ends in a clear
    // "could not reach" state instead of retrying forever.
    reconnect: { maxAttempts: 8 },
    webSocket: TapSocket,
  });
  client = c;
  els.kvEndpoint.textContent = endpointFor(key);
  setConnState('connecting');

  c.on('connected', ({ socketId, reconnected }) => {
    if (client !== c) return;
    mySid = socketId;
    els.kvSocket.textContent = socketId;
    diagram.setMe(short(socketId));
    document.title = `wirefan · ${short(socketId)}`;
    if (reconnected) {
      reconnects += 1;
      els.kvReconnects.textContent = String(reconnects);
      // The client resubscribes on its own; the page is live again once
      // it reports the restore pass.
      setConnState('connecting', 'restoring subscriptions');
    }
  });
  c.on('resubscribed', () => {
    if (client === c && c.state === 'connected' && demoSubscribed) setConnState('live');
  });
  c.on('reconnecting', ({ attempt, delayMs }) => {
    if (client !== c) return;
    setConnState('reconnecting', { attempt, delayMs });
  });
  c.on('disconnected', ({ willReconnect }) => {
    if (client !== c || !willReconnect) return;
    setConnState('reconnecting', { attempt: 0 });
  });
  c.on('error', onClientError);
  c.on('closed', ({ reason }) => {
    if (client !== c) return;
    client = null;
    resetSession();
    setConnState(reason === 'exhausted' ? 'closed' : 'idle');
  });

  try {
    await c.connect();
  } catch (e) {
    if (client === c) {
      client = null;
      resetSession();
      setConnState('error', { message: String((e && e.message) || e) });
    }
    return;
  }
  if (client !== c) return;

  try {
    await c.subscribe(CHANNEL, onChannelEvent);
    demoSubscribed = true;
  } catch (e) {
    if (client === c) {
      showNotice(`Could not join the ${CHANNEL} channel: ${(e && e.message) || e}`, 'err', 8000);
      setConnState('error', { message: String((e && e.message) || e) });
    }
    return;
  }
  if (client !== c) return;
  renderSubs();
  setConnState('live');

  // The stats system channel feeds the connection count. Automatic
  // resubscription restores it after reconnects; only the first subscribe
  // happens here.
  try {
    await c.subscribe(STATS_CHANNEL, onStats);
    statsSubscribed = true;
    renderSubs();
  } catch (e) {
    if (e instanceof WirefanError && e.code === 'RESERVED_CHANNEL') {
      els.statsCaption.textContent = 'This server build keeps the _wirefan-stats channel closed to clients, so the connection count is unavailable.';
      els.statsGrid.hidden = true;
      els.statsRaw.textContent = 'stats channel not open to clients on this server build';
    } else if (client === c) {
      showNotice(`Server stats are unavailable: ${(e && e.message) || e}`, 'info');
    }
  }
}

// Explicit close: the library never reconnects after this.
function dropClient() {
  const c = client;
  client = null;
  if (c) c.close();
  resetSession();
}

function disconnect() {
  if (!client) return;
  userClosed = true;
  dropClient();
  setConnState('idle');
}

function resetSession() {
  mySid = null;
  demoSubscribed = false;
  statsSubscribed = false;
  connections = null;
  prevPublished = null;
  ownPending.clear();
  els.kvSocket.textContent = '–';
  els.statConns.textContent = '–';
  document.title = BASE_TITLE;
  diagram.setMe('');
  renderSubs();
  refreshPeers();
}

function renderSubs() {
  const subs = [];
  if (demoSubscribed) subs.push(CHANNEL);
  if (statsSubscribed) subs.push(STATS_CHANNEL);
  els.kvSubs.textContent = subs.length ? subs.join(', ') : '–';
}

function onClientError(err) {
  const code = err instanceof WirefanError ? err.code : '';
  const op = err instanceof WirefanError ? err.op : undefined;
  if (code === 'RATE_LIMITED' || code === 'RATE_LIMITED_CONN') {
    if (!op || op === 'publish') refusePending();
    showNotice(code === 'RATE_LIMITED'
      ? 'Everyone here shares this demo key and it just hit its rate limit, so the server refused that one. Try again in a moment.'
      : 'This tab reached its per-connection limit. Try again in a moment.', 'warn');
    return;
  }
  if (code === 'NOT_SUBSCRIBED') {
    refusePending();
    showNotice('Still rejoining the channel after a reconnect. Try again in a moment.', 'warn');
    return;
  }
  showNotice(`The server said: ${(err && err.message) || code || 'unknown error'}`, 'err', 6000);
}

// A refused publish never comes back: drop the oldest pending pulse and let
// the hub show the refusal.
function refusePending() {
  const first = ownPending.keys().next();
  if (!first.done) ownPending.delete(first.value);
  diagram.refuse();
}

// ---------------- connection state -> UI ----------------
function setConnState(state, detail = null) {
  connState = state;
  connDetail = detail;
  const labels = {
    idle: 'Offline', connecting: 'Connecting', live: 'Live',
    reconnecting: 'Reconnecting', closed: 'Disconnected', error: 'Error',
  };
  els.status.dataset.state = state;
  els.statusText.textContent = labels[state];
  let kv = state;
  if (typeof detail === 'string') kv += ` (${detail})`;
  else if (detail && detail.attempt) kv += ` (attempt ${detail.attempt}, in ${detail.delayMs} ms)`;
  els.kvState.textContent = kv;
  els.stage.classList.toggle('is-live', state === 'live');
  diagram.setOffline(state !== 'live');

  const live = state === 'live';
  els.btnPulse.disabled = !live;
  els.btnPublish.disabled = !live;
  els.btnStress.disabled = !live || stressActive;
  els.btnDisconnect.disabled = !client;
  els.btnConnect.textContent = client ? 'Reconnect with this key' : 'Connect';

  renderOverlay();
  refreshPeers(); // also re-renders the hint and the stage count
}

function renderOverlay() {
  clearTimeout(overlayTimer);
  let title = '';
  let body = '';
  let action = null;
  switch (connState) {
    case 'live':
      els.overlay.hidden = true;
      return;
    case 'idle':
      if (!activeKey) {
        title = 'No demo key in this link';
        body = 'Links to this demo carry one as ?key=. Running wirefan yourself? Paste a key id.';
        action = ['Paste a key id', openKeyForm];
      } else {
        title = userClosed ? 'Disconnected' : 'Offline';
        body = userClosed ? 'You closed the connection.' : '';
        action = ['Reconnect', () => connect(activeKey)];
      }
      break;
    case 'connecting':
      title = connDetail === 'restoring subscriptions' ? 'Reconnected' : 'Connecting';
      body = connDetail === 'restoring subscriptions'
        ? 'Rejoining the channel.'
        : 'Opening a WebSocket to the server.';
      break;
    case 'reconnecting': {
      title = 'Connection lost';
      const n = connDetail && connDetail.attempt;
      body = n
        ? `Reconnecting (attempt ${n} of 8). Messages sent while this tab is away are not replayed.`
        : 'Reconnecting. Messages sent while this tab is away are not replayed.';
      break;
    }
    case 'closed':
      title = 'Could not reach the server';
      body = 'Gave up after 8 attempts.';
      action = ['Try again', () => connect(activeKey)];
      break;
    case 'error':
      title = 'Could not connect';
      body = connDetail && connDetail.message ? connDetail.message : '';
      action = ['Try again', () => connect(activeKey)];
      break;
    default:
      break;
  }
  const paint = () => {
    els.overlayTitle.textContent = title;
    els.overlayBody.textContent = body;
    if (action) {
      els.overlayAction.textContent = action[0];
      els.overlayAction.onclick = action[1];
      els.overlayAction.hidden = false;
    } else {
      els.overlayAction.hidden = true;
      els.overlayAction.onclick = null;
    }
    els.overlay.hidden = false;
  };
  // A healthy connect takes a few milliseconds; only show the "Connecting"
  // veil if it is still going after a beat, so a normal load never flashes.
  if (connState === 'connecting' && els.overlay.hidden) overlayTimer = setTimeout(paint, 700);
  else paint();
}

function othersCount() {
  return connections === null ? null : Math.max(0, connections - 1);
}

function renderHint() {
  const others = othersCount();
  let text;
  if (connState === 'live') {
    if (others === null) text = 'Connected. Counting the other connections (server stats arrive every 5 seconds).';
    else if (others === 0 && diagram.total === 0) text = 'Only this tab is connected right now. Open a second tab, put the two side by side, and send a pulse from either one.';
    else text = 'Your pulse reaches every tab on this demo, and theirs arrive here. A terminal gets a name once that tab sends something.';
  } else if (connState === 'connecting' || connState === 'reconnecting') {
    text = 'Waiting for the connection. The client library reconnects on its own.';
  } else if (!activeKey) {
    text = 'Offline.';
  } else {
    text = 'Offline. Reconnect to send pulses.';
  }
  els.hint.textContent = text;
  els.hint.hidden = !activeKey && connState === 'idle';

  if (connState === 'reconnecting') els.stageCount.textContent = 'reconnecting';
  else if (connState !== 'live' && connState !== 'connecting') els.stageCount.textContent = 'offline';
  else if (others === null) els.stageCount.textContent = 'waiting for server stats';
  else {
    const shown = diagram.total;
    const drawn = diagram.slots.length;
    if (shown === 0) els.stageCount.textContent = 'only this tab';
    else if (shown > drawn) els.stageCount.textContent = `${plural(shown, 'other connection')}, ${drawn} drawn`;
    else els.stageCount.textContent = plural(shown, 'other connection');
  }
}

// ---------------- peers ----------------
// The diagram draws max(stats count, peers heard from in the last 6 s):
// a tab that just opened can publish before the next 5 s stats snapshot
// counts it. Names come only from _from; there is no presence feature.
function refreshPeers() {
  const now = Date.now();
  for (const [sid, t] of named) if (now - t > NAMED_TTL_MS) named.delete(sid);
  const recent = [...named.entries()].sort((a, b) => b[1] - a[1]);
  const statOthers = othersCount() ?? 0;
  const fresh = recent.filter(([, t]) => now - t < 6_000).length;
  const total = client ? Math.max(statOthers, fresh) : 0;
  const list = recent.slice(0, total).map(([sid]) => ({ sid, label: `·${short(sid)}` }));
  diagram.setPeers(total, list);
  renderHint();
}

// ---------------- incoming events ----------------
function onChannelEvent(ev) {
  const now = performance.now();
  received += 1;
  recvTimes.push(now);
  els.statRecv.textContent = received.toLocaleString();

  const data = ev.data;
  const isObj = data !== null && typeof data === 'object' && !Array.isArray(data);
  // _from rides in the payload because the event frame carries no publisher
  // identity (docs/PROTOCOL.md). Any client can write anything there, so it
  // is a display hint, not an identity claim; only ULID-shaped values count.
  const from = isObj && typeof data._from === 'string' && ULID_RE.test(data._from) ? data._from : null;
  const mine = from !== null && from === mySid;
  const isPulse = isObj && data.kind === 'pulse';

  let rtt = null;
  let hubAt = null;
  if (mine && isPulse && ownPending.has(data.n)) {
    const p = ownPending.get(data.n);
    ownPending.delete(data.n);
    rtt = now - p.t0;
    hubAt = p.hubAt;
    els.statRtt.replaceChildren(String(Math.max(1, Math.round(rtt))), node('small', '', 'ms'));
  }
  if (from && !mine) {
    named.set(from, Date.now());
    refreshPeers();
  }
  diagram.deliver({ from: mine ? 'me' : from, hubAt });
  addFeedRow({ mine, from, rtt, isPulse, data, id: ev.id });

  // The newest event's real protocol facts, in the corner of the diagram.
  let last = `event ${ev.id || ''}`;
  if (mine) last += rtt !== null ? `, yours, back in ${Math.max(1, Math.round(rtt))} ms` : ', yours';
  else if (from) last += ` from ·${short(from)}`;
  els.stageLast.textContent = last;
}

function addFeedRow({ mine, from, rtt, isPulse, data, id }) {
  const empty = els.feed.querySelector('.feed-empty');
  if (empty) empty.remove();
  const li = node('li', 'is-new');
  li.addEventListener('animationend', () => li.classList.remove('is-new'), { once: true });
  if (id) li.title = `event id ${id}`;
  li.append(node('span', 't', clock()));
  const what = node('span', 'what');
  const noun = isPulse ? 'pulse' : 'message';
  let spoken;
  if (mine) {
    what.append(`Your ${noun} came back `);
    if (rtt !== null) what.append('in ', node('span', 'ms', `${Math.max(1, Math.round(rtt))} ms`));
    spoken = rtt !== null ? `Your ${noun} came back in ${Math.round(rtt)} milliseconds.` : `Your ${noun} came back.`;
  } else if (from) {
    what.append(`${isPulse ? 'Pulse' : 'Message'} from `, node('span', 'chip', `·${short(from)}`));
    spoken = `${isPulse ? 'Pulse' : 'Message'} from tab ${short(from)}.`;
  } else {
    what.append('Message from an unnamed client');
    spoken = 'Message from an unnamed client.';
  }
  if (!isPulse) {
    let preview;
    if (typeof data === 'string') preview = data;
    else {
      const copy = data !== null && typeof data === 'object' && !Array.isArray(data) ? { ...data } : data;
      if (copy && typeof copy === 'object' && !Array.isArray(copy)) delete copy._from;
      preview = JSON.stringify(copy);
    }
    if (preview && preview.length > 72) preview = `${preview.slice(0, 71)}…`;
    what.append(node('span', 'preview', preview));
  }
  li.append(what);
  els.feed.prepend(li);
  while (els.feed.children.length > FEED_MAX) els.feed.lastElementChild.remove();
  announce(spoken);
}

// ---------------- stats ----------------
function onStats(ev) {
  let snap = ev.data;
  if (typeof snap === 'string') {
    try { snap = JSON.parse(snap); } catch (_) { /* leave as string */ }
  }
  if (!snap || typeof snap !== 'object') {
    els.statsRaw.textContent = String(snap);
    return;
  }
  lastStatsAt = Date.now();
  if (typeof snap.connections === 'number') {
    connections = snap.connections;
    els.statConns.textContent = connections.toLocaleString();
  }
  if (typeof snap.published === 'number') {
    const t = performance.now();
    if (prevPublished && snap.published >= prevPublished.v && t > prevPublished.t) {
      const r = (snap.published - prevPublished.v) / ((t - prevPublished.t) / 1000);
      els.serverRate.textContent = r < 10 ? r.toFixed(1) : Math.round(r).toLocaleString();
    }
    prevPublished = { v: snap.published, t };
  }
  els.statsGrid.hidden = false;
  els.statsGrid.querySelectorAll('[data-stat]').forEach((n) => {
    const k = n.dataset.stat;
    if (k in snap) n.textContent = typeof snap[k] === 'number' ? snap[k].toLocaleString() : String(snap[k]);
  });
  els.statsRaw.textContent = JSON.stringify(snap, null, 2);
  updateStatsAge();
  refreshPeers();
}

function updateStatsAge() {
  els.statsAge.textContent = lastStatsAt ? `${Math.floor((Date.now() - lastStatsAt) / 1000)}s ago` : '–';
}

function updateRate() {
  const now = performance.now();
  while (recvTimes.length && now - recvTimes[0] > RATE_WINDOW_MS) recvTimes.shift();
  const r = recvTimes.length / (RATE_WINDOW_MS / 1000);
  els.statRate.textContent = r === 0 ? '0' : r < 10 ? r.toFixed(1) : String(Math.round(r));
}

// Display refresh only; this timer never sends anything.
setInterval(() => {
  updateRate();
  updateStatsAge();
}, 500);

// ---------------- publishing ----------------
function canPublish() {
  return connState === 'live' && client && client.state === 'connected' && mySid;
}

let coolTimer = null;
function coolDown() {
  refill();
  const waitMs = Math.max(120, Math.ceil(((1 - bucket.tokens) / PULSE_RATE) * 1000));
  els.btnPulse.classList.add('is-cooling');
  clearTimeout(coolTimer);
  coolTimer = setTimeout(() => els.btnPulse.classList.remove('is-cooling'), waitMs);
  showNotice('Pulses are capped at 4 a second per tab, because every visitor shares this demo key.', 'info', 2600);
}

function sendPulse() {
  if (!canPublish()) return;
  if (!takeToken()) { coolDown(); return; }
  pulseSeq += 1;
  const n = pulseSeq;
  const hubAt = diagram.beginOwnPulse();
  ownPending.set(n, { t0: performance.now(), hubAt });
  // Forget pulses whose echo never came (refused, or lost to a reconnect).
  for (const [k, p] of ownPending) if (performance.now() - p.t0 > 10_000) ownPending.delete(k);
  try {
    client.publish(CHANNEL, { kind: 'pulse', n, _from: mySid });
  } catch (e) {
    ownPending.delete(n);
    diagram.refuse();
    showNotice(`Not sent: ${(e && e.message) || e}`, 'err');
  }
}

function publishCustom() {
  if (!canPublish()) return;
  if (!takeToken()) { coolDown(); return; }
  const raw = els.pubBody.value.trim();
  let data;
  try { data = JSON.parse(raw); } catch (_) { data = raw; }
  // Objects only: a string or number payload is never promoted to an object.
  if (data !== null && typeof data === 'object' && !Array.isArray(data)) data = { ...data, _from: mySid };
  try {
    client.publish(CHANNEL, data);
  } catch (e) {
    showNotice(`Not sent: ${(e && e.message) || e}`, 'err');
  }
}

// ---------------- stress test ----------------
// Raw WebSockets on purpose: phantom connections should NOT reconnect or
// resubscribe; they exist only to push the live connection counter up.
async function runStress() {
  if (stressActive || !activeKey) return;
  stressActive = true;
  els.btnStress.disabled = true;
  let opened = 0;
  let closed = 0;
  const label = () => { els.stressStatus.textContent = `opened ${opened}/${STRESS_CONNS}, closed ${closed}`; };
  label();
  const sockets = [];
  for (let i = 0; i < STRESS_CONNS; i++) {
    try {
      const s = new WebSocket(endpointFor(activeKey));
      s.onopen = () => { opened += 1; label(); };
      s.onclose = () => { closed += 1; label(); };
      s.onerror = () => { /* the close handler counts it */ };
      sockets.push(s);
    } catch (e) {
      showNotice(`Stress dial failed: ${e}`, 'err');
    }
    if (i % 10 === 9) await new Promise((r) => setTimeout(r, 30));
  }
  setTimeout(() => {
    sockets.forEach((s) => { try { s.close(1000, 'stress-end'); } catch (_) { /* ignore */ } });
    stressActive = false;
    els.btnStress.disabled = connState !== 'live';
    els.stressStatus.textContent = `done: opened ${opened}, closed ${closed}`;
  }, STRESS_HOLD_MS);
}

// ---------------- developer panel ----------------
function openKeyForm() {
  els.hood.open = true;
  els.hood.scrollIntoView({ behavior: reducedMotion.matches ? 'auto' : 'smooth', block: 'start' });
  setTimeout(() => els.apiKey.focus({ preventScroll: true }), reducedMotion.matches ? 0 : 350);
}

function setUrlKey(key) {
  const u = new URL(location.href);
  u.searchParams.set('key', key);
  history.replaceState(null, '', u);
  els.btnTab.href = location.href;
}

try {
  if (localStorage.getItem(HOOD_KEY) === '1' || location.hash === '#hood') els.hood.open = true;
} catch (_) { /* storage blocked: stay collapsed */ }
els.hood.addEventListener('toggle', () => {
  try { localStorage.setItem(HOOD_KEY, els.hood.open ? '1' : '0'); } catch (_) { /* ignore */ }
});

// ---------------- wiring ----------------
els.btnPulse.addEventListener('click', sendPulse);
els.keyForm.addEventListener('submit', (e) => {
  e.preventDefault();
  const key = els.apiKey.value.trim();
  if (!key) { els.apiKey.focus(); return; }
  setUrlKey(key);
  connect(key);
});
els.btnDisconnect.addEventListener('click', disconnect);
els.pubForm.addEventListener('submit', (e) => { e.preventDefault(); publishCustom(); });
els.showStats.addEventListener('change', () => els.frames.classList.toggle('show-stats', els.showStats.checked));
els.btnClearFrames.addEventListener('click', () => {
  els.frames.replaceChildren(node('li', 'frames-empty', 'Cleared.'));
  framesEmpty = true;
});
els.btnStress.addEventListener('click', runStress);
reducedMotion.addEventListener('change', () => diagram.layout(true));

els.stageChannel.textContent = CHANNEL;
els.pubChannel.textContent = CHANNEL;
els.btnTab.href = location.href;
els.apiKey.value = activeKey;

if (activeKey) connect(activeKey);
else setConnState('idle');
